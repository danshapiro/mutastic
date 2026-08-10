package deckplugin

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
)

// The manifest's two state indices. The plugin alone drives these via
// setState: DisableAutomaticStates is true in the manifest AND
// disable_automatic_states is true in the profile instance, so OpenDeck
// never flips the icon on its own.
const (
	stateLive  = 0 // mute action state 0: live mic (icons/mutastic-mic)
	stateMuted = 1 // mute action state 1: muted (icons/mutastic-mic-muted)

	stateLightsOff = 0 // light action state 0: all lights off (icons/mutastic-light-off)
	stateLightsOn  = 1 // light action state 1: any light on (icons/mutastic-light-on)
)

// Action UUIDs served by this plugin (manifest Actions[].UUID). Every
// inbound willAppear/willDisappear/keyDown carries one in its "action"
// field; all routing keys off it.
const (
	actionMute  = "com.danshapiro.mutastic.mute"
	actionLight = "com.danshapiro.mutastic.light"
)

// actionOrder is the deterministic per-tick polling order (map
// iteration order would make tests and logs flap).
var actionOrder = []string{actionMute, actionLight}

// PollInterval is how often the plugin polls the daemon's status while
// at least one instance is visible. ~750ms keeps the icon honest within
// a blink of a physical mic-button press. A var so tests can shrink it
// (restore via t.Cleanup, registered before the loop starts u2014 same
// discipline as daemon_test.go's timing knobs).
var PollInterval = 750 * time.Millisecond

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

// actionSpec is one action's behavior table: the daemon commands it
// speaks, how its replies map to state indices, and whether a key
// press also injects F24 (the mute action's meeting-app sweep; never
// for lights).
type actionSpec struct {
	statusCmd     string
	toggleCmd     string
	replyToState  func(string) (int, bool)
	injectOnPress bool
}

// actionState is one action's worth of runtime state — exactly what
// used to be the whole Plugin state back when mute was the only action.
type actionState struct {
	spec      actionSpec
	visible   map[string]bool // context -> instance currently on a visible key
	lastKnown int             // last state observed/pushed; -1 = never known
	pollDown  bool            // daemon was unreachable at the last poll (log transitions, not every 750ms)
	noState   bool            // last poll reply carried no usable state (log transitions, not every 750ms)
}

// Plugin is one running plugin session. All methods are called from a
// single goroutine (Run's select loop feeds HandleMessage and PollOnce),
// so there is no internal locking by design.
type Plugin struct {
	conn   Conn
	daemon DaemonClient
	inject Injector // may be nil (non-Windows): mute keyDown skips the F24 sweep
	logger *log.Logger

	actions map[string]*actionState // action UUID -> that action's state
}

