package light

import (
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
