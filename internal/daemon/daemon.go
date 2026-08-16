package daemon

import (
	"context"
	"errors"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"mutastic/internal/proto"
)

// Device is the minimal HID handle the daemon needs. Implementations must
// return (0, nil) from ReadWithTimeout when the timeout elapses with no
// data; any non-nil error is treated as "device gone" and triggers a
// reconnect.
type Device interface {
	Write(p []byte) (int, error)
	ReadWithTimeout(p []byte, timeout time.Duration) (int, error)
	Close() error
}

// CommandHandler answers one already-trimmed command string. It is how
// device managers other than the mic (today: the PL81 light) plug their
// verbs into the UDP surface without this package importing them.
type CommandHandler interface {
	HandleCommand(cmd string) string
}

var errNoDevice = errors.New("no device")

// Daemon holds shared daemon state: the tracked mute state, the current
// device handle, and the logger.
type Daemon struct {
	Track  Tracker
	Logger *log.Logger
	Light  CommandHandler // nil when no light support is wired in
	Inject KeyInjector    // nil when no key injection is wired in (non-Windows builds)
	// Shutdown requests daemon termination (production wires Run's ctx
	// cancel). serveUDP invokes it only AFTER the "shutting down" reply is
	// on the wire, so the Quit command always gets its ack. Nil: the
	// shutdown command replies an error instead.
	Shutdown func()

	// opMu serializes every mute-state compound operation across the two
	// goroutines that can move the mic or the tracked state (R7-F1): the
	// serveUDP goroutine's verbs (conditional: premise read -> HID write
	// -> tracker update -> F24 sweep; plain: read/write -> tracker set)
	// and the session goroutine's event handling (tracker Apply -> F24
	// sweep) each hold it for the FULL compound, so a physical 0x21 press
	// can never interleave between a matched conditional verb's premise
	// read and its tracker update - the pre-R7-F1 straddle, where the
	// event flipped the tracked state AND swept while the already-matched
	// verb, past its premise read, swept again: one real transition, two
	// sweeps (apps end toggled twice = desynced from the mic).
	opMu sync.Mutex

	gate injectGate // debounces physical mute-button injections; session goroutine only (additionally opMu-covered)

	// lastStatusReply is the reply of the last LOGGED "status" command,
	// used by logCommand to suppress repeated identical status lines.
	// Touched only by the single serveUDP goroutine, so no lock is needed.
	lastStatusReply            string
	lastLightStatusReply       string // like lastStatusReply, for the lights key's "light status" poller
	lastLightSettingsListReply string // like lastStatusReply, for the tray's "light settings list" poller

	mu  sync.Mutex
	dev Device
}

// New returns a Daemon that logs to logger.
func New(logger *log.Logger) *Daemon {
	return &Daemon{Logger: logger}
}

// SetDevice installs the current device handle (nil while disconnected).
func (d *Daemon) SetDevice(dev Device) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dev = dev
}

// WriteReport sends one output report. Writes are serialized by the mutex;
// per the protocol doc, the returned byte count is NOT asserted on (Windows
// hidapi reports 64 for a 65-byte buffer).
func (d *Daemon) WriteReport(report []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dev == nil {
		return errNoDevice
	}
	_, err := d.dev.Write(report)
	return err
}

