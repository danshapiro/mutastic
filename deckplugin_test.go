package main

import (
	"fmt"
	"mutastic/internal/light"
	"net"
	"strings"
	"testing"
	"time"
)

// TestAskDaemon exercises the reply-returning UDP round trip against a
// scripted fake daemon on an ephemeral port (same idiom as
// main_test.go's runClient tests).
func TestAskDaemon(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 64)
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		if string(buf[:n]) != "status" {
			pc.WriteTo([]byte("error: unknown command"), addr)
			return
		}
		pc.WriteTo([]byte("muted\n"), addr) // trailing newline: reply must be trimmed
	}()
	reply, err := askDaemon("status", pc.LocalAddr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("askDaemon: %v", err)
	}
	if reply != "muted" {
		t.Fatalf("reply = %q, want %q (trimmed)", reply, "muted")
	}
}

// TestAskDaemonSettingsListRoundTripsBeyond2048 guards the saved-settings
// list read path (F2): the daemon's store caps at 100 names x <=42 bytes
// (~4.3 KB worst case), so a "light settings list" reply can far exceed a
// small client read buffer. askDaemon reads with an 8192-byte buffer —
// the single root-package read path shared by the CLI, web UI, and tray —
// so a full store's list round-trips byte-exact; under the old 2048-byte
// buffer the reply truncated mid-name and saved names silently vanished
// from the tray menu and web UI.
func TestAskDaemonSettingsListRoundTripsBeyond2048(t *testing.T) {
	// 70 names x 34 bytes (~2.4 KB) > the old 2048-byte buffer, well
	// under 8192.
	names := make([]string, 0, 70)
	for i := 0; i < 70; i++ {
		names = append(names, fmt.Sprintf("saved-light-setting-number-%02d-xxxx", i))
	}
	want := strings.Join(names, "\n")
	if len(want) <= 2048 {
		t.Fatalf("scripted reply is %d bytes, want > 2048 (the test guards truncation past the old buffer)", len(want))
	}
	if len(want) >= 8192 {
		t.Fatalf("scripted reply is %d bytes, want < 8192 (the raised buffer must hold a full store's list)", len(want))
	}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 64)
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		if string(buf[:n]) != "light settings list" {
			pc.WriteTo([]byte("error: unknown command"), addr)
			return
		}
		pc.WriteTo([]byte(want), addr)
	}()
	reply, err := askDaemon("light settings list", pc.LocalAddr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("askDaemon: %v", err)
	}
	if reply != want {
		t.Fatalf("reply %d bytes, want byte-exact %d (truncated mid-name past the 8192-byte buffer floor)", len(reply), len(want))
	}
}

func TestAskDaemonUnreachable(t *testing.T) {
	// Bind then close: guarantees nothing listens on the port.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := pc.LocalAddr().String()
	pc.Close()
	if _, err := askDaemon("status", addr, 200*time.Millisecond); err == nil {
		t.Fatal("askDaemon to a dead port succeeded, want error")
	}
}

// TestCommandTimeout pins the per-verb UDP budgets: mic verbs stay
// snappy at 1s; light verbs get light.CallTimeout+500ms so a wedged
// light's degraded-mode reply (which lands just after CallTimeout) is
// still readable. The prefix rule mirrors the daemon's own routing:
// "light" + end-of-string, space, or '@' — "lightning" is a mic-side
// (unknown) command.
func TestCommandTimeout(t *testing.T) {
	wantLight := light.CallTimeout + 500*time.Millisecond
	cases := []struct {
		cmd  string
		want time.Duration
	}{
		{"status", time.Second},
		{"toggle", time.Second},
		{"light status", wantLight},
		{"light toggle", wantLight},
		{"light", wantLight},
		{"light@desk-right brightness 30", wantLight},
		{"lightning", time.Second},
	}
	for _, c := range cases {
		if got := commandTimeout(c.cmd); got != c.want {
			t.Errorf("commandTimeout(%q) = %v, want %v", c.cmd, got, c.want)
		}
	}
}

// TestLightPluginTimeoutExceedsDaemonStallBound mirrors
// TestLightClientTimeoutExceedsDaemonStallBound in main_test.go: a
// budget at or below light.CallTimeout deterministically misses the
// degraded-mode reply and masks partial success as daemon failure.
func TestLightPluginTimeoutExceedsDaemonStallBound(t *testing.T) {
	if lightPluginTimeout <= light.CallTimeout {
		t.Fatalf("lightPluginTimeout = %v, want > light.CallTimeout (%v)", lightPluginTimeout, light.CallTimeout)
	}
}
