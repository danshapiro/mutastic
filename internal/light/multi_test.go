package light

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeFleet drives a MultiManager against fake serial ports: the
// enumerated port set is mutable mid-test (hot-plug), and open() hands out
// the current fakePort for that name.
type fakeFleet struct {
	mu    sync.Mutex
	ports map[string]*fakePort
	fail  bool // enumerate returns an error when set
}

func newFakeFleet(ports ...string) *fakeFleet {
	f := &fakeFleet{ports: map[string]*fakePort{}}
	for _, p := range ports {
		f.ports[p] = newFakePort()
	}
	return f
}

// set replaces the enumerated port set, keeping existing fakePorts for
// ports that survive.
func (f *fakeFleet) set(ports ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	next := map[string]*fakePort{}
	for _, p := range ports {
		if fp, ok := f.ports[p]; ok {
			next[p] = fp
		} else {
			next[p] = newFakePort()
		}
	}
	f.ports = next
}

func (f *fakeFleet) setFail(fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = fail
}

func (f *fakeFleet) enumerate() ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return nil, errors.New("enumerator glitch")
	}
	var names []string
	for p := range f.ports {
		names = append(names, p)
	}
	return names, nil
}

func (f *fakeFleet) open(name string) (Port, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fp, ok := f.ports[name]
	if !ok {
		return nil, errors.New("port gone")
	}
	return fp, nil
}

func (f *fakeFleet) port(name string) *fakePort {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ports[name]
}

// fastRescan shrinks the rescan ticker for hot-plug tests.
func fastRescan(t *testing.T) {
	t.Helper()
	old := rescanInterval
	rescanInterval = 5 * time.Millisecond
	t.Cleanup(func() { rescanInterval = old })
}

// newTestMulti builds a MultiManager over the fleet. stateDir may be ""
// (no persistence). The registry persists to <stateDir>/light-names.json.
func newTestMulti(t *testing.T, fleet *fakeFleet, stateDir string) (*MultiManager, context.Context) {
	t.Helper()
	regPath := ""
	if stateDir != "" {
		regPath = filepath.Join(stateDir, "light-names.json")
	}
	mm := NewMultiManager(testLogger(), stateDir, NewRegistry(regPath), fleet.enumerate, fleet.open)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		mm.stopAll()
	})
	return mm, ctx
}

func sessionManager(t *testing.T, mm *MultiManager, port string) *Manager {
	t.Helper()
	mm.mu.Lock()
	defer mm.mu.Unlock()
	s, ok := mm.sessions[port]
	if !ok {
		t.Fatalf("no session for %s", port)
	}
	return s.m
}

func waitConnected(t *testing.T, mm *MultiManager, ports ...string) {
	t.Helper()
	waitFor(t, "sessions connected", func() bool {
		mm.mu.Lock()
		sessions := make([]*lightSession, 0, len(ports))
		for _, p := range ports {
			s, ok := mm.sessions[p]
			if !ok {
				mm.mu.Unlock()
				return false
			}
			sessions = append(sessions, s)
		}
		mm.mu.Unlock()
		for _, s := range sessions {
			if !s.m.Connected() {
				return false
			}
		}
		return true
	})
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatal(err)
	}
}

func TestRescanStartsSessionPerPort(t *testing.T) {
	fastTimings(t)
	fleet := newFakeFleet("COM4", "COM7")
	mm, ctx := newTestMulti(t, fleet, "")
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4", "COM7")
	for _, p := range []string{"COM4", "COM7"} {
		fp := fleet.port(p)
		waitFor(t, p+" woken", func() bool { return fp.writeCount() >= 1 })
		if !bytes.Equal(fp.write(0), wakeBytes) {
			t.Fatalf("%s first write = % x, want wake bytes", p, fp.write(0))
		}
	}
}

func TestRescanDiscoversHotPluggedLight(t *testing.T) {
	fastTimings(t)
	fastRescan(t)
	fleet := newFakeFleet("COM4")
	mm, ctx := newTestMulti(t, fleet, "")
	go mm.Run(ctx)
	waitConnected(t, mm, "COM4")

	fleet.set("COM4", "COM7") // plug in a second light, no restart
	waitConnected(t, mm, "COM4", "COM7")

	// The stable light must not be churned by rescans: exactly one wake.
	fp4 := fleet.port("COM4")
	wakes := 0
	for i := 0; i < fp4.writeCount(); i++ {
		if bytes.Equal(fp4.write(i), wakeBytes) {
			wakes++
		}
	}
	if wakes != 1 {
		t.Fatalf("COM4 woken %d times, want 1 (session churn)", wakes)
	}
}