// HandleCommand executes one UDP text command and returns the reply.
// Mic replies are exactly: "muted", "unmuted", "unknown", or
// "error: <reason>"; the atomic conditional mic verbs "mute-if
// <expected>" / "unmute-if <expected>" (expected ∈ muted|unmuted, R6-F2)
// reply "ok" (premise matched: the absolute verb AND one F24 meeting-app
// sweep ran in this same step), "flipped muted|unmuted" or "flipped
// unknown" (premise failed: NO verb, NO inject), or "error: <reason>".
// Commands starting with "light" are delegated to d.Light, whose replies
// are the light's status strings ("on 64% 4950K", "off", "unknown") or
// "error: <reason>".
func (d *Daemon) HandleCommand(cmd string) string {
	if rest, ok := strings.CutPrefix(cmd, "light"); ok && (rest == "" || rest[0] == ' ' || rest[0] == '@') {
		if d.Light == nil {
			return "error: no light support"
		}
		return d.Light.HandleCommand(strings.TrimSpace(rest))
	}
	switch cmd {
	case "status":
		muted, known := d.Track.Status()
		if !known {
			return "unknown"
		}
		if muted {
			return "muted"
		}
		return "unmuted"
	case "mute":
		return d.setMute(true)
	case "unmute":
		return d.setMute(false)
	case "toggle":
		return d.toggleMute()
	case "shutdown":
		// The reply is all that happens here; serveUDP fires d.Shutdown
		// itself after the reply has been written, or the cancel could
		// race the send (Run's ctx watcher closes pc on cancellation).
		if d.Shutdown == nil {
			return "error: shutdown not supported"
		}
		return "shutting down"
	default:
		if targetMuted, expectMuted, ok := proto.ParseConditionalMute(cmd); ok {
			return d.conditionalMute(targetMuted, expectMuted)
		}
		return "error: unknown command"
	}
}

// conditionalMute performs one ATOMIC conditional mic verb (R6-F2): the
// premise check AND the action happen inside this single serveUDP step,
// so a mic-state flip can no longer slip in between a separate status
// probe datagram and the verb datagram (the old tray probe->verb pair
// re-created the R3-F2 double-sweep window the probe gate was meant to
// close). R7-F1 extends that atomicity across goroutines: the full
// compound (premise read -> HID write -> tracker update -> sweep) runs
// under d.opMu, and the session goroutine's event compound (tracker Apply
// -> sweep) holds the same mutex, so a physical 0x21 press can no longer
// straddle the verb's stretch either (one real transition, one sweep,
// whichever side serializes first). If the tracker's current state does
// not equal the expected premise - INCLUDING unknown - nothing runs (no
// HID write, no inject) and the reply is "flipped <current>"; that IS the
// tray's precision-amendment refusal, daemon-side. On a match the
// absolute verb is written and one F24 meeting-app sweep is injected in
// the same step.
//
// The injection deliberately bypasses d.gate: that debounce exists only
// for physical 0x21-button chatter on the session goroutine, is not safe
// for concurrent use, and must never swallow a deliberate verb-driven
// sweep. The residual floor is a physical press the daemon has received
// at the HID level but not yet READ into the tracker when the check runs
// - the tracker's last processed event IS the daemon's authoritative
// best-known state, and every sweeping path keeps the tracker and the
// sweep in the same opMu compound; accepted per the ROUND-8 plan
// amendment. error:-prefixed replies keep the usual shape: a failed HID
// write runs NO sweep (the mic did not move, so sweeping the apps alone
// would desync them from the mic), while an injection failure after a
// successful write is still reported (the mic DID move - the honest
// reading of "mute-everything failed half-way").
func (d *Daemon) conditionalMute(targetMuted, expectMuted bool) string {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	muted, known := d.Track.Status()
	if !known {
		return "flipped unknown"
	}
	if muted != expectMuted {
		if muted {
			return "flipped muted"
		}
		return "flipped unmuted"
	}
	payload := []byte("0")
	if targetMuted {
		payload = []byte("1")
	}
	if err := d.WriteReport(proto.EncodeCommand(proto.OpMute, payload)); err != nil {
		return "error: " + err.Error()
	}
	// The mic moved; record the optimistic state exactly like setMute,
	// then run the meeting-app half. Both halves are reported as one
	// result: the reply is "ok" only when the full mute-everything flow
	// succeeded.
	d.Track.Set(targetMuted)
	if d.Inject == nil {
		return "error: key injection unavailable"
	}
	if err := d.Inject.Inject(); err != nil {
		return "error: " + err.Error()
	}
	return "ok"
}

