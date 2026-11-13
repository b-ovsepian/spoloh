package web

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/b-ovsepian/spoloh/internal/alertsource"
	"github.com/b-ovsepian/spoloh/internal/config"
)

// metricsPath is outside the login: a monitoring system holds no session. It
// is behind its own token instead — see handleMetrics.
const metricsPath = "/metrics"

// handleMetrics answers a monitoring system in the Prometheus text format,
// which Prometheus, VictoriaMetrics, Grafana Agent and Telegraf read natively
// and Zabbix reads with its Prometheus preprocessing; deploy/monitoring has a
// Zabbix template built on it.
//
// No token configured is no endpoint, 404: a device nobody asked to be
// monitored does not describe itself to whoever asks. With one configured,
// the request must carry it as a bearer token, and only its SHA-256 is
// compared — the token is never stored — in constant time.
//
// The format is written by hand rather than with a client library: the
// module graph does not grow, and the format is a line per sample.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.Config.Get()
	want := cfg.Monitoring.TokenHash
	if want == "" {
		http.NotFound(w, r)
		return
	}
	// The scheme name is case-insensitive (RFC 7235), and a token pasted
	// into a monitoring system's settings often brings a space with it.
	scheme, token, ok := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
	ok = ok && strings.EqualFold(scheme, "Bearer")
	token = strings.TrimSpace(token)
	sum := sha256.Sum256([]byte(token))
	if !ok || subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(want)) != 1 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="spoloh"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var m metricsWriter
	s.writeMetrics(&m, cfg, s.now())
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(m.String()))
}

