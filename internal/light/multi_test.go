package light

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	runCtx, stopRun := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		mm.Run(runCtx)
	}()
	// Run - and the session goroutines it owns - read the package-level
	// timing vars (drainTimeout, presenceInterval, ...), so it must be
	// fully stopped BEFORE fastTimings/fastRescan restore them. t.Cleanup
	// is LIFO and this cleanup is registered last, so it runs first.
	t.Cleanup(func() {
		stopRun()
		<-done
	})
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

func TestMultiTargetedAddressing(t *testing.T) {
	fastTimings(t)
	fleet := newFakeFleet("COM4")
	mm, ctx := newTestMulti(t, fleet, t.TempDir())
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4")

	if got := mm.HandleCommand("name COM4 desk"); got != "named COM4 desk" {
		t.Fatalf("name reply = %q", got)
	}
	if got := mm.HandleCommand("@desk brightness 40"); got != "on 40% 4950K" {
		t.Fatalf("@desk brightness = %q", got)
	}
	for _, target := range []string{"@COM4 status", "@com4 status", "@DESK status"} {
		if got := mm.HandleCommand(target); got != "on 40% 4950K" {
			t.Fatalf("%q = %q, want on 40%% 4950K", target, got)
		}
	}
	got := mm.HandleCommand("@nope status")
	want := `error: unknown light "nope" (known: COM4=desk)`
	if got != want {
		t.Fatalf("@nope = %q, want %q", got, want)
	}
	got = mm.HandleCommand("@COM9 status")
	want = "error: light COM9 not connected (known: COM4=desk)"
	if got != want {
		t.Fatalf("@COM9 = %q, want %q", got, want)
	}
}

func TestMultiBareFansOutInPortOrder(t *testing.T) {
	fastTimings(t)
	fleet := newFakeFleet("COM7", "COM12", "COM4")
	mm, ctx := newTestMulti(t, fleet, "")
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4", "COM7", "COM12")
	got := mm.HandleCommand("brightness 50")
	want := "COM4: on 50% 4950K\nCOM7: on 50% 4950K\nCOM12: on 50% 4950K"
	if got != want {
		t.Fatalf("fan-out = %q, want %q", got, want)
	}
	for _, p := range []string{"COM4", "COM7", "COM12"} {
		if fleet.port(p).writeCount() < 2 { // wake + CCT frame
			t.Fatalf("%s got no frame after wake", p)
		}
	}
}

func TestMultiToggleAnyOnTurnsAllOff(t *testing.T) {
	fastTimings(t)
	fleet := newFakeFleet("COM4", "COM7")
	mm, ctx := newTestMulti(t, fleet, "")
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4", "COM7")
	// COM4 on; COM7 stays unknown (counts as off).
	if got := sessionManager(t, mm, "COM4").HandleCommand("brightness 60"); got != "on 60% 4950K" {
		t.Fatalf("setup: %q", got)
	}
	got := mm.HandleCommand("toggle")
	want := "COM4: off\nCOM7: off"
	if got != want {
		t.Fatalf("toggle = %q, want %q", got, want)
	}
}

func TestMultiToggleAllOffTurnsAllOnRestoringLooks(t *testing.T) {
	fastTimings(t)
	fleet := newFakeFleet("COM4", "COM7")
	mm, ctx := newTestMulti(t, fleet, "")
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4", "COM7")
	sessionManager(t, mm, "COM4").HandleCommand("brightness 30")
	sessionManager(t, mm, "COM4").HandleCommand("temp 2900")
	sessionManager(t, mm, "COM7").HandleCommand("brightness 80")
	mm.HandleCommand("off")
	got := mm.HandleCommand("toggle")
	want := "COM4: on 30% 2900K\nCOM7: on 80% 4950K"
	if got != want {
		t.Fatalf("toggle = %q, want %q", got, want)
	}
}

func TestMultiListAndNaming(t *testing.T) {
	fastTimings(t)
	dir := t.TempDir()
	fleet := newFakeFleet("COM4")
	mm, ctx := newTestMulti(t, fleet, dir)
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4")
	sessionManager(t, mm, "COM4").HandleCommand("brightness 30")

	mm.HandleCommand("name COM4 desk")
	mm.HandleCommand("name COM9 spare") // naming a not-yet-attached port is allowed
	got := mm.HandleCommand("list")
	want := "COM4 desk connected on 30% 4950K\nCOM9 spare disconnected"
	if got != want {
		t.Fatalf("list = %q, want %q", got, want)
	}

	// Regression 1: fan-out label carries the name via label()
	got = mm.HandleCommand("status")
	if !strings.HasPrefix(got, "COM4 desk: ") {
		t.Fatalf("fan-out status = %q, want to start with 'COM4 desk: '", got)
	}

	// Regression 2: known-list separator uses ", " (exact substring)
	got = mm.HandleCommand("@nope status")
	if !strings.Contains(got, "(known: COM4=desk, COM9=spare)") {
		t.Fatalf("@nope status = %q, want substring '(known: COM4=desk, COM9=spare)'", got)
	}

	// Regression 3: unname error propagation
	got = mm.HandleCommand("unname nope")
	if !strings.HasPrefix(got, "error: ") {
		t.Fatalf("unname nope = %q, want to start with 'error: '", got)
	}

	if got := mm.HandleCommand("unname spare"); got != "unnamed spare" {
		t.Fatalf("unname = %q", got)
	}
	if got := mm.HandleCommand("list"); got != "COM4 desk connected on 30% 4950K" {
		t.Fatalf("list after unname = %q", got)
	}

	// Names persist: a fresh Registry over the same file still resolves.
	reg2 := NewRegistry(filepath.Join(dir, "light-names.json"))
	if p, ok := reg2.Resolve("desk"); !ok || p != "COM4" {
		t.Fatalf("persisted Resolve(desk) = %q, %v; want COM4, true", p, ok)
	}
}

