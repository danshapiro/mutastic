package main

import (
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
