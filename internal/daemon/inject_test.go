package daemon

import (
	"testing"
	"time"

	"mutastic/internal/proto"
)

func TestGateFiresOnDeviceMute(t *testing.T) {
	var g injectGate
	if !g.shouldInject(proto.Event{Op: proto.EvtDeviceMute, Value: 0x01}, time.Unix(1000, 0)) {
		t.Fatal("first 0x21 event should fire")
	}
}

func TestGateIgnoresSoftwareMute(t *testing.T) {
	var g injectGate
	now := time.Unix(1000, 0)
	if g.shouldInject(proto.Event{Op: proto.EvtSoftwareMute, Value: '0'}, now) {
		t.Fatal("0x20 software echo must never fire (loop risk: F14 -> toggle -> 0x20 -> F24 -> ...)")
	}
	// A rejected 0x20 must not consume the debounce window either.
	if !g.shouldInject(proto.Event{Op: proto.EvtDeviceMute, Value: 0x00}, now) {
		t.Fatal("0x21 right after a 0x20 should still fire")
	}
}

func TestGateFiresOnUndecodableValue(t *testing.T) {
	// The 0x21 value byte is unverified on some firmware
	// (docs/yeti-x-hid-protocol.md:109-115); the gate must fire on the op
	// alone, independent of whether the value decodes.
	var g injectGate
	if !g.shouldInject(proto.Event{Op: proto.EvtDeviceMute, Value: 0x0b}, time.Unix(1000, 0)) {
		t.Fatal("0x21 with an undecodable value byte must still fire")
	}
}

func TestGateDebounce(t *testing.T) {
	var g injectGate
	base := time.Unix(1000, 0)
	press := proto.Event{Op: proto.EvtDeviceMute, Value: 0x01}

	if !g.shouldInject(press, base) {
		t.Fatal("first press should fire")
	}
	if g.shouldInject(press, base.Add(muteInjectDebounce-time.Millisecond)) {
		t.Fatal("chatter just inside the debounce window must be suppressed")
	}
	// Suppressed chatter must NOT extend the window: a press exactly at
	// base+window still fires.
	if !g.shouldInject(press, base.Add(muteInjectDebounce)) {
		t.Fatal("press at the window boundary should fire")
	}
	if g.shouldInject(press, base.Add(muteInjectDebounce+time.Millisecond)) {
		t.Fatal("the boundary press must start a fresh window")
	}
}
