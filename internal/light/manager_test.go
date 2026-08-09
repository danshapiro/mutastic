package light

import (
	"bytes"
	"io"
	"log"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakePort implements Port for tests, mirroring the mic's fakeDevice: reads
// block on channels (10ms poll timeout), writes are recorded.
type fakePort struct {
	mu      sync.Mutex
	writes  [][]byte
	reads   chan []byte
	readErr chan error
}

func newFakePort() *fakePort {
	return &fakePort{reads: make(chan []byte, 8), readErr: make(chan error, 1)}
}

func (f *fakePort) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := make([]byte, len(p))
	copy(c, p)
	f.writes = append(f.writes, c)
	return len(p), nil
}

func (f *fakePort) Read(p []byte) (int, error) {
	select {
	case data := <-f.reads:
		return copy(p, data), nil
	case err := <-f.readErr:
		return 0, err
	case <-time.After(10 * time.Millisecond):
		return 0, nil // timeout, no data
	}
}

func (f *fakePort) Close() error { return nil }

func (f *fakePort) writeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.writes)
}

func (f *fakePort) write(i int) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writes[i]
}

func testLogger() *log.Logger { return log.New(io.Discard, "", 0) }

func setFastWrites(t *testing.T) {
	t.Helper()
	old := writeSpacing
	writeSpacing = time.Millisecond
	t.Cleanup(func() { writeSpacing = old })
}

func TestHandleCommandWithoutPort(t *testing.T) {
	m := NewManager(testLogger(), "")
	if got := m.HandleCommand("on"); got != "error: no light" {
		t.Fatalf("on without port = %q, want %q", got, "error: no light")
	}
	if got := m.HandleCommand("status"); got != "unknown" {
		t.Fatalf("status needs no port; got %q, want unknown", got)
	}
}

func TestHandleCommandToggleCycle(t *testing.T) {
	setFastWrites(t)
	m := NewManager(testLogger(), "")
	p := newFakePort()
	m.setPort(p)

	// Unknown state counts as off: toggle turns ON at the defaults.
	if got := m.HandleCommand("toggle"); got != "on 100% 4950K" {
		t.Fatalf("toggle from unknown = %q, want %q", got, "on 100% 4950K")
	}
	wantOn := []byte{0x3A, 0x02, 0x03, 0x01, 0x64, 0x09, 0x00, 0xAD}
	if p.writeCount() != 1 || !bytes.Equal(p.write(0), wantOn) {
		t.Fatalf("frame 0 = % x, want % x", p.write(0), wantOn)
	}

	// Toggle again: off via brightness 0, temp retained.
	if got := m.HandleCommand("toggle"); got != "off" {
		t.Fatalf("toggle from on = %q, want off", got)
	}
	wantOff := []byte{0x3A, 0x02, 0x03, 0x01, 0x00, 0x09, 0x00, 0x49}
	if !bytes.Equal(p.write(1), wantOff) {
		t.Fatalf("frame 1 = % x, want % x", p.write(1), wantOff)
	}

	// Toggle once more: back on at the remembered brightness.
	if got := m.HandleCommand("toggle"); got != "on 100% 4950K" {
		t.Fatalf("toggle from off = %q, want %q", got, "on 100% 4950K")
	}
}

func TestHandleCommandBrightness(t *testing.T) {
	setFastWrites(t)
	m := NewManager(testLogger(), "")
	p := newFakePort()
	m.setPort(p)

	if got := m.HandleCommand("brightness 40"); got != "on 40% 4950K" {
		t.Fatalf("brightness 40 = %q, want %q", got, "on 40% 4950K")
	}
	want := []byte{0x3A, 0x02, 0x03, 0x01, 0x28, 0x09, 0x00, 0x71}
	if !bytes.Equal(p.write(0), want) {
		t.Fatalf("frame = % x, want % x", p.write(0), want)
	}
	if got := m.HandleCommand("brightness 0"); got != "off" {
		t.Fatalf("brightness 0 = %q, want off", got)
	}
	for _, bad := range []string{"brightness", "brightness 101", "brightness -1", "brightness x"} {
		if got := m.HandleCommand(bad); got != "error: brightness must be 0-100" {
			t.Fatalf("%q = %q, want validation error", bad, got)
		}
	}
	if p.writeCount() != 2 {
		t.Fatalf("invalid commands must not write; wrote %d frames", p.writeCount())
	}
}

