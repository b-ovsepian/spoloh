package web

import (
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/b-ovsepian/spoloh/internal/config"
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
	// Message and Error are what a restore leaves behind. The index itself
	// has no state of its own, so these are the only reason this page is
	// ever rendered with anything but its links.
	Message string
	Error   string
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
