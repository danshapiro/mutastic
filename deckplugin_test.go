package main

import (
	"mutastic/internal/light"
	"net"
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
