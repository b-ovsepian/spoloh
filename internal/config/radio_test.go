package config_test

import (
	"testing"

	"github.com/b-ovsepian/spoloh/internal/config"
)

// A new device does not start playing music at anybody: the radio is a
// choice an operator makes on /radio, never a default.
func TestTheRadioIsOffByDefault(t *testing.T) {
	if config.Default().Radio.Enabled {
		t.Fatal("config.Default() has the radio on")
	}
}

func TestCloneDoesNotShareTheRadioWeekdays(t *testing.T) {
	original := config.Default()
	original.Radio.Weekdays = []int{1, 2, 3}
	clone := original.Clone()
	clone.Radio.Weekdays[0] = 7
	if original.Radio.Weekdays[0] == 7 {
		t.Fatal("Radio.Weekdays is shared with the clone")
	}
}
