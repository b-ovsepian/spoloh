package web

import (
	"context"
	"net/http"
	"time"

	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/timedate"
)

// This screen answers operator feedback in the operator's own words: «не
// можна змінити час / часовий пояс, налаштувати ntp сервер». The device
// decides when to sound a siren from a schedule, so a clock an hour out
// announces the minute of silence at the wrong minute and a wrong time zone
// announces it on the wrong side of a DST change — and until this page there
// was no way to fix either without an SSH session.
//
// config.Device.Timezone already existed, but it only formatted what the
// dashboard displayed; it never touched the system clock. Keeping those two
// in step is what handleTimeZone is careful about.

// timeStateTimeout bounds the `timedatectl show` and `list-timezones` calls
// behind this page. Same reasoning as audio.go's deviceListTimeout: a person
// is watching a phone, and a page that says why it knows nothing beats one
// that never loads.
const timeStateTimeout = 10 * time.Second

// manualLayout is the form timedatectl set-time accepts and the form this
// page hands it. The browser's datetime-local input submits
// browserLayout(s); parseManualTime converts.
const manualLayout = "2006-01-02 15:04:05"

// browserLayouts are what an <input type="datetime-local"> submits: with
// seconds when step allows them, without when the browser rounds to the
// minute. Both are accepted; both are normalised to manualLayout before
// anything is run.
var browserLayouts = []string{"2006-01-02T15:04:05", "2006-01-02T15:04"}

// fallbackZones is the picker's answer when the host cannot be asked for its
// list. Europe/Kyiv is the overwhelmingly common answer and must never
// disappear from the picker just because timedatectl is unreachable; UTC is
// the neutral one. Whatever is configured now is added alongside them by
// buildZoneOptions, so an operator never loses sight of their own setting.
var fallbackZones = []string{"Europe/Kyiv", "UTC"}

// zoneOption is one row of the time zone picker.
type zoneOption struct {
	Value    string
	Selected bool
}

// timeView is what time.html reads.
type timeView struct {
	// Now is the device's own clock, formatted in the configured zone —
	// the question an operator opens this page to answer.
	Now string
	// ConfigTimezone is what the dashboard formats with;
	// SystemTimezone is what the host is actually set to. They are shown
	// separately, and Mismatch marks them disagreeing, because that
	// disagreement is exactly the failure this screen exists to fix and it
	// is invisible from anywhere else.
	ConfigTimezone string
	SystemTimezone string
	Mismatch       bool

	// NTP is whether synchronisation is switched on, Synced whether it has
	// actually succeeded, and CanNTP whether this host has a client systemd
	// can drive at all. NTP on with Synced false is a board still trying —
	// the state an operator would otherwise have to guess at.
	NTP    bool
	Synced bool
	CanNTP bool

	// NTPServer is the configured time server, empty meaning the
	// distribution's own.
	NTPServer string

	// Zones is the picker; ManualTime pre-fills the by-hand field with the
	// device's current reading, so an operator adjusts rather than types a
	// whole timestamp.
	Zones      []zoneOption
	ManualTime string

	// StateNotice explains a page that could not read the host: no manager
	// wired up, the call failing or timing out. ZonesNotice does the same
	// for the picker alone, which falls back to a short list rather than
	// rendering empty.
	StateNotice   string
	ZonesNotice   string
	ClockEditable bool

	// Error is a rejected submission's reason; Message is a completed one's
	// confirmation. Never both at once, the same contract every other
	// screen here keeps.
	Error   string
	Message string
}

// readState asks the host for its clock configuration, turning every way
// that can come up empty — no manager wired up, the call failing, the bound
// expiring — into a Ukrainian notice rather than an error page, the same
// shape audio.go's listDevices gives a broken ALSA.
func (s *Server) readState(ctx context.Context) (timedate.State, string) {
	if s.deps.Timedate == nil {
		return timedate.State{}, "Керування часом недоступне на цьому пристрої."
	}
	callCtx, cancel := context.WithTimeout(ctx, timeStateTimeout)
	defer cancel()

	st, err := s.deps.Timedate.Show(callCtx)
	if err != nil {
		s.deps.Logger.Warn("time: reading the clock state failed", "err", err)
		return timedate.State{}, "Не вдалося прочитати налаштування часу на пристрої."
	}
	return st, ""
}

