package main

import (
	"bytes"
	"net"
	"strings"
	"testing"
	"time"
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
		{[]string{"light", "toggle"}, "light toggle", 2 * time.Second, true},
		{[]string{"light", "list"}, "light list", 2 * time.Second, true},
		{[]string{"light", "name", "COM4", "desk"}, "light name COM4 desk", 2 * time.Second, true},
		{[]string{"light@desk", "toggle"}, "light@desk toggle", 2 * time.Second, true},
		{[]string{"light@COM4", "brightness", "30"}, "light@COM4 brightness 30", 2 * time.Second, true},
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
