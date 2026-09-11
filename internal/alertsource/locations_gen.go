//go:build ignore

// locations_gen.go regenerates locations_table.go from the published
// alerts.in.ua location table.
//
//	go run locations_gen.go              # fetch the published sheet
//	go run locations_gen.go -csv x.csv   # regenerate from a saved copy
//
// It is committed beside the table it writes so that the next person can
// regenerate rather than hand-edit 1,622 rows. Hand-editing the table is the
// one thing that must not happen: a wrong parent link means a device
// announcing for the wrong oblast, and the checks below are the only thing
// standing between the sheet and that outcome.
//
// # Why this is not a runtime download
//
// A device is commissioned on a workbench with no internet, off a phone
// hotspot, or on a LAN that has not been given a route yet. The location list
// has to be there before the first poll succeeds, so it is compiled in.
//
// # How the hierarchy is derived, and why not from row order
//
// The sheet has no parent column. Its rows read as an oblast, then that
// oblast's raions, each followed by its hromadas — so the obvious derivation
// is "the parent is the most recent row of the level above". That derivation
// is wrong, and quietly.
//
// "Автономна Республіка Крим" is listed second, between "Волинська область"
// and Волинь's own four raions, and it has no raions of its own in the sheet.
// The most-recent-oblast rule therefore hands all four Волинь raions — and
// the 58 hromadas under them — to Crimea. A device in Луцьк would broaden to
// an oblast 1,000 km away and announce for alerts nobody near it can hear.
//
// So raion → oblast is not derived from order at all. It is taken from
// raionOblast below, an independent list of Ukraine's 136 post-2020 raions
// and their oblasts, and the generator refuses to emit anything unless the
// sheet's raion set matches that list exactly, name for name. Only
// hromada → raion is taken from row order, where no misplaced row exists to
// corrupt it (verifyOrder checks that too).
package main

