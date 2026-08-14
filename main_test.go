package main

import (
	"bytes"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	"mutastic/internal/light"
)

func TestRunClientRoundTrip(t *testing.T) {
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
		if string(buf[:n]) == "status" {
			pc.WriteTo([]byte("muted"), addr)
		}
	}()

	var out bytes.Buffer
	code := runClient("status", pc.LocalAddr().String(), 2*time.Second, &out)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got := strings.TrimSpace(out.String()); got != "muted" {
		t.Fatalf("output = %q, want muted", got)
	}
}

func TestRunClientErrorReply(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 64)
		_, addr, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		pc.WriteTo([]byte("error: no device"), addr)
	}()

	var out bytes.Buffer
	if code := runClient("mute", pc.LocalAddr().String(), 2*time.Second, &out); code != 1 {
		t.Fatalf("exit code = %d, want 1 for an error reply", code)
	}
}

func TestRunClientNoDaemon(t *testing.T) {
	var out bytes.Buffer
	// Nothing listens on this port; expect timeout/refusal -> exit 2.
	code := runClient("status", "127.0.0.1:59999", 300*time.Millisecond, &out)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 when no daemon is reachable", code)
	}
	if !strings.Contains(out.String(), "no daemon reachable") {
		t.Fatalf("output = %q, want it to mention 'no daemon reachable'", out.String())
	}
}

func TestRunClientPassesMultiWordCommandVerbatim(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	gotCmd := make(chan string, 1)
	go func() {
		buf := make([]byte, 64)
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		gotCmd <- string(buf[:n])
		pc.WriteTo([]byte("on 100% 4950K"), addr)
	}()

	var out bytes.Buffer
	code := runClient("light brightness 100", pc.LocalAddr().String(), time.Second, &out)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (output %q)", code, out.String())
	}
	if got := <-gotCmd; got != "light brightness 100" {
		t.Fatalf("daemon received %q, want %q", got, "light brightness 100")
	}
	if got := strings.TrimSpace(out.String()); got != "on 100% 4950K" {
		t.Fatalf("printed %q, want the reply", got)
	}
}

func TestClientCommand(t *testing.T) {
	cases := []struct {
		args    []string
		cmd     string
		timeout time.Duration
		ok      bool
	}{
		{[]string{"status"}, "status", time.Second, true},
		{[]string{"toggle"}, "toggle", time.Second, true},
		{[]string{"mute"}, "mute", time.Second, true},
		{[]string{"unmute"}, "unmute", time.Second, true},
		{[]string{"shutdown"}, "shutdown", lightClientTimeout, true},
		{[]string{"light", "toggle"}, "light toggle", lightClientTimeout, true},
		{[]string{"light", "list"}, "light list", lightClientTimeout, true},
		{[]string{"light", "name", "COM4", "desk"}, "light name COM4 desk", lightClientTimeout, true},
		{[]string{"light@desk", "toggle"}, "light@desk toggle", lightClientTimeout, true},
		{[]string{"light@COM4", "brightness", "30"}, "light@COM4 brightness 30", lightClientTimeout, true},
		{[]string{"light"}, "", 0, false},
		{[]string{"light@desk"}, "", 0, false},
		{[]string{"frobnicate"}, "", 0, false},
		{nil, "", 0, false},
	}
	for _, c := range cases {
		cmd, timeout, ok := clientCommand(c.args)
		if cmd != c.cmd || timeout != c.timeout || ok != c.ok {
			t.Errorf("clientCommand(%v) = (%q, %v, %v), want (%q, %v, %v)",
				c.args, cmd, timeout, ok, c.cmd, c.timeout, c.ok)
		}
	}
}

func TestRunClientPrintsLargeReply(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	reply := strings.TrimSpace(strings.Repeat("COM4 desk connected on 30% 2900K\n", 12)) // ~390 bytes, > the old 256
	go func() {
		buf := make([]byte, 64)
		_, addr, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		pc.WriteTo([]byte(reply), addr)
	}()
	var out bytes.Buffer
	code := runClient("light list", pc.LocalAddr().String(), time.Second, &out)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if got := strings.TrimSpace(out.String()); got != reply {
		t.Fatalf("reply truncated: got %d bytes, want %d", len(got), len(reply))
	}
}

// TestLightClientTimeoutExceedsDaemonStallBound pins the timeout invariant
// from the wedged-light degraded mode: the daemon's per-light stall bound
// (light.CallTimeout) starts ticking only after the packet arrives, so the
// client's light budget must exceed it by a REAL margin or the daemon's
// reply (healthy lights' results + the per-line "error: timeout") arrives
// after the client already printed "error: no daemon reachable".
func TestLightClientTimeoutExceedsDaemonStallBound(t *testing.T) {
	if lightClientTimeout < 2*light.CallTimeout {
		t.Fatalf("lightClientTimeout = %v, want >= 2x light.CallTimeout (%v): without real headroom over the daemon's per-light stall bound, degraded-mode replies are masked as total daemon failure", lightClientTimeout, light.CallTimeout)
	}
}

// TestRunClientReceivesStalledLightReply exercises the client-timeout /
// daemon-stall interplay end to end at the UDP layer: the fake daemon
// answers a light verb only after light.CallTimeout has fully elapsed
// (exactly what a wedged light causes), and the client - using the real
// budget clientCommand assigns to light verbs - must still print the
// per-light reply lines instead of "no daemon reachable".
func TestRunClientReceivesStalledLightReply(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	reply := "COM4: on 30% 2900K\nCOM7: error: timeout"
	go func() {
		buf := make([]byte, 64)
		_, addr, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		// The daemon's per-light timer starts AFTER the packet arrives.
		time.Sleep(light.CallTimeout + 100*time.Millisecond)
		pc.WriteTo([]byte(reply), addr)
	}()

	cmd, timeout, ok := clientCommand([]string{"light", "on"})
	if !ok {
		t.Fatal("clientCommand rejected 'light on'")
	}
	var out bytes.Buffer
	code := runClient(cmd, pc.LocalAddr().String(), timeout, &out)
	got := strings.TrimSpace(out.String())
	if code != 0 || got != reply {
		t.Fatalf("exit = %d, output = %q; want 0 and the daemon's per-light reply %q (a masked 'no daemon reachable' means the client budget does not cover the daemon's stall bound)", code, got, reply)
	}
}

func TestRunTestInjectUnsupportedOffWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exercises the non-Windows stub")
	}
	var out bytes.Buffer
	if got := runTestInject(&out); got != 1 {
		t.Fatalf("runTestInject() = %d, want 1 on non-Windows builds", got)
	}
	if !strings.Contains(out.String(), "only supported on Windows") {
		t.Fatalf("runTestInject() output = %q, want the platform error", out.String())
	}
}
