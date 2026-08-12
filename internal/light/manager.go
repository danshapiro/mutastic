package light

import (
	"context"
	"errors"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Port is the minimal serial handle the manager needs. Like the mic's
// daemon.Device: Read returns (0, nil) when the poll timeout elapses with
// no data (the Windows adapter fixes the 1 s read timeout once, at open -
// re-issuing it per read could race an in-flight Write); any non-nil
// error means "device gone" and triggers a reconnect.
type Port interface {
	Write(p []byte) (int, error)
	Read(p []byte) (int, error)
	Close() error
}

// writeSpacing is the minimum delay before each frame write; the PL81 is
// rate-sensitive (protocol doc: ~60ms). Var so tests can shrink it.
var writeSpacing = 60 * time.Millisecond

var errNoLight = errors.New("no light")

// Manager owns the PL81: the current port handle, the tracked/persisted
// state, and rate-limited frame writes. Commands arrive via HandleCommand
// (from the UDP goroutine); inbound frames arrive via the session loop.
type Manager struct {
	Logger *log.Logger

	// Present optionally reports whether the device is still attached
	// (USB enumeration; nil disables the check). The session loop consults
	// it during long read silences: the CH340 driver's surprise-removal
	// error behavior is unverified, so presence is checked rather than
	// assumed (Task 5).
	Present func() bool

	state *State

	mu        sync.Mutex
	port      Port
	lastWrite time.Time
}

// NewManager returns a Manager whose restore targets are seeded from
// statePath (which may be "" to disable persistence).
func NewManager(logger *log.Logger, statePath string) *Manager {
	return &Manager{Logger: logger, state: NewState(statePath)}
}

// setPort installs the live port handle (nil while disconnected).
func (m *Manager) setPort(port Port) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.port = port
}

// writeFrame sends one frame, honoring the minimum write spacing. Writes
// are serialized by the mutex (the UDP goroutine is the only caller).
func (m *Manager) writeFrame(f []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.port == nil {
		return errNoLight
	}
	if wait := writeSpacing - time.Since(m.lastWrite); wait > 0 {
		time.Sleep(wait)
	}
	_, err := m.port.Write(f)
	m.lastWrite = time.Now()
	return err
}

// HandleCommand executes one "light ..." UDP command (prefix already
// stripped and trimmed, e.g. "brightness 40"). Success replies are the
// resulting status string; failures use the "error: " prefix.
func (m *Manager) HandleCommand(cmd string) string {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return "error: unknown light command"
	}
	switch fields[0] {
	case "status":
		if len(fields) != 1 {
			return "error: unknown light command"
		}
		// Status is deliberately a memory-only snapshot. State.StatusString
		// takes state.mu but never m.mu and never calls Port, so resident
		// pollers remain responsive even while a serial write is wedged.
		return m.state.StatusString()
	case "on":
		if len(fields) != 1 {
			return "error: unknown light command"
		}
		b, temp := m.state.TargetOn()
		return m.apply(b, temp)
	case "off":
		if len(fields) != 1 {
			return "error: unknown light command"
		}
		_, _, temp, _ := m.state.Status()
		return m.apply(0, temp)
	case "toggle":
		if len(fields) != 1 {
			return "error: unknown light command"
		}
		on, _, temp, known := m.state.Status()
		if known && on {
			return m.apply(0, temp)
		}
		// Unknown state counts as off: turn ON at the persisted look.
		b, tt := m.state.TargetOn()
		return m.apply(b, tt)
	case "brightness":
		if len(fields) != 2 {
			return "error: brightness must be 0-100"
		}
		n, err := strconv.Atoi(fields[1])
		if err != nil || n < 0 || n > 100 {
			return "error: brightness must be 0-100"
		}
		_, _, temp, _ := m.state.Status()
		return m.apply(n, temp)
	case "temp":
		if len(fields) != 2 {
			return "error: temp must be 2900-7000"
		}
		k, err := strconv.Atoi(fields[1])
		if err != nil || k < MinKelvin || k > MaxKelvin {
			return "error: temp must be 2900-7000"
		}
		temp := KelvinToByte(k)
		// While on: keep the brightness. While off/unknown: set temp AND
		// turn on at the restore brightness - a bare temp change is a
		// "make it look like this" request.
		on, b, _, known := m.state.Status()
		if known && on {
			return m.apply(b, temp)
		}
		lb, _ := m.state.TargetOn()
		return m.apply(lb, temp)
	case "preset":
		if len(fields) != 2 {
			return "error: unknown preset"
		}
		p, ok := Presets[fields[1]]
		if !ok {
			return "error: unknown preset"
		}
		return m.apply(p.Brightness, KelvinToByte(p.Kelvin))
	default:
		return "error: unknown light command"
	}
}

