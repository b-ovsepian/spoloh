package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/updater"
)

// settingsLink is one row on the settings index.
//
// Blurb is not decoration. An operator who knows what "Джерела" means does
// not need this page; the one who does not is exactly who opens it.
type settingsLink struct {
	Href  string
	Title string
	Blurb string
}

type settingsView struct {
	Links []settingsLink
	// Message and Error are what a restore, or the update button, leaves
	// behind. The index itself has no state of its own, so these are the
	// only reason this page is ever rendered with anything but its links.
	Message string
	Error   string
	// Update is the version check, and it is nil on every render but
	// /settings/update's. The index must not reach GitHub on its way to
	// drawing six links: a page that hangs because the site's connection is
	// down is a page nobody can use to fix the connection.
	Update *updateView
}

// updateView is what the update card shows.
type updateView struct {
	// Installed is this build, in the words the footer uses; Available is
	// the newest published release, empty when the check could not be made.
	Installed string
	Available string
	UpToDate  bool
	// CanUpdate is whether the button is offered at all — whether this
	// daemon has an updater wired up. It is deliberately separate from
	// Notice: a check that failed because the device cannot reach GitHub
	// still leaves an operator who has just fixed the network a button to
	// press.
	CanUpdate bool
	// Notice explains a check that could not be made. Not an error: a
	// device with no route to the internet is an ordinary device, and this
	// page still has to render and still has to say which build is on the
	// wall.
	Notice string
}

// settingsLinks is the index, in the order an operator setting a device up
// for the first time works through it: where the alerts come from, what the
// device does about them, what it says, out of which output, what else it
// tells, and the clock all of it is timed by.
//
// Мережа is deliberately absent. It is the screen someone opens over the
// device's own access point when nothing else about it is reachable, and one
// level deeper is one tap more at the worst possible moment.
func settingsLinks() []settingsLink {
	return []settingsLink{
		{"/alerts", "Джерела", "Звідки пристрій дізнається про тривоги: область або громада і черга джерел."},
		{"/events", "Події", "Що і коли пристрій озвучує, і тихі години."},
		{"/sounds", "Звуки", "Бібліотека звуків пристрою."},
		// "Аудіовихід" rather than "Звук": that reads one root away from
		// "Звуки" (the sound library) directly above it, and someone
		// scanning this list under pressure — the whole reason that screen
		// exists is a fresh device that cannot make a sound at all — needs
		// to tell them apart at a glance, not by reading past the first
		// four letters.
		{"/audio", "Аудіовихід", "Куди подається звук і наскільки гучно."},
		{"/webhooks", "Вебхуки", "Запити до іншого обладнання під час оповіщення."},
		{"/time", "Час", "Годинник, часовий пояс і синхронізація."},
	}
}

// handleSettings renders the index. It reads nothing and writes nothing: the
// screens it points at own their own state, and a page that cannot fail is
// one fewer way into the dashboard that can.
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	s.renderSettings(w, http.StatusOK, settingsView{})
}

// renderSettings draws the page, links and all. Every exit from
// handleConfigImport goes through here, so a rejection and a fresh GET are
// the same screen with one line of difference.
func (s *Server) renderSettings(w http.ResponseWriter, status int, v settingsView) {
	v.Links = settingsLinks()
	s.render(w, status, "settings.html", v)
}