func TestRescanStopsRemovedLightAfterTwoMisses(t *testing.T) {
	fastTimings(t)
	fleet := newFakeFleet("COM4", "COM7")
	mm, ctx := newTestMulti(t, fleet, "")
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4", "COM7")

	fleet.set("COM4") // unplug COM7
	mm.rescan(ctx)    // first successful miss: debounced, session must survive
	mm.mu.Lock()
	_, still := mm.sessions["COM7"]
	mm.mu.Unlock()
	if !still {
		t.Fatal("COM7 torn down after ONE missing scan; want 2-miss debounce")
	}
	mm.rescan(ctx) // second consecutive miss: teardown
	mm.mu.Lock()
	_, still = mm.sessions["COM7"]
	mm.mu.Unlock()
	if still {
		t.Fatal("COM7 session still tracked after two consecutive missing scans")
	}
	waitConnected(t, mm, "COM4") // survivor untouched
}

func TestRescanMissCounterResetsWhenPortReappears(t *testing.T) {
	fastTimings(t)
	fleet := newFakeFleet("COM4")
	mm, ctx := newTestMulti(t, fleet, "")
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4")

	fleet.set() // one scan misses COM4...
	mm.rescan(ctx)
	fleet.set("COM4") // ...but it reappears: the miss counter must reset
	mm.rescan(ctx)
	fleet.set()
	mm.rescan(ctx) // a NEW first miss: still debounced
	mm.mu.Lock()
	_, still := mm.sessions["COM4"]
	mm.mu.Unlock()
	if !still {
		t.Fatal("session torn down on non-consecutive misses; counter must reset when seen")
	}
}

func TestRescanSurvivesEnumerateError(t *testing.T) {
	fastTimings(t)
	fleet := newFakeFleet("COM4")
	mm, ctx := newTestMulti(t, fleet, "")
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4")
	fleet.setFail(true)
	mm.rescan(ctx) // fail open: keep current sessions
	waitConnected(t, mm, "COM4")
}

// wedgedPort blocks forever in Write/Read, simulating a serial stack that
// never completes I/O after surprise removal (the unprovable CH340 driver
// property the teardown design must not depend on). Closing block releases
// the leaked goroutine at test end.
type wedgedPort struct{ block chan struct{} }

func newWedgedPort() *wedgedPort { return &wedgedPort{block: make(chan struct{})} }

func (w *wedgedPort) Write(p []byte) (int, error) { <-w.block; return 0, errors.New("gone") }
func (w *wedgedPort) Read(p []byte) (int, error)  { <-w.block; return 0, errors.New("gone") }
func (w *wedgedPort) Close() error                { return nil }

func TestRescanUnblockedByWedgedPort(t *testing.T) {
	fastTimings(t) // shrinks drainTimeout to 50ms - the bound under test
	wedged := newWedgedPort()
	defer close(wedged.block) // release the leaked goroutine at test end
	healthy := newFakePort()
	var mu sync.Mutex
	ports := []string{"COM4", "COM7"}
	enumerate := func() ([]string, error) {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), ports...), nil
	}
	open := func(name string) (Port, error) {
		if name == "COM7" {
			return wedged, nil // COM7's wake Write wedges forever
		}
		return healthy, nil
	}
	mm := NewMultiManager(testLogger(), "", NewRegistry(""), enumerate, open)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		mm.stopAll()
	})
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4") // COM7 never connects: it is stuck in its wake Write

	mu.Lock()
	ports = []string{"COM4"} // unplug the wedged COM7
	mu.Unlock()
	start := time.Now()
	mm.rescan(ctx) // miss 1: debounced
	mm.rescan(ctx) // miss 2: teardown - the drain must time out, not hang
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("rescan blocked %v on a wedged session; want bounded drain", elapsed)
	}
	mm.mu.Lock()
	_, still := mm.sessions["COM7"]
	mm.mu.Unlock()
	if still {
		t.Fatal("wedged COM7 session still tracked after two misses")
	}
	// The fleet lock is free and the survivor keeps working.
	if got := sessionManager(t, mm, "COM4").HandleCommand("brightness 40"); got != "on 40% 4950K" {
		t.Fatalf("survivor reply = %q, want %q", got, "on 40% 4950K")
	}
}

