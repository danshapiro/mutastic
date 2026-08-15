package light

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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

// CallTimeout is the production bound on every per-light command/poll
// call: a light exceeding it yields a per-line "error: timeout" while the
// rest of the fleet still answers. Exported so the CLI client can size its
// read budget ABOVE it - the daemon's timer starts only after the packet
// arrives, so a client budget <= CallTimeout deterministically misses the
// degraded-mode reply and masks partial success as "no daemon reachable".
const CallTimeout = 2 * time.Second

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
	// both knobs in one edit). Production value is CallTimeout.
	lightCallTimeout = CallTimeout
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
	settings  *SettingsStore // saved named settings at <stateDir>/light-settings.json
	stateDir  string         // per-port state files live here; "" disables persistence
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
// rescan loop. The saved-settings store lives beside the per-port state
// files in stateDir ("" stateDir disables it).
func NewMultiManager(logger *log.Logger, stateDir string, reg *Registry, enumerate Enumerate, openPort OpenPort) *MultiManager {
	settingsPath := ""
	if stateDir != "" {
		settingsPath = filepath.Join(stateDir, "light-settings.json")
	}
	return &MultiManager{
		logger:    logger,
		reg:       reg,
		settings:  NewSettingsStore(settingsPath),
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

// HandleCommand implements the daemon's CommandHandler for the whole
// fleet. The daemon strips the "light" prefix and trims, so cmd looks
// like "toggle", "@desk brightness 40", "name COM4 desk", "unname desk",
// "list", or a "settings save|list|apply|delete" store verb.
func (mm *MultiManager) HandleCommand(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if strings.HasPrefix(cmd, "@") {
		return mm.handleTargeted(cmd)
	}
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return "error: unknown light command"
	}
	switch fields[0] {
	case "name":
		if len(fields) != 3 {
			return "error: usage: light name <COMx> <name>"
		}
		if err := mm.reg.Assign(fields[1], fields[2]); err != nil {
			return "error: " + err.Error()
		}
		port, _ := NormalizePort(fields[1])
		return fmt.Sprintf("named %s %s", port, strings.ToLower(fields[2]))
	case "unname":
		if len(fields) != 2 {
			return "error: usage: light unname <name|COMx>"
		}
		name, err := mm.reg.Unname(fields[1])
		if err != nil {
			return "error: " + err.Error()
		}
		return "unnamed " + name
	case "list":
		if len(fields) != 1 {
			return "error: unknown light command"
		}
		return mm.list()
	case "settings":
		return mm.handleSettings(cmd)
	case "brightness-delta":
		if len(fields) != 2 {
			return "error: brightness-delta must be between -20 and 20"
		}
		delta, err := strconv.Atoi(fields[1])
		if err != nil || delta < -20 || delta > 20 {
			return "error: brightness-delta must be between -20 and 20"
		}
		return mm.handleDelta(deltaKindBrightness, delta)
	case "temp-step-delta":
		if len(fields) != 2 {
			return "error: temp-step-delta must be between -3 and 3"
		}
		delta, err := strconv.Atoi(fields[1])
		if err != nil || delta < -3 || delta > 3 {
			return "error: temp-step-delta must be between -3 and 3"
		}
		return mm.handleDelta(deltaKindTemperature, delta)
	}
	return mm.handleAll(cmd)
}

// handleTargeted answers "@<name|COMx> <verb...>" against a single light,
// returning the bare Manager reply (same shape as the old single-light
// replies).
func (mm *MultiManager) handleTargeted(cmd string) string {
	target, rest, _ := strings.Cut(cmd[1:], " ")
	rest = strings.TrimSpace(rest)
	if target == "" || rest == "" {
		return "error: usage: light@<name|COMx> <command>"
	}
	port, ok := mm.reg.Resolve(target)
	if !ok {
		return fmt.Sprintf("error: unknown light %q (known: %s)", target, mm.known())
	}
	mm.mu.Lock()
	s, ok := mm.sessions[port]
	mm.mu.Unlock()
	if !ok {
		return fmt.Sprintf("error: light %s not connected (known: %s)", port, mm.known())
	}
	// One bounded call: a wedged light must not stall the UDP loop.
	return mm.callLight(port, func() string { return s.m.HandleCommand(rest) })
}

// callLight runs one per-light call with a deadline. A light exceeding
// lightCallTimeout (wedged serial I/O) yields "error: timeout"; the
// abandoned call finishes on its own goroutine and its result is
// discarded (buffered channel - nothing leaks beyond the wedged serial
// write itself, which is unavoidable: the library has no write timeout).
func (mm *MultiManager) callLight(port string, call func() string) string {
	ch := make(chan string, 1)
	go func() { ch <- call() }()
	select {
	case reply := <-ch:
		return reply
	case <-time.After(lightCallTimeout):
		mm.logger.Printf("light %s: call timed out after %v", port, lightCallTimeout)
		return "error: timeout"
	}
}

// handleAll fans a bare verb out to every tracked light IN PARALLEL, one
// reply line per light assembled in sorted port order (stable output,
// byte-identical to serial fan-out for healthy lights). Every per-light
// call is bounded by lightCallTimeout so one wedged light cannot stall
// the fleet or the daemon's UDP loop. "toggle" is fleet-level: if ANY
// light is on, all go off; otherwise all go on (each restoring its own
// persisted look; unknown - or timed out - counts as off).
// The "status" verb is the cheap resident-poller path: Manager.HandleCommand
// reads State only and does not touch the serial Port or its write mutex.
func (mm *MultiManager) handleAll(cmd string) string {
	mm.mu.Lock()
	ports := mm.portsLocked()
	managers := make(map[string]*Manager, len(ports))
	for _, p := range ports {
		managers[p] = mm.sessions[p].m
	}
	mm.mu.Unlock()
	if len(ports) == 0 {
		return "error: no light"
	}
	if cmd == "toggle" {
		cmd = "on"
		on := make([]bool, len(ports))
		var wg sync.WaitGroup
		for i, p := range ports {
			wg.Add(1)
			go func(i int, p string, m *Manager) {
				defer wg.Done()
				reply := mm.callLight(p, func() string {
					if isOn, _ := m.PowerState(); isOn {
						return "on"
					}
					return "off"
				})
				on[i] = reply == "on" // "error: timeout" counts as off (logged)
			}(i, p, managers[p])
		}
		wg.Wait()
		for i := range on {
			if on[i] {
				cmd = "off"
				break
			}
		}
	}
	lines := make([]string, len(ports))
	var wg sync.WaitGroup
	for i, p := range ports {
		wg.Add(1)
		go func(i int, p string, m *Manager) {
			defer wg.Done()
			lines[i] = mm.label(p) + ": " + mm.callLight(p, func() string { return m.HandleCommand(cmd) })
		}(i, p, managers[p])
	}
	wg.Wait()
	return strings.Join(lines, "\n")
}

type deltaKind uint8

const (
	deltaKindBrightness deltaKind = iota
	deltaKindTemperature
)

// handleDelta applies one relative fleet change from the daemon's single UDP
// command loop. It does not issue a nested "list" or a sequence of targeted
// UDP commands: all reads, calculations, and writes happen before the caller
// returns to serveUDP and reads another datagram. Per-light calls are still
// bounded and run in parallel, matching the existing collective operations.
func (mm *MultiManager) handleDelta(kind deltaKind, delta int) string {
	mm.mu.Lock()
	ports := mm.portsLocked()
	managers := make(map[string]*Manager, len(ports))
	for _, port := range ports {
		managers[port] = mm.sessions[port].m
	}
	mm.mu.Unlock()
	for _, port := range mm.reg.All() {
		if _, ok := managers[port]; ok {
			continue
		}
		managers[port] = nil
		ports = append(ports, port)
	}
	sortPorts(ports)
	if len(ports) == 0 {
		return "error: no light"
	}

	lines := make([]string, len(ports))
	var wg sync.WaitGroup
	for i, port := range ports {
		wg.Add(1)
		go func(i int, port string, manager *Manager) {
			defer wg.Done()
			if manager == nil {
				lines[i] = mm.label(port) + ": disconnected"
				return
			}
			reply := mm.callLight(port, func() string {
				return applyDelta(manager, kind, delta)
			})
			lines[i] = mm.label(port) + ": " + reply
		}(i, port, managers[port])
	}
	wg.Wait()
	return strings.Join(lines, "\n")
}

// applyDelta reads and updates one connected, known, on light. The caller
// bounds this function with callLight so a wedged serial write becomes a
// stable per-light error instead of blocking the fleet command forever.
func applyDelta(manager *Manager, kind deltaKind, delta int) string {
	if !manager.Connected() {
		return "disconnected"
	}
	on, brightness, temp, known := manager.state.Status()
	if !known {
		return "unknown"
	}
	if !on {
		return "off"
	}

	switch kind {
	case deltaKindBrightness:
		return manager.apply(clampInt(brightness+delta, 1, 100), temp)
	case deltaKindTemperature:
		index := clampInt(int(temp)+delta, 0, int(maxTempByte))
		return manager.apply(brightness, byte(index))
	default:
		return "error: unknown light delta"
	}
}

func clampInt(value, min, max int) int {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

// handleSettings answers the "settings save|list|apply|delete <name>"
// sub-verbs against the daemon-owned store. The name is re-read from the
// RAW suffix after the sub-verb (not from field-splitting) so an embedded
// newline is never collapsed into a space before validation sees it; it is
// then trimmed - names are outer-trimmed at the pipeline boundary, so
// leading/trailing whitespace is never meaningful and "foo " IS "foo".
func (mm *MultiManager) handleSettings(cmd string) string {
	sub := strings.TrimSpace(strings.TrimPrefix(cmd, "settings"))
	fields := strings.Fields(sub)
	if len(fields) == 0 {
		return "error: usage: light settings <save|list|apply|delete> [name]"
	}
	verb := fields[0]
	name := strings.TrimSpace(sub[len(verb):])
	if verb == "list" {
		if name != "" {
			return "error: usage: light settings <save|list|apply|delete> [name]"
		}
		if err := mm.settings.refusal(); err != nil {
			return "error: " + err.Error()
		}
		return strings.Join(mm.settings.List(), "\n")
	}
	if verb != "save" && verb != "apply" && verb != "delete" {
		return "error: usage: light settings <save|list|apply|delete> [name]"
	}
	// A disabled or disabled-corrupt store short-circuits every mutating
	// verb with the same single-line refusal; then the name grammar
	// (identical for save, apply, and delete) runs before any store work.
	if err := mm.settings.refusal(); err != nil {
		return "error: " + err.Error()
	}
	if invalid := validateSettingsName(name); invalid != "" {
		return invalid
	}
	switch verb {
	case "save":
		return mm.settingsSave(name)
	case "apply":
		return mm.settingsApply(name)
	default:
		return mm.settingsDelete(name)
	}
}

// settingsSave snapshots the live-connected, known-state fleet and stores
// it under name (overwriting by exact name).
func (mm *MultiManager) settingsSave(name string) string {
	snap := mm.settingsSnapshot()
	if len(snap.Lights) == 0 {
		return "error: no known light state to save"
	}
	if err := mm.settings.Save(name, snap); err != nil {
		if errors.Is(err, errSettingsCap) {
			return "error: " + err.Error()
		}
		return fmt.Sprintf("error: settings save failed: %v", err)
	}
	return fmt.Sprintf("saved %q (%d lights)", name, len(snap.Lights))
}

// settingsSnapshot captures the same LIVE-CONNECTED light set the fleet
// fan-out enumerates (mm.sessions under mm.mu - NEVER per-light
// Manager.Connected, which takes the per-light mutex and can block behind
// a wedged write). All reads are memory-only state reads, so a wedged
// light cannot stall the UDP loop. Lights whose state is still UNKNOWN
// (after a daemon restart, until an echo or knob event) are OMITTED:
// snapshotting one would record invented defaults instead of the current
// hardware look. Entries are keyed by COM port path ONLY; an off light
// saves its restore-target brightness, not 0.
func (mm *MultiManager) settingsSnapshot() SavedSetting {
	mm.mu.Lock()
	ports := mm.portsLocked()
	managers := make(map[string]*Manager, len(ports))
	for _, p := range ports {
		managers[p] = mm.sessions[p].m
	}
	mm.mu.Unlock()
	lights := make(map[string]SavedLightState, len(ports))
	for _, p := range ports {
		m := managers[p]
		on, brightness, temp, known := m.state.Status()
		if !known {
			continue
		}
		entry := SavedLightState{On: on, Brightness: brightness, TempByte: temp}
		if !on {
			entry.Brightness, _ = m.state.TargetOn()
		}
		lights[p] = entry
	}
	return SavedSetting{Lights: lights}
}

// settingsApply replays one named snapshot across the fleet IN PARALLEL
// exactly like the handleAll fan-out: goroutine per key, wg.Wait, reply
// lines preallocated in keys-sorted order so the reply is deterministic.
// Keys are COM port paths ONLY and resolve against the live session on
// that port - never through the (mutable) registry - so a port with no
// live session reports unreachable and is skipped while the rest apply.
func (mm *MultiManager) settingsApply(name string) string {
	snap, ok := mm.settings.Get(name)
	if !ok {
		return fmt.Sprintf("error: unknown setting %q", name)
	}
	mm.mu.Lock()
	ports := mm.portsLocked()
	managers := make(map[string]*Manager, len(ports))
	for _, p := range ports {
		managers[p] = mm.sessions[p].m
	}
	mm.mu.Unlock()
	if len(ports) == 0 {
		return "error: no lights connected"
	}
	keys := make([]string, 0, len(snap.Lights))
	for k := range snap.Lights {
		keys = append(keys, k)
	}
	sortPorts(keys)
	lines := make([]string, len(keys))
	var wg sync.WaitGroup
	for i, key := range keys {
		wg.Add(1)
		go func(i int, key string, entry SavedLightState) {
			defer wg.Done()
			m := managers[key]
			if m == nil {
				lines[i] = fmt.Sprintf("error: light %q: unreachable, skipped", key)
				return
			}
			// Each key's whole frame sequence runs inside ONE callLight, so
			// a wedged light costs one bounded lightCallTimeout once. The
			// fleet label still renders display names into reply lines;
			// identity stays port-only.
			reply := mm.callLight(key, func() string { return applySaved(m, entry) })
			lines[i] = mm.label(key) + ": " + reply
		}(i, key, snap.Lights[key])
	}
	wg.Wait()
	return strings.Join(lines, "\n")
}

// applySaved replays one saved entry through the per-light command path
// with the power-state frame ALWAYS LAST: an ON entry plays on ->
// brightness -> temp; an OFF entry plays brightness -> temp -> off, so the
// saved brightness/temp land in the light's restore targets BEFORE the off
// frame parks it and a later "on" restores the saved look. Brightness/temp
// writes to an off light briefly ENERGIZE it - firmware behavior; no
// silent alternative exists - so applying an off entry flashes the light
// momentarily before the off frame lands. The stored temp byte renders
// through ByteToKelvin, re-quantizing to the same step.
func applySaved(m *Manager, entry SavedLightState) string {
	cmds := make([]string, 0, 3)
	if entry.On {
		cmds = append(cmds, "on")
	}
	cmds = append(cmds,
		fmt.Sprintf("brightness %d", entry.Brightness),
		fmt.Sprintf("temp %d", ByteToKelvin(entry.TempByte)),
	)
	if !entry.On {
		cmds = append(cmds, "off")
	}
	reply := ""
	for _, cmd := range cmds {
		reply = m.HandleCommand(cmd)
	}
	return reply
}

// settingsDelete removes a saved name, persisting the store with the same
// write→close→replace discipline as save.
func (mm *MultiManager) settingsDelete(name string) string {
	if err := mm.settings.Delete(name); err != nil {
		if errors.Is(err, errUnknownSetting) {
			return fmt.Sprintf("error: unknown setting %q", name)
		}
		return fmt.Sprintf("error: settings delete failed: %v", err)
	}
	return fmt.Sprintf("deleted %q", name)
}

// label renders "COM4 desk" for named lights, bare "COM4" otherwise.
func (mm *MultiManager) label(port string) string {
	if name := mm.reg.NameFor(port); name != "" {
		return port + " " + name
	}
	return port
}

// known renders every addressable light for error messages:
// "COM4=desk, COM7" (attached ports plus named-but-absent ports).
func (mm *MultiManager) known() string {
	set := map[string]bool{}
	mm.mu.Lock()
	for p := range mm.sessions {
		set[p] = true
	}
	mm.mu.Unlock()
	for _, p := range mm.reg.All() {
		set[p] = true
	}
	if len(set) == 0 {
		return "none"
	}
	all := make([]string, 0, len(set))
	for p := range set {
		all = append(all, p)
	}
	sortPorts(all)
	parts := make([]string, len(all))
	for i, p := range all {
		if name := mm.reg.NameFor(p); name != "" {
			parts[i] = p + "=" + name
		} else {
			parts[i] = p
		}
	}
	return strings.Join(parts, ", ")
}

// list renders one line per known light:
// "<port> <name|-> connected <state>" or "<port> <name|-> disconnected".
func (mm *MultiManager) list() string {
	managers := map[string]*Manager{}
	mm.mu.Lock()
	for p, s := range mm.sessions {
		managers[p] = s.m
	}
	mm.mu.Unlock()
	set := map[string]bool{}
	for p := range managers {
		set[p] = true
	}
	for _, p := range mm.reg.All() {
		set[p] = true
	}
	if len(set) == 0 {
		return "no lights known"
	}
	ports := make([]string, 0, len(set))
	for p := range set {
		ports = append(ports, p)
	}
	sortPorts(ports)
	lines := make([]string, len(ports))
	for i, p := range ports {
		name := mm.reg.NameFor(p)
		if name == "" {
			name = "-"
		}
		m, ok := managers[p]
		if !ok {
			lines[i] = fmt.Sprintf("%s %s disconnected", p, name)
			continue
		}
		// The probe must be bounded too: Connected() takes m.mu, which an
		// abandoned timed-out call can hold forever inside a wedged
		// writeFrame - an unbounded probe here would wedge the UDP loop.
		lines[i] = fmt.Sprintf("%s %s %s", p, name, mm.callLight(p, func() string {
			if m.Connected() {
				return "connected " + m.HandleCommand("status")
			}
			return "disconnected"
		}))
	}
	return strings.Join(lines, "\n")
}
