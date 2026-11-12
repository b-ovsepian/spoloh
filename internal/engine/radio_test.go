package engine_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/b-ovsepian/spoloh/internal/alertstate"
	"github.com/b-ovsepian/spoloh/internal/clock"
	"github.com/b-ovsepian/spoloh/internal/config"
	"github.com/b-ovsepian/spoloh/internal/engine"
	"github.com/b-ovsepian/spoloh/internal/player"
	"github.com/b-ovsepian/spoloh/internal/radio"
)

// A Wednesday, inside a weekday 08:00–18:00 window.
var radioNoon = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

func radioHarness(t *testing.T) (*harness, *clock.Fake) {
	t.Helper()
	clk := clock.NewFake(radioNoon)
	h := newHarnessWithClock(t, clk)
	h.cfg.Audio.MasterVolume = 80
	h.cfg.Radio = config.Radio{
		Enabled: true, Station: "hitfm", Volume: 50,
		Weekdays: []int{1, 2, 3, 4, 5}, From: "08:00", To: "18:00",
	}
	return h, clk
}

func TestTheRadioPlaysInsideItsWindow(t *testing.T) {
	h, _ := radioHarness(t)
	h.eng.Tick(context.Background())

	// The card at the master volume and the radio quieter in software: an
	// announcement whose own volume stage fails then plays at the master
	// volume, never at whatever the radio was turned down to.
	hit, _ := radio.Lookup("hitfm")
	if got, want := h.pl.Background(), (player.Background{URL: hit.URL, Volume: 80, Gain: 50}); got != want {
		t.Fatalf("background = %+v, want %+v", got, want)
	}
}

func waitStream(t *testing.T, r *recorder, want string) {
	t.Helper()
	select {
	case got := <-r.streams:
		if got != want {
			t.Fatalf("radio went %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("radio never went %q", want)
	}
}

// End to end, through the real player: the radio is playing, a raid begins,
// the radio is cut, the siren plays, and the radio comes back.
func TestASirenCutsTheRadioAndTheRadioReturns(t *testing.T) {
	h, _ := radioHarness(t)
	ctx := context.Background()
	pollTimes(t, h, ctx, 1)
	h.eng.Tick(ctx)
	waitStream(t, h.rec, "on")

	h.set(true)
	pollTimes(t, h, ctx, 1)
	waitStream(t, h.rec, "off")
	if got := waitPlayed(t, h.rec); got != "siren.wav" {
		t.Fatalf("played %q, want siren.wav", got)
	}
	waitStream(t, h.rec, "on")
}

// A stream of the operator's own may carry a password or a token; neither
// reaches the journal, from the radio starting or from it failing.
func TestTheRadioKeepsItsSecretsOutOfTheJournal(t *testing.T) {
	clk := clock.NewFake(radioNoon)
	var buf bytes.Buffer
	h := newHarnessWith(t, alertstate.New(), clk, func(d *engine.Deps) {
		d.Logger = slog.New(slog.NewTextHandler(&buf, nil))
	})
	const secretURL = "https://user:hunter2@stream.example/live?token=s3cret"
	h.cfg.Radio = config.Radio{
		Enabled: true, Station: radio.Custom, URL: secretURL, Volume: 50,
		Weekdays: []int{1, 2, 3, 4, 5}, From: "08:00", To: "18:00",
	}
	h.eng.Tick(context.Background())
	h.pl.OnBackgroundError(h.pl.Background(), errors.New("Error opening input file "+secretURL+"."))

	out := buf.String()
	if !strings.Contains(out, "stream.example") {
		t.Fatalf("the journal does not say which stream:\n%s", out)
	}
	for _, secret := range []string{"hunter2", "s3cret"} {
		if strings.Contains(out, secret) {
			t.Errorf("the journal carries %q:\n%s", secret, out)
		}
	}
}

func TestTheRadioStopsWhenItsWindowCloses(t *testing.T) {
	h, clk := radioHarness(t)
	h.eng.Tick(context.Background())
	clk.Set(radioNoon.Add(6 * time.Hour))
	h.eng.Tick(context.Background())

	if got := h.pl.Background(); got != (player.Background{}) {
		t.Fatalf("background = %+v at 18:00, want none", got)
	}
}

// Between a raid's announcements the radio plays on — the default the
// operator asked for — and only an operator who ticked the box gets it
// paused from siren to all-clear.
func TestARaidPausesTheRadioOnlyWhenAskedTo(t *testing.T) {
	h, _ := radioHarness(t)
	ctx := context.Background()
	pollTimes(t, h, ctx, 1)
	h.set(true)
	pollTimes(t, h, ctx, 1)
	waitPlayed(t, h.rec)

	h.eng.Tick(ctx)
	if h.pl.Background() == (player.Background{}) {
		t.Fatal("a raid stopped the radio with PauseDuringAlert off")
	}

	h.cfg.Radio.PauseDuringAlert = true
	h.eng.Tick(ctx)
	if got := h.pl.Background(); got != (player.Background{}) {
		t.Fatalf("background = %+v during a raid with PauseDuringAlert on, want none", got)
	}
}

// A dead station is retried every few minutes for as long as it stays
// configured; the journal hears about it once per half hour, not per retry.
func TestAFailingStationIsLoggedAtMostEveryHalfHour(t *testing.T) {
	clk := clock.NewFake(radioNoon)
	var buf bytes.Buffer
	h := newHarnessWith(t, alertstate.New(), clk, func(d *engine.Deps) {
		d.Logger = slog.New(slog.NewTextHandler(&buf, nil))
	})
	bg := player.Background{URL: "https://online.hitfm.ua/HitFM", Volume: 40}
	fail := func() { h.pl.OnBackgroundError(bg, errors.New("Server returned 404 Not Found")) }

	fail()
	clk.Advance(5 * time.Minute)
	fail()
	clk.Advance(20 * time.Minute)
	fail()
	if n := strings.Count(buf.String(), "radio stream failed"); n != 1 {
		t.Fatalf("%d lines in 25 minutes of failures, want 1:\n%s", n, buf.String())
	}
	clk.Advance(6 * time.Minute)
	fail()
	if n := strings.Count(buf.String(), "radio stream failed"); n != 2 {
		t.Fatalf("%d lines after 31 minutes, want 2:\n%s", n, buf.String())
	}
}
