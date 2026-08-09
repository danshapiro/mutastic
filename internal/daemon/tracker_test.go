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

func TestTrackerAppliesSoftwareUnmuteASCII(t *testing.T) {
	var tr Tracker
	tr.Apply(proto.Event{Op: proto.EvtSoftwareMute, Value: '1'})
	tr.Apply(proto.Event{Op: proto.EvtSoftwareMute, Value: '0'})
	muted, known := tr.Status()
	if !known || muted {
		t.Fatalf("Status() = %v, %v; want false, true", muted, known)
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
