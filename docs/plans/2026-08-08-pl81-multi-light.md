# PL81 Multi-Light Support Implementation Plan

> **For agentic workers:** This plan is executed task-by-task by the
> workflow's execute stage: a fresh implementer per task, with a spec +
> quality review after each task. Steps use checkbox (`- [ ]`) syntax
> for tracking.

**Goal:** Extend mutastic's single NEEWER PL81 PRO light support to multiple lights with hot-plug discovery, persistent user-assigned names, per-light addressing (`light@desk`), and collective control (bare `light toggle` toggles ALL lights — the F13 pedal behavior).

**Architecture:** Keep the existing `light.Manager` as the untouched per-light brick (one session per COM port: state tracker, reconnect loop, rate-limited writes). Add a new `light.MultiManager` that owns `map[COMport]*Manager`, rescans USB enumeration every 5 s to start/stop sessions (hot-plug), and implements the daemon's `CommandHandler` interface with addressing, naming, and collective fan-out. A new `light.Registry` persists name↔port bindings. Discovery stays in `package main` behind injected function types, so all fleet logic is unit-testable against the existing `fakePort`.

**Tech Stack:** Go 1.26.3, `go.bug.st/serial` v1.8.0 (pinned), mingw cross-compile via `build.sh`. No new dependencies.

## Global Constraints

- Module is `mutastic`; Go 1.26.3; dependencies are frozen: `go.bug.st/serial v1.8.0` (pinned deliberately), `github.com/sstallion/go-hid v0.15.0`. Do NOT add or upgrade dependencies.
- Gate after every task: `go test -race ./... && go vet ./...` clean (run from the repo root `/home/dan/code/mutastic/.worktrees/pl81-multi-light`). All existing mic + single-light tests keep passing.
- Windows cross-compile must stay green: `./build.sh` produces `bin/mutastic.exe` (mingw + cgo). Any signature change to `light_windows.go` must be mirrored in `light_other.go` (build tags).
- Persistent files live in `%LOCALAPPDATA%\mutastic\` via `os.UserCacheDir()` + `"mutastic"`: `mutastic.log`, `light-names.json` (new), `light-state-<COMx>.json` (new, replaces `light-state.json`).
- Light identity = Windows COM port name. CH340 bridges (VID `1A86` PID `7523`) have NO unique USB serial number; the COM port is the only discriminator and is stable per physical USB jack. Never key on anything else.
- Exact reply strings are contract (tests, README, exit codes depend on them). The single-`Manager` reply strings (`on 40% 4950K`, `off`, `unknown`, `error: ...`) are unchanged.
- Collective toggle semantics: if ANY light is on → turn ALL off; otherwise turn ALL on (each restoring its own persisted brightness/temp). Unknown state counts as off.
- `internal/daemon` must NOT import `internal/light` — the `CommandHandler` interface seam stays.
- `ahk/MuteAllMeetings.ahk` is UTF-8 with BOM + CRLF, AHK v1.1. It is NOT modified in this plan (F13 already sends `light toggle`, which now means "toggle all") — verify only.
- No `t.Parallel()` in `package light` (timing knobs are package-level vars mutated by `fastTimings`/`setFastWrites`).
- Daemon inbound UDP buffer is 64 bytes — do not change it; name length is capped at 16 chars so every command fits. Client reply buffer grows to 2048 bytes (Task 6).
- README.md and docs/pl81-pro-serial-protocol.md are the only docs to edit; this plan file is a working doc.
- The user's ONE live light is on COM4, currently ON at 30% 2900K. Every live test ends with it ON at 30% 2900K and the mic UNMUTED.
- WSL→Windows interop has been intermittently flaky (vsock errors): retry interop commands a few times over a couple of minutes before treating a failure as a blocker.

## Scope Check

One subsystem: the light control path (CLI → UDP → daemon routing → light sessions). The mic path is untouched apart from shared `main()` dispatch. One plan is appropriate; whole-system coverage comes from the existing daemon/mic suites plus the live E2E task at the end.

## File Structure

| File | Action | Responsibility |
|---|---|---|
| `internal/light/names.go` | Create | Persistent name↔port registry (`light-names.json`) |
| `internal/light/names_test.go` | Create | Registry unit tests (assign/move/clear/resolve/persist) |
| `internal/light/manager.go` | Modify | Add `Connected()` + `PowerState()` fleet hooks (nothing else) |
| `internal/light/manager_test.go` | Modify | Test for the new hooks |
| `internal/light/multi.go` | Create | `MultiManager`: rescan loop, per-port sessions, state migration, addressing, collective commands |
| `internal/light/multi_test.go` | Create | Fleet lifecycle + command surface tests (fake serial only) |
| `internal/daemon/daemon.go` | Modify | Route `light@...` to the light handler (one condition) |
| `internal/daemon/daemon_test.go` | Modify | Routing test for `light@` |
| `main.go` | Modify | `clientCommand` dispatch helper, `light@` case, 2048-byte reply buffer, `usage()`, `lightStateDir()`, MultiManager wiring |
| `main_test.go` | Modify | `clientCommand` table test + large-reply test |
| `light_windows.go` | Modify | `enumeratePL81Ports` / `openPL81Port` / `pl81PortPresent` (replace single-light `openPL81`/`pl81Present`) |
| `light_other.go` | Modify | Non-Windows stubs for the same three functions |
| `README.md` | Modify | Command table, multi-light semantics, collective toggle doc, troubleshooting + deploy-hang note |
| `docs/pl81-pro-serial-protocol.md` | Modify | CH340 no-serial identity note, `## Multiple panels` section, human question |

---

### Task 1: Persistent name registry

**Files:**
- Create: `internal/light/names.go`
- Test: `internal/light/names_test.go`

**Interfaces:**
- Consumes: nothing new (stdlib only; lives in existing `package light`).
- Produces (later tasks rely on these exact signatures):
  - `func NewRegistry(path string) *Registry` — loads `light-names.json`; `""` disables persistence; missing/corrupt file starts empty.
  - `func NormalizePort(s string) (string, error)` — `"com4"` → `"COM4"`; error unless `^COM[0-9]+$` after uppercasing.
  - `func (r *Registry) Assign(port, name string) error` — bijective bind; replaces the port's old name; moves the name if bound elsewhere.
  - `func (r *Registry) Unname(target string) (string, error)` — by name or port; returns removed name.
  - `func (r *Registry) Resolve(target string) (string, bool)` — name (case-insensitive) or COM-port literal → canonical port.
  - `func (r *Registry) NameFor(port string) string` — `""` when unnamed.
  - `func (r *Registry) All() map[string]string` — copy of name→port bindings.

- [ ] **Step 1: Write the failing tests**

Create `internal/light/names_test.go`:

```go
package light

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRegistryAssignResolveNameFor(t *testing.T) {
	r := NewRegistry(filepath.Join(t.TempDir(), "light-names.json"))
	if err := r.Assign("com4", "Desk"); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"desk", "DESK", "com4", "COM4"} {
		p, ok := r.Resolve(target)
		if !ok || p != "COM4" {
			t.Fatalf("Resolve(%q) = %q, %v; want COM4, true", target, p, ok)
		}
	}
	if got := r.NameFor("COM4"); got != "desk" {
		t.Fatalf("NameFor(COM4) = %q, want desk", got)
	}
	if got := r.NameFor("COM7"); got != "" {
		t.Fatalf("NameFor(COM7) = %q, want empty", got)
	}
}

func TestRegistryResolvesPortLiterals(t *testing.T) {
	r := NewRegistry("")
	if p, ok := r.Resolve("com9"); !ok || p != "COM9" {
		t.Fatalf("Resolve(com9) = %q, %v; want COM9, true", p, ok)
	}
	if _, ok := r.Resolve("nope"); ok {
		t.Fatal("Resolve(nope) should fail")
	}
}

func TestRegistryReassignMovesName(t *testing.T) {
	r := NewRegistry("")
	if err := r.Assign("COM4", "desk"); err != nil {
		t.Fatal(err)
	}
	if err := r.Assign("COM7", "desk"); err != nil {
		t.Fatal(err)
	}
	if p, _ := r.Resolve("desk"); p != "COM7" {
		t.Fatalf("desk -> %q, want COM7", p)
	}
	if got := r.NameFor("COM4"); got != "" {
		t.Fatalf("COM4 still named %q, want unnamed", got)
	}
}

func TestRegistryRenamingPortReplacesOldName(t *testing.T) {
	r := NewRegistry("")
	r.Assign("COM4", "desk")
	r.Assign("COM4", "key")
	if got := r.NameFor("COM4"); got != "key" {
		t.Fatalf("NameFor(COM4) = %q, want key", got)
	}
	if _, ok := r.Resolve("desk"); ok {
		t.Fatal("old name desk should be gone")
	}
}

func TestRegistryUnname(t *testing.T) {
	r := NewRegistry("")
	r.Assign("COM4", "desk")
	name, err := r.Unname("desk")
	if err != nil || name != "desk" {
		t.Fatalf("Unname(desk) = %q, %v; want desk, nil", name, err)
	}
	r.Assign("COM4", "desk")
	name, err = r.Unname("com4") // clearing by port works too
	if err != nil || name != "desk" {
		t.Fatalf("Unname(com4) = %q, %v; want desk, nil", name, err)
	}
	if _, err := r.Unname("desk"); err == nil {
		t.Fatal("Unname of unknown target should error")
	}
}

func TestRegistryValidation(t *testing.T) {
	r := NewRegistry("")
	for _, name := range []string{"", "9lives", "has space", "com7", "COM7", "this-name-is-way-too-long", "upper!"} {
		if err := r.Assign("COM4", name); err == nil {
			t.Fatalf("Assign accepted bad name %q", name)
		}
	}
	if err := r.Assign("USB0", "desk"); err == nil {
		t.Fatal("Assign accepted bad port USB0")
	}
}

func TestRegistryPersistsAndToleratesCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "light-names.json")
	r := NewRegistry(path)
	if err := r.Assign("COM4", "desk"); err != nil {
		t.Fatal(err)
	}
	r2 := NewRegistry(path)
	if p, ok := r2.Resolve("desk"); !ok || p != "COM4" {
		t.Fatalf("reloaded Resolve(desk) = %q, %v; want COM4, true", p, ok)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	r3 := NewRegistry(path)
	if _, ok := r3.Resolve("desk"); ok {
		t.Fatal("corrupt file should start empty")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /home/dan/code/mutastic/.worktrees/pl81-multi-light && go test ./internal/light/`