// readZones asks the host for its zone list, falling back to fallbackZones
// when it cannot be asked. An empty picker would leave an operator no way to
// change a zone on precisely the board whose zone is wrong.
func (s *Server) readZones(ctx context.Context) ([]string, string) {
	if s.deps.Timedate == nil {
		return nil, "Список часових поясів недоступний; показано лише основні."
	}
	callCtx, cancel := context.WithTimeout(ctx, timeStateTimeout)
	defer cancel()

	zones, err := s.deps.Timedate.Zones(callCtx)
	if err != nil {
		s.deps.Logger.Warn("time: reading the zone list failed", "err", err)
		return nil, "Не вдалося отримати список часових поясів; показано лише основні."
	}
	if len(zones) == 0 {
		return nil, "Пристрій не повідомив жодного часового поясу; показано лише основні."
	}
	return zones, ""
}

// buildZoneOptions puts Europe/Kyiv first and marks current selected.
//
// Kyiv first rather than merely present: it is the answer for nearly every
// device this is installed on, and a list of six hundred zones with it
// buried alphabetically between Europe/Kirov and Europe/Lisbon makes the
// common case the hard one. Everything else keeps the host's own order,
// which is alphabetical. current is always in the list even when the host
// does not report it — a zone that vanishes from the picker is a setting an
// operator can see and not keep.
func buildZoneOptions(zones []string, current string) []zoneOption {
	seen := map[string]bool{}
	ordered := make([]string, 0, len(zones)+3)
	add := func(z string) {
		if z == "" || seen[z] {
			return
		}
		seen[z] = true
		ordered = append(ordered, z)
	}

	add("Europe/Kyiv")
	add(current)
	if len(zones) == 0 {
		for _, z := range fallbackZones {
			add(z)
		}
	}
	for _, z := range zones {
		add(z)
	}

	options := make([]zoneOption, len(ordered))
	for i, z := range ordered {
		options[i] = zoneOption{Value: z, Selected: z == current}
	}
	return options
}

// parseManualTime reads what an <input type="datetime-local"> submitted and
// returns it in the form timedatectl set-time accepts. An empty returned
// string means the value was not a timestamp.
//
// It is parsed here rather than handed to timedatectl as typed so that the
// only thing that ever reaches that command is a string this package
// produced. The zone is the configured one: a person standing at the device
// types the wall-clock time they can see, not UTC.
func parseManualTime(value string, loc *time.Location) string {
	for _, layout := range browserLayouts {
		if t, err := time.ParseInLocation(layout, value, loc); err == nil {
			return t.Format(manualLayout)
		}
	}
	return ""
}

// renderTimePage re-renders the whole screen from the host's current state,
// then lets mutate adjust the view — a rejection's Error or a success's
// Message. Every handler here ends through this rather than redirecting, the
// same shape alerts.go, events.go, sounds.go and audio.go use.
func (s *Server) renderTimePage(w http.ResponseWriter, r *http.Request, status int, mutate func(*timeView)) {
	cfg := s.deps.Config.Get()
	st, notice := s.readState(r.Context())
	zones, zonesNotice := s.readZones(r.Context())
	loc := s.location()
	now := s.now().In(loc)

	view := timeView{
		Now:            now.Format("02.01.2006 15:04:05"),
		ConfigTimezone: cfg.Device.Timezone,
		SystemTimezone: st.Timezone,
		// Only a zone actually read off the host can disagree with the
		// configuration; an unreadable host reports "", which is not a
		// disagreement, only an unknown.
		Mismatch:    st.Timezone != "" && st.Timezone != cfg.Device.Timezone,
		NTP:         st.NTP,
		Synced:      st.NTPSynchronized,
		CanNTP:      st.CanNTP,
		NTPServer:   cfg.Device.NTPServer,
		Zones:       buildZoneOptions(zones, cfg.Device.Timezone),
		ManualTime:  now.Format(browserLayouts[0]),
		StateNotice: notice,
		ZonesNotice: zonesNotice,
		// systemd refuses set-time while NTP is on, so the form is not
		// offered then. Saying so up front is the difference between a
		// screen that explains itself and one that takes a submission
		// only to hand back systemd's English refusal.
		ClockEditable: notice == "" && !st.NTP,
	}
	if mutate != nil {
		mutate(&view)
	}
	s.render(w, status, "time.html", view)
}