// New builds a Plugin serving both actions. inject may be nil (no key
// injection on this platform); logger must not be nil (tests pass
// log.New(io.Discard,"",0)).
func New(conn Conn, daemonClient DaemonClient, inject Injector, logger *log.Logger) *Plugin {
	return &Plugin{
		conn:   conn,
		daemon: daemonClient,
		inject: inject,
		logger: logger,
		actions: map[string]*actionState{
			actionMute: {
				spec:      actionSpec{statusCmd: "status", toggleCmd: "toggle", replyToState: desiredState, injectOnPress: true},
				visible:   make(map[string]bool),
				lastKnown: -1,
			},
			actionLight: {
				spec:      actionSpec{statusCmd: "light status", toggleCmd: "light toggle", replyToState: lightAnyOn, injectOnPress: false},
				visible:   make(map[string]bool),
				lastKnown: -1,
			},
		},
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

// lightAnyOn maps a "light status" (or "light toggle") fan-out reply to
// the light action's state index. Per-line grammar (multi.go handleAll):
// "<COMx>[ <name>]: <status>" where status is "on <N>% <K>K", "off",
// "unknown", or "error: <reason>"; labels never contain ':' so the
// first ": " split is unambiguous even for "COM7: error: timeout".
// Rules: ANY light on -> stateLightsOn; else any light known-off — or
// the zero-lights reply "error: no light" (nothing attached, nothing
// is on) -> stateLightsOff; else (all unknown/errors/unparseable) ->
// ok=false: no usable state, the caller holds the current icon. This
// mirrors the fleet toggle's own predicate, which counts unknown and
// timed-out lights as off.
func lightAnyOn(reply string) (state int, ok bool) {
	if reply == "error: no light" {
		return stateLightsOff, true
	}
	if strings.HasPrefix(reply, "error:") {
		return 0, false
	}
	sawOff := false
	for _, line := range strings.Split(reply, "\n") {
		_, status, found := strings.Cut(line, ": ")
		if !found {
			continue
		}
		if status == "on" || strings.HasPrefix(status, "on ") {
			return stateLightsOn, true
		}
		if status == "off" {
			sawOff = true
		}
	}
	if sawOff {
		return stateLightsOff, true
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
	case "willAppear", "willDisappear", "keyDown":
	default:
		return // titleParametersDidChange etc.: ignored by design
	}
	st, ok := p.actions[ev.Action]
	if !ok {
		p.logger.Printf("%s %s: unknown action %q, ignoring", ev.Event, ev.Context, ev.Action)
		return
	}
	switch ev.Event {
	case "willAppear":
		st.visible[ev.Context] = true
		p.logger.Printf("willAppear %s %s (visible: %d)", ev.Action, ev.Context, len(st.visible))
		// Correct this key's icon immediately instead of waiting a tick.
		if reply, err := p.daemon.Command(st.spec.statusCmd); err != nil {
			p.logger.Printf("willAppear %s: %s failed: %v", ev.Context, st.spec.statusCmd, err)
		} else if s, ok := st.spec.replyToState(reply); ok && s != st.lastKnown {
			// The probe observed a state change: every visible instance
			// of THIS action is stale, not just this one. Recording
			// lastKnown without pushing to all would make the next poll
			// see "no change" and leave the older keys wrong. pushAll
			// covers ev.Context too (just added).
			st.lastKnown = s
			p.pushAll(st)
			return
		}
		// Unchanged, or unknown/unreachable with a prior known state:
		// correct only the appearing key.
		if st.lastKnown >= 0 {
			p.sendSetState(ev.Context, st.lastKnown)
		}
	case "willDisappear":
		delete(st.visible, ev.Context)
		p.logger.Printf("willDisappear %s %s (visible: %d)", ev.Action, ev.Context, len(st.visible))
	case "keyDown":
		p.handleKeyDown(ev, st)
	}
}

// PollOnce polls the daemon once per action that has visible instances
// and, when an action's state is known and has CHANGED, pushes setState
// to that action's visible instances. Pushing only on change matters:
// OpenDeck persists the profile to disk on every setState. Unknown or
// unreachable leaves the icons untouched. Both actions share the one
// 750ms tick — a visible light key costs one extra UDP round trip per
// tick, never a second timer.
func (p *Plugin) PollOnce() {
	for _, action := range actionOrder {
		st := p.actions[action]
		if len(st.visible) == 0 {
			continue
		}
		reply, err := p.daemon.Command(st.spec.statusCmd)
		if err != nil {
			if !st.pollDown {
				st.pollDown = true
				p.logger.Printf("%s poll: daemon unreachable, keeping icon: %v", st.spec.statusCmd, err)
			}
			continue
		}
		if st.pollDown {
			st.pollDown = false
			p.logger.Printf("%s poll: daemon reachable again", st.spec.statusCmd)
		}
		s, ok := st.spec.replyToState(reply)
		if !ok {
			// Unknown / error reply: hold the current icon, and log the
			// TRANSITION into this condition (all-unknown lights after a
			// daemon restart would otherwise spam a line every 750ms).
			if !st.noState {
				st.noState = true
				p.logger.Printf("%s poll: no usable state in %q, keeping icon", st.spec.statusCmd, reply)
			}
			continue
		}
		st.noState = false
		if s == st.lastKnown {
			continue
		}
		st.lastKnown = s
		p.pushAll(st)
	}
}

// pushAll sends the action's last-known state to its visible instances.
func (p *Plugin) pushAll(st *actionState) {
	for ctx := range st.visible {
		p.sendSetState(ctx, st.lastKnown)
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

// handleKeyDown routes a key press to its action: send the action's
// toggle command, update the icon from the reply (the toggle reply IS
// the new state — don't wait a tick), and — for the mute action only —
// inject exactly one F24 for the meeting-app sweep. Each half runs even
// if the other fails. LOOP HAZARD: never inject F24 in reaction to a
// state change or the daemon's own injection — F24 must only ever mean
// "sweep the meeting apps once for this key press". The light action
// never injects: lights have nothing to do with meetings.
func (p *Plugin) handleKeyDown(ev Event, st *actionState) {
	reply, err := p.daemon.Command(st.spec.toggleCmd)
	if err != nil {
		p.logger.Printf("keyDown %s: %s failed: %v", ev.Context, st.spec.toggleCmd, err)
	} else {
		p.logger.Printf("keyDown %s: %s -> %q", ev.Context, st.spec.toggleCmd, reply)
		if s, ok := st.spec.replyToState(reply); ok && s != st.lastKnown {
			st.lastKnown = s
			p.pushAll(st)
		}
	}
	if !st.spec.injectOnPress {
		return
	}
	if p.inject == nil {
		p.logger.Printf("keyDown %s: no key injector on this platform, skipping F24 sweep", ev.Context)
	} else if err := p.inject.Inject(); err != nil {
		p.logger.Printf("keyDown %s: F24 inject failed: %v", ev.Context, err)
	} else {
		p.logger.Printf("keyDown %s: injected F24 app sweep", ev.Context)
	}
}

// Run registers with OpenDeck and processes events until the WebSocket
// closes or ctx is cancelled. A read error is the NORMAL end of life
// (OpenDeck kills or closes plugins when it exits and never restarts
// them), so it returns nil. One reader goroutine feeds the select loop;
// HandleMessage and PollOnce only ever run on this goroutine, which is
// what makes the lock-free Plugin state safe.
func (p *Plugin) Run(ctx context.Context, registerEvent, pluginUUID string) error {
	if err := p.conn.WriteMessage(EncodeRegister(registerEvent, pluginUUID)); err != nil {
		return fmt.Errorf("register: %w", err)
	}
	p.logger.Printf("registered as %s (event %s)", pluginUUID, registerEvent)

	frames := make(chan []byte)
	readErrs := make(chan error, 1)
	go func() {
		for {
			data, err := p.conn.ReadMessage()
			if err != nil {
				readErrs <- err
				return
			}
			select {
			case frames <- data:
			case <-ctx.Done():
				return
			}
		}
	}()

	tick := time.NewTicker(PollInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-readErrs:
			p.logger.Printf("websocket closed: %v", err)
			return nil
		case data := <-frames:
			p.HandleMessage(data)
		case <-tick.C:
			p.PollOnce()
		}
	}
}
