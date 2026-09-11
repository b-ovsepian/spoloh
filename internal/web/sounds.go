package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/library"
)

// This is the screen the events page assumes exists: /events lets an
// operator choose which sound each event plays, from whatever the library
// holds, but nothing on that page can put a sound into the library or take
// one out. This one does both, over library.Library's existing Add/Remove —
// spec §5.4's on-upload conversion already lives there, so this is a form
// over an API that exists, not new business logic.

// maxUploadBytes caps one uploaded sound. The built-in sounds are tens of
// kilobytes; an announcement someone records on a phone is a few megabytes.
// The ceiling exists because this runs on a board with 512 MB of RAM whose
// other job is to keep announcing, and http.Request.ParseMultipartForm will
// happily buffer whatever it is given.
const maxUploadBytes = 16 << 20

// uploadTimeout bounds how long a single upload's conversion (ffmpeg, when
// the upload is not already a conforming WAV) is allowed to run — the same
// reasoning network.go's networkConnectTimeout gives a Wi-Fi join: an
// operator is standing there waiting for the page to come back, and a
// wedged converter must not hang the request forever.
const uploadTimeout = 30 * time.Second

// soundRowView is what one row of sounds.html reads.
type soundRowView struct {
	Name         string
	DurationText string
	SizeText     string
	ModTimeText  string
	// UsedByText names, comma-joined, the events that play this sound —
	// empty when none do. It exists so the page shows, before an operator
	// ever tries, the exact reason handleSoundsDelete would refuse it.
	UsedByText string
}

// soundsView is what sounds.html reads.
type soundsView struct {
	Rows []soundRowView
	// LibraryNotice explains an empty Rows: no library wired up, or its
	// List call failed — matching homeView.Sounds and eventsView.Sounds,
	// the two other pages a nil or failing Deps.Library must not break.
	LibraryNotice string

	// Error is a rejected submission's reason; Message is a completed one's
	// confirmation. Never both at once — every handler here sets exactly
	// one before rendering, the same contract events.go's eventsView keeps.
	Error   string
	Message string
}

// soundUsers returns the label of every event that plays the given sound —
// its Name when set, its ID otherwise, the same fallback the hidden "id"
// field on every events.html row form already commits to. config.Validate
// cannot make this check at all — the library lives on disk, not in the
// config — so this is the only thing standing between an operator and an
// event left pointing at a file that is gone, which fails at the one moment
// it matters: when the engine tries to play it.
func soundUsers(cfg *config.Config, name string) []string {
	var users []string
	for _, ev := range cfg.Events {
		if ev.Sound != name {
			continue
		}
		label := ev.Name
		if label == "" {
			label = ev.ID
		}
		users = append(users, label)
	}
	return users
}

// humanSize renders a byte count the way an operator reads one, not the way
// Go stores one. Uploads are bounded at maxUploadBytes (16 MiB), so this
// never needs to reach past megabytes.
func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f МБ", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f КБ", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d Б", n)
	}
}

// humanDuration renders a playback length the way someone reads a clock —
// "0:12", "1:05", "4:30" — not as a Go duration string ("1m5s", "4m30s").
// known is false whenever library.Sound could not determine the length: a
// header that would not parse, or one with a byte rate of zero to divide by.
// Rendering that case as "0:00" would tell an operator a sound is silent,
// which is a different, worse claim than "I could not tell" — they read
// those two facts differently, and an event that repeats what turns out to
// be a four-minute siren three times is not a mistake this page should be
// able to hide.
func humanDuration(d time.Duration, known bool) string {
	if !known {
		return "невідомо"
	}
	totalSeconds := int64(d / time.Second)
	if totalSeconds < 0 {
		totalSeconds = 0
	}
	return fmt.Sprintf("%d:%02d", totalSeconds/60, totalSeconds%60)
}

// uploadRejectionMessage turns library.Add's own error into one sentence an
// operator can act on. Only library.go's two named sentinels are specific
// enough to translate — everything else collapses to one generic sentence
// rather than being mapped string by string, which would be a treadmill the
// next ffmpeg version resets, and would risk leaking exactly the kind of
// text (an ffmpeg stderr transcript, in the worst case) this function
// exists to keep off the page. The caller logs the real err at Warn, so
// nothing is actually lost — it just does not reach the browser.
func uploadRejectionMessage(err error) string {
	switch {
	case errors.Is(err, library.ErrConversionUnavailable):
		return "Перетворення файлів на цьому пристрої недоступне: приймаються лише WAV-файли у форматі PCM 16-біт, 44100 Гц, стерео."
	case errors.Is(err, library.ErrBadName):
		return "Некоректна назва файлу: використовуйте лише літери, цифри, крапку, дефіс або підкреслення."
	default:
		return "Не вдалося розпізнати файл як аудіо. Спробуйте WAV або MP3."
	}
}

