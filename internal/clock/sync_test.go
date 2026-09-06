package clock_test

import (
	"context"
	"errors"
	"testing"

	"github.com/spoloh/spoloh/internal/clock"
)

func TestTimedatectlSynced(t *testing.T) {
	tests := []struct {
		name string
		out  string
		err  error
		want bool
	}{
		{"synchronised", "yes\n", nil, true},
		{"not synchronised", "no\n", nil, false},
		{"command missing", "", errors.New("exec: not found"), false},
		{"unexpected output", "maybe\n", nil, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := clock.Timedatectl{
				Run: func(context.Context, string, ...string) ([]byte, error) {
					return []byte(tc.out), tc.err
				},
			}
			if got := c.Synced(); got != tc.want {
				t.Fatalf("Synced() = %v, want %v", got, tc.want)
			}
		})
	}
}
