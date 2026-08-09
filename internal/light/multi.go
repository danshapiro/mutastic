package light

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// rescanInterval is how often MultiManager re-enumerates PL81 ports to
// pick up plugged-in lights and tear down removed ones. Var so tests can
// shrink it.
var rescanInterval = 5 * time.Second

// missThreshold: a session is torn down only after its port is absent
// from this many CONSECUTIVE SUCCESSFUL scans. A successful enumeration
// can transiently omit a present device (the churn window is unverifiable
// here), so a single missing scan is never trusted; erroring scans keep
// all sessions and reset nothing.
const missThreshold = 2

// Bounded-wait knobs. Vars so fastTimings can shrink them. Nothing in the
// fleet may block indefinitely on one light's I/O: the CH340 driver's
// surprise-removal I/O promptness is unprovable, so it is never relied on.
var (
	// drainTimeout bounds how long teardown waits for a cancelled session
	// goroutine to exit. On expiry the goroutine (and its port handle) is
	// abandoned: a truly wedged write leaks one goroutine + handle -
	// degraded, never wedged; same-COM re-adoption self-heals via the
	// open-retry loop once the driver completes the I/O.
	drainTimeout = 2 * time.Second
	// lightCallTimeout bounds every per-light command/poll call (consumed
	// by the Task 4 command surface; declared here so fastTimings covers
	// both knobs in one edit).
	lightCallTimeout = 2 * time.Second
)

// Enumerate lists the COM port names of every PL81 currently attached.
type Enumerate func() ([]string, error)

// OpenPort opens one specific PL81 serial port by COM name.
type OpenPort func(name string) (Port, error)

// MultiManager owns one Manager per attached PL81 and answers the
// daemon's light commands: bare verbs fan out to every light, "@target"
// addresses one, plus name/unname/list bookkeeping (Task 4). A light's
// identity is its COM port - CH340 bridges have no USB serial number.
type MultiManager struct {
	logger    *log.Logger
	reg       *Registry
	stateDir  string // per-port state files live here; "" disables persistence
	enumerate Enumerate
	openPort  OpenPort

	mu       sync.Mutex
	sessions map[string]*lightSession // key: canonical port name ("COM4")
	misses   map[string]int           // consecutive successful scans missing each port
}

type lightSession struct {
	port   string
	m      *Manager
	cancel context.CancelFunc
	done   chan struct{}
}

// NewMultiManager wires the discovery/open callbacks; Run starts the
// rescan loop.
func NewMultiManager(logger *log.Logger, stateDir string, reg *Registry, enumerate Enumerate, openPort OpenPort) *MultiManager {
	return &MultiManager{
		logger:    logger,
		reg:       reg,
		stateDir:  stateDir,
		enumerate: enumerate,
		openPort:  openPort,
		sessions:  map[string]*lightSession{},
		misses:    map[string]int{},
	}
}

// Run rescans immediately, then every rescanInterval, until ctx is done.
// On exit all sessions are stopped.
func (mm *MultiManager) Run(ctx context.Context) {
	mm.rescan(ctx)
	t := time.NewTicker(rescanInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			mm.stopAll()
			return
		case <-t.C:
			mm.rescan(ctx)
		}
	}
}

// rescan diffs the enumerated port set against running sessions: new
// ports get a session (with one-time legacy state migration); vanished
// ports are torn down only after missThreshold CONSECUTIVE successful
// misses (debounce - a successful scan can transiently omit a present
// device). Enumeration errors keep the current set and reset nothing
// (fail open - never kill sessions on an enumerator glitch). Teardown
// waits run OUTSIDE mm.mu and are bounded by drainTimeout: one light's
// wedged I/O must never block the fleet lock.
func (mm *MultiManager) rescan(ctx context.Context) {
	raw, err := mm.enumerate()
	if err != nil {
		mm.logger.Printf("light: rescan: %v (keeping current sessions)", err)
		return
	}
	var ports []string
	for _, r := range raw {
		if p, err := NormalizePort(r); err == nil {
			ports = append(ports, p)
		}
	}
	var doomed []*lightSession
	mm.mu.Lock()
	seen := map[string]bool{}
	changed := false
	for _, port := range ports {
		seen[port] = true
		delete(mm.misses, port) // seen: reset the miss counter
		if _, ok := mm.sessions[port]; !ok {
			mm.startSessionLocked(ctx, port, len(ports))
			changed = true
		}
	}
	for port, s := range mm.sessions {
		if seen[port] {
			continue
		}
		mm.misses[port]++
		if mm.misses[port] < missThreshold {
			continue // debounce: one missing scan is not trusted
		}
		mm.logger.Printf("light %s: port gone, stopping session", port)
		s.cancel()
		delete(mm.sessions, port)
		delete(mm.misses, port)
		doomed = append(doomed, s)
		changed = true
	}
	if changed {
		mm.logger.Printf("light: rescan: ports now [%s]", strings.Join(mm.portsLocked(), " "))
	}
	mm.mu.Unlock()
	for _, s := range doomed {
		mm.drain(s)
	}
}

