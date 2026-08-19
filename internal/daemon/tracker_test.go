package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"mutastic/internal/proto"
)

func TestTrackerStartsUnknown(t *testing.T) {
	var tr Tracker
	if _, known := tr.Status(); known {
		t.Fatal("new Tracker should report known=false")
	}
}

func TestTrackerAppliesDeviceMuteBinary(t *testing.T) {
	var tr Tracker
	if !tr.Apply(proto.Event{Op: proto.EvtDeviceMute, Value: 0x01}) {
		t.Fatal("Apply should return true for a device mute event")
	}
	muted, known := tr.Status()
	if !known || !muted {
		t.Fatalf("Status() = %v, %v; want true, true", muted, known)
	}
}

func TestTrackerIgnoresSoftwareMuteEvenWithDecodableValue(t *testing.T) {
	var tr Tracker
	if tr.Apply(proto.Event{Op: proto.EvtSoftwareMute, Value: '1'}) {
		t.Fatal("Apply should return false for a 0x20 SoftwareMute echo, even with a decodable-looking value")
	}
	if _, known := tr.Status(); known {
		t.Fatal("0x20 SoftwareMute echo must never set known state")
	}
}

func TestTrackerIgnoresSoftwareMuteGarbageValue(t *testing.T) {
	var tr Tracker
	tr.Set(true) // simulate optimistic state set after an outbound mute command
	if tr.Apply(proto.Event{Op: proto.EvtSoftwareMute, Value: 0x0b}) {
		t.Fatal("Apply should return false for the production garbage tag byte 0x0b")
	}
	muted, known := tr.Status()
	if !known || !muted {
		t.Fatalf("Status() after 0x20 garbage echo = %v, %v; want true, true (must not reset optimistic state)", muted, known)
	}
}

func TestTrackerIgnoresNonMuteEvents(t *testing.T) {
	var tr Tracker
	if tr.Apply(proto.Event{Op: 0x23, Value: 0x32}) { // SoftwareVolume event
		t.Fatal("Apply should return false for non-mute events")
	}
	if _, known := tr.Status(); known {
		t.Fatal("non-mute events must not set known")
	}
}

func TestTrackerIgnoresUndecodableMuteValue(t *testing.T) {
	var tr Tracker
	if tr.Apply(proto.Event{Op: proto.EvtDeviceMute, Value: 0x42}) {
		t.Fatal("Apply should return false for an undecodable value byte")
	}
}

func TestTrackerSet(t *testing.T) {
	var tr Tracker
	tr.Set(true)
	muted, known := tr.Status()
	if !known || !muted {
		t.Fatalf("Status() after Set(true) = %v, %v; want true, true", muted, known)
	}
}

// TestTrackerPersistRoundTrip pins the boot-hydration contract: a tracker
// whose state was persisted hydrates a fresh tracker on the same path, so
// a daemon restart boots into the last known mute state instead of
// "unknown". Without persistence the Yeti's firmware offers no state
// query; without hydration the UI would show "Checking the mic…" until the
// first press of every session.
func TestTrackerPersistRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mic-state.json")
	var a Tracker
	a.SetPersistPath(path, nil)
	a.Set(true)
	var b Tracker
	b.SetPersistPath(path, nil)
	muted, known := b.Status()
	if !known || !muted {
		t.Fatalf("hydrated Status() = %v, %v; want true, true", muted, known)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("persist file missing after Set: %v", err)
	}
	if got := string(raw); got != `{"muted":true}` {
		t.Fatalf("persist file = %q, want %q", got, `{"muted":true}`)
	}
}

// TestTrackerPersistMissingFile pins that a boot with no prior state file
// leaves the tracker unknown (first-ever run) and does not create the
// file until a transition happens.
func TestTrackerPersistMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mic-state.json")
	var tr Tracker
	tr.SetPersistPath(path, nil)
	if _, known := tr.Status(); known {
		t.Fatal("missing persist file must leave the tracker unknown")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("persist file must not be created before the first transition, stat err = %v", err)
	}
}

// TestTrackerPersistCorruptFile pins that an undecodable state file leaves
// the tracker unknown rather than guessing, and is overwritten by the next
// real transition.
func TestTrackerPersistCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mic-state.json")
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	var tr Tracker
	tr.SetPersistPath(path, nil)
	if _, known := tr.Status(); known {
		t.Fatal("corrupt persist file must leave the tracker unknown")
	}
	tr.Set(false)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); got != `{"muted":false}` {
		t.Fatalf("persist file after Set = %q, want %q", got, `{"muted":false}`)
	}
}

// TestTrackerPersistOnPhysicalPress pins that physical-button transitions
// (Tracker.Apply) persist too — otherwise the file would only ever reflect
// software verbs and a press made while the daemon was up would be
// forgotten at the next restart.
func TestTrackerPersistOnPhysicalPress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mic-state.json")
	var tr Tracker
	tr.SetPersistPath(path, nil)
	if !tr.Apply(proto.Event{Op: proto.EvtDeviceMute, Value: 0x01}) {
		t.Fatal("Apply should accept a device mute event")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("persist file missing after physical press: %v", err)
	}
	if got := string(raw); got != `{"muted":true}` {
		t.Fatalf("persist file after physical press = %q, want %q", got, `{"muted":true}`)
	}
}

// TestTrackerReset pins the R8-F2 primitive: Reset drops a known state
// back to unknown, and the next Set/event re-establishes truth normally.
func TestTrackerReset(t *testing.T) {
	var tr Tracker
	tr.Set(true)
	tr.Reset()
	if _, known := tr.Status(); known {
		t.Fatal("Status() after Reset must report known=false")
	}
	if !tr.Apply(proto.Event{Op: proto.EvtDeviceMute, Value: 0x01}) {
		t.Fatal("Apply after Reset must track normally again")
	}
	if muted, known := tr.Status(); !known || !muted {
		t.Fatalf("Status() after Reset+Apply = %v, %v; want true, true", muted, known)
	}
}
