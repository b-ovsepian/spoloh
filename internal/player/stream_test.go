package player

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

var _ Streamer = ALSA{}

// The whole command, because every flag in it is load-bearing: the protocol
// whitelist is what stops an operator's URL reading files off the board, the
// reconnect flags ride out a dropped connection without the player's backoff,
// -rw_timeout ends a connection that stalls without dropping (no reconnect
// fires for that, and ffmpeg would wait on it forever while the page said the
// radio played), the volume filter is the radio's own level — the card stays
// at the master volume, see Background — and -nostdin keeps ffmpeg off the
// daemon's stdin.
func TestStreamRunsFfmpegIntoTheDevice(t *testing.T) {
	var argv []string
	a := ALSA{Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		argv = append([]string{name}, args...)
		return nil, nil
	}}
	if err := a.Stream(context.Background(), "plughw:CARD=Headphones,DEV=0", "https://online.hitfm.ua/HitFM", 35); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	want := []string{
		"ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error",
		"-protocol_whitelist", "http,https,tls,tcp,crypto",
		"-rw_timeout", "15000000",
		"-reconnect", "1", "-reconnect_streamed", "1", "-reconnect_delay_max", "30",
		"-i", "https://online.hitfm.ua/HitFM",
		"-af", "volume=0.35",
		"-f", "alsa", "plughw:CARD=Headphones,DEV=0",
	}
	if !slices.Equal(argv, want) {
		t.Fatalf("argv = %q\nwant  %q", argv, want)
	}
}

func TestStreamIntoTheDefaultDeviceNamesIt(t *testing.T) {
	var argv []string
	a := ALSA{Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		argv = args
		return nil, nil
	}}
	_ = a.Stream(context.Background(), "", "https://online.hitfm.ua/HitFM", 50)
	if argv[len(argv)-1] != "default" {
		t.Fatalf("argv ends %q, want the default device named", argv[len(argv)-1])
	}
}

// Verbatim from a Raspberry Pi: ffmpeg asked for a stream the server does not
// have. The error has to carry ffmpeg's own reason — the operator reading
// "exit status 8" has nothing to act on, "404 Not Found" tells them the
// station moved.
func TestAFailedStreamCarriesFfmpegsReason(t *testing.T) {
	out := readFixture(t, "ffmpeg-stream-404.txt")
	a := ALSA{Run: func(context.Context, string, ...string) ([]byte, error) {
		return out, errors.New("exit status 8")
	}}
	err := a.Stream(context.Background(), "plughw:CARD=Headphones,DEV=0", "https://online.hitfm.ua/NoSuchStream", 50)
	if err == nil {
		t.Fatal("a failed ffmpeg was not an error")
	}
	if !strings.Contains(err.Error(), "404 Not Found") {
		t.Errorf("err = %v, want ffmpeg's reason in it", err)
	}
	if !strings.Contains(err.Error(), "plughw:CARD=Headphones,DEV=0") {
		t.Errorf("err = %v, want the device named", err)
	}
}

// A stream runs for hours, and ffmpeg at -loglevel error still prints a line
// for every damaged frame after a reconnect. All of it held in memory and
// then written as one journal line is the wrong shape for a board with 400 MB;
// the end of it is where the reason is.
func TestATailBufferKeepsOnlyTheEnd(t *testing.T) {
	var tb tailBuffer
	tb.limit = 16
	for i := 0; i < 100; i++ {
		_, _ = tb.Write([]byte("Header missing\n"))
	}
	_, _ = tb.Write([]byte("404 Not Found"))
	got := tb.String()
	if len(got) > 16 {
		t.Fatalf("kept %d bytes, want at most 16", len(got))
	}
	if !strings.HasSuffix(got, "404 Not Found") {
		t.Fatalf("kept %q, want the last bytes written", got)
	}
}
