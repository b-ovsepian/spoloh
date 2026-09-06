package clock_test

import (
	"testing"
	"time"

	"github.com/spoloh/spoloh/internal/clock"
)

func TestFakeAdvance(t *testing.T) {
	base := time.Date(2026, 9, 6, 9, 0, 0, 0, time.UTC)
	c := clock.NewFake(base)

	if got := c.Now(); !got.Equal(base) {
		t.Fatalf("Now() = %v, want %v", got, base)
	}

	c.Advance(90 * time.Second)
	want := base.Add(90 * time.Second)
	if got := c.Now(); !got.Equal(want) {
		t.Fatalf("after Advance, Now() = %v, want %v", got, want)
	}
}

func TestSystemUsesLocation(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Kyiv")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	c := clock.System{Loc: loc}
	if got := c.Now().Location().String(); got != "Europe/Kyiv" {
		t.Fatalf("location = %q, want Europe/Kyiv", got)
	}
}
