package light

import (
	"os"
	"path/filepath"
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
