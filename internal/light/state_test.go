package light

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestStateUnknownUntilFirstSet(t *testing.T) {
	s := NewState("")
	if got := s.StatusString(); got != "unknown" {
		t.Fatalf("fresh state = %q, want unknown", got)
	}
	if err := s.Set(64, 0x09); err != nil {
		t.Fatal(err)
	}
	if got := s.StatusString(); got != "on 64% 4950K" {
		t.Fatalf("after Set(64, 0x09) = %q, want %q", got, "on 64% 4950K")
	}
	if err := s.Set(0, 0x09); err != nil {
		t.Fatal(err)
	}
	if got := s.StatusString(); got != "off" {
		t.Fatalf("after Set(0, 0x09) = %q, want off", got)
	}
}

func TestStateTargetOnDefaults(t *testing.T) {
	s := NewState("")
	b, temp := s.TargetOn()
	if b != 100 || temp != 0x09 { // 100%, 5000K quantized to byte 0x09
		t.Fatalf("defaults = (%d, 0x%02x), want (100, 0x09)", b, temp)
	}
}

func TestStateRemembersLastNonZeroBrightness(t *testing.T) {
	s := NewState("")
	if err := s.Set(40, 0x0C); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(0, 0x0C); err != nil { // off must not clobber the restore target
		t.Fatal(err)
	}
	b, temp := s.TargetOn()
	if b != 40 || temp != 0x0C {
		t.Fatalf("restore target = (%d, 0x%02x), want (40, 0x0C)", b, temp)
	}
}

func TestStatePersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "light-state.json")
	s := NewState(path)
	if err := s.Set(64, 0x12); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(0, 0x12); err != nil { // turned off; the look must survive
		t.Fatal(err)
	}

	s2 := NewState(path) // simulated daemon restart
	if got := s2.StatusString(); got != "unknown" {
		t.Fatalf("restored state must stay unknown until a frame arrives, got %q", got)
	}
	b, temp := s2.TargetOn()
	if b != 64 || temp != 0x12 {
		t.Fatalf("restored target = (%d, 0x%02x), want (64, 0x12)", b, temp)
	}
}

// TestStateSnapshot pins the save-facing read (R5-F4): ONE locked read of
// the whole four-tuple - power, the brightness a save records (live when
// on, the restore target when off, never 0), the temp byte, and known-ness.
func TestStateSnapshot(t *testing.T) {
	s := NewState("")
	// Fresh: known=false (the only field the save path consults of an
	// unknown light - it is skipped); brightness still maps to the default
	// restore target.
	if on, b, temp, known := s.Snapshot(); known || on || b != defaultBrightness || temp != KelvinToByte(defaultKelvin) {
		t.Fatalf("fresh snapshot = (%v, %d, 0x%02x, %v), want (false, %d, 0x%02x, false)", on, b, temp, known, defaultBrightness, KelvinToByte(defaultKelvin))
	}
	if err := s.Set(47, 0); err != nil {
		t.Fatal(err)
	}
	if on, b, temp, known := s.Snapshot(); !known || !on || b != 47 || temp != 0 {
		t.Fatalf("on snapshot = (%v, %d, 0x%02x, %v), want (true, 47, 0x00, true)", on, b, temp, known)
	}
	if err := s.Set(0, 9); err != nil {
		t.Fatal(err)
	}
	// Off: the snapshot brightness is the RESTORE TARGET (47), not 0, and
	// the temp byte tracks the latest state.
	if on, b, temp, known := s.Snapshot(); !known || on || b != 47 || temp != 9 {
		t.Fatalf("off snapshot = (%v, %d, 0x%02x, %v), want (false, 47, 0x09, true)", on, b, temp, known)
	}
}

// TestStateSnapshotNeverHybridizes pins R5-F4's behavioral guarantee: the
// save-facing four-tuple is read under ONE lock, so a mutation landing
// mid-snapshot can never mix two states (the old separate
// Status()/TargetOn() reads could yield, say, the off flag of the old look
// with the restore-target brightness of the new). A mutator cycles two
// ON/OFF looks; concurrent snapshots must ALWAYS observe one of the four
// states WHOLE. Under -race a regression to unlocked or split-locked reads
// data-races with Set and fails deterministically.
func TestStateSnapshotNeverHybridizes(t *testing.T) {
	s := NewState("")
	type tup struct {
		on   bool
		b    int
		temp byte
	}
	// The mutation cycle: an ON look, its OFF (snapshot brightness = its
	// restore target), a second ON look, its OFF (restore target updated).
	// The LEGAL snapshot set is exactly the four whole states.
	steps := []struct {
		b    int
		temp byte
	}{{30, 9}, {0, 3}, {80, 5}, {0, 7}}
	lastOn := defaultBrightness
	legal := map[tup]bool{}
	put := func(b int, temp byte) {
		if err := s.Set(b, temp); err != nil { // "" path: persistence is off, never errors
			t.Fatal(err)
		}
		if b > 0 {
			lastOn = b
		}
		legal[tup{b > 0, lastOn, temp}] = true
	}
	// Run the whole cycle once: the legal set now holds all four whole
	// states AND the state is KNOWN before the readers spawn.
	for _, st := range steps {
		put(st.b, st.temp)
	}

	const rounds = 2000
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < rounds; i++ {
			for _, st := range steps {
				s.Set(st.b, st.temp)
			}
		}
	}()

	type observation struct {
		on    bool
		b     int
		temp  byte
		known bool
	}
	hybrid := make(chan observation, 1)
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				on, b, temp, known := s.Snapshot()
				if !known || !legal[tup{on, b, temp}] {
					select {
					case hybrid <- observation{on, b, temp, known}:
					default: // one report is enough
					}
					return
				}
			}
		}()
	}
	wg.Wait()
	<-done
	select {
	case got := <-hybrid:
		t.Fatalf("Snapshot observed %+v, a tuple no single Set state ever produced (a hybrid torn across a mutation)", got)
	default:
	}
}

func TestStateSurvivesCorruptStateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "light-state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewState(path)
	b, temp := s.TargetOn()
	if b != 100 || temp != 0x09 {
		t.Fatalf("corrupt file must fall back to defaults, got (%d, 0x%02x)", b, temp)
	}
}