func TestHandleCommandTemp(t *testing.T) {
	setFastWrites(t)
	m := NewManager(testLogger(), "")
	p := newFakePort()
	m.setPort(p)

	// While off/unknown: temp change turns the light ON at the restore
	// brightness (documented choice).
	if got := m.HandleCommand("temp 3400"); got != "on 100% 3356K" {
		t.Fatalf("temp while unknown = %q, want %q", got, "on 100% 3356K")
	}
	want := []byte{0x3A, 0x02, 0x03, 0x01, 0x64, 0x02, 0x00, 0xA6}
	if !bytes.Equal(p.write(0), want) {
		t.Fatalf("frame = % x, want % x", p.write(0), want)
	}

	// While on: brightness is kept.
	if got := m.HandleCommand("brightness 40"); got != "on 40% 3356K" {
		t.Fatalf("brightness 40 = %q", got)
	}
	if got := m.HandleCommand("temp 7000"); got != "on 40% 7000K" {
		t.Fatalf("temp while on = %q, want %q", got, "on 40% 7000K")
	}
	wantHot := []byte{0x3A, 0x02, 0x03, 0x01, 0x28, 0x12, 0x00, 0x7A}
	if !bytes.Equal(p.write(2), wantHot) {
		t.Fatalf("frame = % x, want % x", p.write(2), wantHot)
	}

	for _, bad := range []string{"temp", "temp 2899", "temp 7001", "temp warm"} {
		if got := m.HandleCommand(bad); got != "error: temp must be 2900-7000" {
			t.Fatalf("%q = %q, want validation error", bad, got)
		}
	}
}

func TestHandleCommandPreset(t *testing.T) {
	setFastWrites(t)
	m := NewManager(testLogger(), "")
	p := newFakePort()
	m.setPort(p)

	if got := m.HandleCommand("preset candle"); got != "on 28% 3356K" {
		t.Fatalf("preset candle = %q, want %q", got, "on 28% 3356K")
	}
	want := []byte{0x3A, 0x02, 0x03, 0x01, 0x1C, 0x02, 0x00, 0x5E}
	if !bytes.Equal(p.write(0), want) {
		t.Fatalf("frame = % x, want % x", p.write(0), want)
	}
	for _, bad := range []string{"preset", "preset disco"} {
		if got := m.HandleCommand(bad); got != "error: unknown preset" {
			t.Fatalf("%q = %q, want unknown preset error", bad, got)
		}
	}
}

func TestHandleCommandUnknownVerb(t *testing.T) {
	m := NewManager(testLogger(), "")
	for _, bad := range []string{"", "blink", "status extra"} {
		if got := m.HandleCommand(bad); got != "error: unknown light command" {
			t.Fatalf("%q = %q, want unknown light command error", bad, got)
		}
	}
}

func TestHandleCommandOnUsesPersistedLook(t *testing.T) {
	setFastWrites(t)
	path := filepath.Join(t.TempDir(), "light-state.json")

	m1 := NewManager(testLogger(), path)
	p1 := newFakePort()
	m1.setPort(p1)
	m1.HandleCommand("brightness 40")
	m1.HandleCommand("temp 7000")
	m1.HandleCommand("off")

	m2 := NewManager(testLogger(), path) // simulated daemon restart
	p2 := newFakePort()
	m2.setPort(p2)
	if got := m2.HandleCommand("status"); got != "unknown" {
		t.Fatalf("status after restart = %q, want unknown", got)
	}
	if got := m2.HandleCommand("on"); got != "on 40% 7000K" {
		t.Fatalf("on after restart = %q, want %q", got, "on 40% 7000K")
	}
	want := []byte{0x3A, 0x02, 0x03, 0x01, 0x28, 0x12, 0x00, 0x7A}
	if !bytes.Equal(p2.write(0), want) {
		t.Fatalf("restored frame = % x, want % x", p2.write(0), want)
	}
}