// handleTimePage renders the clock screen.
func (s *Server) handleTimePage(w http.ResponseWriter, r *http.Request) {
	s.renderTimePage(w, r, http.StatusOK, nil)
}

// timeUnavailable is the one sentence every handler here answers with when
// there is no manager to ask.
const timeUnavailable = "Керування часом недоступне на цьому пристрої."

// guard rejects a submission this device cannot carry out, and reports
// whether the caller may proceed. Kept in one place so a handler cannot
// forget either half of it — the cross-origin check or the missing manager.
func (s *Server) guardTime(w http.ResponseWriter, r *http.Request) bool {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return false
	}
	if s.deps.Timedate == nil {
		s.renderTimePage(w, r, http.StatusServiceUnavailable, func(v *timeView) {
			v.Error = timeUnavailable
		})
		return false
	}
	return true
}

// handleTimeZone changes the time zone in both places it lives: the system's
// own setting and config.Device.Timezone, which is what the dashboard and
// the scheduler format with. Leaving one of them changed and not the other
// is the drift this screen exists to end, so a failure must leave neither
// applied.
//
// The order is: write the configuration first, then the system, and put the
// configuration back if the system half fails.
//
// It is that way round because of which rollback can be relied on. The half
// that will actually fail on a real board is the privileged one — it goes
// through polkit, and deploy/50-spoloh-timedate.rules has never been
// confirmed on hardware — and undoing a system time zone change means
// calling the very command that just refused. Undoing the configuration
// write needs no privilege at all, and restores a value that was valid by
// construction because it was already stored. The other order would put the
// unreliable mechanism on the recovery path, which is where it can do the
// most harm.
//
// It has a second benefit: config.Update validates the zone through
// time.LoadLocation, so a zone this build cannot resolve is refused before
// anything privileged is attempted at all.
func (s *Server) handleTimeZone(w http.ResponseWriter, r *http.Request) {
	if !s.guardTime(w, r) {
		return
	}

	tz := r.FormValue("timezone")
	rejectWith := func(status int, message string) {
		s.renderTimePage(w, r, status, func(v *timeView) { v.Error = message })
	}

	if tz == "" {
		rejectWith(http.StatusBadRequest, "Оберіть часовий пояс зі списку.")
		return
	}

	previous := s.deps.Config.Get().Device.Timezone
	if err := s.deps.Config.Update(func(c *config.Config) error {
		c.Device.Timezone = tz
		return nil
	}); err != nil {
		s.deps.Logger.Error("time: timezone rejected", "tz", tz, "err", err)
		rejectWith(http.StatusBadRequest, "Не вдалося зберегти часовий пояс. Оберіть його зі списку.")
		return
	}

	if err := s.deps.Timedate.SetTimezone(r.Context(), tz); err != nil {
		s.deps.Logger.Error("time: setting the system timezone failed", "tz", tz, "err", err)
		// Put the configuration back, so the two halves cannot be left
		// disagreeing. If even this fails there is nothing further this
		// process can do about it, and the operator is told that the two
		// may now differ rather than being told the change simply did not
		// happen.
		if err := s.deps.Config.Update(func(c *config.Config) error {
			c.Device.Timezone = previous
			return nil
		}); err != nil {
			s.deps.Logger.Error("time: rolling the timezone back failed", "tz", previous, "err", err)
			rejectWith(http.StatusInternalServerError,
				"Не вдалося змінити часовий пояс системи, і не вдалося повернути попереднє значення. Перевірте налаштування пристрою.")
			return
		}
		rejectWith(http.StatusServiceUnavailable, "Не вдалося змінити часовий пояс системи. Нічого не змінено.")
		return
	}

	s.renderTimePage(w, r, http.StatusOK, func(v *timeView) {
		v.Message = "Часовий пояс змінено."
	})
}

