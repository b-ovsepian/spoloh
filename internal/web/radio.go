package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/radio"
)

type radioView struct {
	Message string
	Error   string

	Radio          config.Radio
	Groups         []radio.Group
	Custom         string
	WeekdayOptions []weekdayOption
	// Playing names what the player has been asked to stream right now, or
	// is empty when nothing is: the answer to "why is there music", and to
	// "why is there none", without opening the journal. Held says the stop
	// button is holding it off, and LastError how it last failed to play —
	// "увімкнено" over a stream that fails every five minutes would be a
	// page that lies.
	Playing   string
	Held      bool
	LastError string
}

// radioFieldMessages say a radio.FieldError in the operator's language.
var radioFieldMessages = map[string]string{
	radio.FieldVolume:   "Гучність має бути від 0 до 100.",
	radio.FieldWeekdays: "Оберіть хоча б один день тижня.",
	radio.FieldTime:     "Вкажіть час у форматі ГГ:ХХ.",
	radio.FieldWindow:   "Час закінчення має бути пізніше за час початку.",
	radio.FieldStation:  "Оберіть станцію зі списку.",
	radio.FieldURL:      "Адреса потоку має починатися з http:// або https:// і містити адресу сервера.",
}

func (s *Server) handleRadioPage(w http.ResponseWriter, r *http.Request) {
	s.renderRadioPage(w, http.StatusOK, nil)
}

// handleRadioSave stores what the form says. The engine reads it on its next
// tick, so a saved change is heard within a second; there is nothing to
// restart.
func (s *Server) handleRadioSave(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	// A second button on the same form, like /audio's test sound: it lifts
	// the hold the stop button put on the radio, and saves nothing.
	if r.PostForm.Get("action") == "resume" {
		if s.deps.Player != nil {
			s.deps.Player.ResumeBackground()
		}
		s.renderRadioPage(w, http.StatusOK, func(v *radioView) { v.Message = "Радіо відновлено." })
		return
	}
	submitted := config.Radio{
		// A checkbox that is not ticked is not in the form at all.
		Enabled:          r.PostForm.Get("enabled") != "",
		Station:          r.PostForm.Get("station"),
		Weekdays:         parseWeekdays(r.PostForm["weekdays"]),
		From:             r.PostForm.Get("from"),
		To:               r.PostForm.Get("to"),
		PauseDuringAlert: r.PostForm.Get("pause_during_alert") != "",
	}
	// The URL is kept only for the station that uses it, so switching back
	// to a listed station does not leave a stale address in config.json.
	if submitted.Station == radio.Custom {
		submitted.URL = r.PostForm.Get("url")
	}
	reject := func(message string) {
		s.renderRadioPage(w, http.StatusBadRequest, func(v *radioView) {
			v.Radio = submitted
			v.Error = message
		})
	}
	volume, err := strconv.Atoi(r.PostForm.Get("volume"))
	if err != nil {
		reject(radioFieldMessages[radio.FieldVolume])
		return
	}
	submitted.Volume = volume
	if err := radio.Validate(submitted); err != nil {
		var fe *radio.FieldError
		message := "Не вдалося зберегти налаштування радіо."
		if errors.As(err, &fe) && radioFieldMessages[fe.Field] != "" {
			message = radioFieldMessages[fe.Field]
		}
		reject(message)
		return
	}
	if err := s.deps.Config.Update(func(c *config.Config) error {
		c.Radio = submitted
		return nil
	}); err != nil {
		s.deps.Logger.Error("radio: save rejected", "err", err)
		reject("Не вдалося зберегти налаштування радіо.")
		return
	}
	s.renderRadioPage(w, http.StatusOK, func(v *radioView) { v.Message = "Налаштування радіо збережено." })
}

func (s *Server) renderRadioPage(w http.ResponseWriter, status int, mutate func(*radioView)) {
	stored := s.deps.Config.Get().Radio
	// A config written before the radio existed has none of it; the form
	// starts from the defaults rather than from blank fields.
	if stored.Station == "" {
		stored = config.Default().Radio
	}
	view := radioView{
		Radio:          stored,
		Groups:         radio.Groups(),
		Custom:         radio.Custom,
		WeekdayOptions: weekdayOptions,
	}
	if s.deps.Player != nil {
		if bg := s.deps.Player.Background(); bg.URL != "" {
			view.Playing = stationName(bg.URL)
			view.Held = s.deps.Player.BackgroundHeld()
			if err, at := s.deps.Player.BackgroundError(); err != nil {
				redacted := radio.Redact(bg.URL)
				view.LastError = at.In(s.location()).Format("15:04") + " — " +
					strings.ReplaceAll(err.Error(), bg.URL, redacted)
			}
		}
	}
	if mutate != nil {
		mutate(&view)
	}
	s.render(w, status, "radio.html", view)
}

// stationName names a stream by its catalogue entry, or by its address —
// without any secret in it — when it is the operator's own.
func stationName(url string) string {
	for _, st := range radio.Stations {
		if st.URL == url {
			return st.Name
		}
	}
	return radio.Redact(url)
}