// apply sends one CCT frame and optimistically records the result; the
// device's byte-for-byte echo then re-confirms it via the session loop.
func (m *Manager) apply(brightness int, temp byte) string {
	if err := m.writeFrame(CCT(byte(brightness), temp)); err != nil {
		return "error: " + err.Error()
	}
	if err := m.state.Set(brightness, temp); err != nil {
		m.Logger.Printf("light: persist state: %v", err)
	}
	return m.state.StatusString()
}

// OpenFunc opens the PL81's serial port (enumerated by VID/PID).
type OpenFunc func() (Port, error)

// Timing knobs, vars so tests can shrink them.
var (
	wakeDelay      = 100 * time.Millisecond // settle time after the wake bytes
	openRetryDelay = 3 * time.Second        // "not present yet" backoff
	reconnectDelay = 2 * time.Second        // "was here, went away" backoff
	// presenceInterval is how long the session tolerates continuous read
	// silence before consulting Manager.Present (nil = never). A silent
	// idle light is normal (no status stream), but the CH340 driver's
	// surprise-removal error behavior is unverified - so during long
	// silences the device's continued USB presence is checked directly.
	presenceInterval = 10 * time.Second
)

// wakeBytes is the raw wake sequence (not a frame - no header/checksum).
var wakeBytes = []byte{0x00, 0x00, 0x00, 0x00}

// Run maintains the light session until ctx is cancelled: open, wake, read
// continuously, reconnect on any error. Mirrors the mic's reconnect loop in
// internal/daemon.
func (m *Manager) Run(ctx context.Context, open OpenFunc) {
	for ctx.Err() == nil {
		port, err := open()
		if err != nil {
			m.Logger.Printf("light: open: %v (retrying in %v)", err, openRetryDelay)
			sleepCtx(ctx, openRetryDelay)
			continue
		}
		m.Logger.Printf("light: port opened")
		err = m.session(ctx, port)
		// Close only after session has returned: reader and closer are the
		// same goroutine, so Close never races a pending Read (go.bug.st
		// issue #219).
		port.Close()
		if ctx.Err() != nil {
			break
		}
		m.Logger.Printf("light: session ended: %v (reconnecting in %v)", err, reconnectDelay)
		sleepCtx(ctx, reconnectDelay)
	}
}

// session wakes the device then reads frames until the port errors
// (unplug) or ctx is cancelled. Echoes of our own commands and unprompted
// knob broadcasts look identical and both simply update the state.
//
// Unlike the mic session there is deliberately NO query-based liveness
// gate: the PL81 has no query command, so a healthy idle light is silent
// and cannot be distinguished from a dead one by silence alone. Instead,
// during long silences the loop re-checks that the device is still
// enumerated (Manager.Present) - the CH340 driver is not trusted to
// surface an unplug as a read error.
func (m *Manager) session(ctx context.Context, port Port) error {
	if _, err := port.Write(wakeBytes); err != nil {
		return err
	}
	time.Sleep(wakeDelay)
	m.setPort(port)
	defer m.setPort(nil)

	var parser Parser
	buf := make([]byte, 64)
	lastData := time.Now()
	for ctx.Err() == nil {
		n, err := port.Read(buf) // 1 s poll timeout, fixed at open
		if err != nil {
			return err
		}
		if n == 0 { // timeout, no data
			if m.Present != nil && time.Since(lastData) >= presenceInterval {
				if !m.Present() {
					return errors.New("device no longer present")
				}
				lastData = time.Now() // present; recheck one interval from now
			}
			continue
		}
		lastData = time.Now()
		for _, fr := range parser.Feed(buf[:n]) {
			if fr.Tag == TagCCT && len(fr.Payload) == 3 {
				// Upstream captures show the pwr byte can carry panel-off
				// state (0x00/0x02) with a non-zero brightness field; treat
				// anything but 0x01 as OFF so status never lies "on".
				b := int(fr.Payload[1])
				if fr.Payload[0] != 0x01 {
					b = 0
				}
				if err := m.state.Set(b, fr.Payload[2]); err != nil {
					m.Logger.Printf("light: persist state: %v", err)
				}
				m.Logger.Printf("light: frame pwr=0x%02x brightness=%d temp=0x%02x -> %s",
					fr.Payload[0], fr.Payload[1], fr.Payload[2], m.state.StatusString())
			} else {
				m.Logger.Printf("light: frame tag=0x%02x payload=% x (ignored)", fr.Tag, fr.Payload)
			}
		}
	}
	return ctx.Err()
}

func sleepCtx(ctx context.Context, dur time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(dur):
	}
}

// Connected reports whether a serial port is currently attached to this
// manager (a live session has completed its wake). Used by the fleet's
// list command.
func (m *Manager) Connected() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.port != nil
}

// PowerState reports the tracked power state. known stays false until the
// first echo/broadcast or optimistic write; the fleet toggle treats
// unknown as off.
func (m *Manager) PowerState() (on, known bool) {
	on, _, _, known = m.state.Status()
	return on, known
}