Expected: FAIL — build error `undefined: NewRegistry` (and `NormalizePort`).

- [ ] **Step 3: Write the implementation**

Create `internal/light/names.go`:

```go
package light

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Registry is a persistent, bijective name<->port map for addressing
// lights. Names are case-insensitive identifiers ("desk"); ports are
// Windows COM names ("COM4"). Reassigning a name moves it; naming a port
// replaces its old name. Backed by light-names.json ("" disables
// persistence; missing or corrupt files silently start empty, mirroring
// NewState).
type Registry struct {
	mu    sync.Mutex
	path  string
	names map[string]string // lowercase name -> canonical port ("COM4")
}

var (
	// namePattern: 1-16 chars of a-z 0-9 '-', starting with a letter.
	namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,15}$`)
	// portPattern matches canonical (uppercased) Windows COM names.
	portPattern = regexp.MustCompile(`^COM[0-9]+$`)
)

// NewRegistry loads light-names.json from path if it exists.
func NewRegistry(path string) *Registry {
	r := &Registry{path: path, names: map[string]string{}}
	if path == "" {
		return r
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return r
	}
	var m map[string]string
	if json.Unmarshal(data, &m) != nil {
		return r
	}
	for name, port := range m {
		if namePattern.MatchString(name) && portPattern.MatchString(port) {
			r.names[name] = port
		}
	}
	return r
}

// NormalizePort validates and canonicalizes a COM port name
// ("com4" -> "COM4").
func NormalizePort(s string) (string, error) {
	p := strings.ToUpper(s)
	if !portPattern.MatchString(p) {
		return "", fmt.Errorf("invalid port %q (want COM<n>)", s)
	}
	return p, nil
}

// Assign binds name to port (case-insensitive), replacing any existing
// name for that port and moving the name if it was bound elsewhere.
func (r *Registry) Assign(port, name string) error {
	p, err := NormalizePort(port)
	if err != nil {
		return err
	}
	n := strings.ToLower(name)
	if portPattern.MatchString(strings.ToUpper(n)) {
		return errors.New("invalid name: looks like a COM port")
	}
	if !namePattern.MatchString(n) {
		return errors.New("invalid name: want 1-16 chars of a-z 0-9 '-', starting with a letter")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for existing, bound := range r.names {
		if bound == p {
			delete(r.names, existing)
		}
	}
	r.names[n] = p
	return r.saveLocked()
}

// Unname removes a binding by name or port, returning the removed name.
func (r *Registry) Unname(target string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := strings.ToLower(target)
	if _, ok := r.names[n]; ok {
		delete(r.names, n)
		return n, r.saveLocked()
	}
	if p, err := NormalizePort(target); err == nil {
		for name, bound := range r.names {
			if bound == p {
				delete(r.names, name)
				return name, r.saveLocked()
			}
		}
	}
	return "", fmt.Errorf("no name for %q", target)
}

// Resolve maps a target - a name or a COM port, case-insensitive - to the
// canonical port name. Port literals always resolve (the caller decides
// whether that port is actually attached).
func (r *Registry) Resolve(target string) (string, bool) {
	if p, err := NormalizePort(target); err == nil {
		return p, true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.names[strings.ToLower(target)]
	return p, ok
}

// NameFor returns the name bound to port ("" when unnamed).
func (r *Registry) NameFor(port string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for n, p := range r.names {
		if p == port {
			return n
		}
	}
	return ""
}

// All returns a copy of every name->port binding.
func (r *Registry) All() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(r.names))
	for n, p := range r.names {
		out[n] = p
	}
	return out
}