// drain waits (bounded) for one cancelled session goroutine to exit,
// NEVER while holding mm.mu. On timeout the goroutine and its port handle
// are abandoned: a truly wedged write leaks one goroutine + handle -
// degraded, never wedged; same-COM re-adoption self-heals via the
// open-retry loop once the driver completes the I/O.
func (mm *MultiManager) drain(s *lightSession) {
	select {
	case <-s.done:
	case <-time.After(drainTimeout):
		mm.logger.Printf("light %s: session still draining", s.port)
	}
}

// stillPresent is every session's Manager.Present: it consults the
// debounced rescan snapshot instead of re-enumerating (sessions never
// call the enumerator - one shared view of the bus). A port counts as
// present until missThreshold consecutive successful scans omitted it;
// with no fresh successful-scan data this fails open (zero misses).
func (mm *MultiManager) stillPresent(port string) bool {
	mm.mu.Lock()
	defer mm.mu.Unlock()
	return mm.misses[port] < missThreshold
}

// startSessionLocked creates and launches the per-port Manager. When this
// is the only attached light and no per-port state exists yet, the legacy
// single-light state file is migrated (renamed) so a one-light setup
// keeps its remembered look across the upgrade.
func (mm *MultiManager) startSessionLocked(ctx context.Context, port string, scanCount int) {
	statePath := mm.statePath(port)
	if statePath != "" && scanCount == 1 {
		if _, err := os.Stat(statePath); os.IsNotExist(err) {
			legacy := filepath.Join(mm.stateDir, "light-state.json")
			if err := os.Rename(legacy, statePath); err == nil {
				mm.logger.Printf("light %s: migrated legacy light-state.json", port)
			}
		}
	}
	logger := log.New(mm.logger.Writer(), port+" ", mm.logger.Flags()|log.Lmsgprefix)
	m := NewManager(logger, statePath)
	p := port
	// Presence reads the debounced rescan snapshot - sessions never call
	// the enumerator directly (one shared, rate-limited view of the bus).
	m.Present = func() bool { return mm.stillPresent(p) }
	sessCtx, cancel := context.WithCancel(ctx)
	s := &lightSession{port: port, m: m, cancel: cancel, done: make(chan struct{})}
	mm.sessions[port] = s
	mm.logger.Printf("light %s: starting session", port)
	go func() {
		defer close(s.done)
		m.Run(sessCtx, func() (Port, error) { return mm.openPort(p) })
	}()
}

// statePath returns the per-port persistence file
// (<stateDir>/light-state-<COMx>.json), or "" when persistence is off.
func (mm *MultiManager) statePath(port string) string {
	if mm.stateDir == "" {
		return ""
	}
	return filepath.Join(mm.stateDir, "light-state-"+port+".json")
}

func (mm *MultiManager) portsLocked() []string {
	ports := make([]string, 0, len(mm.sessions))
	for p := range mm.sessions {
		ports = append(ports, p)
	}
	sortPorts(ports)
	return ports
}

// sortPorts orders COM names numerically (COM4 before COM12): all names
// are "COM"+digits, so shorter-first then lexicographic is numeric order.
func sortPorts(ports []string) {
	sort.Slice(ports, func(i, j int) bool {
		a, b := ports[i], ports[j]
		if len(a) != len(b) {
			return len(a) < len(b)
		}
		return a < b
	})
}

// stopAll cancels every session and waits for each to exit - bounded by
// drainTimeout and outside mm.mu, exactly like rescan's teardown.
func (mm *MultiManager) stopAll() {
	mm.mu.Lock()
	var doomed []*lightSession
	for port, s := range mm.sessions {
		s.cancel()
		delete(mm.sessions, port)
		doomed = append(doomed, s)
	}
	mm.misses = map[string]int{}
	mm.mu.Unlock()
	for _, s := range doomed {
		mm.drain(s)
	}
}

// fmt is used by Task 4 (command surface); referenced here so the import
// stays valid if tasks land separately.
var _ = fmt.Sprintf