// handleConfigExport sends the configuration as a file.
//
// A GET, because it changes nothing and because a link works with scripts
// off. The "secrets" parameter is the operator's choice, made on the
// settings page before they click — never a default, and never remembered:
// a download that quietly includes credentials because it did last time is
// how one ends up in a chat window.
func (s *Server) handleConfigExport(w http.ResponseWriter, r *http.Request) {
	withSecrets := r.URL.Query().Get("secrets") == "1"
	data, err := config.Export(s.deps.Config.Get(), withSecrets)
	if err != nil {
		s.deps.Logger.Error("settings: export failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	name := "spoloh-config.json"
	if withSecrets {
		name = "spoloh-config-with-secrets.json"
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// maxConfigBytes caps an uploaded configuration before anything reads it.
// The device's own export is a few kilobytes; a megabyte is room for a
// library of events and webhooks nobody has yet and still nothing on a board
// with 512 MB of RAM whose other job is to keep announcing. Without a cap,
// ParseMultipartForm buffers whatever it is handed — the same reason
// handleSoundsUpload has one, with two orders of magnitude less to allow for.
const maxConfigBytes = 1 << 20

// handleConfigImport restores a configuration from a file.
//
// The order here is the whole point. Nothing is stored until config.Import
// has folded the file into the running configuration and Validate has
// accepted the result: this writes the file the daemon starts from, and a
// configuration that fails to load is a device that announces nothing. A
// rejection re-renders this page with the reason and leaves the running
// settings exactly as they were.
//
// Auth never comes from the file — see config.Import — which is what makes
// this a restore rather than a lockout.
func (s *Server) handleConfigImport(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxConfigBytes)
	if err := r.ParseMultipartForm(maxConfigBytes); err != nil {
		message := "Не вдалося обробити завантаження."
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			message = fmt.Sprintf("Файл завеликий: максимум %d КБ.", maxConfigBytes/(1<<10))
		}
		s.renderSettings(w, http.StatusBadRequest, settingsView{Error: message})
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}

	file, _, err := r.FormFile("config")
	if err != nil {
		s.renderSettings(w, http.StatusBadRequest, settingsView{Error: "Оберіть файл з налаштуваннями."})
		return
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		s.renderSettings(w, http.StatusBadRequest, settingsView{Error: "Не вдалося прочитати файл."})
		return
	}

	// Parsed and validated against the configuration as it is right now,
	// then applied through Update, which clones, validates and writes
	// atomically under its own lock.
	//
	// Keeping the operator signed in is config.Import's job and only its
	// job. Re-applying Auth again here would be a second copy of the same
	// rule, and the one thing worse than a rule in two places is that the
	// copy here would quietly cover for Import losing it — the session test
	// in settings_test.go would go on passing over a lockout waiting to
	// happen on the next caller that does not have a copy.
	next, err := config.Import(s.deps.Config.Get(), data)
	if err != nil {
		// The technical reason goes to the daemon log, where whoever has a
		// shell can read it; the page says which of the two problems it is,
		// because they have different answers — pick another file, or fix
		// the setting on the screen that owns it. A Go error printed onto a
		// Ukrainian page tells the operator standing at the device nothing.
		s.deps.Logger.Error("settings: import rejected", "err", err)
		message := "Файл не підходить: це не збережені налаштування Сполоху."
		if errors.Is(err, config.ErrImportUnusable) {
			message = "Файл не підходить: з такими налаштуваннями пристрій не запуститься. Нічого не змінено."
		}
		s.renderSettings(w, http.StatusBadRequest, settingsView{Error: message})
		return
	}
	if err := s.deps.Config.Update(func(c *config.Config) error {
		*c = *next
		return nil
	}); err != nil {
		s.deps.Logger.Error("settings: import failed to store", "err", err)
		s.renderSettings(w, http.StatusInternalServerError, settingsView{
			Error: "Не вдалося зберегти налаштування. Нічого не змінено.",
		})
		return
	}

	s.deps.Logger.Info("settings: configuration restored from a file")
	s.renderSettings(w, http.StatusOK, settingsView{
		// The restart sentence is not politeness. The alert cascade, the
		// time zone and the audio output are read once when the daemon
		// starts, so until it does, the page says one thing and the device
		// does another.
		Message: "Налаштування відновлено. Пароль до панелі не змінився. Щоб застосувати часовий пояс, аудіовихід і чергу джерел, перезавантажте пристрій.",
	})
}

// ReleaseChecker answers which release is newest. It is the read half of
// internal/updater and the only half this daemon performs: asking is a plain
// HTTPS GET that needs no privilege at all, which is what lets this page say
// "installed X, available Y" without being able to touch anything.
// updater.Updater satisfies it.
type ReleaseChecker interface {
	Latest(ctx context.Context) (updater.Release, error)
}

// RunFunc executes a command and returns its combined output. Injected so
// this package can be tested without a systemd host, the same seam
// internal/timedate and internal/netmgr use for the same reason.
type RunFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

// updateUnit is the root oneshot that replaces this device's binary. The
// daemon runs as spoloh and can neither write /usr/local/bin/spoloh nor
// restart its own unit — and must not be able to.
// deploy/50-spoloh-update.rules grants manage-units for this one unit name
// and this one verb, so this constant and that rule have to agree: change
// one and polkit denies the other. deploy/install_test.go checks they still
// do.
const updateUnit = "spoloh-update.service"

// updateCheckTimeout bounds the version check behind the page. Same
// reasoning as time.go's timeStateTimeout: somebody is watching a phone, and
// a page that says it could not reach GitHub beats one that never loads.
const updateCheckTimeout = 10 * time.Second

// updateStartTimeout bounds the systemctl call. It only enqueues a job — see
// handleUpdateStart — so this is a bound on a D-Bus round trip and a polkit
// decision, not on the update itself.
const updateStartTimeout = 10 * time.Second

// execWaitDelay is how long a killed child gets to let go of its output pipes
// before they are closed out from under it; see netmgr's execRun for the
// measurement this number comes from.
const execWaitDelay = 2 * time.Second

// updateStartedMessage is the flash after the button. It says the two things
// an operator cannot see for themselves: that the device restarts itself,
// and that this page will never report the outcome — the update replaces the
// binary serving it. Without that sentence the honest conclusion from a
// dashboard that goes away and comes back unchanged for a minute is that the
// button did nothing.
const updateStartedMessage = "Оновлення запущено. Пристрій завантажить нову версію і перезапуститься сам — панель на кілька хвилин стане недоступною. Ця сторінка результату не покаже: відкрийте її знову після перезапуску і звірте версію."

// updateStartErrors maps the code a redirect carries to the sentence shown.
// A code rather than the sentence itself, for the reason audio.go's
// testSoundErrors gives: text read out of a query string is
// attacker-controllable, and anyone who can get a signed-in operator to open
// a crafted link must not be able to put words on the device's own
// dashboard.
var updateStartErrors = map[string]string{
	"unavailable": "Оновлення з цієї сторінки на цьому пристрої недоступне.",
	"failed":      "Не вдалося запустити оновлення. Перевірте журнал пристрою: можливо, службу оновлення не встановлено або її запуск заборонено.",
}

// checkUpdate asks where the newest release is, turning every way that can
// come up empty — no updater wired up, GitHub unreachable, the bound
// expiring — into a Ukrainian notice rather than an error page, the same
// shape time.go's readState gives an unreachable timedatectl.
func (s *Server) checkUpdate(ctx context.Context) *updateView {
	v := &updateView{Installed: s.versionText()}
	if s.deps.Updater == nil {
		v.Notice = updateStartErrors["unavailable"]
		return v
	}
	v.CanUpdate = true

	callCtx, cancel := context.WithTimeout(ctx, updateCheckTimeout)
	defer cancel()

	rel, err := s.deps.Updater.Latest(callCtx)
	if err != nil {
		// Warn, not Error: a board on a site with no internet reaches this
		// every time somebody opens the page, and it is not a fault.
		s.deps.Logger.Warn("update: checking for the latest release failed", "err", err)
		v.Notice = "Не вдалося перевірити наявність оновлень — пристрій не отримав відповіді з github.com."
		return v
	}
	v.Available = rel.Version
	// Compared against the stamped version, not against what the footer
	// prints: versionText answers «збірка для розробки» for an unstamped
	// build, and a release tag will never equal that anyway.
	v.UpToDate = rel.Version == s.deps.Version
	return v
}

// handleUpdatePage shows what is installed, what is available, and the
// button. The check runs on this route only, which is why it is a route and
// not a card on the index.
func (s *Server) handleUpdatePage(w http.ResponseWriter, r *http.Request) {
	v := settingsView{Update: s.checkUpdate(r.Context())}
	// The one-shot notice the redirect below carries back. Query parameters
	// rather than a cookie, exactly as /audio and /play do it: it is meant
	// to be seen once, by the browser about to render this response.
	q := r.URL.Query()
	switch {
	case q.Has("updating"):
		v.Message = updateStartedMessage
	case q.Has("update_error"):
		v.Error = updateStartErrors[q.Get("update_error")]
	}
	s.renderSettings(w, http.StatusOK, v)
}

// handleUpdateStart starts the privileged unit, and that is the whole of its
// authority: one unit name, fixed here, with nothing from the request
// reaching the command line. The unit is what downloads, verifies and
// installs — see deploy/spoloh-update.service.
//
// --no-block is not an optimisation. Without it `systemctl start` waits for
// the job to finish, and this job ends by restarting spoloh.service: the
// request that started it would be killed by its own update, and the
// operator would get a dropped connection instead of the sentence explaining
// what is happening. Authorisation is unaffected — polkit decides the
// StartUnit call itself, so a denial is still reported here and now — and so
// is the grant, which is pinned to the job's verb ("start") and not to the
// mode it was enqueued with.
func (s *Server) handleUpdateStart(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	// The same nil that makes the page say so is what keeps a daemon with no
	// updater wired up from ever starting the unit.
	if s.deps.Updater == nil {
		s.redirectUpdate(w, r, "update_error", "unavailable")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), updateStartTimeout)
	defer cancel()

	if out, err := s.runCommand(ctx, "systemctl", "start", "--no-block", updateUnit); err != nil {
		// The command's own output carries the polkit denial, which is the
		// only thing that distinguishes "not authorised" from "no such
		// unit" — and on a headless board the log is the whole diagnostic
		// surface. Nothing on this command line is a secret.
		s.deps.Logger.Error("update: starting the update unit failed",
			"unit", updateUnit, "err", err, "output", strings.TrimSpace(string(out)))
		s.redirectUpdate(w, r, "update_error", "failed")
		return
	}
	s.deps.Logger.Info("update: the update unit was started", "unit", updateUnit)
	s.redirectUpdate(w, r, "updating", "1")
}

// redirectUpdate sends the browser back to this page carrying one flash
// parameter. 303 so a reload does not press the button again.
func (s *Server) redirectUpdate(w http.ResponseWriter, r *http.Request, key, value string) {
	http.Redirect(w, r, "/settings/update?"+url.Values{key: {value}}.Encode(), http.StatusSeeOther)
}

// runCommand executes name with args through the injected seam, or for real
// when there is none — the rule every other seam in this repository keeps.
func (s *Server) runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	if s.deps.Run != nil {
		return s.deps.Run(ctx, name, args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	// See netmgr.execRun: without WaitDelay the context deadline bounds the
	// process, not the call, because CombinedOutput goes on waiting for an
	// inherited pipe a descendant still holds.
	cmd.WaitDelay = execWaitDelay
	return cmd.CombinedOutput()
}