import (
	"bytes"
	"encoding/csv"
	"flag"
	"fmt"
	"go/format"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// sheetURL is the published table. It 307-redirects to a googleusercontent
// host; http.Client follows that by default.
const sheetURL = "https://docs.google.com/spreadsheets/d/1XnTOzcPHd1LZUrarR1Fk43FUyl8Ae6a6M7pcwDRjNdA/export?format=csv&gid=0"

// The sheet's four "Тип" values, and the counts each one must have. The
// counts are assertions, not documentation: Ukraine has 24 oblasts plus the
// Autonomous Republic of Crimea, two cities of special status, and 136
// raions of which 10 are in Crimea and absent here. A sheet that grew or
// shrank is a sheet somebody restructured, and this generator should stop
// rather than emit a table whose shape nobody checked.
const (
	typeOblast      = "Область"
	typeRaion       = "Район"
	typeHromada     = "Громада"
	typeSpecialCity = "Місто з спеціальним статусом"

	wantOblasts       = 25
	wantSpecialCities = 2
	wantRaions        = 126
	wantHromadas      = 1469
)

// raionOblast is Ukraine's post-2020 raion list, read off the Wikipedia
// article "Райони України" rather than off the sheet, so that it is genuinely
// independent evidence about the one link that matters. Crimea's ten raions
// are listed there and not here: the sheet carries none of them, and
// verifyRaions would reject an entry it never sees.
//
// Format is one "raion|oblast" per line so that a diff against the article
// stays readable.
const raionOblast = `
Вінницький район|Вінницька область
Гайсинський район|Вінницька область
Жмеринський район|Вінницька область
Могилів-Подільський район|Вінницька область
Тульчинський район|Вінницька область
Хмільницький район|Вінницька область
Володимирський район|Волинська область
Камінь-Каширський район|Волинська область
Ковельський район|Волинська область
Луцький район|Волинська область
Дніпровський район|Дніпропетровська область
Кам'янський район|Дніпропетровська область
Криворізький район|Дніпропетровська область
Нікопольський район|Дніпропетровська область
Павлоградський район|Дніпропетровська область
Самарівський район|Дніпропетровська область
Синельниківський район|Дніпропетровська область
Бахмутський район|Донецька область
Волноваський район|Донецька область
Горлівський район|Донецька область
Донецький район|Донецька область
Кальміуський район|Донецька область
Краматорський район|Донецька область
Маріупольський район|Донецька область
Покровський район|Донецька область
Бердичівський район|Житомирська область
Житомирський район|Житомирська область
Звягельський район|Житомирська область
Коростенський район|Житомирська область
Берегівський район|Закарпатська область
Мукачівський район|Закарпатська область
Рахівський район|Закарпатська область
Тячівський район|Закарпатська область
Ужгородський район|Закарпатська область
Хустський район|Закарпатська область
Бердянський район|Запорізька область
Василівський район|Запорізька область
Запорізький район|Запорізька область
Мелітопольський район|Запорізька область
Пологівський район|Запорізька область
Верховинський район|Івано-Франківська область
Івано-Франківський район|Івано-Франківська область
Калуський район|Івано-Франківська область
Коломийський район|Івано-Франківська область
Косівський район|Івано-Франківська область
Надвірнянський район|Івано-Франківська область
Білоцерківський район|Київська область
Бориспільський район|Київська область
Броварський район|Київська область
Бучанський район|Київська область
Вишгородський район|Київська область
Обухівський район|Київська область
Фастівський район|Київська область
Голованівський район|Кіровоградська область
Кропивницький район|Кіровоградська область
Новоукраїнський район|Кіровоградська область
Олександрійський район|Кіровоградська область
Алчевський район|Луганська область
Довжанський район|Луганська область
Луганський район|Луганська область
Ровеньківський район|Луганська область
Сватівський район|Луганська область
Сіверськодонецький район|Луганська область
Старобільський район|Луганська область
Щастинський район|Луганська область
Дрогобицький район|Львівська область
Золочівський район|Львівська область
Львівський район|Львівська область
Самбірський район|Львівська область
Стрийський район|Львівська область
Шептицький район|Львівська область
Яворівський район|Львівська область
Баштанський район|Миколаївська область
Вознесенський район|Миколаївська область
Миколаївський район|Миколаївська область
Первомайський район|Миколаївська область
Березівський район|Одеська область
Білгород-Дністровський район|Одеська область
Болградський район|Одеська область
Ізмаїльський район|Одеська область
Одеський район|Одеська область
Подільський район|Одеська область
Роздільнянський район|Одеська область
Кременчуцький район|Полтавська область
Лубенський район|Полтавська область
Миргородський район|Полтавська область
Полтавський район|Полтавська область
Вараський район|Рівненська область
Дубенський район|Рівненська область
Рівненський район|Рівненська область
Сарненський район|Рівненська область
Конотопський район|Сумська область
Охтирський район|Сумська область
Роменський район|Сумська область
Сумський район|Сумська область
Шосткинський район|Сумська область
Кременецький район|Тернопільська область
Тернопільський район|Тернопільська область
Чортківський район|Тернопільська область
Берестинський район|Харківська область
Богодухівський район|Харківська область
Ізюмський район|Харківська область
Куп'янський район|Харківська область
Лозівський район|Харківська область
Харківський район|Харківська область
Чугуївський район|Харківська область
Бериславський район|Херсонська область
Генічеський район|Херсонська область
Каховський район|Херсонська область
Скадовський район|Херсонська область
Херсонський район|Херсонська область
Кам'янець-Подільський район|Хмельницька область
Хмельницький район|Хмельницька область
Шепетівський район|Хмельницька область
Звенигородський район|Черкаська область
Золотоніський район|Черкаська область
Уманський район|Черкаська область
Черкаський район|Черкаська область
Вижницький район|Чернівецька область
Дністровський район|Чернівецька область
Чернівецький район|Чернівецька область
Корюківський район|Чернігівська область
Ніжинський район|Чернігівська область
Новгород-Сіверський район|Чернігівська область
Прилуцький район|Чернігівська область
Чернігівський район|Чернігівська область
`

// ubillingRegions are the 25 keys the six Ubilling cascade sources report,
// copied from Regions() in snapshot.go. Every one of them has to be a name in
// the sheet, or a device already in the field — every one of which stores one
// of these strings in alerts.region — would stop resolving to a location the
// moment this table became the thing that resolves it.
var ubillingRegions = []string{
	"Вінницька область", "Волинська область", "Дніпропетровська область",
	"Донецька область", "Житомирська область", "Закарпатська область",
	"Запорізька область", "Івано-Франківська область", "Київська область",
	"Кіровоградська область", "Луганська область", "Львівська область",
	"Миколаївська область", "Одеська область", "Полтавська область",
	"Рівненська область", "Сумська область", "Тернопільська область",
	"Харківська область", "Херсонська область", "Хмельницька область",
	"Черкаська область", "Чернівецька область", "Чернігівська область",
	"м. Київ",
}

type row struct {
	uid  int
	name string
	typ  string
}

type loc struct {
	uid    int
	name   string
	label  string
	kind   string
	parent int
}

func main() {
	csvPath := flag.String("csv", "", "read the table from this file instead of fetching the published sheet")
	out := flag.String("out", "locations_table.go", "file to write")
	flag.Parse()

	// Provenance is always the published sheet, even when -csv replayed a
	// saved copy of it: the table's origin is the sheet, and a local path
	// from somebody's laptop is not something the next reader can follow.
	data, err := load(*csvPath)
	if err != nil {
		log.Fatal(err)
	}
	rows, err := parse(data)
	if err != nil {
		log.Fatal(err)
	}
	if err := verifyCounts(rows); err != nil {
		log.Fatal(err)
	}
	if err := verifyRaions(rows); err != nil {
		log.Fatal(err)
	}
	if err := verifyOrder(rows); err != nil {
		log.Fatal(err)
	}
	locs, err := build(rows)
	if err != nil {
		log.Fatal(err)
	}
	if err := verifyLocations(locs); err != nil {
		log.Fatal(err)
	}
	src, err := emit(locs)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(*out, src, 0o644); err != nil {
		log.Fatal(err)
	}
	log.Printf("wrote %s: %d locations", *out, len(locs))
}

func load(path string) ([]byte, error) {
	if path != "" {
		return os.ReadFile(path)
	}
	hc := &http.Client{Timeout: 60 * time.Second}
	resp, err := hc.Get(sheetURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: http %d", sheetURL, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// parse skips the sheet's three-line title block and its header row, then
// reads every row that starts with a numeric UID. FieldsPerRecord is unset
// because the title lines have a different width from the data.
func parse(data []byte) ([]row, error) {
	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = -1
	records, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("parse csv: %w", err)
	}
	var header int
	for i, rec := range records {
		if len(rec) >= 3 && rec[0] == "UID" && rec[2] == "Тип" {
			header = i + 1
			break
		}
	}
	if header == 0 {
		return nil, fmt.Errorf("no %q header row found; the sheet's columns changed", "UID,Назва,Тип,Примітки")
	}
	var out []row
	for _, rec := range records[header:] {
		if len(rec) < 3 || strings.TrimSpace(rec[0]) == "" {
			continue
		}
		uid, err := strconv.Atoi(strings.TrimSpace(rec[0]))
		if err != nil {
			return nil, fmt.Errorf("row %q: UID is not a number: %w", rec, err)
		}
		name := strings.TrimSpace(rec[1])
		typ := strings.TrimSpace(rec[2])
		switch typ {
		case typeOblast, typeRaion, typeHromada, typeSpecialCity:
		default:
			return nil, fmt.Errorf("row %d (%s): unknown Тип %q", uid, name, typ)
		}
		if name == "" {
			return nil, fmt.Errorf("row %d has no name", uid)
		}
		out = append(out, row{uid: uid, name: name, typ: typ})
	}
	return out, nil
}

func verifyCounts(rows []row) error {
	seen := map[int]string{}
	count := map[string]int{}
	for _, r := range rows {
		if prev, dup := seen[r.uid]; dup {
			return fmt.Errorf("UID %d is used twice: %q and %q", r.uid, prev, r.name)
		}
		seen[r.uid] = r.name
		count[r.typ]++
	}
	want := map[string]int{
		typeOblast:      wantOblasts,
		typeSpecialCity: wantSpecialCities,
		typeRaion:       wantRaions,
		typeHromada:     wantHromadas,
	}
	for typ, n := range want {
		if count[typ] != n {
			return fmt.Errorf("sheet has %d rows of type %q, expected %d; "+
				"the administrative division changed or the sheet was restructured — "+
				"check the change before raising the constant", count[typ], typ, n)
		}
	}
	return nil
}

// verifyRaions is the check the whole generator exists for. It compares the
// sheet's raion set against raionOblast — the independent list — and fails on
// any difference in either direction, so a renamed, added or removed raion
// stops the build instead of silently re-parenting the hromadas under it.
func verifyRaions(rows []row) error {
	ref := referenceRaions()
	sheet := map[string]bool{}
	for _, r := range rows {
		if r.typ != typeRaion {
			continue
		}
		if !ref[r.name] {
			return fmt.Errorf("sheet raion %q (UID %d) is not in the independent list; "+
				"check it against the current administrative division before adding it", r.name, r.uid)
		}
		sheet[r.name] = true
	}
	var missing []string
	for name := range ref {
		if !sheet[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("the independent list has %d raions the sheet does not: %s",
			len(missing), strings.Join(missing, ", "))
	}
	return nil
}

// verifyOrder checks the one derivation still taken from row order: a hromada
// belongs to the nearest raion above it. It fails if a hromada appears before
// any raion, and — the case that would actually corrupt the table — if an
// oblast or a special-status city row ever appears between a raion and its
// hromadas, which is how a stray top-level row would capture the run below
// it.
// verifyOrder checks the one derivation still taken from row order: a hromada
// belongs to the nearest raion above it. It fails if a hromada appears before
// any raion, and — the case that would actually corrupt the table — if an
// oblast or a special-status city row sits immediately above a hromada, which
// is how a stray top-level row would capture the run below it. The sheet does
// contain one such stray row, "Автономна Республіка Крим"; it is harmless
// only because the row under it is a raion, and that is exactly what this
// checks.
func verifyOrder(rows []row) error {
	seenRaion := false
	for i, r := range rows {
		switch r.typ {
		case typeOblast, typeSpecialCity:
			if i+1 < len(rows) && rows[i+1].typ == typeHromada {
				return fmt.Errorf("row %d (%d %q, %s) sits directly above hromada %q; "+
					"hromada→raion can no longer be derived from row order",
					i, r.uid, r.name, r.typ, rows[i+1].name)
			}
		case typeRaion:
			seenRaion = true
		case typeHromada:
			if !seenRaion {
				return fmt.Errorf("row %d (%d %q) is a hromada before any raion", i, r.uid, r.name)
			}
		}
	}
	return nil
}

func build(rows []row) ([]loc, error) {
	ref := referenceRaionOblast()
	oblastUID := map[string]int{}
	for _, r := range rows {
		if r.typ == typeOblast {
			oblastUID[r.name] = r.uid
		}
	}

	raionName := map[int]string{}

	var out []loc
	currentRaion := 0
	for _, r := range rows {
		l := loc{uid: r.uid, name: r.name}
		switch r.typ {
		case typeOblast:
			l.kind = "KindOblast"
		case typeSpecialCity:
			l.kind = "KindSpecialCity"
		case typeRaion:
			l.kind = "KindRaion"
			ob, ok := ref[r.name]
			if !ok {
				return nil, fmt.Errorf("raion %q has no oblast in the independent list", r.name)
			}
			uid, ok := oblastUID[ob]
			if !ok {
				return nil, fmt.Errorf("raion %q names oblast %q, which the sheet does not list", r.name, ob)
			}
			l.parent = uid
			currentRaion = r.uid
			raionName[r.uid] = r.name
		case typeHromada:
			l.kind = "KindHromada"
			l.parent = currentRaion
		}
		out = append(out, l)
	}

	// Label is what alerts.region stores and what the picker shows, so it has
	// to be unique across the whole table — a stored label that resolves to
	// two places is a device that may be watching either.
	//
	// Oblast, raion and special-city names are already unique nationally, so
	// they are their own labels and a config written before this table
	// existed goes on resolving unchanged. Hromada names are not: 73 of them
	// repeat, seven of those inside a single oblast, and "Миколаївська
	// територіальна громада" appears twice inside Сумський район alone. So a
	// repeated name is qualified by its raion, and the pair the sheet lists
	// twice in one raion — the only case a name plus a parent cannot separate
	// — additionally by its UID, which is the only thing that tells them
	// apart at all.
	nameCount := map[string]int{}
	for _, l := range out {
		nameCount[l.name]++
	}
	for i := range out {
		l := &out[i]
		if nameCount[l.name] == 1 {
			l.label = l.name
			continue
		}
		l.label = fmt.Sprintf("%s (%s)", l.name, raionName[l.parent])
	}
	labelCount := map[string]int{}
	for _, l := range out {
		labelCount[l.label]++
	}
	for i := range out {
		l := &out[i]
		if labelCount[l.label] > 1 {
			l.label = fmt.Sprintf("%s (%s, UID %d)", l.name, raionName[l.parent], l.uid)
		}
	}
	return out, nil
}

func verifyLocations(locs []loc) error {
	byUID := map[int]loc{}
	for _, l := range locs {
		byUID[l.uid] = l
	}
	labels := map[string]int{}
	for _, l := range locs {
		if l.label == "" {
			return fmt.Errorf("location %d (%q) has no label", l.uid, l.name)
		}
		if prev, dup := labels[l.label]; dup {
			return fmt.Errorf("label %q is shared by UID %d and UID %d", l.label, prev, l.uid)
		}
		labels[l.label] = l.uid
	}

	// Every non-top-level row must walk up to an oblast in at most two steps,
	// and no chain may loop.
	oblastOf := func(l loc) (loc, error) {
		for i := 0; i < 4; i++ {
			if l.kind == "KindOblast" {
				return l, nil
			}
			if l.parent == 0 {
				return loc{}, fmt.Errorf("%q (UID %d) reaches no oblast", l.name, l.uid)
			}
			next, ok := byUID[l.parent]
			if !ok {
				return loc{}, fmt.Errorf("%q (UID %d) has parent %d, which is not in the table", l.name, l.uid, l.parent)
			}
			l = next
		}
		return loc{}, fmt.Errorf("parent chain from UID %d does not terminate", l.uid)
	}
	children := map[int]int{}
	for _, l := range locs {
		switch l.kind {
		case "KindOblast", "KindSpecialCity":
			if l.parent != 0 {
				return fmt.Errorf("%q (UID %d) is top-level but has parent %d", l.name, l.uid, l.parent)
			}
			continue
		}
		ob, err := oblastOf(l)
		if err != nil {
			return err
		}
		children[ob.uid]++
	}

	// Two rows read out of the live sheet by hand. They are the operator's
	// own location and its raion, and they are asserted here because a table
	// that regenerates into something subtly different should fail loudly
	// rather than move somebody's siren to another oblast.
	shostka, ok := byUID[1198]
	if !ok {
		return fmt.Errorf("UID 1198 is missing; it is the hromada this feature was built for")
	}
	if want := "м. Шостка та Шосткинська територіальна громада"; shostka.name != want {
		return fmt.Errorf("UID 1198 is %q, expected %q", shostka.name, want)
	}
	if shostka.kind != "KindHromada" || shostka.parent != 115 {
		return fmt.Errorf("UID 1198 is %s under %d, expected KindHromada under 115", shostka.kind, shostka.parent)
	}
	raion := byUID[115]
	if raion.name != "Шосткинський район" || raion.kind != "KindRaion" {
		return fmt.Errorf("UID 115 is %q (%s), expected %q (KindRaion)", raion.name, raion.kind, "Шосткинський район")
	}
	ob, err := oblastOf(shostka)
	if err != nil {
		return err
	}
	if ob.name != "Сумська область" {
		return fmt.Errorf("UID 1198 resolves to oblast %q, expected %q", ob.name, "Сумська область")
	}

	// Every Ubilling key has to resolve, or an existing alerts.region stops
	// naming anything.
	for _, name := range ubillingRegions {
		if _, ok := labels[name]; !ok {
			return fmt.Errorf("Ubilling region %q is not a label in this table", name)
		}
	}

	// Crimea is the oblast-level row with no children, and the reason row
	// order cannot be trusted. Asserting it keeps the comment above honest:
	// if the sheet ever gives Crimea its raions, this fails and whoever is
	// regenerating has to re-read why the rule is what it is.
	for _, l := range locs {
		if l.kind != "KindOblast" {
			continue
		}
		n := children[l.uid]
		if l.name == "Автономна Республіка Крим" {
			if n != 0 {
				return fmt.Errorf("%q now has %d descendants; re-read the note on row order", l.name, n)
			}
			continue
		}
		if n < 3 {
			return fmt.Errorf("%q has only %d descendants, which is too few for an oblast", l.name, n)
		}
	}
	return nil
}

func emit(locs []loc) ([]byte, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, `// Code generated by locations_gen.go; DO NOT EDIT.
//
// Source: %s
// Generated: %s
//
// Regenerate with "go generate ./internal/alertsource/" and commit the
// result. Do not hand-edit: locations_gen.go carries the checks that keep a
// hromada attached to the oblast it is actually in, and an edit made here
// goes through none of them.

package alertsource

// locations is the published alerts.in.ua location table, in the sheet's own
// row order — which is the order the /alerts picker offers, so an operator
// scrolls past their oblast, then their raion, then their hromada.
var locations = []Location{
`, sheetURL, time.Now().UTC().Format("2006-01-02"))
	for _, l := range locs {
		fmt.Fprintf(&b, "\t{UID: %d, Name: %q, Label: %q, Kind: %s, Parent: %d},\n",
			l.uid, l.name, l.label, l.kind, l.parent)
	}
	b.WriteString("}\n")
	return format.Source(b.Bytes())
}

func referenceRaions() map[string]bool {
	out := map[string]bool{}
	for name := range referenceRaionOblast() {
		out[name] = true
	}
	return out
}

func referenceRaionOblast() map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(raionOblast), "\n") {
		name, oblast, ok := strings.Cut(strings.TrimSpace(line), "|")
		if !ok {
			continue
		}
		out[name] = oblast
	}
	return out
}
