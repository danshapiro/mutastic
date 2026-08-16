package daemon

import (
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