// writeMetrics is every sample /metrics reports. Times are Unix seconds, and
// 0 means "never" or "not now": a monitoring system computes "how long ago"
// itself, from its own clock, which on a board with no RTC is the better of
// the two.
func (s *Server) writeMetrics(m *metricsWriter, cfg *config.Config, now time.Time) {
	version := s.deps.Version
	if version == "" {
		version = "dev"
	}
	m.gauge("spoloh_build_info", "The running build; always 1.", 1, "version", version)
	m.gauge("spoloh_start_time_seconds", "When the dashboard started, Unix seconds.", float64(s.started.Unix()))

	if s.deps.Engine != nil {
		st := s.deps.Engine.Status()
		m.gauge("spoloh_alert_active", "1 while an air raid alert is announced for the configured region.",
			b2f(st.Alert), "region", cfg.Alerts.Region)
		m.gauge("spoloh_alert_red", "1 while the alert is red, or of a level its source does not report.",
			b2f(st.Alert && st.Level.TreatAsRed()))
		m.gauge("spoloh_alert_since_timestamp_seconds", "When the current alert began, Unix seconds; 0 with none.",
			unixOrZero(st.Since, st.Alert))
		m.gauge("spoloh_poll_last_success_timestamp_seconds", "When an alert source last answered, Unix seconds; 0 if never.",
			unixOrZero(st.LastSuccess, true))
		// The age as well as the time: both are this device's clock, so the
		// difference is right even on a board whose clock is not, where a
		// monitoring system subtracting from its own clock would not be.
		//
		// Nothing answered yet counts from the start: silent for as long as
		// the daemon has run. A sentinel such as -1 would read as "silent"
		// on the first scrape after every reboot, before any poll had had
		// its chance — on a board with no RTC, the minute or two until NTP
		// lets TLS through.
		since := st.LastSuccess
		if since.IsZero() {
			since = s.started
		}
		m.gauge("spoloh_poll_last_success_age_seconds", "Seconds since an alert source last answered, or since start if none has.",
			float64(int64(max(0, now.Sub(since).Seconds()))))
		m.gauge("spoloh_poll_failing", "1 while the last poll of the alert sources failed.", b2f(st.Failing))
		m.gauge("spoloh_poll_paused", "1 while polling is paused because the device serves its own access point.",
			b2f(st.PollPaused))
		m.help("spoloh_source_failed", "gauge", "Per source asked in the last poll: 1 if it failed, 0 if it answered.")
		for _, src := range st.Sources {
			if src.Outcome == alertsource.OutcomeNotReached {
				continue
			}
			m.sample("spoloh_source_failed", b2f(src.Outcome == alertsource.OutcomeFailed),
				"source", src.Key, "reason", string(src.Reason))
		}
		m.gauge("spoloh_clock_synced", "1 when the clock is NTP-synchronised; scheduled events wait for it.",
			b2f(st.ClockSynced))
		m.gauge("spoloh_playing", "1 while an announcement is playing.", b2f(st.PlayingEventID != ""))
	}

	m.gauge("spoloh_alert_start_disabled", "1 when the alert-start announcement is switched off or at volume 0.",
		b2f(alertStartDisabled(cfg)))
	m.gauge("spoloh_quiet_hours_silence_alerts", "1 when quiet hours are set to silence air raid alerts.",
		b2f(cfg.QuietHours.SilencesAlerts()))

	if st, ok := s.deps.NetStatus.Latest(now); ok {
		m.gauge("spoloh_network_connected", "1 when the device is joined to an upstream network.", b2f(st.Connected))
		m.gauge("spoloh_network_hotspot_up", "1 while the device serves its own access point.", b2f(st.HotspotUp))
	}

	m.gauge("spoloh_radio_enabled", "1 when the radio is switched on in the settings.", b2f(cfg.Radio.Enabled))
	var radioOn, radioHeld bool
	var radioErrAt time.Time
	if p := s.deps.Player; p != nil {
		radioOn = p.Background().URL != ""
		radioHeld = radioOn && p.BackgroundHeld()
		if err, at := p.BackgroundError(); err != nil {
			radioErrAt = at
		}
	}
	m.gauge("spoloh_radio_on", "1 while the schedule says the radio should play.", b2f(radioOn))
	m.gauge("spoloh_radio_held", "1 while the stop button holds the radio off.", b2f(radioHeld))
	m.gauge("spoloh_radio_last_error_timestamp_seconds", "When the radio stream last failed, Unix seconds; 0 if not since it was set.",
		unixOrZero(radioErrAt, !radioErrAt.IsZero()))

	m.help("spoloh_events_total", "counter", "Event log entries since the daemon started, by kind and outcome.")
	for _, c := range s.deps.Log.Counts() {
		m.sample("spoloh_events_total", float64(c.N), "kind", c.Kind, "ok", fmt.Sprint(c.OK))
	}
	m.help("spoloh_eventlog_write_errors_total", "counter",
		"Event log entries that could not be written since the daemon started; a read-only SD card, typically.")
	m.sample("spoloh_eventlog_write_errors_total", float64(s.deps.Log.WriteErrors()))
}

// metricsWriter builds the Prometheus text format.
type metricsWriter struct{ b strings.Builder }

func (m *metricsWriter) help(name, typ, help string) {
	fmt.Fprintf(&m.b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

// gauge writes a gauge's header and its one sample.
func (m *metricsWriter) gauge(name, help string, v float64, labels ...string) {
	m.help(name, "gauge", help)
	m.sample(name, v, labels...)
}

// sample writes one line; labels are name, value pairs.
func (m *metricsWriter) sample(name string, v float64, labels ...string) {
	m.b.WriteString(name)
	if len(labels) > 0 {
		m.b.WriteByte('{')
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				m.b.WriteByte(',')
			}
			fmt.Fprintf(&m.b, `%s="%s"`, labels[i], labelEscaper.Replace(labels[i+1]))
		}
		m.b.WriteByte('}')
	}
	fmt.Fprintf(&m.b, " %s\n", formatSample(v))
}

func (m *metricsWriter) String() string { return m.b.String() }

// labelEscaper is the text format's own escaping for a label value: a quote,
// a backslash or a newline from an operator's region name must not end the
// line it sits in.
var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func formatSample(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%g", v)
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func unixOrZero(t time.Time, ok bool) float64 {
	if !ok || t.IsZero() {
		return 0
	}
	return float64(t.Unix())
}
