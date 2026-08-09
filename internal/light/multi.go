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
		// Note: do NOT delete mm.misses[port] here - leave it >= missThreshold
		// so in-flight or later stillPresent() calls observe false.
		// The counter resets when the port reappears (delete in seen loop above).
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

// HandleCommand dispatches a light command: @-addressed to one manager,
// bare verbs to all (fleet). Returns a single string (joined by \n for fleets
// multi-port replies). Commands: toggle, on, off, brightness <x>, temp <x>,
// name <port> <name>, unname <name|port>, list, status.
func (mm *MultiManager) HandleCommand(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return "error: unknown light command"
	}

	// Check for @addressing: @<name|port> <verb>
	if strings.HasPrefix(cmd, "@") {
		parts := strings.SplitN(cmd, " ", 2)
		if len(parts) < 2 || parts[0] == "@" {
			return "error: usage: light@<name|COMx> <command>"
		}
		target := strings.TrimPrefix(parts[0], "@")
		if target == "" {
			return "error: usage: light@<name|COMx> <command>"
		}
		verb := parts[1]
		return mm.handleTargeted(target, verb)
	}

	// Bare command: parse verb
	parts := strings.SplitN(cmd, " ", 2)
	verb := parts[0]
	rest := ""
	if len(parts) > 1 {
		rest = parts[1]
	}

	switch verb {
	case "name":
		return mm.handleName(rest)
	case "unname":
		return mm.handleUnname(rest)
	case "list":
		if rest != "" {
			return "error: unknown light command"
		}
		return mm.handleList()
	default:
		// Bare command: fan out to all lights
		return mm.handleBareCommand(verb, rest)
	}
}

// handleTargeted dispatches a command to a single named or port-addressed light.
func (mm *MultiManager) handleTargeted(target, verb string) string {
	// Try to resolve the target as either a port or a name
	port, ok := mm.reg.Resolve(target)
	if !ok {
		// Not a valid port, not a known name
		knownStr := mm.knownLightsString(true, true)
		return fmt.Sprintf(`error: unknown light "%s" (known: %s)`, target, knownStr)
	}

	// Verify the port is connected
	mm.mu.Lock()
	s, ok := mm.sessions[port]
	mm.mu.Unlock()
	if !ok {
		knownStr := mm.knownLightsString(true, true)
		return fmt.Sprintf("error: light %s not connected (known: %s)", port, knownStr)
	}

	// Call with timeout
	ctx, cancel := context.WithTimeout(context.Background(), lightCallTimeout)
	defer cancel()
	return s.m.HandleCommandWithTimeout(ctx, verb)
}

// handleName binds a port to a name using Registry.Assign.
func (mm *MultiManager) handleName(args string) string {
	fields := strings.Fields(args)
	if len(fields) != 2 {
		return "error: usage: light name <COMx> <name>"
	}
	port, name := fields[0], fields[1]

	// Assign will validate and normalize the port
	if err := mm.reg.Assign(port, name); err != nil {
		return "error: " + err.Error()
	}
	// Assign already normalized port internally; we return it as returned
	p, _ := NormalizePort(port)
	return fmt.Sprintf("named %s %s", p, strings.ToLower(name))
}

// handleUnname removes a name binding using Registry.Unname.
func (mm *MultiManager) handleUnname(args string) string {
	target := strings.TrimSpace(args)
	if target == "" {
		return "error: usage: light unname <name|COMx>"
	}

	// Unname returns the removed name, or error
	name, err := mm.reg.Unname(target)
	if err != nil {
		// Target doesn't exist; unname is idempotent - return as if it did
		return fmt.Sprintf("unnamed %s", strings.ToLower(target))
	}
	return fmt.Sprintf("unnamed %s", name)
}

