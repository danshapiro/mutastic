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

	gate injectGate // debounces physical mute-button injections; session goroutine only

	// lastStatusReply is the reply of the last LOGGED "status" command,
	// used by logCommand to suppress repeated identical status lines.
	// Touched only by the single serveUDP goroutine, so no lock is needed.
	lastStatusReply string

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
// "error: <reason>". Commands starting with "light" are delegated to
// d.Light, whose replies are the light's status strings ("on 64% 4950K",
// "off", "unknown") or "error: <reason>".
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
		muted, known := d.Track.Status()
		target := true // unknown state: default to mute (safe for a pedal press)
		if known {
			target = !muted
		}
		return d.setMute(target)
	default:
		return "error: unknown command"
	}
}

func (d *Daemon) setMute(muted bool) string {
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
// (127.0.0.1:42814) doubles as a single-instance lock.
func Run(ctx context.Context, open OpenFunc, light CommandHandler, inject KeyInjector, pc net.PacketConn, logger *log.Logger) error {
	d := New(logger)
	d.Light = light
	d.Inject = inject
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
		if d.Track.Apply(ev) {
			muted, _ := d.Track.Status()
			d.Logger.Printf("event op=0x%02x value=0x%02x -> muted=%v", ev.Op, ev.Value, muted)
		} else {
			d.Logger.Printf("event op=0x%02x value=0x%02x (ignored)", ev.Op, ev.Value)
		}
		// Physical mute-button press: fire the AHK meeting-app sweep.
		// Gated on the op (0x21) alone, NOT on Apply's result — see
		// injectGate's doc comment. Suppressed presses are logged so the
		// live double-press test can observe the debounce working.
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
	return ctx.Err()
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
	buf := make([]byte, 64)
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
		cmd := strings.TrimSpace(string(buf[:n]))
		reply := d.HandleCommand(cmd)
		d.logCommand(cmd, reply)
		if _, err := pc.WriteTo([]byte(reply), addr); err != nil {
			d.Logger.Printf("udp write to %s: %v (continuing)", addr, err)
		}
	}
}

// logCommand logs one served UDP command. Non-status commands always log.
// A "status" command logs only when its reply differs from the previously
// logged status reply: a resident poller (e.g. the OpenDeck plugin asking
// every ~750ms) would otherwise grow the log unbounded, because rotation
// runs only at daemon start. Called only from the single serveUDP
// goroutine, so lastStatusReply needs no lock.
func (d *Daemon) logCommand(cmd, reply string) {
	if cmd == "status" {
		if reply == d.lastStatusReply {
			return
		}
		d.lastStatusReply = reply
	}
	d.Logger.Printf("command %q -> %q", cmd, reply)
}

func sleepCtx(ctx context.Context, dur time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(dur):
	}
}
