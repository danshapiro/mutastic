package daemon

import (
	"time"

	"mutastic/internal/proto"
)

// KeyInjector delivers one synthetic keystroke to the OS. The daemon uses
// it to fire the AHK script's meeting-app sweep (F24) when the mic's own
// mute button is pressed. The Windows implementation (SendInput) lives in
// package main, keeping this package platform-free — same pattern as
// Daemon.Light/CommandHandler.
type KeyInjector interface {
	Inject() error
}

// muteInjectDebounce is how long after an acted-on 0x21 DeviceMute event
// further 0x21 events are ignored. Chatter has never been observed (all
// logged presses arrive one event apiece), so this is cheap insurance,
// not a measured fix: 400ms would outlast plausible chatter while staying
// shorter than the fastest observed intentional repeat press (~1s). Var
// (not const) so tests can shrink it — and a one-line change to disable
// if live use proves it unnecessary.
var muteInjectDebounce = 400 * time.Millisecond

// injectGate decides whether a decoded event should trigger a keystroke
// injection. Only physical-press events (0x21 DeviceMute) qualify: 0x20
// SoftwareMute echoes are host-initiated, and injecting on them would
// loop (F14 -> mutastic toggle -> 0x20 echo -> F24 -> sweep -> ...). The
// gate fires on the op alone, independent of whether the value byte
// decodes — 0x21 value semantics are unverified on some firmware
// (docs/yeti-x-hid-protocol.md).
//
// Not safe for concurrent use: only the daemon's session goroutine calls
// shouldInject.
type injectGate struct {
	last time.Time
}

// shouldInject reports whether ev, observed at now, should trigger an
// injection, recording now as the last firing time when it does.
// Suppressed events do NOT extend the debounce window, so a genuine
// second press still fires as soon as the window from the last ACTED-ON
// event lapses.
func (g *injectGate) shouldInject(ev proto.Event, now time.Time) bool {
	if ev.Op != proto.EvtDeviceMute {
		return false
	}
	if !g.last.IsZero() && now.Sub(g.last) < muteInjectDebounce {
		return false
	}
	g.last = now
	return true
}