// toggleMute flips the tracked state (unknown defaults to mute, safe for
// a pedal press). The direction pick and the verb are one opMu compound
// like every other verb (R7-F1): plain verbs never sweep, so a pre-R7-F1
// event interleave could flap only the tracked value - convergent, never
// a desync - but one uniform rule (every mute-state move serialized, all
// sweeping paths pairwise exclusive) is simpler to reason about than two.
func (d *Daemon) toggleMute() string {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	muted, known := d.Track.Status()
	target := true // unknown state: default to mute (safe for a pedal press)
	if known {
		target = !muted
	}
	return d.setMuteLocked(target)
}

func (d *Daemon) setMute(muted bool) string {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	return d.setMuteLocked(muted)
}

// resetTracker drops the tracked mute state to unknown under opMu
// (R8-F2). It exists for callers that do NOT already hold the mutex -
// Run's session binding point; handleEvent resets the tracker directly
// because it already holds opMu for its whole compound.
func (d *Daemon) resetTracker() {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	d.Track.Reset()
}

// setMuteLocked is setMute's body with opMu already held (R7-F1); the
// replies are byte-identical to the pre-opMu verbs (no behavior change,
// only the serialization).
func (d *Daemon) setMuteLocked(muted bool) string {
	payload := []byte("0")
	if muted {
		payload = []byte("1")
	}
	if err := d.WriteReport(proto.EncodeCommand(proto.OpMute, payload)); err != nil {
		return "error: " + err.Error()
	}
	// Optimistic: this firmware's 0x20 echo only confirms receipt of the
	// command (a zero-payload ack with a garbage value byte) and carries no
	// state -- see Tracker.Apply. Tracked state also follows real 0x21
	// DeviceMute events (physical button presses).
	d.Track.Set(muted)
	if muted {
		return "muted"
	}
	return "unmuted"
}

// OpenFunc opens the Yeti X HID control interface.
type OpenFunc func() (Device, error)

// Run serves UDP commands on pc and maintains the device session until ctx
// is cancelled. The caller owns pc; binding the production port
// (127.0.0.1:42814) doubles as a single-instance lock. shutdown (may be nil)
// is invoked when a client sends the "shutdown" command; production passes
// the cancellable ctx's cancel func so the command ends the daemon cleanly.
func Run(ctx context.Context, open OpenFunc, light CommandHandler, inject KeyInjector, shutdown func(), pc net.PacketConn, logger *log.Logger) error {
	d := New(logger)
	d.Light = light
	d.Inject = inject
	d.Shutdown = shutdown
	go func() {
		<-ctx.Done()
		pc.Close()
	}()
	go d.serveUDP(pc)

	for ctx.Err() == nil {
		dev, err := open()
		if err != nil {
			logger.Printf("open device: %v (retrying in 3s)", err)
			sleepCtx(ctx, 3*time.Second)
			continue
		}
		logger.Printf("device opened")
		d.SetDevice(dev)
		// R8-F2b: a fresh session cannot inherit the PREVIOUS session's
		// tracked mute state - the mic may have been toggled while the
		// daemon was disconnected, and there is no readable state query
		// (the handshake does not report the state either). Reset to
		// UNKNOWN the moment the new device binds, so conditional verbs
		// refuse with "flipped unknown" until a real event or a successful
		// verb re-establishes truth, instead of acting on the dead
		// session's belief. Serialized with every other tracker move under
		// opMu (R7-F1's uniform rule).
		d.resetTracker()
		err = d.session(ctx, dev)
		d.SetDevice(nil)
		dev.Close()
		if ctx.Err() != nil {
			break
		}
		logger.Printf("device session ended: %v (reconnecting in 2s)", err)
		sleepCtx(ctx, 2*time.Second)
	}
	return nil
}

// handshakeLiveness bounds how long a fresh session may stay silent after the
// handshake. The protocol doc calls the handshake "somewhat flaky": writes can
// succeed yet the event stream never comes up, which would leave the daemon
// deaf with no error to trigger a reconnect. The GetVolume request sent during
// the handshake provokes a reply, so a live session always produces at least
// one input report quickly. Var (not const) so tests can shorten it.
var handshakeLiveness = 5 * time.Second

var errHandshakeSilence = errors.New("no input report within the handshake liveness window (flaky handshake); reinitializing")

