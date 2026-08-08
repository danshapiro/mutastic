package daemon

import (
	"io"
	"log"
	"sync"
	"testing"
	"time"
)

// fakeDevice implements Device for tests. Reads block on the events channel
// (10ms poll timeout); Writes are recorded.
type fakeDevice struct {
	mu      sync.Mutex
	writes  [][]byte
	events  chan []byte
	readErr chan error
}

func newFakeDevice() *fakeDevice {
	return &fakeDevice{events: make(chan []byte, 8), readErr: make(chan error, 1)}
}

func (f *fakeDevice) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := make([]byte, len(p))
	copy(c, p)
	f.writes = append(f.writes, c)
	return len(p), nil
}

func (f *fakeDevice) ReadWithTimeout(p []byte, _ time.Duration) (int, error) {
	select {
	case ev := <-f.events:
		return copy(p, ev), nil
	case err := <-f.readErr:
		return 0, err
	case <-time.After(10 * time.Millisecond):
		return 0, nil // timeout, no data
	}
}

func (f *fakeDevice) Close() error { return nil }

func (f *fakeDevice) writeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.writes)
}

func (f *fakeDevice) write(i int) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writes[i]
}

func testLogger() *log.Logger { return log.New(io.Discard, "", 0) }

func TestHandleCommandNoDevice(t *testing.T) {
	d := New(testLogger())
	if got := d.HandleCommand("toggle"); got != "error: no device" {
		t.Fatalf("toggle with no device = %q, want %q", got, "error: no device")
	}
	if got := d.HandleCommand("status"); got != "unknown" {
		t.Fatalf("status with no state = %q, want %q", got, "unknown")
	}
}

func TestHandleCommandMuteUnmute(t *testing.T) {
	d := New(testLogger())
	dev := newFakeDevice()
	d.SetDevice(dev)

	if got := d.HandleCommand("mute"); got != "muted" {
		t.Fatalf("mute = %q, want %q", got, "muted")
	}
	w := dev.write(0)
	if w[4] != 0x20 || w[8] != 0x09 || w[9] != '1' {
		t.Fatalf("mute wrote % x; want op 0x20 len 0x09 payload '1'", w[:12])
	}
	if got := d.HandleCommand("status"); got != "muted" {
		t.Fatalf("status after mute = %q, want %q", got, "muted")
	}

	if got := d.HandleCommand("unmute"); got != "unmuted" {
		t.Fatalf("unmute = %q, want %q", got, "unmuted")
	}
	w = dev.write(1)
	if w[4] != 0x20 || w[9] != '0' {
		t.Fatalf("unmute wrote % x; want op 0x20 payload '0'", w[:12])
	}
}

func TestHandleCommandToggle(t *testing.T) {
	d := New(testLogger())
	dev := newFakeDevice()
	d.SetDevice(dev)

	// Unknown state: toggle defaults to mute.
	if got := d.HandleCommand("toggle"); got != "muted" {
		t.Fatalf("first toggle = %q, want %q", got, "muted")
	}
	if w := dev.write(0); w[9] != '1' {
		t.Fatalf("first toggle payload = %q, want '1'", w[9])
	}
	// Now known muted: toggle unmutes.
	if got := d.HandleCommand("toggle"); got != "unmuted" {
		t.Fatalf("second toggle = %q, want %q", got, "unmuted")
	}
	if w := dev.write(1); w[9] != '0' {
		t.Fatalf("second toggle payload = %q, want '0'", w[9])
	}
}

func TestHandleCommandUnknown(t *testing.T) {
	d := New(testLogger())
	if got := d.HandleCommand("frobnicate"); got != "error: unknown command" {
		t.Fatalf("unknown command reply = %q", got)
	}
}