// handleTimeNTP switches network time synchronisation on or off.
func (s *Server) handleTimeNTP(w http.ResponseWriter, r *http.Request) {
	if !s.guardTime(w, r) {
		return
	}

	var on bool
	switch r.FormValue("ntp") {
	case "on":
		on = true
	case "off":
		on = false
	default:
		// Neither value means a form that did not say which way to go —
		// including an empty one. Nothing is switched on a guess: this is
		// the setting that decides whether the board keeps its own idea of
		// the time.
		s.renderTimePage(w, r, http.StatusBadRequest, func(v *timeView) {
			v.Error = "Не вказано, вмикати чи вимикати синхронізацію часу."
		})
		return
	}

	if err := s.deps.Timedate.SetNTP(r.Context(), on); err != nil {
		s.deps.Logger.Error("time: switching NTP failed", "on", on, "err", err)
		s.renderTimePage(w, r, http.StatusServiceUnavailable, func(v *timeView) {
			v.Error = "Не вдалося змінити синхронізацію часу."
		})
		return
	}

	message := "Синхронізацію часу вимкнено."
	if on {
		message = "Синхронізацію часу увімкнено."
	}
	s.renderTimePage(w, r, http.StatusOK, func(v *timeView) { v.Message = message })
}

// handleTimeManual sets the clock by hand.
//
// systemd refuses this outright while NTP is on, so it is refused here first
// and with a sentence that says what to do about it. Letting the submission
// through would hand the operator systemd's own English refusal on a
// Ukrainian dashboard — and would do it after the page had already offered
// the field as though it worked.
func (s *Server) handleTimeManual(w http.ResponseWriter, r *http.Request) {
	if !s.guardTime(w, r) {
		return
	}

	st, notice := s.readState(r.Context())
	if notice != "" {
		s.renderTimePage(w, r, http.StatusServiceUnavailable, func(v *timeView) { v.Error = notice })
		return
	}
	if st.NTP {
		s.renderTimePage(w, r, http.StatusConflict, func(v *timeView) {
			v.Error = "Спершу вимкніть синхронізацію часу — поки вона увімкнена, встановити час вручну неможливо."
		})
		return
	}

	stamp := parseManualTime(r.FormValue("manual_time"), s.location())
	if stamp == "" {
		s.renderTimePage(w, r, http.StatusBadRequest, func(v *timeView) {
			v.Error = "Вкажіть дату й час."
		})
		return
	}

	if err := s.deps.Timedate.SetTime(r.Context(), stamp); err != nil {
		s.deps.Logger.Error("time: setting the clock failed", "err", err)
		s.renderTimePage(w, r, http.StatusServiceUnavailable, func(v *timeView) {
			v.Error = "Не вдалося встановити час."
		})
		return
	}

	s.renderTimePage(w, r, http.StatusOK, func(v *timeView) { v.Message = "Час встановлено." })
}

// handleTimeServer stores the NTP server and asks the system to adopt it.
//
// The configuration is written first, for the same reason as the time zone:
// config.Validate is where the value is refused (a newline, a quote or an
// "=" in a value bound for a root-owned config file), and it is the half
// that can be put back without privilege if the privileged half fails.
func (s *Server) handleTimeServer(w http.ResponseWriter, r *http.Request) {
	if !s.guardTime(w, r) {
		return
	}

	server := r.FormValue("ntp_server")
	previous := s.deps.Config.Get().Device.NTPServer

	if err := s.deps.Config.Update(func(c *config.Config) error {
		c.Device.NTPServer = server
		return nil
	}); err != nil {
		s.deps.Logger.Warn("time: ntp server rejected", "err", err)
		s.renderTimePage(w, r, http.StatusBadRequest, func(v *timeView) {
			v.Error = "Адреса сервера часу має бути іменем хоста або IP-адресою."
			v.NTPServer = server
		})
		return
	}

	if err := s.deps.Timedate.ApplyNTPServer(r.Context(), server); err != nil {
		s.deps.Logger.Error("time: applying the ntp server failed", "err", err)
		if err := s.deps.Config.Update(func(c *config.Config) error {
			c.Device.NTPServer = previous
			return nil
		}); err != nil {
			s.deps.Logger.Error("time: rolling the ntp server back failed", "err", err)
		}
		s.renderTimePage(w, r, http.StatusServiceUnavailable, func(v *timeView) {
			v.Error = "Не вдалося застосувати сервер часу. Нічого не змінено."
		})
		return
	}

	s.renderTimePage(w, r, http.StatusOK, func(v *timeView) { v.Message = "Сервер часу збережено." })
}