// session performs the init handshake then reads input reports until the
// device errors (unplug), the handshake proves dead (liveness gate), or ctx
// is cancelled.
func (d *Daemon) session(ctx context.Context, dev Device) error {
	if err := d.WriteReport(proto.EncodeCommand(proto.OpInit, nil)); err != nil {
		return err
	}
	if err := d.WriteReport(proto.EncodeCommand(proto.OpGetVolume, nil)); err != nil {
		return err
	}
	deadline := time.Now().Add(handshakeLiveness)
	live := false // becomes true on the first input report of this session
	buf := make([]byte, 128)
	for ctx.Err() == nil {
		n, err := dev.ReadWithTimeout(buf, time.Second)
		if err != nil {
			return err
		}
		if n == 0 {
			if !live && time.Now().After(deadline) {
				return errHandshakeSilence
			}
			continue // timeout, no data
		}
		live = true
		ev, ok := proto.DecodeEvent(buf[:n])
		if !ok {
			continue
		}
		d.handleEvent(ev)
	}
	return ctx.Err()
}

// handleEvent processes one decoded input report: the tracker Apply and -
// for a physical mute-button press (0x21) - the debounced F24 meeting-app
// sweep. The WHOLE stretch is one d.opMu compound (R7-F1) against the
// serveUDP goroutine's verb compounds, so a press can never land between
// a matched conditional verb's premise read and its tracker update (the
// pre-R7-F1 straddle: state flipped + swept while the verb, already past
// its read, swept again). The injection is gated on the op (0x21) alone,
// NOT on Apply's result — see injectGate's doc comment. Suppressed
// presses are logged so the live double-press test can observe the
// debounce working. Called only on the session goroutine.
func (d *Daemon) handleEvent(ev proto.Event) {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	if d.Track.Apply(ev) {
		muted, _ := d.Track.Status()
		d.Logger.Printf("event op=0x%02x value=0x%02x -> muted=%v", ev.Op, ev.Value, muted)
	} else if ev.Op == proto.EvtDeviceMute {
		// R8-F2a: a PHYSICAL press whose value byte does not decode means
		// the mic moved but we cannot read the direction, so every premise
		// built on the tracker's belief is unsafe. Drop the tracked state
		// to UNKNOWN - conditional verbs then refuse with "flipped unknown"
		// until truth re-arrives - while the legacy F24 sweep below STILL
		// runs exactly as before: that sweep predates the conditional verbs
		// (it exists so the physical button carries the meeting apps along
		// even on firmware whose value bytes this build cannot read), and
		// skipping it here would silently change the button's app behavior.
		d.Track.Reset()
		d.Logger.Printf("event op=0x%02x value=0x%02x: undecodable physical press - tracked state reset to unknown (premise safety); sweep still runs", ev.Op, ev.Value)
	} else {
		d.Logger.Printf("event op=0x%02x value=0x%02x (ignored)", ev.Op, ev.Value)
	}
	if d.Inject != nil && ev.Op == proto.EvtDeviceMute {
		if d.gate.shouldInject(ev, time.Now()) {
			if err := d.Inject.Inject(); err != nil {
				d.Logger.Printf("mic button -> F24 app sweep: inject failed: %v", err)
			} else {
				d.Logger.Printf("mic button -> F24 app sweep")
			}
		} else {
			d.Logger.Printf("mic button ignored (debounce)")
		}
	}
}