func TestMultiNoLights(t *testing.T) {
	fleet := newFakeFleet()
	mm, ctx := newTestMulti(t, fleet, "")
	mm.rescan(ctx)
	if got := mm.HandleCommand("toggle"); got != "error: no light" {
		t.Fatalf("toggle = %q, want error: no light", got)
	}
	if got := mm.HandleCommand("list"); got != "no lights known" {
		t.Fatalf("list = %q, want no lights known", got)
	}
	got := mm.HandleCommand("@desk status")
	want := `error: unknown light "desk" (known: none)`
	if got != want {
		t.Fatalf("@desk = %q, want %q", got, want)
	}
}

func TestMultiUsageErrors(t *testing.T) {
	fleet := newFakeFleet()
	mm, ctx := newTestMulti(t, fleet, "")
	mm.rescan(ctx)
	cases := map[string]string{
		"@desk":         "error: usage: light@<name|COMx> <command>",
		"@ toggle":      "error: usage: light@<name|COMx> <command>",
		"name COM4":     "error: usage: light name <COMx> <name>",
		"name COM4 a b": "error: usage: light name <COMx> <name>",
		"name COM4 no!": "error: invalid name: want 1-16 chars of a-z 0-9 '-', starting with a letter",
		"unname":        "error: usage: light unname <name|COMx>",
		"":              "error: unknown light command",
		"list extra":    "error: unknown light command",
	}
	for cmd, want := range cases {
		if got := mm.HandleCommand(cmd); got != want {
			t.Errorf("HandleCommand(%q) = %q, want %q", cmd, got, want)
		}
	}
}

// stuckPort completes the first Write (the wake, so the session connects)
// then blocks forever on every later Write and on Read - a light that
// wedges mid-session. Intentionally distinct from Task 3's wedgedPort
// (which blocks even the wake): here the session must CONNECT first so
// the fan-out reaches its Write. Closing block releases the leaked
// goroutines.
type stuckPort struct {
	mu     sync.Mutex
	writes int
	block  chan struct{}
}

func newStuckPort() *stuckPort { return &stuckPort{block: make(chan struct{})} }

func (s *stuckPort) Write(p []byte) (int, error) {
	s.mu.Lock()
	s.writes++
	first := s.writes == 1
	s.mu.Unlock()
	if first {
		return len(p), nil // wake succeeds; the session connects
	}
	<-s.block
	return 0, errors.New("gone")
}

func (s *stuckPort) Read(p []byte) (int, error) { <-s.block; return 0, errors.New("gone") }
func (s *stuckPort) Close() error               { return nil }

func TestMultiFanOutBoundedByWedgedLight(t *testing.T) {
	fastTimings(t) // shrinks lightCallTimeout to 50ms - the bound under test
	stuck := newStuckPort()
	defer close(stuck.block) // release the leaked goroutines at test end
	healthy := newFakePort()
	enumerate := func() ([]string, error) { return []string{"COM4", "COM7"}, nil }
	open := func(name string) (Port, error) {
		if name == "COM7" {
			return stuck, nil
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
	waitConnected(t, mm, "COM4", "COM7") // both wakes succeeded

	start := time.Now()
	got := mm.HandleCommand("brightness 40")
	want := "COM4: on 40% 4950K\nCOM7: error: timeout"
	if got != want {
		t.Fatalf("fan-out = %q, want %q", got, want)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("fan-out took %v; want bounded by lightCallTimeout", elapsed)
	}

	// list must be bounded too: the abandoned fan-out goroutine is parked
	// inside writeFrame HOLDING COM7's m.mu, so an unbounded Connected()
	// probe would block forever.
	start = time.Now()
	got = mm.HandleCommand("list")
	want = "COM4 - connected on 40% 4950K\nCOM7 - error: timeout"
	if got != want {
		t.Fatalf("list = %q, want %q", got, want)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("list took %v; want bounded by lightCallTimeout", elapsed)
	}
}
