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