// serveUDP answers commands until pc is closed. Transient socket errors must
// NOT kill this loop: on Windows (the production platform), a reply that
// lands after a one-shot client already closed its socket elicits ICMP Port
// Unreachable, which poisons the socket so the next ReadFrom fails with
// WSAECONNRESET — Go's net package never disables SIO_UDP_CONNRESET
// (golang/go#5834). Because the bound port doubles as the single-instance
// lock, a dead loop here would leave a healthy-looking daemon that can never
// answer another command and blocks any replacement from starting. Only
// net.ErrClosed (the shutdown path: Run's goroutine closing pc) ends the
// loop; every other error is logged and survived.
func (d *Daemon) serveUDP(pc net.PacketConn) {
	// 128-byte receive buffer (R7-F3) plus the ROUND-9 full-buffer refusal
	// rule (R8-F1). The largest LEGAL command on the wire is exactly 64
	// bytes (the 22-byte "light settings delete " prefix + the store's
	// 42-byte name cap), so an oversize legal-shape command (a 65-byte
	// delete of a 43-byte name) arrives WHOLE and parsing rejects it with
	// the documented too-long error on every platform - under the old
	// 64-byte buffer it TRUNCATED on Unix into the 42-byte prefix name and
	// deleted it (R4-F3's class, one layer down: the wire itself). R8-F1
	// closes what raw headroom cannot: a read that FILLS the entire buffer
	// (n == 128) is definitionally a truncated datagram (Unix) or hostile
	// exact-fit filler - never a legal command - and the surviving HEAD of
	// a truncated command can parse as a DIFFERENT VALID command with the
	// padding swallowed by TrimSpace and the settings handler's raw-suffix
	// name read: the 100-spaces pad attack ("light settings delete " + 100
	// spaces + "target" + a >128-byte suffix) truncates at the buffer into
	// exactly "...delete <spaces> target", which parses as deleting the
	// EXISTING setting "target" - a hostile command manufactured from
	// nothing but whitespace. So a full-buffer read is answered
	// "error: command too long" and is NEVER dispatched (no handler runs,
	// no shutdown hook can fire on a padded "shutdown" either). Datagrams
	// beyond the buffer on Windows fail the read instead (WSAEMSGSIZE -
	// logged, survived; the client sees a timeout), which is identical in
	// effect: the command never dispatches.
	buf := make([]byte, 128)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return // listener closed on shutdown
			}
			d.Logger.Printf("udp read: %v (continuing)", err)
			time.Sleep(100 * time.Millisecond) // don't spin if the error repeats
			continue
		}
		if n == len(buf) {
			// Full buffer: truncated or hostile (see the header comment).
			// Refuse without dispatching - the reply still goes out so
			// legitimate clients see the same error: shape they parse.
			d.logCommand("<full datagram, refused>", "error: command too long")
			if _, err := pc.WriteTo([]byte("error: command too long"), addr); err != nil {
				d.Logger.Printf("udp write to %s: %v (continuing)", addr, err)
			}
			continue
		}
		cmd := strings.TrimSpace(string(buf[:n]))
		reply := d.HandleCommand(cmd)
		d.logCommand(cmd, reply)
		if _, err := pc.WriteTo([]byte(reply), addr); err != nil {
			d.Logger.Printf("udp write to %s: %v (continuing)", addr, err)
		}
		// Reply first, then shut down: cancelling earlier could close pc
		// before WriteTo runs. A failed WriteTo does not veto the shutdown
		// - the caller asked to stop the daemon; the ack is best-effort.
		if cmd == "shutdown" && d.Shutdown != nil {
			d.Shutdown()
		}
	}
}

// logCommand logs one served UDP command. Non-poll commands always log.
// The resident-poller commands ("status" from the mute key, "light
// status" from the lights key, each every ~750ms, and "light settings
// list" from the tray's saved-settings reconciliation every 2s) log only
// when their reply differs from the previously logged reply for that
// command: rotation runs only at daemon start, so unconditional logging
// would grow the log unbounded. Each latch is independent. Called only
// from the single serveUDP goroutine, so the latches need no lock.
func (d *Daemon) logCommand(cmd, reply string) {
	switch cmd {
	case "status":
		if reply == d.lastStatusReply {
			return
		}
		d.lastStatusReply = reply
	case "light status":
		if reply == d.lastLightStatusReply {
			return
		}
		d.lastLightStatusReply = reply
	case "light settings list":
		if reply == d.lastLightSettingsListReply {
			return
		}
		d.lastLightSettingsListReply = reply
	}
	d.Logger.Printf("command %q -> %q", cmd, reply)
}

func sleepCtx(ctx context.Context, dur time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(dur):
	}
}
