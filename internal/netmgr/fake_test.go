package netmgr_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/b-ovsepian/spoloh/internal/netmgr"
)

func TestFakeScanReturnsScriptedNetworks(t *testing.T) {
	f := &netmgr.Fake{Networks: []netmgr.Network{{SSID: "A", Signal: 50}}}
	got, err := f.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 1 || got[0].SSID != "A" {
		t.Fatalf("Scan = %+v, want the scripted network", got)
	}
}

func TestFakeScanReturnsScriptedError(t *testing.T) {
	wantErr := errors.New("scan failed")
	f := &netmgr.Fake{ScanErr: wantErr}
	_, err := f.Scan(context.Background())
	if !errors.Is(err, wantErr) {
		t.Fatalf("Scan err = %v, want %v", err, wantErr)
	}
}

func TestFakeConnectUpdatesStatusOnSuccess(t *testing.T) {
	f := &netmgr.Fake{}
	if err := f.Connect(context.Background(), "Kyivstar_Home", "secret"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	st, err := f.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Connected || st.SSID != "Kyivstar_Home" {
		t.Fatalf("Status after Connect = %+v, want connected to Kyivstar_Home", st)
	}
}

func TestFakeConnectDoesNotUpdateStatusOnError(t *testing.T) {
	wantErr := errors.New("bad password")
	f := &netmgr.Fake{ConnectErr: wantErr}
	if err := f.Connect(context.Background(), "Kyivstar_Home", "wrong"); !errors.Is(err, wantErr) {
		t.Fatalf("Connect err = %v, want %v", err, wantErr)
	}
	st, _ := f.Status(context.Background())
	if st.Connected {
		t.Fatalf("Status after failed Connect = %+v, want unchanged", st)
	}
}

func TestFakeStartAndStopHotspot(t *testing.T) {
	f := &netmgr.Fake{}
	if err := f.StartHotspot(context.Background(), "Spoloh-Setup", "changeme"); err != nil {
		t.Fatalf("StartHotspot: %v", err)
	}
	st, _ := f.Status(context.Background())
	if !st.HotspotUp {
		t.Fatal("HotspotUp = false after StartHotspot")
	}
	if err := f.StopHotspot(context.Background()); err != nil {
		t.Fatalf("StopHotspot: %v", err)
	}
	st, _ = f.Status(context.Background())
	if st.HotspotUp {
		t.Fatal("HotspotUp = true after StopHotspot")
	}
}

func TestFakeRecordsCallLog(t *testing.T) {
	f := &netmgr.Fake{}
	ctx := context.Background()
	_, _ = f.Scan(ctx)
	_ = f.Connect(ctx, "SSID", "psk")
	_ = f.StartHotspot(ctx, "Hotspot", "pw")
	_ = f.StopHotspot(ctx)
	_, _ = f.Status(ctx)

	want := []string{"Scan", "Connect", "StartHotspot", "StopHotspot", "Status"}
	calls := f.Calls()
	if len(calls) != len(want) {
		t.Fatalf("Calls = %+v, want %d entries", calls, len(want))
	}
	for i, m := range want {
		if calls[i].Method != m {
			t.Errorf("Calls[%d].Method = %q, want %q", i, calls[i].Method, m)
		}
	}
	if calls[1].SSID != "SSID" || calls[1].PSK != "psk" {
		t.Errorf("Connect call not recorded with its arguments: %+v", calls[1])
	}
}

// The supervisor and an HTTP handler both hold the same Fake concurrently;
// -race must find nothing wrong with that.
func TestFakeIsSafeForConcurrentUse(t *testing.T) {
	f := &netmgr.Fake{Networks: []netmgr.Network{{SSID: "A", Signal: 1}}}
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(4)
		go func() { defer wg.Done(); _, _ = f.Scan(ctx) }()
		go func() { defer wg.Done(); _, _ = f.Status(ctx) }()
		go func() { defer wg.Done(); _ = f.Connect(ctx, "X", "y") }()
		go func() { defer wg.Done(); _ = f.StartHotspot(ctx, "X", "y") }()
	}
	wg.Wait()
}

var (
	_ netmgr.Manager = (*netmgr.Fake)(nil)
	_ netmgr.Manager = netmgr.NMCLI{}
)