// soundsUploadResult is what the upload form's XHR reads instead of a whole
// re-rendered page. One sentence either way: Message carries the
// confirmation when OK, and the rejection when not — the same text the HTML
// branch would have put in soundsView.Message or soundsView.Error, so the
// two paths can never drift into telling an operator different things.
type soundsUploadResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// wantsJSON reports whether the caller asked for the small JSON result rather
// than the whole screen.
//
// Content negotiation rather than a query parameter or a custom header,
// because it leaves the form's action attribute alone: the progress bar's
// XHR and the plain no-JavaScript submit post to exactly the same URL, and
// only the representation of the answer differs. A browser form post never
// asks for this — its Accept is "text/html,application/xhtml+xml,...,*/*" —
// so the HTML path stays byte-identical, including leak_test.go's route walk,
// which sends no Accept header at all.
//
// "*/*" deliberately does not match: several things that are not this page's
// script (curl, a probe, an operator poking at the device) send it, and they
// should get the page a browser gets.
func wantsJSON(r *http.Request) bool {
	for _, accept := range r.Header.Values("Accept") {
		if strings.Contains(accept, "application/json") {
			return true
		}
	}
	return false
}

// respondToUpload answers one upload attempt in whichever shape the caller
// asked for. Every exit from handleSoundsUpload below goes through here, so
// the status code an operator's browser sees is the same either way.
func (s *Server) respondToUpload(w http.ResponseWriter, r *http.Request, status int, ok bool, message string) {
	if wantsJSON(r) {
		// Buffered like render(), and for the same reason: an encode that
		// fails half-way must not leave a truncated body behind a 200.
		body, err := json.Marshal(soundsUploadResult{OK: ok, Message: message})
		if err != nil {
			s.deps.Logger.Error("sounds: encoding the upload result", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write(body)
		return
	}
	s.renderSoundsPage(w, status, func(v *soundsView) {
		if ok {
			v.Message = message
			return
		}
		v.Error = message
	})
}

// soundsBaseView builds the view a fresh GET, a success and a rejection all
// start from, the same shape eventsBaseView gives /events.
func (s *Server) soundsBaseView(cfg *config.Config) soundsView {
	var view soundsView

	// Library is nil until Task 9 wires the dashboard up and in this
	// package's own tests that don't need it — same tolerance every other
	// page here gives it.
	if s.deps.Library == nil {
		view.LibraryNotice = "Бібліотека звуків недоступна на цьому пристрої."
		return view
	}
	sounds, err := s.deps.Library.List()
	if err != nil {
		s.deps.Logger.Error("library: list", "err", err)
		view.LibraryNotice = "Не вдалося завантажити список звуків."
		return view
	}

	loc := s.location()
	view.Rows = make([]soundRowView, len(sounds))
	for i, snd := range sounds {
		view.Rows[i] = soundRowView{
			Name:         snd.Name,
			DurationText: humanDuration(snd.Duration, snd.DurationKnown),
			SizeText:     humanSize(snd.Size),
			ModTimeText:  snd.ModTime.In(loc).Format("2006-01-02 15:04:05"),
			UsedByText:   strings.Join(soundUsers(cfg, snd.Name), ", "),
		}
	}
	return view
}

// renderSoundsPage re-renders the whole screen from the store's current
// state, then lets mutate adjust the view — a rejection's Error or a
// success's Message. Every handler below ends through here rather than
// redirecting, the same shape events.go and network.go use for a rejected
// submission: the operator sees the result directly, with nothing lost.
func (s *Server) renderSoundsPage(w http.ResponseWriter, status int, mutate func(*soundsView)) {
	view := s.soundsBaseView(s.deps.Config.Get())
	if mutate != nil {
		mutate(&view)
	}
	s.render(w, status, "sounds.html", view)
}

// handleSoundsPage renders the sounds screen: every sound the library
// holds, and the forms to add, preview or remove one.
//
// The flash is /play's answer arriving back here. Each row's preview button
// posts to /play naming this page as where to return, so the notice that
// redirect carries — "Відтворюється: …", or the reason it could not — has to
// land on the page the operator is standing on. It is read through the same
// flashMessage the dashboard uses rather than a second copy of the rules,
// because the two pages must not disagree about which names are safe to
// repeat back.
func (s *Server) handleSoundsPage(w http.ResponseWriter, r *http.Request) {
	flash := s.flashMessage(r)
	// A failure belongs in Error, which the page renders as a destructive
	// alert; a confirmation belongs in Message. soundsView's contract is
	// that exactly one is ever set.
	failed := r.URL.Query().Has("playfailed")
	s.renderSoundsPage(w, http.StatusOK, func(v *soundsView) {
		switch {
		case flash == "":
		case failed:
			v.Error = flash
		default:
			v.Message = flash
		}
	})
}

// handleSoundsUpload adds a sound to the library. The body is capped at
// maxUploadBytes before anything else touches it: ParseMultipartForm will
// buffer whatever it is given, and a device with 512 MB of RAM whose other
// job is to keep announcing cannot afford to find that out from an upload.
//
// A request whose Content-Type is not multipart/form-data at all — which is
// exactly what leak_test.go's whole-route walk sends here, an empty
// application/x-www-form-urlencoded body — is handled by the same generic
// branch as every other malformed submission: ParseMultipartForm reports it
// as an ordinary error, nothing panics, and nothing is written.
func (s *Server) handleSoundsUpload(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	if s.deps.Library == nil {
		s.respondToUpload(w, r, http.StatusServiceUnavailable, false, "Бібліотека звуків недоступна на цьому пристрої.")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
		message := "Не вдалося обробити завантаження."
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			message = fmt.Sprintf("Файл завеликий: максимум %d МБ.", maxUploadBytes/(1<<20))
		}
		s.respondToUpload(w, r, http.StatusBadRequest, false, message)
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}

	file, header, err := r.FormFile("sound")
	if err != nil {
		s.respondToUpload(w, r, http.StatusBadRequest, false, "Оберіть файл для завантаження.")
		return
	}
	defer file.Close()

	ctx, cancel := context.WithTimeout(r.Context(), uploadTimeout)
	defer cancel()

	// library.Add's own error is NOT echoed to the page — unlike
	// handleNetworkConnect, which does pass Network.Connect's error text
	// straight through. That precedent does not transfer here: nmcli's
	// errors are one short line and the only diagnostic a headless board
	// offers to someone already doing something technical (joining Wi-Fi);
	// library.Add's errors are developer English at best (see
	// uploadRejectionMessage) and, on a failed conversion, a full ffmpeg
	// stderr transcript at worst (see Add's "ffmpeg: %w: %s" wrapping) —
	// meaningless to someone who just tried to upload a recording of a
	// site bell. The real error is still logged at Warn, for whoever
	// reads the device's log.
	if _, err := s.deps.Library.Add(ctx, header.Filename, file); err != nil {
		s.deps.Logger.Warn("sounds: upload rejected", "filename", header.Filename, "err", err)
		message := uploadRejectionMessage(err)
		s.respondToUpload(w, r, http.StatusBadRequest, false, fmt.Sprintf("Не вдалося завантажити «%s»: %s", header.Filename, message))
		return
	}

	s.respondToUpload(w, r, http.StatusOK, true, fmt.Sprintf("Звук «%s» завантажено.", header.Filename))
}

// handleSoundsDelete removes a sound from the library — unless some event
// still plays it. That check reads only the config, not the library, so it
// runs (and can refuse) even before Deps.Library is consulted at all: the
// rule it enforces has nothing to do with whether the library is reachable
// right now, only with what would happen to an event if the file vanished
// under it.
func (s *Server) handleSoundsDelete(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}

	name := r.FormValue("name")
	cfg := s.deps.Config.Get()
	if users := soundUsers(cfg, name); len(users) > 0 {
		s.renderSoundsPage(w, http.StatusBadRequest, func(v *soundsView) {
			v.Error = fmt.Sprintf("Звук «%s» не можна видалити — його використовують події: %s.", name, strings.Join(users, ", "))
		})
		return
	}

	// Deliberately checked after the in-use rule above, not before: the rule
	// is correct whether or not a library is wired up at all, and
	// TestSoundsRefusesToDeleteASoundAnEventUses runs against a server with
	// no library and still expects the refusal. Swapping this ahead of the
	// in-use check would make that test fail for a reason that has nothing
	// to do with the rule it is testing.
	if s.deps.Library == nil {
		s.renderSoundsPage(w, http.StatusServiceUnavailable, func(v *soundsView) {
			v.Error = "Бібліотека звуків недоступна на цьому пристрої."
		})
		return
	}

	if err := s.deps.Library.Remove(name); err != nil {
		s.deps.Logger.Error("sounds: delete rejected", "name", name, "err", err)
		s.renderSoundsPage(w, http.StatusBadRequest, func(v *soundsView) {
			v.Error = "Не вдалося видалити звук."
		})
		return
	}
	s.renderSoundsPage(w, http.StatusOK, func(v *soundsView) { v.Message = "Звук видалено." })
}
