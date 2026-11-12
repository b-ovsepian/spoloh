package engine

import (
	"strings"
	"time"

	"github.com/b-ovsepian/spoloh/internal/player"
	"github.com/b-ovsepian/spoloh/internal/radio"
)

// radioFailureLogInterval throttles the line for a stream that will not play.
// The player retries a dead station every few minutes for as long as it stays
// configured, and a line per retry is hundreds a day onto an SD card that
// wears out, about something that is not an emergency.
const radioFailureLogInterval = 30 * time.Minute

// radioTick tells the player what, if anything, to stream while it has
// nothing to announce. Once a second, like everything else in Tick: the
// player ignores a background it already has, so saying it again costs
// nothing, and saying it every second is what makes a window opening, quiet
// hours starting or a raid beginning take effect within a second.
func (e *Engine) radioTick(now time.Time) {
	if e.d.Player == nil {
		return
	}
	e.mu.RLock()
	alert := e.alert
	e.mu.RUnlock()
	// A nil checker counts as synchronised, as it does for the scheduler.
	synced := e.d.Sync == nil || e.d.Sync.Synced()

	cfg := e.d.Config()
	var want player.Background
	if url, ok := radio.Due(cfg, now, radio.State{Synced: synced, Alert: alert}); ok {
		// The card at the master volume, which no announcement exceeds, and
		// the radio's own level applied in software: see player.Background.
		want = player.Background{URL: url, Volume: cfg.Audio.MasterVolume, Gain: cfg.Radio.Volume}
	}
	if want != e.radioWant {
		if want.URL == "" {
			e.d.Logger.Info("radio off")
		} else {
			e.d.Logger.Info("radio on", "url", radio.Redact(want.URL), "volume", want.Gain)
		}
		e.radioWant = want
	}
	e.d.Player.SetBackground(want)
}

// radioFailed is the player's report of a stream that would not play. It is
// retried regardless; this only says so, at most every
// radioFailureLogInterval.
func (e *Engine) radioFailed(b player.Background, err error) {
	now := e.d.Clock.Now()
	e.radioErrMu.Lock()
	quiet := !e.radioErrLogged.IsZero() && now.Sub(e.radioErrLogged) < radioFailureLogInterval && !now.Before(e.radioErrLogged)
	if !quiet {
		e.radioErrLogged = now
	}
	e.radioErrMu.Unlock()
	if !quiet {
		// ffmpeg names the URL it could not open, whole; the journal gets it
		// without whatever secret a stream of the operator's own carries.
		redacted := radio.Redact(b.URL)
		e.d.Logger.Warn("radio stream failed; retrying",
			"url", redacted, "err", strings.ReplaceAll(err.Error(), b.URL, redacted))
	}
}