func (r *Registry) saveLocked() error {
	if r.path == "" {
		return nil
	}
	data, err := json.Marshal(r.names)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(r.path, data, 0o644)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/light/ -run TestRegistry -v`
Expected: PASS (all 7 `TestRegistry*` tests).

- [ ] **Step 5: Gate**

Run: `go test -race ./... && go vet ./...`
Expected: all packages ok, vet silent.

- [ ] **Step 6: Commit**

```bash
git -C /home/dan/code/mutastic/.worktrees/pl81-multi-light add internal/light/names.go internal/light/names_test.go
git -C /home/dan/code/mutastic/.worktrees/pl81-multi-light commit -m "feat: persistent light name registry (light-names.json)"
```

---

### Task 2: Manager fleet hooks — `Connected()` and `PowerState()`

**Files:**
- Modify: `internal/light/manager.go` (append two methods; touch nothing else)
- Test: `internal/light/manager_test.go` (append one test)

**Interfaces:**
- Consumes: existing `Manager` internals (`m.mu`, `m.port`, `m.state.Status()` — `Status() (on bool, brightness int, temp byte, known bool)` at `state.go:105`).
- Produces:
  - `func (m *Manager) Connected() bool` — a serial port is currently attached.
  - `func (m *Manager) PowerState() (on, known bool)` — tracked power state; `known=false` until first echo/optimistic write.

- [ ] **Step 1: Write the failing test**

Append to `internal/light/manager_test.go`:

```go
func TestPowerStateAndConnected(t *testing.T) {
	setFastWrites(t)
	m := NewManager(testLogger(), "")
	if m.Connected() {
		t.Fatal("Connected = true with no port")
	}
	if on, known := m.PowerState(); on || known {
		t.Fatalf("PowerState = (%v, %v), want (false, false) before any state", on, known)
	}
	p := newFakePort()
	m.setPort(p)
	if !m.Connected() {
		t.Fatal("Connected = false with port set")
	}
	m.HandleCommand("brightness 40")
	if on, known := m.PowerState(); !on || !known {
		t.Fatalf("PowerState = (%v, %v), want (true, true) after brightness 40", on, known)
	}
	m.HandleCommand("off")
	if on, known := m.PowerState(); on || !known {
		t.Fatalf("PowerState = (%v, %v), want (false, true) after off", on, known)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/light/ -run TestPowerStateAndConnected`
Expected: FAIL — build error `m.Connected undefined` / `m.PowerState undefined`.

- [ ] **Step 3: Write the implementation**

Append to `internal/light/manager.go` (after `setPort`, around line 62):

```go
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/light/ -run TestPowerStateAndConnected -v`
Expected: PASS.

- [ ] **Step 5: Gate**

Run: `go test -race ./... && go vet ./...`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git -C /home/dan/code/mutastic/.worktrees/pl81-multi-light add internal/light/manager.go internal/light/manager_test.go
git -C /home/dan/code/mutastic/.worktrees/pl81-multi-light commit -m "feat: Manager.Connected and Manager.PowerState fleet hooks"
```

---

### Task 3: MultiManager session lifecycle — rescan, hot-plug, per-port state, migration

**Files:**
- Create: `internal/light/multi.go`
- Test: `internal/light/multi_test.go`

**Interfaces:**
- Consumes: `Manager` (`NewManager(logger *log.Logger, statePath string) *Manager`, `Run(ctx context.Context, open OpenFunc)`, `Present func() bool` field, `Connected() bool`), `Port`, `Registry` (Task 1), test seams `fakePort`/`newFakePort()`/`fastTimings(t)`/`waitFor(t, what, cond)`/`wakeBytes` (all in-package).
- Produces:
  - `type Enumerate func() ([]string, error)` — lists COM names of attached PL81s.
  - `type OpenPort func(name string) (Port, error)` — opens one specific port.
  - `func NewMultiManager(logger *log.Logger, stateDir string, reg *Registry, enumerate Enumerate, openPort OpenPort, present func(string) bool) *MultiManager`
  - `func (mm *MultiManager) Run(ctx context.Context)` — immediate rescan, then every `rescanInterval` (package var, 5 s).
  - Unexported (used by Task 4 and tests): `rescan(ctx)`, `stopAll()`, `statePath(port) string`, `portsLocked() []string`, `sortPorts([]string)`, `sessions map[string]*lightSession` with `lightSession{m *Manager; cancel context.CancelFunc; done chan struct{}}`.
  - Per-port state file: `<stateDir>/light-state-<COMx>.json`. Legacy migration: when a session starts for a port, the scan saw exactly ONE port, and no per-port file exists, `light-state.json` is renamed to the per-port file (a one-light setup keeps its remembered settings across the upgrade; multi-port upgrades fall back to defaults — documented in Task 8).
  - Log lines (E2E depends on these): `"light %s: starting session"`, `"light %s: port gone, stopping session"`, `"light: rescan: ports now [%s]"` (only when the set changes), `"light: rescan: %v (keeping current sessions)"`, `"light %s: migrated legacy light-state.json"`. Per-session Manager logs gain a `"<PORT> "` message prefix (e.g. `COM4 light: port opened`).

- [ ] **Step 1: Write the failing tests**

Create `internal/light/multi_test.go`:

```go
package light

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeFleet drives a MultiManager against fake serial ports: the
// enumerated port set is mutable mid-test (hot-plug), and open() hands out
// the current fakePort for that name.
type fakeFleet struct {
	mu    sync.Mutex
	ports map[string]*fakePort
	fail  bool // enumerate returns an error when set
}

func newFakeFleet(ports ...string) *fakeFleet {
	f := &fakeFleet{ports: map[string]*fakePort{}}
	for _, p := range ports {
		f.ports[p] = newFakePort()
	}
	return f
}

// set replaces the enumerated port set, keeping existing fakePorts for
// ports that survive.
func (f *fakeFleet) set(ports ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	next := map[string]*fakePort{}
	for _, p := range ports {
		if fp, ok := f.ports[p]; ok {
			next[p] = fp
		} else {
			next[p] = newFakePort()
		}
	}
	f.ports = next
}

func (f *fakeFleet) setFail(fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = fail
}

func (f *fakeFleet) enumerate() ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return nil, errors.New("enumerator glitch")
	}
	var names []string
	for p := range f.ports {
		names = append(names, p)
	}
	return names, nil
}

func (f *fakeFleet) open(name string) (Port, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fp, ok := f.ports[name]
	if !ok {
		return nil, errors.New("port gone")
	}
	return fp, nil
}

func (f *fakeFleet) port(name string) *fakePort {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ports[name]
}

// fastRescan shrinks the rescan ticker for hot-plug tests.
func fastRescan(t *testing.T) {
	t.Helper()
	old := rescanInterval
	rescanInterval = 5 * time.Millisecond
	t.Cleanup(func() { rescanInterval = old })
}

// newTestMulti builds a MultiManager over the fleet. stateDir may be ""
// (no persistence). The registry persists to <stateDir>/light-names.json.
func newTestMulti(t *testing.T, fleet *fakeFleet, stateDir string) (*MultiManager, context.Context) {
	t.Helper()
	regPath := ""
	if stateDir != "" {
		regPath = filepath.Join(stateDir, "light-names.json")
	}
	mm := NewMultiManager(testLogger(), stateDir, NewRegistry(regPath), fleet.enumerate, fleet.open, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		mm.stopAll()
	})
	return mm, ctx
}

func sessionManager(t *testing.T, mm *MultiManager, port string) *Manager {
	t.Helper()
	mm.mu.Lock()
	defer mm.mu.Unlock()
	s, ok := mm.sessions[port]
	if !ok {
		t.Fatalf("no session for %s", port)
	}
	return s.m
}

func waitConnected(t *testing.T, mm *MultiManager, ports ...string) {
	t.Helper()
	waitFor(t, "sessions connected", func() bool {
		mm.mu.Lock()
		defer mm.mu.Unlock()
		for _, p := range ports {
			s, ok := mm.sessions[p]
			if !ok || !s.m.Connected() {
				return false
			}
		}
		return true
	})
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatal(err)
	}
}

func TestRescanStartsSessionPerPort(t *testing.T) {
	fastTimings(t)
	fleet := newFakeFleet("COM4", "COM7")
	mm, ctx := newTestMulti(t, fleet, "")
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4", "COM7")
	for _, p := range []string{"COM4", "COM7"} {
		fp := fleet.port(p)
		waitFor(t, p+" woken", func() bool { return fp.writeCount() >= 1 })
		if !bytes.Equal(fp.write(0), wakeBytes) {
			t.Fatalf("%s first write = % x, want wake bytes", p, fp.write(0))
		}
	}
}

func TestRescanDiscoversHotPluggedLight(t *testing.T) {
	fastTimings(t)
	fastRescan(t)
	fleet := newFakeFleet("COM4")
	mm, ctx := newTestMulti(t, fleet, "")
	go mm.Run(ctx)
	waitConnected(t, mm, "COM4")

	fleet.set("COM4", "COM7") // plug in a second light, no restart
	waitConnected(t, mm, "COM4", "COM7")

	// The stable light must not be churned by rescans: exactly one wake.
	fp4 := fleet.port("COM4")
	wakes := 0
	for i := 0; i < fp4.writeCount(); i++ {
		if bytes.Equal(fp4.write(i), wakeBytes) {
			wakes++
		}
	}
	if wakes != 1 {
		t.Fatalf("COM4 woken %d times, want 1 (session churn)", wakes)
	}
}

func TestRescanStopsRemovedLight(t *testing.T) {
	fastTimings(t)
	fastRescan(t)
	fleet := newFakeFleet("COM4", "COM7")
	mm, ctx := newTestMulti(t, fleet, "")
	go mm.Run(ctx)
	waitConnected(t, mm, "COM4", "COM7")

	fleet.set("COM4") // unplug COM7
	waitFor(t, "COM7 torn down", func() bool {
		mm.mu.Lock()
		defer mm.mu.Unlock()
		_, ok := mm.sessions["COM7"]
		return !ok
	})
	waitConnected(t, mm, "COM4") // survivor untouched
}

func TestRescanSurvivesEnumerateError(t *testing.T) {
	fastTimings(t)
	fleet := newFakeFleet("COM4")
	mm, ctx := newTestMulti(t, fleet, "")
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4")
	fleet.setFail(true)
	mm.rescan(ctx) // fail open: keep current sessions
	waitConnected(t, mm, "COM4")
}

func TestPerPortStateFiles(t *testing.T) {
	fastTimings(t)
	dir := t.TempDir()
	fleet := newFakeFleet("COM4", "COM7")
	mm, ctx := newTestMulti(t, fleet, dir)
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4", "COM7")
	if got := sessionManager(t, mm, "COM4").HandleCommand("brightness 40"); got != "on 40% 4950K" {
		t.Fatalf("COM4 brightness = %q", got)
	}
	if got := sessionManager(t, mm, "COM7").HandleCommand("brightness 80"); got != "on 80% 4950K" {
		t.Fatalf("COM7 brightness = %q", got)
	}
	var got4, got7 struct {
		Brightness int `json:"brightness"`
	}
	readJSON(t, filepath.Join(dir, "light-state-COM4.json"), &got4)
	readJSON(t, filepath.Join(dir, "light-state-COM7.json"), &got7)
	if got4.Brightness != 40 || got7.Brightness != 80 {
		t.Fatalf("persisted brightness = %d/%d, want 40/80", got4.Brightness, got7.Brightness)
	}
}

func TestLegacyStateMigratesForSinglePort(t *testing.T) {
	fastTimings(t)
	dir := t.TempDir()
	legacy := filepath.Join(dir, "light-state.json")
	if err := os.WriteFile(legacy, []byte(`{"on":true,"brightness":30,"temp_byte":0}`), 0o644); err != nil {
		t.Fatal(err)
	}
	fleet := newFakeFleet("COM4")
	mm, ctx := newTestMulti(t, fleet, dir)
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4")
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy file should be gone (stat err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "light-state-COM4.json")); err != nil {
		t.Fatalf("per-port state file missing: %v", err)
	}
	// The restore target carried over: "on" restores 30% 2900K.
	if got := sessionManager(t, mm, "COM4").HandleCommand("on"); got != "on 30% 2900K" {
		t.Fatalf("on after migration = %q, want %q", got, "on 30% 2900K")
	}
}

func TestLegacyStateKeptWhenTwoPortsPresent(t *testing.T) {
	fastTimings(t)
	dir := t.TempDir()
	legacy := filepath.Join(dir, "light-state.json")
	if err := os.WriteFile(legacy, []byte(`{"on":true,"brightness":30,"temp_byte":0}`), 0o644); err != nil {
		t.Fatal(err)
	}
	fleet := newFakeFleet("COM4", "COM7")
	mm, ctx := newTestMulti(t, fleet, dir)
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4", "COM7")
	// Ambiguous which port owned the legacy state: nobody inherits it.
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("legacy file should be untouched: %v", err)
	}
	if got := sessionManager(t, mm, "COM4").HandleCommand("on"); got != "on 100% 4950K" {
		t.Fatalf("on = %q, want defaults %q", got, "on 100% 4950K")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/light/`
Expected: FAIL — build error `undefined: NewMultiManager` (and `rescanInterval`, `MultiManager`).

- [ ] **Step 3: Write the implementation**

Create `internal/light/multi.go`:

```go
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
	present   func(name string) bool // per-port USB presence; nil disables

	mu       sync.Mutex
	sessions map[string]*lightSession // key: canonical port name ("COM4")
}

type lightSession struct {
	m      *Manager
	cancel context.CancelFunc
	done   chan struct{}
}

// NewMultiManager wires the discovery/open callbacks; Run starts the
// rescan loop.
func NewMultiManager(logger *log.Logger, stateDir string, reg *Registry, enumerate Enumerate, openPort OpenPort, present func(string) bool) *MultiManager {
	return &MultiManager{
		logger:    logger,
		reg:       reg,
		stateDir:  stateDir,
		enumerate: enumerate,
		openPort:  openPort,
		present:   present,
		sessions:  map[string]*lightSession{},
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
// ports get a session (with one-time legacy state migration), vanished
// ports are torn down. Enumeration errors keep the current set (fail
// open - never kill sessions on an enumerator glitch).
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
	mm.mu.Lock()
	defer mm.mu.Unlock()
	seen := map[string]bool{}
	changed := false
	for _, port := range ports {
		seen[port] = true
		if _, ok := mm.sessions[port]; !ok {
			mm.startSessionLocked(ctx, port, len(ports))
			changed = true
		}
	}
	for port, s := range mm.sessions {
		if seen[port] {
			continue
		}
		mm.logger.Printf("light %s: port gone, stopping session", port)
		s.cancel()
		<-s.done
		delete(mm.sessions, port)
		changed = true
	}
	if changed {
		mm.logger.Printf("light: rescan: ports now [%s]", strings.Join(mm.portsLocked(), " "))
	}
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
	if mm.present != nil {
		p := port
		m.Present = func() bool { return mm.present(p) }
	}
	sessCtx, cancel := context.WithCancel(ctx)
	s := &lightSession{m: m, cancel: cancel, done: make(chan struct{})}
	mm.sessions[port] = s
	mm.logger.Printf("light %s: starting session", port)
	go func() {
		defer close(s.done)
		p := port
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

// stopAll cancels every session and waits for each to exit.
func (mm *MultiManager) stopAll() {
	mm.mu.Lock()
	defer mm.mu.Unlock()
	for port, s := range mm.sessions {
		s.cancel()
		<-s.done
		delete(mm.sessions, port)
	}
}

// fmt is used by Task 4 (command surface); referenced here so the import
// stays valid if tasks land separately.
var _ = fmt.Sprintf
```

Note: the `var _ = fmt.Sprintf` line is scaffolding for this task only; Task 4 removes it when `fmt` gains real uses.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/light/ -run 'TestRescan|TestPerPortStateFiles|TestLegacyState' -v`
Expected: PASS (7 tests).

- [ ] **Step 5: Gate**

Run: `go test -race ./... && go vet ./...`
Expected: clean. The race detector matters here: rescan vs session goroutines.

- [ ] **Step 6: Commit**

```bash
git -C /home/dan/code/mutastic/.worktrees/pl81-multi-light add internal/light/multi.go internal/light/multi_test.go
git -C /home/dan/code/mutastic/.worktrees/pl81-multi-light commit -m "feat: MultiManager with 5s rescan, per-port sessions and state migration"
```

---

### Task 4: MultiManager command surface — addressing, naming, collective control

**Files:**
- Modify: `internal/light/multi.go` (append command handling; delete the `var _ = fmt.Sprintf` scaffold)
- Test: `internal/light/multi_test.go` (append tests)

**Interfaces:**
- Consumes: Task 1 `Registry`, Task 2 `PowerState`/`Connected`, Task 3 `MultiManager` internals, existing `Manager.HandleCommand` verb table (`status|on|off|toggle|brightness N|temp K|preset name`).
- Produces: `func (mm *MultiManager) HandleCommand(cmd string) string` — satisfies `daemon.CommandHandler` (`internal/daemon/daemon.go:28`). Input is the daemon-trimmed remainder after the `light` prefix. Grammar and exact reply formats (contract for Tasks 5/6/8/9):
  - `@<name|COMx> <verb...>` → single light, reply is the bare `Manager` reply (`on 30% 2900K`). Unresolved target → `error: unknown light "<target>" (known: COM4=desk, COM7)`; resolved port with no session → `error: light COM9 not connected (known: ...)`; `known:` is `none` when nothing is known.
  - `name <COMx> <name>` → `named COM4 desk` (naming a not-yet-attached port is allowed).
  - `unname <name|COMx>` → `unnamed desk`.
  - `list` → one line per known light (sessions ∪ named ports), port order: `COM4 desk connected on 30% 2900K` / `COM9 spare disconnected` / name placeholder `-`; zero known → `no lights known`.
  - Bare verb → fan out to every tracked session in port order, one line per light: `COM4 desk: on 30% 2900K\nCOM7: off`. Zero sessions → `error: no light`.
  - Bare `toggle` → fleet decision: if ANY light reports on, send `off` to all; otherwise send `on` to all (each restores its own persisted look). Unknown counts as off.

- [ ] **Step 1: Write the failing tests**

Append to `internal/light/multi_test.go`:

```go
func TestMultiTargetedAddressing(t *testing.T) {
	fastTimings(t)
	fleet := newFakeFleet("COM4")
	mm, ctx := newTestMulti(t, fleet, t.TempDir())
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4")

	if got := mm.HandleCommand("name COM4 desk"); got != "named COM4 desk" {
		t.Fatalf("name reply = %q", got)
	}
	if got := mm.HandleCommand("@desk brightness 40"); got != "on 40% 4950K" {
		t.Fatalf("@desk brightness = %q", got)
	}
	for _, target := range []string{"@COM4 status", "@com4 status", "@DESK status"} {
		if got := mm.HandleCommand(target); got != "on 40% 4950K" {
			t.Fatalf("%q = %q, want on 40%% 4950K", target, got)
		}
	}
	got := mm.HandleCommand("@nope status")
	want := `error: unknown light "nope" (known: COM4=desk)`
	if got != want {
		t.Fatalf("@nope = %q, want %q", got, want)
	}
	got = mm.HandleCommand("@COM9 status")
	want = "error: light COM9 not connected (known: COM4=desk)"
	if got != want {
		t.Fatalf("@COM9 = %q, want %q", got, want)
	}
}

func TestMultiBareFansOutInPortOrder(t *testing.T) {
	fastTimings(t)
	fleet := newFakeFleet("COM7", "COM12", "COM4")
	mm, ctx := newTestMulti(t, fleet, "")
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4", "COM7", "COM12")
	got := mm.HandleCommand("brightness 50")
	want := "COM4: on 50% 4950K\nCOM7: on 50% 4950K\nCOM12: on 50% 4950K"
	if got != want {
		t.Fatalf("fan-out = %q, want %q", got, want)
	}
	for _, p := range []string{"COM4", "COM7", "COM12"} {
		if fleet.port(p).writeCount() < 2 { // wake + CCT frame
			t.Fatalf("%s got no frame after wake", p)
		}
	}
}

func TestMultiToggleAnyOnTurnsAllOff(t *testing.T) {
	fastTimings(t)
	fleet := newFakeFleet("COM4", "COM7")
	mm, ctx := newTestMulti(t, fleet, "")
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4", "COM7")
	// COM4 on; COM7 stays unknown (counts as off).
	if got := sessionManager(t, mm, "COM4").HandleCommand("brightness 60"); got != "on 60% 4950K" {
		t.Fatalf("setup: %q", got)
	}
	got := mm.HandleCommand("toggle")
	want := "COM4: off\nCOM7: off"
	if got != want {
		t.Fatalf("toggle = %q, want %q", got, want)
	}
}

func TestMultiToggleAllOffTurnsAllOnRestoringLooks(t *testing.T) {
	fastTimings(t)
	fleet := newFakeFleet("COM4", "COM7")
	mm, ctx := newTestMulti(t, fleet, "")
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4", "COM7")
	sessionManager(t, mm, "COM4").HandleCommand("brightness 30")
	sessionManager(t, mm, "COM4").HandleCommand("temp 2900")
	sessionManager(t, mm, "COM7").HandleCommand("brightness 80")
	mm.HandleCommand("off")
	got := mm.HandleCommand("toggle")
	want := "COM4: on 30% 2900K\nCOM7: on 80% 4950K"
	if got != want {
		t.Fatalf("toggle = %q, want %q", got, want)
	}
}

func TestMultiListAndNaming(t *testing.T) {
	fastTimings(t)
	dir := t.TempDir()
	fleet := newFakeFleet("COM4")
	mm, ctx := newTestMulti(t, fleet, dir)
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4")
	sessionManager(t, mm, "COM4").HandleCommand("brightness 30")

	mm.HandleCommand("name COM4 desk")
	mm.HandleCommand("name COM9 spare") // naming a not-yet-attached port is allowed
	got := mm.HandleCommand("list")
	want := "COM4 desk connected on 30% 4950K\nCOM9 spare disconnected"
	if got != want {
		t.Fatalf("list = %q, want %q", got, want)
	}

	if got := mm.HandleCommand("unname spare"); got != "unnamed spare" {
		t.Fatalf("unname = %q", got)
	}
	if got := mm.HandleCommand("list"); got != "COM4 desk connected on 30% 4950K" {
		t.Fatalf("list after unname = %q", got)
	}

	// Names persist: a fresh Registry over the same file still resolves.
	reg2 := NewRegistry(filepath.Join(dir, "light-names.json"))
	if p, ok := reg2.Resolve("desk"); !ok || p != "COM4" {
		t.Fatalf("persisted Resolve(desk) = %q, %v; want COM4, true", p, ok)
	}
}

func TestMultiNoLights(t *testing.T) {
	fleet := newFakeFleet()
	mm, ctx := newTestMulti(t, fleet, "")
	mm.rescan(ctx)
	if got := mm.HandleCommand("toggle"); got != "error: no light" {
		t.Fatalf("toggle = %q, want error: no light", got)
	}
	if got := mm.HandleCommand("list"); got != "no lights known" {
		t.Fatalf("list = %q, want no lights known", got)
	}
	got := mm.HandleCommand("@desk status")
	want := `error: unknown light "desk" (known: none)`
	if got != want {
		t.Fatalf("@desk = %q, want %q", got, want)
	}
}

func TestMultiUsageErrors(t *testing.T) {
	fleet := newFakeFleet()
	mm, ctx := newTestMulti(t, fleet, "")
	mm.rescan(ctx)
	cases := map[string]string{
		"@desk":         "error: usage: light@<name|COMx> <command>",
		"@ toggle":      "error: usage: light@<name|COMx> <command>",
		"name COM4":     "error: usage: light name <COMx> <name>",
		"name COM4 a b": "error: usage: light name <COMx> <name>",
		"name COM4 no!": "error: invalid name: want 1-16 chars of a-z 0-9 '-', starting with a letter",
		"unname":        "error: usage: light unname <name|COMx>",
		"":              "error: unknown light command",
		"list extra":    "error: unknown light command",
	}
	for cmd, want := range cases {
		if got := mm.HandleCommand(cmd); got != want {
			t.Errorf("HandleCommand(%q) = %q, want %q", cmd, got, want)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/light/`
Expected: FAIL — build error `mm.HandleCommand undefined`.

- [ ] **Step 3: Write the implementation**

In `internal/light/multi.go`, delete the trailing scaffold line `var _ = fmt.Sprintf` and append:

```go
// HandleCommand implements the daemon's CommandHandler for the whole
// fleet. The daemon strips the "light" prefix and trims, so cmd looks
// like "toggle", "@desk brightness 40", "name COM4 desk", "unname desk",
// or "list".
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
	return s.m.HandleCommand(rest)
}

// handleAll fans a bare verb out to every tracked light, one reply line
// per light in port order. "toggle" is fleet-level: if ANY light is on,
// all go off; otherwise all go on (each restoring its own persisted
// look; unknown counts as off).
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
		for _, m := range managers {
			if on, _ := m.PowerState(); on {
				cmd = "off"
				break
			}
		}
	}
	lines := make([]string, len(ports))
	for i, p := range ports {
		lines[i] = mm.label(p) + ": " + managers[p].HandleCommand(cmd)
	}
	return strings.Join(lines, "\n")
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
		if m, ok := managers[p]; ok && m.Connected() {
			lines[i] = fmt.Sprintf("%s %s connected %s", p, name, m.HandleCommand("status"))
		} else {
			lines[i] = fmt.Sprintf("%s %s disconnected", p, name)
		}
	}
	return strings.Join(lines, "\n")
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/light/ -run TestMulti -v`
Expected: PASS (7 `TestMulti*` tests).

- [ ] **Step 5: Gate**

Run: `go test -race ./... && go vet ./...`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git -C /home/dan/code/mutastic/.worktrees/pl81-multi-light add internal/light/multi.go internal/light/multi_test.go
git -C /home/dan/code/mutastic/.worktrees/pl81-multi-light commit -m "feat: fleet command surface - @addressing, naming, list, collective toggle"
```

---

### Task 5: Daemon routes `light@...`

**Files:**
- Modify: `internal/daemon/daemon.go:76` (one condition)
- Test: `internal/daemon/daemon_test.go` (append one test)

**Interfaces:**
- Consumes: `CommandHandler` interface (`daemon.go:28`), existing `fakeLightHandler` double (`daemon_test.go:127-135`), `New(testLogger())` construction used by the sibling routing tests.
- Produces: the daemon forwards `light@desk toggle` to the light handler as `@desk toggle`. `lightning` etc. remain unrouted (existing guard test keeps passing).

- [ ] **Step 1: Write the failing test**

Append to `internal/daemon/daemon_test.go` (mirror the construction of `TestHandleCommandRoutesLightPrefix` at `daemon_test.go:138` — it builds the daemon and sets the `Light` field to a `fakeLightHandler`; if the construction below differs from that sibling test, copy the sibling's exact construction and keep the assertion lines as written):

```go
func TestHandleCommandRoutesLightAtPrefix(t *testing.T) {
	f := &fakeLightHandler{reply: "on 40% 4950K"}
	d := New(testLogger())
	d.Light = f
	if got := d.HandleCommand("light@desk toggle"); got != "on 40% 4950K" {
		t.Fatalf("reply = %q, want pass-through of handler reply", got)
	}
	if len(f.got) != 1 || f.got[0] != "@desk toggle" {
		t.Fatalf("handler received %v, want [\"@desk toggle\"]", f.got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/daemon/ -run TestHandleCommandRoutesLightAtPrefix`
Expected: FAIL — `reply = "error: unknown command", want pass-through of handler reply` (the `@` byte fails the current `rest == "" || rest[0] == ' '` condition).

- [ ] **Step 3: Write the implementation**

In `internal/daemon/daemon.go:76`, change the routing condition to:

```go
	if rest, ok := strings.CutPrefix(cmd, "light"); ok && (rest == "" || rest[0] == ' ' || rest[0] == '@') {
```

(The body is unchanged: nil-handler guard, then `d.Light.HandleCommand(strings.TrimSpace(rest))` — for `light@desk toggle` the handler receives `@desk toggle`.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/daemon/ -v`
Expected: PASS, including the existing `TestHandleCommandDoesNotRouteLightPrefixWords` (`lightning` still unrouted) and `TestServeUDPSurvivesTransientErrors`.

- [ ] **Step 5: Gate**

Run: `go test -race ./... && go vet ./...`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git -C /home/dan/code/mutastic/.worktrees/pl81-multi-light add internal/daemon/daemon.go internal/daemon/daemon_test.go
git -C /home/dan/code/mutastic/.worktrees/pl81-multi-light commit -m "feat: daemon routes light@<target> commands to the light handler"
```

---

### Task 6: CLI dispatch — `light@` case, testable arg mapping, 2048-byte replies

**Files:**
- Modify: `main.go` (extract `clientCommand`, rewrite `main()` switch, grow `runClient` buffer, update `usage()`)
- Test: `main_test.go` (append two tests)

**Interfaces:**
- Consumes: existing `runClient(cmd, addr string, timeout time.Duration, out io.Writer) int` (`main.go:56`), `udpAddr` const.
- Produces: `func clientCommand(args []string) (cmd string, timeout time.Duration, ok bool)` — argv (without program name) → UDP command + timeout; `ok=false` means bad usage. Mic verbs: 1 s; `light`/`light@...`: joined verbatim, 2 s. `runClient` reads replies into a 2048-byte buffer (multi-line `list`/fan-out replies).

- [ ] **Step 1: Write the failing tests**

Append to `main_test.go`:

```go
func TestClientCommand(t *testing.T) {
	cases := []struct {
		args    []string
		cmd     string
		timeout time.Duration
		ok      bool
	}{
		{[]string{"status"}, "status", time.Second, true},
		{[]string{"toggle"}, "toggle", time.Second, true},
		{[]string{"mute"}, "mute", time.Second, true},
		{[]string{"unmute"}, "unmute", time.Second, true},
		{[]string{"light", "toggle"}, "light toggle", 2 * time.Second, true},
		{[]string{"light", "list"}, "light list", 2 * time.Second, true},
		{[]string{"light", "name", "COM4", "desk"}, "light name COM4 desk", 2 * time.Second, true},
		{[]string{"light@desk", "toggle"}, "light@desk toggle", 2 * time.Second, true},
		{[]string{"light@COM4", "brightness", "30"}, "light@COM4 brightness 30", 2 * time.Second, true},
		{[]string{"light"}, "", 0, false},
		{[]string{"light@desk"}, "", 0, false},
		{[]string{"frobnicate"}, "", 0, false},
		{nil, "", 0, false},
	}
	for _, c := range cases {
		cmd, timeout, ok := clientCommand(c.args)
		if cmd != c.cmd || timeout != c.timeout || ok != c.ok {
			t.Errorf("clientCommand(%v) = (%q, %v, %v), want (%q, %v, %v)",
				c.args, cmd, timeout, ok, c.cmd, c.timeout, c.ok)
		}
	}
}

func TestRunClientPrintsLargeReply(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	reply := strings.TrimSpace(strings.Repeat("COM4 desk connected on 30% 2900K\n", 12)) // ~390 bytes, > the old 256
	go func() {
		buf := make([]byte, 64)
		_, addr, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		pc.WriteTo([]byte(reply), addr)
	}()
	var out bytes.Buffer
	code := runClient("light list", pc.LocalAddr().String(), time.Second, &out)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if got := strings.TrimSpace(out.String()); got != reply {
		t.Fatalf("reply truncated: got %d bytes, want %d", len(got), len(reply))
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test . -run 'TestClientCommand|TestRunClientPrintsLargeReply'`
Expected: FAIL — build error `undefined: clientCommand`. (After Step 3's `clientCommand` exists but before the buffer change, `TestRunClientPrintsLargeReply` fails with `reply truncated: got 256 bytes...` — run the step-3 edits in the order given and re-run to watch both failure modes if desired.)

- [ ] **Step 3: Write the implementation**

In `main.go`:

1. Replace the body of `main()` (the `switch os.Args[1]` at `main.go:26-46`) and add `clientCommand`:

```go
func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	if os.Args[1] == "daemon" {
		os.Exit(runDaemon())
	}
	cmd, timeout, ok := clientCommand(os.Args[1:])
	if !ok {
		usage()
		os.Exit(2)
	}
	os.Exit(runClient(cmd, udpAddr, timeout, os.Stdout))
}

// clientCommand maps argv (without the program name) to the UDP command
// string and timeout. ok=false means bad usage. Light commands are a dumb
// verbatim pass-through - the daemon owns the grammar.
func clientCommand(args []string) (cmd string, timeout time.Duration, ok bool) {
	if len(args) == 0 {
		return "", 0, false
	}
	switch {
	case args[0] == "toggle" || args[0] == "mute" || args[0] == "unmute" || args[0] == "status":
		return args[0], time.Second, true
	case args[0] == "light" || strings.HasPrefix(args[0], "light@"):
		if len(args) < 2 {
			return "", 0, false
		}
		return strings.Join(args, " "), 2 * time.Second, true
	}
	return "", 0, false
}
```

(`runDaemon` and its exit-code handling stay exactly as they are today — only the dispatch moved.)

2. In `runClient` (`main.go:68`), change `buf := make([]byte, 256)` to:

```go
	buf := make([]byte, 2048) // multi-light list/fan-out replies exceed 256 bytes
```

3. Replace `usage()` (`main.go:48-52`) with:

```go
func usage() {
	fmt.Fprintln(os.Stderr, "usage: mutastic daemon | toggle | mute | unmute | status")
	fmt.Fprintln(os.Stderr, "       mutastic light toggle|on|off|status|list  (bare light commands act on ALL lights)")
	fmt.Fprintln(os.Stderr, "       mutastic light brightness <0-100> | temp <2900-7000> | preset <cold|sunlight|afternoon|sunset|candle>")
	fmt.Fprintln(os.Stderr, "       mutastic light name <COMx> <name> | unname <name|COMx>")
	fmt.Fprintln(os.Stderr, "       mutastic light@<name|COMx> <command>  (one light)")
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test . -v`
Expected: PASS — the two new tests plus the four existing `TestRunClient*` tests (including `TestRunClientPassesMultiWordCommandVerbatim`).

- [ ] **Step 5: Gate**

Run: `go test -race ./... && go vet ./...`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git -C /home/dan/code/mutastic/.worktrees/pl81-multi-light add main.go main_test.go
git -C /home/dan/code/mutastic/.worktrees/pl81-multi-light commit -m "feat: CLI light@ addressing, testable dispatch, 2048-byte reply buffer"
```

---

### Task 7: Windows discovery refactor + daemon wiring

**Files:**
- Modify: `light_windows.go` (replace `openPL81`/`pl81Present` with `enumeratePL81Ports`/`openPL81Port`/`pl81PortPresent`)
- Modify: `light_other.go` (mirror the three signatures)
- Modify: `main.go` (replace `lightStatePath()` with `lightStateDir()`; rewire `runDaemon`'s light block, `main.go:100-105`)

**Interfaces:**
- Consumes: Task 3/4 `light.NewMultiManager` + `light.NewRegistry`, `go.bug.st/serial` + `enumerator` (already imported by `light_windows.go`).
- Produces:
  - `func enumeratePL81Ports() ([]string, error)` — matches `light.Enumerate`.
  - `func openPL81Port(name string) (light.Port, error)` — matches `light.OpenPort`.
  - `func pl81PortPresent(name string) bool` — per-port presence for `Manager.Present`.
  - `func lightStateDir() string` — `%LOCALAPPDATA%\mutastic` (or `""` to disable persistence).

**TDD note:** `light_windows.go` is build-tagged `windows` and cannot be unit-tested from WSL; `light_other.go` stubs are exercised by the existing Linux test run. The verification gates for this task are the full Linux test suite plus the Windows cross-compile (`./build.sh`); behavior is proven live in Task 9. All fleet logic was already TDD'd in Tasks 3-4 behind the injected function types — this task is pure wiring.

- [ ] **Step 1: Baseline compile check**

Run: `cd /home/dan/code/mutastic/.worktrees/pl81-multi-light && ./build.sh && ls -l bin/mutastic.exe`
Expected: builds `bin/mutastic.exe` (this is the pre-change baseline).

- [ ] **Step 2: Rewrite `light_windows.go`**

Replace the functions `openPL81` and `pl81Present` with the three functions below (keep the file header, build tag, `pl81VID`/`pl81PID` consts, and the `serialPort` adapter exactly as they are; drop the `log` and `errors` imports if they become unused):

```go
// enumeratePL81Ports lists the COM name of every CH340 bridge currently
// attached (VID 1A86 PID 7523). Two identical panels are distinguished
// ONLY by COM name - CH340s expose no USB serial number - so the COM port
// is a light's identity (stable per physical USB jack; see
// docs/pl81-pro-serial-protocol.md).
func enumeratePL81Ports() ([]string, error) {
	ports, err := enumerator.GetDetailedPortsList()
	if err != nil {
		return nil, err
	}
	var names []string
	for _, p := range ports {
		if p.IsUSB && strings.EqualFold(p.VID, pl81VID) && strings.EqualFold(p.PID, pl81PID) {
			names = append(names, p.Name)
		}
	}
	return names, nil
}

// openPL81Port opens one specific PL81 serial port at 115200 8N1 with
// both buffers flushed. The wake sequence is the session's job, not this
// function's.
func openPL81Port(name string) (light.Port, error) {
	mode := &serial.Mode{
		BaudRate: 115200,
		DataBits: 8,
		Parity:   serial.NoParity,
		StopBits: serial.OneStopBit,
		// The proven 2026-08-08 probe ran on .NET SerialPort defaults: DTR
		// and RTS DEASSERTED. go.bug.st's default asserts both, and CH340
		// boards are often line-state sensitive - replicate the proven
		// configuration explicitly; trust neither stack's default.
		InitialStatusBits: &serial.ModemOutputBits{RTS: false, DTR: false},
	}
	port, err := serial.Open(name, mode)
	if err != nil {
		// Typically "access denied" if something else holds the port
		// exclusively (e.g. NEEWER Control Center).
		return nil, fmt.Errorf("open %s: %w", name, err)
	}
	if err := port.ResetInputBuffer(); err != nil {
		port.Close()
		return nil, err
	}
	if err := port.ResetOutputBuffer(); err != nil {
		port.Close()
		return nil, err
	}
	// Fix the poll timeout exactly once, here, before the port is shared:
	// v1.8.0 opens in NoTimeout mode (a Read would block forever), and
	// re-issuing SetCommTimeouts per read can race an in-flight Write.
	if err := port.SetReadTimeout(time.Second); err != nil {
		port.Close()
		return nil, err
	}
	return serialPort{port}, nil
}

// pl81PortPresent reports whether the specific COM port is still
// enumerated as a PL81. The session loop uses it as its liveness fallback
// during long read silences. Enumeration failures count as present (fail
// open - never kill a session on an enumerator glitch).
func pl81PortPresent(name string) bool {
	ports, err := enumeratePL81Ports()
	if err != nil {
		return true
	}
	for _, p := range ports {
		if strings.EqualFold(p, name) {
			return true
		}
	}
	return false
}
```

(Note the per-candidate `light: serial port: ...` log line goes away with `openPL81` — the MultiManager's `light: rescan: ports now [...]` line replaces it as the discovery diagnostic. Task 8 updates the README troubleshooting text accordingly.)

- [ ] **Step 3: Rewrite `light_other.go`**

Replace the file contents with:

```go
//go:build !windows

package main

import (
	"errors"

	"mutastic/internal/light"
)

// The daemon only supports Windows; these stubs keep cross-platform
// builds and tests compiling. enumeratePL81Ports erroring means the
// rescan loop fails open with zero sessions.
func enumeratePL81Ports() ([]string, error) {
	return nil, errors.New("the mutastic daemon only supports Windows")
}

func openPL81Port(_ string) (light.Port, error) {
	return nil, errors.New("the mutastic daemon only supports Windows")
}

// pl81PortPresent is never consulted off-Windows (no session can start).
func pl81PortPresent(_ string) bool { return false }
```

- [ ] **Step 4: Rewire `main.go`**

1. Replace `lightStatePath()` (`main.go:131-140`) with:

```go
// lightStateDir returns %LOCALAPPDATA%\mutastic (the same directory as
// mutastic.log); per-light state files and the name registry live here.
// An empty string disables persistence rather than failing the daemon.
func lightStateDir() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "mutastic")
}
```

2. In `runDaemon`, replace the light wiring block (currently `main.go:100-105`):

```go
	open := func() (daemon.Device, error) { return openYetiX(logger) }
	ctx := context.Background()
	lm := light.NewManager(logger, lightStatePath())
	lm.Present = pl81Present
	go lm.Run(ctx, func() (light.Port, error) { return openPL81(logger) })
	daemon.Run(ctx, open, lm, pc, logger)
```

with:

```go
	open := func() (daemon.Device, error) { return openYetiX(logger) }
	ctx := context.Background()
	stateDir := lightStateDir()
	namesPath := ""
	if stateDir != "" {
		namesPath = filepath.Join(stateDir, "light-names.json")
	}
	reg := light.NewRegistry(namesPath)
	lights := light.NewMultiManager(logger, stateDir, reg, enumeratePL81Ports, openPL81Port, pl81PortPresent)
	go lights.Run(ctx)
	daemon.Run(ctx, open, lights, pc, logger)
```

(Everything before/after the block — including how `daemon.Run`'s return feeds `runDaemon`'s exit path — stays byte-identical.)

- [ ] **Step 5: Gate (Linux tests + vet)**

Run: `go test -race ./... && go vet ./...`
Expected: clean — the Linux build now compiles `light_other.go`'s new stubs into `main`.

- [ ] **Step 6: Gate (Windows cross-compile)**

Run: `./build.sh && ls -l bin/mutastic.exe`
Expected: builds cleanly — this is the only compile check that sees `light_windows.go`.

- [ ] **Step 7: Commit**

```bash
git -C /home/dan/code/mutastic/.worktrees/pl81-multi-light add light_windows.go light_other.go main.go
git -C /home/dan/code/mutastic/.worktrees/pl81-multi-light commit -m "feat: per-port PL81 discovery and MultiManager daemon wiring"
```

---

### Task 8: Documentation — README, protocol doc, AHK verification

**Files:**
- Modify: `README.md`
- Modify: `docs/pl81-pro-serial-protocol.md`
- Verify (NO edit): `ahk/MuteAllMeetings.ahk`

**Interfaces:**
- Consumes: reply formats and command grammar from Tasks 4/6 (copy them exactly — docs are contract).
- Produces: user-facing docs matching the shipped behavior.

- [ ] **Step 1: Verify the AHK pedal needs no change**

Run: `grep -a -A2 'F13::' /home/dan/code/mutastic/.worktrees/pl81-multi-light/ahk/MuteAllMeetings.ahk`
Expected output (CRLF file, `-a` guards against binary detection):

```
F13::
Run, "%A_ScriptDir%\mutastic.exe" light toggle, %A_ScriptDir%, Hide UseErrorLevel
return
```

`light toggle` is now the collective toggle — the pedal toggles ALL lights with zero AHK changes. Do NOT edit this file (UTF-8 BOM + CRLF; byte-sensitive).

- [ ] **Step 2: Update README.md**

Make these edits (read the current file first; anchors below are pre-task line numbers):

1. In the daemon bullet's light half (`README.md:27-30`), replace:

```markdown
  Also owns the NEEWER PL81 PRO light (CH340 serial, VID 1A86 PID 7523,
  115200 8N1) with its own independent reconnect loop, tracking the light's
  true state from its echo/broadcast frames and persisting the last look to
  `%LOCALAPPDATA%\mutastic\light-state.json`.
```

with:

```markdown
  Also owns every attached NEEWER PL81 PRO light (CH340 serial, VID 1A86
  PID 7523, 115200 8N1): a rescan every 5 s discovers newly plugged-in
  lights and tears down removed ones (no restart needed), with one
  independent reconnect loop per light, tracking each light's true state
  from its echo/broadcast frames and persisting each last look to
  `%LOCALAPPDATA%\mutastic\light-state-<COMx>.json`.
```

2. Replace the light-commands bullet (`README.md:35-46`) with:

```markdown
- **Light commands** — every attached PL81 PRO is discovered automatically.
  Bare `mutastic light <cmd>` acts on ALL lights, one reply line per light
  (`COM4 desk: on 30% 2900K`); `mutastic light@<name|COMx> <cmd>` targets
  one light and replies bare (`on 30% 2900K`).

  | Command | Effect |
  |---|---|
  | `mutastic light toggle` | if ANY light is on, ALL turn off; otherwise ALL turn on, each restoring its own last look (this is the F13 pedal behavior) |
  | `mutastic light on \| off \| status` | power / status, all lights |
  | `mutastic light brightness <0-100>` | set brightness, all lights |
  | `mutastic light temp <2900-7000>` | set color temperature, all lights |
  | `mutastic light preset <cold\|sunlight\|afternoon\|sunset\|candle>` | apply a preset, all lights |
  | `mutastic light list` | every known light: port, name (`-` if none), connected/disconnected, state |
  | `mutastic light name <COMx> <name>` | give a light a persistent name (case-insensitive; reassigning moves it) |
  | `mutastic light unname <name\|COMx>` | clear a name |
  | `mutastic light@desk toggle` | any command above, one light (by name or COM port) |

  Per-light replies: `on 64% 4950K`, `off`, `unknown`, or `error: <reason>`
  (same exit codes as the mic commands). Notes: OFF is brightness 0 (the
  panel has no working power command); `on`/`toggle` restore each light's
  last non-zero brightness and temperature (default 100% / 5000 K); setting
  `temp` while a light is off turns it on at the restored brightness;
  temperatures are quantized to the panel's 19 hardware steps (~228 K), so
  `temp 5000` reads back as `4950K`; `status` is `unknown` after a daemon
  restart until a light first echoes or its knob is touched (the hardware
  has no query command). Names persist in
  `%LOCALAPPDATA%\mutastic\light-names.json`; per-light state in
  `light-state-<COMx>.json`. A light's identity is its COM port — CH340
  bridges expose no USB serial number; the COM assignment is stable per
  physical USB jack (moving a light to another jack gives it a new COM
  port, i.e. a new identity). On first multi-light startup with exactly one
  light attached, the old single-light `light-state.json` is migrated to
  that light's per-port file; with several lights attached the old file is
  ambiguous and defaults apply.
```

3. In `## Troubleshooting` (`README.md:81-` region): the old diagnostic string `light: serial port:` no longer exists. Replace any mention of it with the new discovery diagnostics: `light: rescan: ports now [COM4]`, `light COM4: starting session`, and per-light prefixed session lines like `COM4 light: port opened`. Keep the `light: session ended` guidance, noting it is now prefixed per light (`COM4 light: session ended: ...`).

4. In `## Deploy (on Windows)` (`README.md:62-`), append this note:

```markdown
> **Deploying from WSL:** run `deploy.cmd` via `cmd.exe` with output
> redirected to a file — the `start` of the daemon inherits the interop
> console handle, so the invocation may never return to bash even though
> the deploy succeeded. Treat a transcript ending in `Deploy complete.`
> (plus fresh file timestamps and both processes running) as success, not
> the exit code:
>
> ```bash
> timeout 90 cmd.exe /c '\\wsl.localhost\Ubuntu\...\deploy\deploy.cmd' '\\wsl.localhost\Ubuntu\...' > /tmp/deploy.log 2>&1
> cat /tmp/deploy.log   # must end with: Deploy complete.
> ```
>
> The UNC path must be single-quoted (double quotes collapse `\\` to `\`).
```

- [ ] **Step 3: Update docs/pl81-pro-serial-protocol.md**

1. In `## Transport` (after the existing "Do NOT hardcode COM4 — enumerate by VID/PID" bullet at line 14), append:

```markdown
- With multiple panels, VID/PID no longer discriminates and the CH340
  exposes **no USB serial number** — the **COM port name is the light's
  identity** (this is what `light name`/`light-state-<COMx>.json` key on).
  Windows keeps the COM assignment stable per physical USB jack; moving a
  panel to a different jack gives it a new COM number, i.e. a new
  identity (re-run `light name`).
```

2. Insert a new section after `## Practical notes` (line 94) and before `## Daemon integration results` (line 95):

```markdown
## Multiple panels

- The daemon enumerates ALL VID 1A86 / PID 7523 ports and runs one
  independent session per port (own state tracker, reconnect loop, and
  60 ms rate-limited writes). A rescan every 5 s starts sessions for
  newly plugged-in panels and tears down sessions whose port vanished —
  no daemon restart needed.
- Identity = COM port name (see Transport). User-facing names map to
  ports in `%LOCALAPPDATA%\mutastic\light-names.json`; each panel's last
  look persists in `light-state-<COMx>.json`.
- Collective toggle: if ANY panel is on, all turn off; otherwise all turn
  on, each restoring its own persisted look (unknown state counts as
  off). Bare `light` commands fan out serially (~60 ms/panel minimum
  write spacing per panel, independent clocks).
```

3. In `## Recorded human questions` (line 107, currently 5 numbered items), append:

```markdown
6. When the two additional PL81 PRO panels arrive: plug each in, confirm
   the daemon discovers it within ~5 s (`light list` gains a row; log
   shows `light COM<n>: starting session`), name them, confirm which COM
   maps to which physical light, and verify the mapping survives a
   replug into the SAME USB jack. Also verify clean teardown on unplug
   (`light COM<n>: port gone, stopping session`) — untestable today with
   only the single live light in use.
```

- [ ] **Step 4: Gate**

Run: `go test -race ./... && go vet ./...`
Expected: clean (docs-only change; gate is cheap insurance).

- [ ] **Step 5: Commit**

```bash
git -C /home/dan/code/mutastic/.worktrees/pl81-multi-light add README.md docs/pl81-pro-serial-protocol.md
git -C /home/dan/code/mutastic/.worktrees/pl81-multi-light commit -m "docs: multi-light commands, CH340 COM-port identity, deploy-from-WSL note"
```

---

### Task 9: Deploy + live E2E on the real light (COM4)

**Files:** none modified — this task produces deployment + live evidence. All commands run from `/home/dan/code/mutastic/.worktrees/pl81-multi-light`.

**Interfaces:**
- Consumes: everything above; `deploy/deploy.cmd`; the deployed tree `C:\Users\dan\code\mutastic-deploy\`; daemon log `%LOCALAPPDATA%\mutastic\mutastic.log`.
- Produces: a deployed, verified daemon. End state (MANDATORY): light ON at 30% 2900K, mic UNMUTED.

**Interop flakiness rule:** any `cmd.exe`/`powershell.exe`/`*.exe` invocation below that fails with vsock/interop errors gets retried up to 3 times over ~2 minutes before being surfaced as a blocker.

- [ ] **Step 1: Full gate + fresh build**

```bash
cd /home/dan/code/mutastic/.worktrees/pl81-multi-light
go test -race ./... && go vet ./...
./build.sh && ls -l bin/mutastic.exe
```
Expected: tests/vet clean; fresh `bin/mutastic.exe` timestamp.

- [ ] **Step 2: Deploy with the documented hang workaround**

```bash
timeout 90 cmd.exe /c '\\wsl.localhost\Ubuntu\home\dan\code\mutastic\.worktrees\pl81-multi-light\deploy\deploy.cmd' '\\wsl.localhost\Ubuntu\home\dan\code\mutastic\.worktrees\pl81-multi-light' > /tmp/deploy-multi.log 2>&1
cat /tmp/deploy-multi.log
```
Expected: the invocation MAY hit the 90 s timeout (normal — the started daemon inherits the interop console). Success evidence is the transcript ending `Deploy complete.` (failure prints `DEPLOY FAILED`). Then confirm both processes:

```bash
tasklist.exe | grep -i -E 'mutastic|AutoHotkey'
```
Expected: `mutastic.exe` and `AutoHotkeyU64.exe` present.

- [ ] **Step 3: Verify daemon startup + discovery + migration in the log**

```bash
LOG=/mnt/c/Users/dan/AppData/Local/mutastic/mutastic.log
tail -40 "$LOG"
ls /mnt/c/Users/dan/AppData/Local/mutastic/
```
Expected in the fresh tail: `mutastic daemon starting`, `light COM4: starting session`, `light: rescan: ports now [COM4]`, `COM4 light: port opened`, and (first run only) `light COM4: migrated legacy light-state.json`. Directory listing shows `light-state-COM4.json` and NO `light-state.json`.

- [ ] **Step 4: Live command round-trip (spec acceptance)**

```bash
M=/mnt/c/Users/dan/code/mutastic-deploy/mutastic.exe
"$M" light list                 # expect: COM4 - connected <state>   (state may be "unknown" right after restart)
"$M" light name COM4 desk       # expect: named COM4 desk
"$M" light@desk status          # expect: a bare state string (unknown/off/on ...)
"$M" light@COM4 brightness 30   # expect: on 30% <K>K
"$M" light@COM4 temp 2900       # expect: on 30% 2900K
"$M" light toggle               # expect: COM4 desk: off        (any-on -> all off)
"$M" light toggle               # expect: COM4 desk: on 30% 2900K  (all-off -> all on, look restored)
"$M" light list                 # expect: COM4 desk connected on 30% 2900K
```
Also verify unknown-target handling: `"$M" light@nope status` → `error: unknown light "nope" (known: COM4=desk)`, exit code 1 (`echo $?`).
The light's echo frames are the state authority; ask the user to eyeball only if a reply disagrees with expectations.

- [ ] **Step 5: Rescan stability (no churn with a stable port set)**

Physical hot-plug is NOT tested now — the only attached device is the user's live light; do NOT unplug anything. Hot-plug add/remove behavior is proven by `TestRescanDiscoversHotPluggedLight` / `TestRescanStopsRemovedLight` (ticker-driven), and the physical test is recorded as a human question. Live evidence here is stability:

```bash
sleep 45
START=$(grep -n 'mutastic daemon starting' "$LOG" | tail -1 | cut -d: -f1)
tail -n +"$START" "$LOG" | grep -c 'starting session'   # expect: 1  (COM4 only, once)
tail -n +"$START" "$LOG" | grep -c 'port gone'          # expect: 0
```
Expected: exactly one `starting session`, zero `port gone` — the 5 s rescan ticks are not churning the stable session.

- [ ] **Step 6: Mic round-trip + final state**

```bash
"$M" status        # muted|unmuted
"$M" unmute        # ensure unmuted
"$M" status        # expect: unmuted
"$M" light status  # expect: COM4 desk: on 30% 2900K
```
Expected end state: light ON at 30% 2900K, mic UNMUTED. If the light state line differs, re-issue `"$M" light@COM4 brightness 30` and `"$M" light@COM4 temp 2900` until `light status` reads `COM4 desk: on 30% 2900K`.

- [ ] **Step 7: Verify deployed AHK (read-only)**

```bash
grep -a -A2 'F13::' /mnt/c/Users/dan/code/mutastic-deploy/MuteAllMeetings.ahk
```
Expected: the `Run, "%A_ScriptDir%\mutastic.exe" light toggle, ...` line — the pedal now toggles ALL lights with no AHK change.

- [ ] **Step 8: Confirm clean tree**

```bash
git -C /home/dan/code/mutastic/.worktrees/pl81-multi-light status --short
```
Expected: empty (this task changes no tracked files; nothing to commit).

---

## Recorded human questions (surface to the user at the end of the run)

1. **When the two new PL81 PRO panels arrive:** plug each into its permanent USB jack; within ~5 s `mutastic light list` should gain a `COM<n> - connected` row with no restart (log: `light COM<n>: starting session`). Name them (`mutastic light name COM<n> <name>`), note which COM belongs to which physical light, and confirm the mapping survives a replug into the SAME jack. Unplug one to confirm clean teardown (`light COM<n>: port gone, stopping session`). Physical hot-plug could not be exercised during implementation — the only attached device was the live COM4 light, which must not be unplugged.
2. **Pedal check with multiple lights:** once ≥2 lights are attached, press the left pedal (F13) and confirm all lights toggle together with the any-on→all-off rule.
