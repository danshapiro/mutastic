package deckplugin

import (
	"log"
)

// The manifest's two state indices. The plugin alone drives these via
// setState: DisableAutomaticStates is true in the manifest AND
// disable_automatic_states is true in the profile instance, so OpenDeck
// never flips the icon on its own.
const (
	stateLive  = 0 // state 0: live mic (icons/mutastic-mic)
	stateMuted = 1 // state 1: muted (icons/mutastic-mic-muted)
)

// Conn is the minimal WebSocket surface the plugin needs. Implemented by
// package main's gorilla/websocket adapter; tests use a channel-backed
// fake (mirrors daemon.Device / light.Port).
type Conn interface {
	ReadMessage() ([]byte, error) // blocks until one text frame arrives
	WriteMessage(data []byte) error
}

// DaemonClient sends one plain-text command to the mutastic daemon
// (UDP 127.0.0.1:42814) and returns the trimmed reply: "muted",
// "unmuted", "unknown", or "error: <reason>". A non-nil error means the
// daemon was unreachable (no reply arrived).
type DaemonClient interface {
	Command(cmd string) (string, error)
}

// Injector delivers one synthetic F24 keystroke. Structurally identical
// to daemon.KeyInjector so package main's newKeyInjector() satisfies it;
// redeclared here to keep this brick free of daemon imports.
type Injector interface {
	Inject() error
}

// Plugin is one running plugin session. All methods are called from a
// single goroutine (Run's select loop feeds HandleMessage and PollOnce),
// so there is no internal locking by design.
type Plugin struct {
	conn   Conn
	daemon DaemonClient
	inject Injector // may be nil (non-Windows): keyDown skips the F24 sweep
	logger *log.Logger

	visible   map[string]bool // context -> instance currently on a visible key
	lastKnown int             // last state observed/pushed; -1 = never known
	pollDown  bool            // daemon was unreachable at the last poll (log transitions, not every 750ms)
}

// New builds a Plugin. inject may be nil (no key injection on this
// platform); logger must not be nil (tests pass log.New(io.Discard,"",0)).
func New(conn Conn, daemonClient DaemonClient, inject Injector, logger *log.Logger) *Plugin {
	return &Plugin{
		conn:      conn,
		daemon:    daemonClient,
		inject:    inject,
		logger:    logger,
		visible:   make(map[string]bool),
		lastKnown: -1,
	}
}

// desiredState maps a daemon status/toggle reply to the OpenDeck state
// index. ok=false means the reply carries no usable state ("unknown" is
// normal after a daemon restart; "error: ..." likewise) and the caller
// must leave the current icon alone.
func desiredState(reply string) (state int, ok bool) {
	switch reply {
	case "muted":
		return stateMuted, true
	case "unmuted":
		return stateLive, true
	}
	return 0, false
}

// HandleMessage processes one inbound frame. Events the plugin doesn't
// handle are ignored by design: titleParametersDidChange follows every
// willAppear, and deviceDidConnect / systemDidWakeUp / keyUp arrive
// unrequested (keyUp can be suppressed by OpenDeck on profile switches,
// which is why the mute flow acts on keyDown alone).
func (p *Plugin) HandleMessage(data []byte) {
	ev, err := DecodeEvent(data)
	if err != nil {
		p.logger.Printf("ignoring undecodable frame: %v", err)
		return
	}
	switch ev.Event {
	case "willAppear":
		p.visible[ev.Context] = true
		p.logger.Printf("willAppear %s (visible: %d)", ev.Context, len(p.visible))
		// Correct this key's icon immediately instead of waiting a tick.
		if reply, err := p.daemon.Command("status"); err != nil {
			p.logger.Printf("willAppear %s: status failed: %v", ev.Context, err)
		} else if st, ok := desiredState(reply); ok && st != p.lastKnown {
			// The probe observed a state change: every visible instance is
			// stale, not just this one. Recording lastKnown without pushing
			// to all would make the next poll see "no change" and leave the
			// older keys wrong. pushAll covers ev.Context too (just added).
			p.lastKnown = st
			p.pushAll()
			return
		}
		// Unchanged, or unknown/unreachable with a prior known state:
		// correct only the appearing key.
		if p.lastKnown >= 0 {
			p.sendSetState(ev.Context, p.lastKnown)
		}
	case "willDisappear":
		delete(p.visible, ev.Context)
		p.logger.Printf("willDisappear %s (visible: %d)", ev.Context, len(p.visible))
	case "keyDown":
		p.handleKeyDown(ev)
	}
}

// PollOnce queries the daemon's status once and, when the mute state is
// known and has CHANGED, pushes setState to every visible instance.
// Pushing only on change matters: OpenDeck persists the profile to disk
// on every setState. Unknown or unreachable leaves the icons untouched.
func (p *Plugin) PollOnce() {
	if len(p.visible) == 0 {
		return
	}
	reply, err := p.daemon.Command("status")
	if err != nil {
		if !p.pollDown {
			p.pollDown = true
			p.logger.Printf("status poll: daemon unreachable, keeping icon: %v", err)
		}
		return
	}
	if p.pollDown {
		p.pollDown = false
		p.logger.Printf("status poll: daemon reachable again")
	}
	st, ok := desiredState(reply)
	if !ok {
		return // "unknown" or "error: ...": keep the current icon
	}
	if st == p.lastKnown {
		return
	}
	p.lastKnown = st
	p.pushAll()
}

// pushAll sends the last-known state to every visible instance.
func (p *Plugin) pushAll() {
	for ctx := range p.visible {
		p.sendSetState(ctx, p.lastKnown)
	}
}

// sendSetState writes one setState frame and logs it. The log line is
// load-bearing: the live E2E greps deckplugin.log for
// "setState <context> -> <state>".
func (p *Plugin) sendSetState(ctx string, state int) {
	if err := p.conn.WriteMessage(EncodeSetState(ctx, state)); err != nil {
		p.logger.Printf("setState %s -> %d: write failed: %v", ctx, state, err)
		return
	}
	p.logger.Printf("setState %s -> %d", ctx, state)
}

// handleKeyDown runs the full mute-everything flow in-process, mirroring
// deploy/mute-everything.cmd's two unconditional lines: daemon toggle
// FIRST, then exactly one F24 injection for the meeting-app sweep. Each
// half runs even if the other fails. LOOP HAZARD: never inject F24 in
// reaction to a state change or the daemon's own injection — F24 must
// only ever mean "sweep the meeting apps once for this key press".
func (p *Plugin) handleKeyDown(ev Event) {
	reply, err := p.daemon.Command("toggle")
	if err != nil {
		p.logger.Printf("keyDown %s: toggle failed: %v", ev.Context, err)
	} else {
		p.logger.Printf("keyDown %s: toggle -> %q", ev.Context, reply)
		// The toggle reply is the NEW state — update the icon now
		// instead of waiting for the next poll tick.
		if st, ok := desiredState(reply); ok && st != p.lastKnown {
			p.lastKnown = st
			p.pushAll()
		}
	}
	if p.inject == nil {
		p.logger.Printf("keyDown %s: no key injector on this platform, skipping F24 sweep", ev.Context)
	} else if err := p.inject.Inject(); err != nil {
		p.logger.Printf("keyDown %s: F24 inject failed: %v", ev.Context, err)
	} else {
		p.logger.Printf("keyDown %s: injected F24 app sweep", ev.Context)
	}
}