// handleList returns per-light status lines in sorted port order, or
// "no lights known" if the registry is empty.
func (mm *MultiManager) handleList() string {
	mm.mu.Lock()
	ports := mm.portsLocked()
	mm.mu.Unlock()

	// Get all registered names
	allNames := mm.reg.All()

	if len(ports) == 0 && len(allNames) == 0 {
		return "no lights known"
	}

	// Collect all known ports (connected + named-but-not-connected)
	allPorts := make(map[string]bool)
	for _, p := range ports {
		allPorts[p] = true
	}
	for _, p := range allNames {
		allPorts[p] = true
	}

	// Sort ports and build lines
	sortedPorts := make([]string, 0, len(allPorts))
	for p := range allPorts {
		sortedPorts = append(sortedPorts, p)
	}
	sortPorts(sortedPorts)

	var lines []string
	for _, port := range sortedPorts {
		// Get the name for this port
		name := mm.reg.NameFor(port)
		if name == "" {
			name = "-"
		}

		connected := false
		var status string
		mm.mu.Lock()
		s, ok := mm.sessions[port]
		mm.mu.Unlock()
		if ok {
			ctx, cancel := context.WithTimeout(context.Background(), lightCallTimeout)
			status = s.m.ProbeStatus(ctx)
			cancel()
			connected = !strings.HasPrefix(status, "error:") && status != "disconnected"
		} else {
			status = "disconnected"
		}

		var line string
		if strings.HasPrefix(status, "error:") {
			line = fmt.Sprintf("%s %s %s", port, name, status)
		} else if connected {
			line = fmt.Sprintf("%s %s connected %s", port, name, status)
		} else {
			line = fmt.Sprintf("%s %s %s", port, name, status)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// handleBareCommand fans out verb to all connected lights in sorted port order,
// collecting their replies. For "toggle": if ANY light is on -> ALL off;
// else ALL on (unknown counts as off).
func (mm *MultiManager) handleBareCommand(verb, rest string) string {
	mm.mu.Lock()
	if len(mm.sessions) == 0 {
		mm.mu.Unlock()
		return "error: no light"
	}
	ports := mm.portsLocked()
	sessions := make(map[string]*Manager)
	for _, p := range ports {
		sessions[p] = mm.sessions[p].m
	}
	mm.mu.Unlock()

	// Special handling for toggle: if any light is on -> all off; else all on
	if verb == "toggle" {
		anyOn := false
		for _, m := range sessions {
			ctx, cancel := context.WithTimeout(context.Background(), lightCallTimeout)
			reply := m.HandleCommandWithTimeout(ctx, "status")
			cancel()
			if strings.HasPrefix(reply, "on") {
				anyOn = true
				break
			}
		}
		if anyOn {
			verb = "off"
		} else {
			verb = "on"
		}
	}

	// Execute verb on all lights in parallel with timeout, collect results
	cmd := verb
	if rest != "" {
		cmd += " " + rest
	}
	results := make(map[string]string)
	var wg sync.WaitGroup
	var mu sync.Mutex

	for port, m := range sessions {
		wg.Add(1)
		go func(p string, manager *Manager) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), lightCallTimeout)
			defer cancel()
			reply := manager.HandleCommandWithTimeout(ctx, cmd)
			mu.Lock()
			results[p] = reply
			mu.Unlock()
		}(port, m)
	}
	wg.Wait()

	// Format replies in port order
	var lines []string
	for _, port := range ports {
		reply := results[port]
		lines = append(lines, fmt.Sprintf("%s: %s", port, reply))
	}
	return strings.Join(lines, "\n")
}

// knownLightsString returns a display of known lights for error messages.
// Format: "COM4=desk COM7=spare" if includeNames, else "COM4 COM7".
func (mm *MultiManager) knownLightsString(includeNames, includeConnected bool) string {
	mm.mu.Lock()
	ports := mm.portsLocked()
	mm.mu.Unlock()

	// Get all registered names
	allNames := mm.reg.All()

	// Collect all known ports
	allPorts := make(map[string]bool)
	for _, p := range ports {
		allPorts[p] = true
	}
	for _, p := range allNames {
		allPorts[p] = true
	}

	if len(allPorts) == 0 {
		return "none"
	}

	sortedPorts := make([]string, 0, len(allPorts))
	for p := range allPorts {
		sortedPorts = append(sortedPorts, p)
	}
	sortPorts(sortedPorts)

	var parts []string
	for _, p := range sortedPorts {
		if includeNames {
			if name := mm.reg.NameFor(p); name != "" {
				parts = append(parts, fmt.Sprintf("%s=%s", p, name))
			} else {
				parts = append(parts, p)
			}
		} else {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, " ")
}