func TestPerPortStateFiles(t *testing.T) {
	fastTimings(t)
	dir := t.TempDir()
	fleet := newFakeFleet("COM4", "COM7")
	mm, ctx := newTestMulti(t, fleet, dir)
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4", "COM7")
	if got := sessionManager(t, mm, "COM4").HandleCommand("brightness 40"); got != "on 40% 4950K" {
		t.Fatalf("COM4 brightness = %q", got)
	}
	if got := sessionManager(t, mm, "COM7").HandleCommand("brightness 80"); got != "on 80% 4950K" {
		t.Fatalf("COM7 brightness = %q", got)
	}
	var got4, got7 struct {
		Brightness int `json:"brightness"`
	}
	readJSON(t, filepath.Join(dir, "light-state-COM4.json"), &got4)
	readJSON(t, filepath.Join(dir, "light-state-COM7.json"), &got7)
	if got4.Brightness != 40 || got7.Brightness != 80 {
		t.Fatalf("persisted brightness = %d/%d, want 40/80", got4.Brightness, got7.Brightness)
	}
}

func TestLegacyStateMigratesForSinglePort(t *testing.T) {
	fastTimings(t)
	dir := t.TempDir()
	legacy := filepath.Join(dir, "light-state.json")
	if err := os.WriteFile(legacy, []byte(`{"on":true,"brightness":30,"temp_byte":0}`), 0o644); err != nil {
		t.Fatal(err)
	}
	fleet := newFakeFleet("COM4")
	mm, ctx := newTestMulti(t, fleet, dir)
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4")
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy file should be gone (stat err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "light-state-COM4.json")); err != nil {
		t.Fatalf("per-port state file missing: %v", err)
	}
	// The restore target carried over: "on" restores 30% 2900K.
	if got := sessionManager(t, mm, "COM4").HandleCommand("on"); got != "on 30% 2900K" {
		t.Fatalf("on after migration = %q, want %q", got, "on 30% 2900K")
	}
}

func TestLegacyStateKeptWhenTwoPortsPresent(t *testing.T) {
	fastTimings(t)
	dir := t.TempDir()
	legacy := filepath.Join(dir, "light-state.json")
	if err := os.WriteFile(legacy, []byte(`{"on":true,"brightness":30,"temp_byte":0}`), 0o644); err != nil {
		t.Fatal(err)
	}
	fleet := newFakeFleet("COM4", "COM7")
	mm, ctx := newTestMulti(t, fleet, dir)
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4", "COM7")
	// Ambiguous which port owned the legacy state: nobody inherits it.
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("legacy file should be untouched: %v", err)
	}
	if got := sessionManager(t, mm, "COM4").HandleCommand("on"); got != "on 100% 4950K" {
		t.Fatalf("on = %q, want defaults %q", got, "on 100% 4950K")
	}
}

func TestStillPresentObservesFalseMiss(t *testing.T) {
	fastTimings(t)
	fleet := newFakeFleet("COM4")
	mm, ctx := newTestMulti(t, fleet, "")
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4")

	// First successful miss: debounce - stillPresent must be true
	fleet.set()
	mm.rescan(ctx)
	if !mm.stillPresent("COM4") {
		t.Fatal("stillPresent = false after one miss; want debounce true")
	}

	// Second consecutive miss: teardown - stillPresent must be false
	mm.rescan(ctx)
	if mm.stillPresent("COM4") {
		t.Fatal("stillPresent = true after two consecutive misses; want false to signal torn-down session")
	}

	// Verify session is actually torn down
	mm.mu.Lock()
	_, still := mm.sessions["COM4"]
	mm.mu.Unlock()
	if still {
		t.Fatal("session still tracked after two misses")
	}

	// Port reappears: stillPresent returns true and session restarts
	fleet.set("COM4")
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4")
	if !mm.stillPresent("COM4") {
		t.Fatal("stillPresent = false after port reappears and counter resets; want true")
	}
}
