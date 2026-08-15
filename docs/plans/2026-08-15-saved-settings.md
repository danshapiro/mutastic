# Saved Settings and Dynamic Mute Menu Implementation Plan

> **For agentic workers:** Execute this plan task by task with a fresh
> implementer and a specification-plus-quality review after every task. Track
> progress with the checkbox steps below.

## User Request

### Requested result
Add a "save" feature so the user can save the current light settings under a chosen name from the web UI; saved named settings appear in both the web UI and the tray menu, and applying one from either surface restores those light settings. Move audio (microphone mute) settings into the web UI so the UI is not light-only. Change the tray menu's static "Muted" checkable item into a dynamic action whose label shows "Mute" when the mic is currently unmuted and "Unmute" when currently muted, with the click performing that named action.

### Explicit constraints
- Saved named settings must show up in both the tray and the web UI.
- Audio settings must live in the web UI (previously light-only surface).
- Tray mute item label is state-dependent: "Mute" shown when not currently muted, "Unmute" shown when muted; clicking executes exactly the displayed action.

### Accepted tradeoffs and residuals
- None stated for this request.

**Goal:** The user can save the current light look under a chosen name from the web UI, see and apply saved named settings from both the web UI and the tray menu, adjust microphone mute from the web UI, and use a tray action whose label reads "Mute" or "Unmute" according to current mic state and performs exactly the displayed action.

**Architecture:** The daemon owns a new persisted light-settings store (`internal/light/settings.go`, a `SettingsStore` mirroring the `Registry` precedent) exposed through three new `light settings save|list|apply` UDP verbs, so both client surfaces share one source of truth. The web UI (`mutastic ui`, a pure UDP client) gains a microphone card and a save/apply settings section beside the light controls, with the same `mutation_queue.js` discipline and polling cadence. The tray replaces its checkable "Muted" item with a dynamic Mute/Unmute action and gains a "Saved settings" submenu reconciled from `light settings list` on the 2 s poll.

**Tech Stack:** Go 1.26.3, gorilla-free standard stack + existing deps (go-hid, go.bug.st/serial, energye/systray fork for Windows tray), embedded HTML/JS SPA + mutation_queue.js, UDP text protocol on 127.0.0.1:42814.

## Global Constraints

- Go 1.26.3, module `mutastic`; no new third-party dependencies; no vendor changes.
- Gate: `go build ./... && go test ./... -count=1 && go vet ./...` from the worktree root, green at the end of every task.
- `bash build.sh` must keep producing bin/mutastic.exe (windows/amd64, CGO, x86_64-w64-mingw32-gcc); it is the ONLY compile gate for *_windows.go because the Linux gate skips them — it runs in every task that touches tray_windows.go and in the final docs task.
- Windows-only code stays behind build tags; all decision logic in Linux-testable pure functions (traystate.go / internal/light).
- README.md is the only end-user documentation; content-contract tests (ui_test.go, deploy_test.go) are updated in the SAME task as the docs/assets they pin.
- The wire reply shapes above are contracts; ui and tray code parse exactly those shapes ("(daemon unreachable)"/"(no saved settings)" are UI-side strings, never sent by the daemon).
- Out of scope (say so in the plan): gain/pattern/volume audio controls (daemon firmware opcodes exist but are not implemented), audio device selection, delete/rename verbs for saved settings (YAGNI).
- The daemon replies use exact wire shapes documented in the contract.

---

### Task 1: Daemon-side saved light settings store and verbs

Both new surfaces (web UI and tray) are pure UDP clients, so the daemon owns the store: a `SettingsStore` beside the `Registry` inside `MultiManager`, reached through three new `settings` sub-verbs on the existing `light` command path. `daemon.HandleCommand` already routes every `light ...` datagram here and the CLI's verbatim light-args pass-through delivers `light settings save "movie mode"` unchanged — no daemon-core or `main.go` change. Out of scope: a `settings delete` verb (YAGNI).

**Files:**
- Create: `internal/light/settings.go`
- Test: `internal/light/settings_test.go`
- Modify: `internal/light/multi.go:62-72` (add the `settings` field), `multi.go:83-93` (construct the store from `stateDir`), `multi.go:303-308` (add the `settings` case to the `HandleCommand` switch), `multi.go:478` (append methods after `handleDelta`)

**Interfaces:**
- Consumes: `Registry.Resolve` / `Registry.NameFor` (`names.go`); per-Manager `state.Status()` / `state.TargetOn()` (`state.go`); the per-light verb path `Manager.HandleCommand` (`manager.go:82-159`); fleet conventions `callLight` + `label` (`multi.go`); `ByteToKelvin` (`frame.go`); the `Registry.saveLocked` persistence idiom (non-atomic `json.Marshal` + `os.MkdirAll` + `os.WriteFile(.., 0o644)`, tolerate-corrupt load).
- Produces, exactly as fixed by the shared contract (§1):

```go
type SavedLightState struct {
    On         bool `json:"on"`
    Brightness int  `json:"brightness"`
    TempByte   byte `json:"temp_byte"`
}

type SavedSetting struct {
    Lights map[string]SavedLightState `json:"lights"` // key: registry name when named, else COM port path
}

type SettingsStore struct { /* unexported: mu sync.Mutex, path string, byName map[string]SavedSetting */ }

func NewSettingsStore(path string) *SettingsStore      // "" path -> disabled store; loads; missing/corrupt file silently defaults empty
func (s *SettingsStore) Enabled() bool
func (s *SettingsStore) Save(name string, snap SavedSetting) // overwrite by exact name
func (s *SettingsStore) List() []string                // sorted names
func (s *SettingsStore) Get(name string) (SavedSetting, bool)
```

- Produces, the exact wire verbs (contract §3):
  - `settings save <name>` → snapshot every currently-connected light (source: each light's Manager state: `Status()`/`TargetOn()`), keying by registry name if the light is named else its port path → reply `saved "<name>" (N lights)`; empty/whitespace name or name containing a newline → `error: invalid settings name`; disabled store → `error: settings persistence disabled`.
  - `settings list` → sorted names newline-joined; empty store → EMPTY string reply (`""` is the wire contract for "none").
  - `settings apply <name>` → unknown name → `error: unknown setting "<name>"`; else for each entry: resolve key via `reg.Resolve(name)` first, then direct port path; apply On, Brightness, TempByte in that order through the existing per-light command path; reply lines mirror the fleet fan-out shape exactly (`COM4 desk: on 47% 2900K`); unreachable/unresolvable keys → `error: light "<key>": unreachable, skipped`; when ZERO lights connected → `error: no lights connected`.

- [ ] **Step 1: Write the failing behavioral test**

Create `internal/light/settings_test.go` (complete file; uses the existing `fakeFleet` harness plus `newTestMulti`, `sessionManager`, `waitConnected`, `fastTimings`, `readJSON`, and the in-package `CCT` frame builder; snapshots use the memory-only `state.Set`, so the fake-port frame log holds exactly the apply sequence):

```go
package light

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSavedSettingsStoreSaveListGetRoundTrip(t *testing.T) {
	s := NewSettingsStore(filepath.Join(t.TempDir(), "light-settings.json"))
	if !s.Enabled() {
		t.Fatal("Enabled = false for a real path")
	}
	if got := s.List(); len(got) != 0 {
		t.Fatalf("fresh List = %v, want empty", got)
	}
	s.Save("zebra", SavedSetting{Lights: map[string]SavedLightState{"COM4": {On: true, Brightness: 40, TempByte: 9}}})
	s.Save("alpha", SavedSetting{Lights: map[string]SavedLightState{"desk": {On: false, Brightness: 20, TempByte: 0}}})
	if got := s.List(); !reflect.DeepEqual(got, []string{"alpha", "zebra"}) { // names sorted
		t.Fatalf("List = %v, want [alpha zebra]", got)
	}
	got, ok := s.Get("zebra")
	if !ok || got.Lights["COM4"] != (SavedLightState{On: true, Brightness: 40, TempByte: 9}) {
		t.Fatalf("Get(zebra) = %+v, %v", got, ok)
	}
	if _, ok := s.Get("nope"); ok {
		t.Fatal("Get(nope) = true, want false")
	}
	s.Save("zebra", SavedSetting{Lights: map[string]SavedLightState{"COM9": {On: true, Brightness: 99, TempByte: 18}}}) // overwrite by exact name
	got, ok = s.Get("zebra")
	if !ok || len(got.Lights) != 1 || got.Lights["COM9"].Brightness != 99 {
		t.Fatalf("overwritten Get(zebra) = %+v, %v", got, ok)
	}
	if got := s.List(); !reflect.DeepEqual(got, []string{"alpha", "zebra"}) {
		t.Fatalf("List after overwrite = %v, want [alpha zebra]", got)
	}
}

func TestSavedSettingsStorePersistedAcrossReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "light-settings.json")
	NewSettingsStore(path).Save("movie mode", SavedSetting{Lights: map[string]SavedLightState{"desk": {On: true, Brightness: 47, TempByte: 3}}})
	// A SECOND NewSettingsStore on the same path sees the saved snapshot.
	got, ok := NewSettingsStore(path).Get("movie mode")
	if !ok || got.Lights["desk"] != (SavedLightState{On: true, Brightness: 47, TempByte: 3}) {
		t.Fatalf("reloaded = %+v, %v", got, ok)
	}
	if err := os.WriteFile(path, []byte("{nope"), 0o644); err != nil { // corrupt file
		t.Fatal(err)
	}
	if got := NewSettingsStore(path).List(); len(got) != 0 {
		t.Fatalf("corrupt-file List = %v, want empty (silent default)", got)
	}
	disabled := NewSettingsStore("") // "" path disables the store
	if disabled.Enabled() {
		t.Fatal("Enabled = true for empty path")
	}
	disabled.Save("x", SavedSetting{Lights: map[string]SavedLightState{}})
	if got := disabled.List(); len(got) != 0 {
		t.Fatalf("disabled List = %v, want empty", got)
	}
}

func TestSavedSettingsSaveAndListWireVerbs(t *testing.T) {
	fastTimings(t)
	dir := t.TempDir()
	fleet := newFakeFleet("COM4", "COM7")
	mm, ctx := newTestMulti(t, fleet, dir)
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4", "COM7")

	// Wire contract: empty store lists as ""; list is sorted, newline-joined.
	if got := mm.HandleCommand("settings list"); got != "" {
		t.Fatalf("empty settings list = %q, want empty", got)
	}
	if got := mm.HandleCommand("settings save movie mode"); got != `saved "movie mode" (2 lights)` {
		t.Fatalf("save reply = %q", got)
	}
	if got := mm.HandleCommand("settings list"); got != "movie mode" {
		t.Fatalf("list = %q", got)
	}
	if got := mm.HandleCommand("settings save alpha"); got != `saved "alpha" (2 lights)` {
		t.Fatalf("save alpha = %q", got)
	}
	if got := mm.HandleCommand("settings list"); got != "alpha\nmovie mode" {
		t.Fatalf("sorted list = %q", got)
	}
	mm.HandleCommand("settings save alpha") // overwrite same name: still one entry
	if got := mm.HandleCommand("settings list"); got != "alpha\nmovie mode" {
		t.Fatalf("list after overwrite = %q", got)
	}
	var onDisk map[string]SavedSetting
	readJSON(t, filepath.Join(dir, "light-settings.json"), &onDisk)
	if len(onDisk) != 2 || onDisk["movie mode"].Lights == nil {
		t.Fatalf("light-settings.json = %v", onDisk)
	}
}

func TestSavedSettingsSnapshotKeysByNameElsePort(t *testing.T) {
	fastTimings(t)
	dir := t.TempDir()
	fleet := newFakeFleet("COM4", "COM7")
	mm, ctx := newTestMulti(t, fleet, dir)
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4", "COM7")
	if got := mm.HandleCommand("name COM4 desk"); got != "named COM4 desk" {
		t.Fatalf("name = %q", got)
	}
	sessionManager(t, mm, "COM4").state.Set(47, 0) // on, 47%, byte 0 = 2900K
	sessionManager(t, mm, "COM7").state.Set(0, 5)  // off; restore look = default 100%, byte 5

	mm.HandleCommand("settings save movie")
	want := map[string]SavedLightState{
		"desk": {On: true, Brightness: 47, TempByte: 0},   // named light: keyed by registry name
		"COM7": {On: false, Brightness: 100, TempByte: 5}, // unnamed: port; off saves restore brightness
	}
	got, ok := mm.settings.Get("movie")
	if !ok || !reflect.DeepEqual(got.Lights, want) {
		t.Fatalf("snapshot = %+v, %v; want %+v", got.Lights, ok, want)
	}
	reloaded, ok := NewSettingsStore(filepath.Join(dir, "light-settings.json")).Get("movie")
	if !ok || !reflect.DeepEqual(reloaded.Lights, want) {
		t.Fatalf("reloaded = %+v, %v; want %+v", reloaded.Lights, ok, want)
	}
}

func TestSavedSettingsApplyRestoresLiveStateInOrder(t *testing.T) {
	fastTimings(t)
	dir := t.TempDir()
	fleet := newFakeFleet("COM4", "COM7")
	mm, ctx := newTestMulti(t, fleet, dir)
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4", "COM7")
	sessionManager(t, mm, "COM4").state.Set(47, 0) // save: on 47% 2900K
	sessionManager(t, mm, "COM7").state.Set(0, 9)  // save: off
	mm.HandleCommand("settings save movie")
	sessionManager(t, mm, "COM4").state.Set(80, 18) // disturb: on 80% 7000K
	sessionManager(t, mm, "COM7").state.Set(60, 12) // disturb: on 60%
	fp4, fp7 := fleet.port("COM4"), fleet.port("COM7")
	w4, w7 := fp4.writeCount(), fp7.writeCount()

	got := mm.HandleCommand("settings apply movie")
	want := "COM4: on 47% 2900K\nCOM7: off"
	if got != want {
		t.Fatalf("apply reply = %q, want %q", got, want)
	}
	// On, Brightness, Temp in order: exactly three COM4 frames ("on" first
	// restores the disturbed 80%/byte-18 look); one COM7 off frame.
	if got := fp4.writeCount() - w4; got != 3 {
		t.Fatalf("COM4 wrote %d frames, want 3 (on, brightness, temp)", got)
	}
	for i, want := range [][]byte{CCT(80, 18), CCT(47, 18), CCT(47, 0)} {
		if got := fp4.write(w4 + i); !bytes.Equal(got, want) {
			t.Fatalf("COM4 frame %d = % x, want % x", i, got, want)
		}
	}
	if got := fp7.writeCount() - w7; got != 1 || !bytes.Equal(fp7.write(w7), CCT(0, 12)) {
		t.Fatalf("COM7 wrote %d frames, want the single off frame CCT(0, 12)", got)
	}
	if got := sessionManager(t, mm, "COM4").HandleCommand("status"); got != "on 47% 2900K" {
		t.Fatalf("COM4 status after apply = %q", got)
	}
	if got := sessionManager(t, mm, "COM7").HandleCommand("status"); got != "off" {
		t.Fatalf("COM7 status after apply = %q", got)
	}
}

func TestSavedSettingsApplySkipsUnreachableKeys(t *testing.T) {
	fastTimings(t)
	dir := t.TempDir()
	fleet := newFakeFleet("COM4", "COM7")
	mm, ctx := newTestMulti(t, fleet, dir)
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4", "COM7")
	mm.HandleCommand("name COM4 desk")
	sessionManager(t, mm, "COM4").state.Set(30, 3)
	sessionManager(t, mm, "COM7").state.Set(55, 9) // byte 9 = 4950K
	mm.HandleCommand("settings save movie")
	// Saved under the name, then the binding vanishes: "desk" no longer resolves.
	mm.HandleCommand("unname desk")
	sessionManager(t, mm, "COM7").state.Set(1, 18)

	got := mm.HandleCommand("settings apply movie")
	want := "COM7: on 55% 4950K\n" + `error: light "desk": unreachable, skipped`
	if got != want {
		t.Fatalf("apply reply = %q, want %q", got, want)
	}
	if got := sessionManager(t, mm, "COM7").HandleCommand("status"); got != "on 55% 4950K" {
		t.Fatalf("COM7 status = %q, want the resolvable entry applied", got)
	}

	// A COM-literal key (resolves) with no live session skips the same way.
	mm.settings.Save("partial", SavedSetting{Lights: map[string]SavedLightState{
		"COM7": {On: true, Brightness: 47, TempByte: 0},
		"COM9": {On: true, Brightness: 50, TempByte: 9},
	}})
	got = mm.HandleCommand("settings apply partial")
	want = "COM7: on 47% 2900K\n" + `error: light "COM9": unreachable, skipped`
	if got != want {
		t.Fatalf("apply partial reply = %q, want %q", got, want)
	}
}

func TestSavedSettingsApplyErrorShapes(t *testing.T) {
	fastTimings(t)
	fleet := newFakeFleet("COM4")
	mm, ctx := newTestMulti(t, fleet, t.TempDir())
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4")
	if got := mm.HandleCommand("settings apply nope"); got != `error: unknown setting "nope"` {
		t.Fatalf("apply unknown = %q", got)
	}

	zero, zeroCtx := newTestMulti(t, newFakeFleet(), t.TempDir())
	zero.rescan(zeroCtx)
	zero.settings.Save("movie", SavedSetting{Lights: map[string]SavedLightState{"COM4": {On: true, Brightness: 47, TempByte: 0}}})
	if got := zero.HandleCommand("settings apply movie"); got != "error: no lights connected" {
		t.Fatalf("apply with no lights = %q", got)
	}
}

func TestSavedSettingsNameValidation(t *testing.T) {
	fastTimings(t)
	fleet := newFakeFleet("COM4")
	mm, ctx := newTestMulti(t, fleet, t.TempDir())
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4")
	for _, cmd := range []string{
		"settings save",       // empty name
		"settings save a\nb",  // newline in name
		"settings apply",      // empty name
		"settings apply a\nb", // newline in name
	} {
		if got := mm.HandleCommand(cmd); got != "error: invalid settings name" {
			t.Errorf("HandleCommand(%q) = %q, want invalid settings name", cmd, got)
		}
	}
	if got := mm.HandleCommand("settings list"); got != "" {
		t.Errorf("list after invalid saves = %q, want empty", got)
	}
}

func TestSavedSettingsDisabledStore(t *testing.T) {
	fastTimings(t)
	fleet := newFakeFleet("COM4")
	mm, ctx := newTestMulti(t, fleet, "") // "" stateDir disables persistence
	mm.rescan(ctx)
	waitConnected(t, mm, "COM4")
	if mm.settings.Enabled() {
		t.Fatal("settings store enabled with empty stateDir")
	}
	if got := mm.HandleCommand("settings save movie"); got != "error: settings persistence disabled" {
		t.Errorf("save = %q", got)
	}
	if got := mm.HandleCommand("settings apply movie"); got != "error: settings persistence disabled" {
		t.Errorf("apply = %q", got)
	}
	if got := mm.HandleCommand("settings list"); got != "" { // reads as empty
		t.Errorf("list = %q, want empty", got)
	}
}
```

- [ ] **Step 2: Run the test and verify the intended failure**

Run: `go test ./internal/light/ -run TestSavedSettings -count=1`

Expected: FAIL because the package does not compile — `undefined: NewSettingsStore`, `undefined: SavedSetting`, `undefined: SavedLightState`, `mm.settings undefined` (store type, field, and verbs do not exist yet): the intended missing-behavior failure, not a syntax accident.

- [ ] **Step 3: Add the minimal production implementation**

Create `internal/light/settings.go` (complete new file, stdlib-only):

```go
package light

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// SavedLightState is one light's saved look: power, target brightness (the
// restore-target brightness when saved while off), and the temp step.
type SavedLightState struct {
	On         bool `json:"on"`
	Brightness int  `json:"brightness"`
	TempByte   byte `json:"temp_byte"`
}

// SavedSetting is a fleet snapshot stored under one settings name.
type SavedSetting struct {
	Lights map[string]SavedLightState `json:"lights"` // key: registry name when named, else COM port path
}

// SettingsStore is a persistent name -> snapshot map backed by
// <stateDir>/light-settings.json; "" disables persistence, missing or
// corrupt files silently start empty (NewRegistry/NewState precedent).
type SettingsStore struct {
	mu     sync.Mutex
	path   string
	byName map[string]SavedSetting
}

// NewSettingsStore loads the settings file from path if it exists.
func NewSettingsStore(path string) *SettingsStore {
	s := &SettingsStore{path: path, byName: map[string]SavedSetting{}}
	if path == "" {
		return s
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	var m map[string]SavedSetting
	if json.Unmarshal(data, &m) != nil {
		return s
	}
	for name, snap := range m {
		if validSettingsName(name) && snap.Lights != nil {
			s.byName[name] = snap
		}
	}
	return s
}

// Enabled reports whether persistence is on.
func (s *SettingsStore) Enabled() bool { return s.path != "" }

// Save stores snap under name (replacing any existing entry) and persists
// the whole map with the daemon's non-atomic Marshal + MkdirAll + WriteFile
// idiom. A disabled store is a no-op; a dropped write mirrors State.Set:
// the in-memory update stands.
func (s *SettingsStore) Save(name string, snap SavedSetting) {
	if !s.Enabled() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byName[name] = snap
	_ = s.saveLocked()
}

// List returns the saved settings names in sorted order (empty when none).
func (s *SettingsStore) List() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.byName))
	for name := range s.byName {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Get returns the snapshot stored under name.
func (s *SettingsStore) Get(name string) (SavedSetting, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, ok := s.byName[name]
	return snap, ok
}

func (s *SettingsStore) saveLocked() error {
	data, err := json.Marshal(s.byName)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0o644)
}

// validSettingsName: non-empty, no newlines (list replies are
// newline-joined; a newline in a name would corrupt the wire shape).
func validSettingsName(name string) bool {
	return name != "" && !strings.ContainsAny(name, "\r\n")
}
```

Modify `internal/light/multi.go`, four regions (no new imports: `fmt`, `sort`, `strconv`, `strings`, `path/filepath` are already imported).

Region 1 — the `MultiManager` struct (multi.go:62-72) becomes:

```go
type MultiManager struct {
	logger    *log.Logger
	reg       *Registry
	stateDir  string         // per-port state files live here; "" disables persistence
	settings  *SettingsStore // named saved light settings; disabled when stateDir == ""
	enumerate Enumerate
	openPort  OpenPort

	mu       sync.Mutex
	sessions map[string]*lightSession // key: canonical port name ("COM4")
	misses   map[string]int           // consecutive successful scans missing each port
}
```

Region 2 — `NewMultiManager` (multi.go:81-93) becomes (the `stateDir` guard mirrors `statePath` so the store never gets a bogus relative path):

```go
// NewMultiManager wires the discovery/open callbacks; Run starts the
// rescan loop. The saved-settings store lives beside the per-port state
// files in stateDir; "" disables persistence.
func NewMultiManager(logger *log.Logger, stateDir string, reg *Registry, enumerate Enumerate, openPort OpenPort) *MultiManager {
	settingsPath := ""
	if stateDir != "" {
		settingsPath = filepath.Join(stateDir, "light-settings.json")
	}
	return &MultiManager{
		logger:    logger,
		reg:       reg,
		stateDir:  stateDir,
		settings:  NewSettingsStore(settingsPath),
		enumerate: enumerate,
		openPort:  openPort,
		sessions:  map[string]*lightSession{},
		misses:    map[string]int{},
	}
}
```

Region 3 — in `HandleCommand`'s switch (multi.go:284-326), insert the `settings` case between `list` and `brightness-delta`:

```go
	case "list":
		if len(fields) != 1 {
			return "error: unknown light command"
		}
		return mm.list()
	case "settings":
		return mm.handleSettingsCommand(cmd, fields)
	case "brightness-delta":
```

Region 4 — append after `handleDelta` (multi.go:478):

```go
// handleSettingsCommand routes "settings save|list|apply <name>". Names are
// free-form (spaces allowed), so the name is re-read from the raw cmd after
// the sub-verb: strings.Fields would collapse an embedded newline into a
// space, hiding it from validSettingsName.
func (mm *MultiManager) handleSettingsCommand(cmd string, fields []string) string {
	if len(fields) < 2 {
		return "error: usage: light settings save|list|apply [name]"
	}
	switch fields[1] {
	case "list":
		if len(fields) != 2 {
			return "error: unknown light command"
		}
		return strings.Join(mm.settings.List(), "\n")
	case "save", "apply":
		_, rest, _ := strings.Cut(cmd, fields[1])
		name := strings.TrimSpace(rest)
		if !validSettingsName(name) {
			return "error: invalid settings name"
		}
		if !mm.settings.Enabled() {
			return "error: settings persistence disabled"
		}
		if fields[1] == "save" {
			return mm.settingsSave(name)
		}
		return mm.settingsApply(name)
	default:
		return "error: usage: light settings save|list|apply [name]"
	}
}

// settingsSave snapshots every light with a live session, keyed by registry
// name when named else by COM port. State reads are memory-only (like the
// fan-out "status" verb), so a wedged light cannot stall the UDP loop. An
// off light saves its restore-target brightness, not 0.
func (mm *MultiManager) settingsSave(name string) string {
	mm.mu.Lock()
	managers := make(map[string]*Manager, len(mm.sessions))
	for port, s := range mm.sessions {
		managers[port] = s.m
	}
	mm.mu.Unlock()
	snap := SavedSetting{Lights: make(map[string]SavedLightState, len(managers))}
	for port, m := range managers {
		on, brightness, temp, _ := m.state.Status()
		if !on {
			brightness, _ = m.state.TargetOn()
		}
		key := port
		if n := mm.reg.NameFor(port); n != "" {
			key = n
		}
		snap.Lights[key] = SavedLightState{On: on, Brightness: brightness, TempByte: temp}
	}
	mm.settings.Save(name, snap)
	return fmt.Sprintf("saved %q (%d lights)", name, len(snap.Lights))
}

// settingsApply restores the snapshot under name onto the live fleet,
// entries in sorted key order for a stable reply. Lines mirror the fleet
// fan-out shape ("COM4 desk: on 47% 2900K"); keys that resolve nowhere - or
// to a light with no live session - get "unreachable, skipped" and the
// rest still apply.
func (mm *MultiManager) settingsApply(name string) string {
	snap, ok := mm.settings.Get(name)
	if !ok {
		return fmt.Sprintf("error: unknown setting %q", name)
	}
	mm.mu.Lock()
	managers := make(map[string]*Manager, len(mm.sessions))
	for port, s := range mm.sessions {
		managers[port] = s.m
	}
	mm.mu.Unlock()
	if len(managers) == 0 {
		return "error: no lights connected"
	}
	keys := make([]string, 0, len(snap.Lights))
	for key := range snap.Lights {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		st := snap.Lights[key]
		port, ok := mm.reg.Resolve(key)
		m := managers[port] // nil when the key resolved but is not connected
		if !ok || m == nil {
			lines = append(lines, fmt.Sprintf("error: light %q: unreachable, skipped", key))
			continue
		}
		reply := mm.callLight(port, func() string { return applySavedState(m, st) })
		lines = append(lines, mm.label(port)+": "+reply)
	}
	return strings.Join(lines, "\n")
}

// applySavedState plays one snapshot entry back through the regular
// per-light command verbs (the "@light ..." path): power, then brightness,
// then temp - temp keeps the just-set brightness on an on light, so its
// reply is the restored look. An off entry is a single "off" (off keeps the
// current temp byte). The temp verb takes Kelvin, so the stored byte renders
// through ByteToKelvin, re-quantizing to the same step.
func applySavedState(m *Manager, st SavedLightState) string {
	if !st.On {
		return m.HandleCommand("off")
	}
	m.HandleCommand("on")
	m.HandleCommand("brightness " + strconv.Itoa(st.Brightness))
	return m.HandleCommand("temp " + strconv.Itoa(ByteToKelvin(st.TempByte)))
}
```

- [ ] **Step 4: Run the focused test**

Run: `go test ./internal/light/ -run TestSavedSettings -count=1`

Expected: PASS

- [ ] **Step 5: Refactor while green**

No refactor needed: `SettingsStore` deliberately mirrors `Registry`/`State.persistLocked` (one persistence idiom), and `settingsSave`/`settingsApply` reuse the existing `callLight` bound, `label`, and sorted-output conventions instead of a parallel fan-out (a shared helper would serve single-use call sites — YAGNI).

- [ ] **Step 6: Run impacted-test verification**

Impacted set: `internal/light` (the modified `HandleCommand` switch and `NewMultiManager` are exercised by every light test) and `internal/daemon`, which owns the UDP dispatch: its `"light"`-prefix CutPrefix routing (`daemon.go:89-128`) is the seam delivering `settings ...` unchanged, pinned by its routing/unknown-command tests (`TestHandleCommandDoesNotRouteLightPrefixWords`, `TestHandleCommandUnknown`). No other package consumes `MultiManager` (`main.go` constructs it with an unchanged signature).

Run: `go test ./internal/light/ ./internal/daemon/ -count=1`

Expected: PASS

- [ ] **Step 7: Commit the task**

```bash
git add internal/light/settings.go internal/light/settings_test.go internal/light/multi.go && git commit -m "light: daemon-owned named saved settings (save/list/apply verbs with persisted store)"
```
### Task 2: Daemon log dedupe for settings list + loopback coverage + protocol docs

**Files:**
- Modify: `internal/daemon/daemon.go` (Daemon struct latches ~52-53, logCommand 290-311)
- Modify: `internal/daemon/daemon_test.go` (startDaemon harness, logCommand tests)
- Modify: `README.md` (light commands table ~104-116 + notes paragraph ~118-134)

**Interfaces:**
- Consumes: Task 1's `settings save|list|apply` verbs in `MultiManager.HandleCommand`; the daemon's untouched `light` cut-prefix routing (`daemon.go:89-128`); contract §3 wire shapes; contract §4 latch mandate.
- Produces: loopback proof the three verbs traverse the daemon unchanged (empty-string list reply included); the `light settings list` logCommand latch; README protocol docs; harness variant `startDaemonLight(t, open, light CommandHandler)`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/daemon/daemon_test.go` (plus `"sort"` in the imports).

Refactor the existing harness: move the current `startDaemonInject` body verbatim into a new `startDaemonAll(t *testing.T, open OpenFunc, light CommandHandler, inject KeyInjector)`, changing only its `Run` call to `Run(ctx, open, light, inject, nil, pc, testLogger())`, then make all three helpers delegate (delete the stale "previous body moved verbatim" comment):

```go
// startDaemon runs Run() with the given OpenFunc on an ephemeral UDP port
// and returns the UDP address plus a UDP request helper.
func startDaemon(t *testing.T, open OpenFunc) (addr string, ask func(cmd string) string) {
	t.Helper()
	return startDaemonAll(t, open, nil, nil)
}

// startDaemonInject is startDaemon with a KeyInjector wired into Run.
func startDaemonInject(t *testing.T, open OpenFunc, inject KeyInjector) (addr string, ask func(cmd string) string) {
	t.Helper()
	return startDaemonAll(t, open, nil, inject)
}

// startDaemonLight is startDaemon with a light CommandHandler wired into Run.
func startDaemonLight(t *testing.T, open OpenFunc, light CommandHandler) (addr string, ask func(cmd string) string) {
	t.Helper()
	return startDaemonAll(t, open, light, nil)
}
```

Add the scripted fleet and the two new tests (near the other logCommand tests):

```go
// scriptedSettingsFleet stands in for a two-light MultiManager fleet owning
// the Task 1 settings verbs, proving "light settings ..." commands traverse
// the daemon's cut-prefix routing unchanged over real UDP.
type scriptedSettingsFleet struct {
	mu    sync.Mutex
	got   []string
	names []string // kept sorted, mirroring SettingsStore.List
}

func (f *scriptedSettingsFleet) HandleCommand(cmd string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, cmd)
	switch {
	case cmd == "settings list":
		return strings.Join(f.names, "\n")
	case strings.HasPrefix(cmd, "settings save "):
		name := strings.TrimPrefix(cmd, "settings save ")
		f.names = append(f.names, name)
		sort.Strings(f.names)
		return fmt.Sprintf("saved %q (%d lights)", name, 2)
	case strings.HasPrefix(cmd, "settings apply "):
		name := strings.TrimPrefix(cmd, "settings apply ")
		for _, n := range f.names {
			if n == name {
				return "COM4 desk-right: on 47% 2900K\nCOM6 desk-left: off"
			}
		}
		return fmt.Sprintf("error: unknown setting %q", name)
	}
	return "error: unknown command"
}

func (f *scriptedSettingsFleet) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}

// TestLightSettingsVerbsTraverseDaemonOverUDP pins the contract §3 wire
// shapes at the daemon boundary: verbs reach the light handler verbatim and
// replies round-trip byte-for-byte; the empty-store list reply is the empty
// string — a zero-length UDP datagram. (Characterization coverage: routing
// already exists, so this passes on first run; it guards regressions, e.g.
// a colliding top-level "settings" verb.)
func TestLightSettingsVerbsTraverseDaemonOverUDP(t *testing.T) {
	fleet := &scriptedSettingsFleet{}
	open := func() (Device, error) { return newFakeDevice(), nil }
	_, ask := startDaemonLight(t, open, fleet)

	if got := ask("light settings list"); got != "" {
		t.Fatalf("empty list = %q, want %q (the none-saved wire contract)", got, "")
	}
	if got := ask("light settings save movie mode"); got != `saved "movie mode" (2 lights)` {
		t.Fatalf("save = %q, want %q", got, `saved "movie mode" (2 lights)`)
	}
	if got := ask("light settings list"); got != "movie mode" {
		t.Fatalf("list after save = %q, want %q", got, "movie mode")
	}
	wantApply := "COM4 desk-right: on 47% 2900K\nCOM6 desk-left: off"
	if got := ask("light settings apply movie mode"); got != wantApply {
		t.Fatalf("apply = %q, want %q", got, wantApply)
	}
	if got := ask("light settings apply nope"); got != `error: unknown setting "nope"` {
		t.Fatalf("apply unknown = %q, want %q", got, `error: unknown setting "nope"`)
	}

	want := "settings list|settings save movie mode|settings list|settings apply movie mode|settings apply nope"
	if got := strings.Join(fleet.commands(), "|"); got != want {
		t.Fatalf("handler received %q, want %q (commands must arrive verbatim)", got, want)
	}
}

// TestLogCommandSuppressesRepeatedSettingsList: the tray polls "light
// settings list" on its 2 s tick; like "status"/"light status" it must be
// reply-latched or the log grows unbounded (rotation only at daemon start).
func TestLogCommandSuppressesRepeatedSettingsList(t *testing.T) {
	var buf bytes.Buffer
	d := &Daemon{Logger: log.New(&buf, "", 0)}
	d.logCommand("light settings list", "")            // first: logs (empty store)
	d.logCommand("light settings list", "")            // identical: suppressed
	d.logCommand("light settings list", "movie mode")  // changed: logs
	d.logCommand("light settings list", "movie mode")  // identical: suppressed
	d.logCommand("status", "muted")                    // independent latch untouched
	// Non-poll verbs always log, even identical repeats:
	d.logCommand("light settings save movie mode", `saved "movie mode" (2 lights)`)
	d.logCommand("light settings save movie mode", `saved "movie mode" (2 lights)`)
	if got := strings.Count(buf.String(), `"light settings list"`); got != 2 {
		t.Fatalf("settings list logged %d times, want 2 (first + change):\n%s", got, buf.String())
	}
	if got := strings.Count(buf.String(), `"light settings save movie mode"`); got != 2 {
		t.Fatalf("settings save logged %d times, want 2 (non-poll verbs always log):\n%s", got, buf.String())
	}
}
```

- [ ] **Step 2: Run the tests and verify the intended failure**

Run: `go test ./internal/daemon/ -run 'TestLogCommandSuppressesRepeatedSettingsList|TestLightSettingsVerbsTraverseDaemonOverUDP' -count=1`

Expected: FAIL — `TestLogCommandSuppressesRepeatedSettingsList` sees list logged 4 times (no latch yet). `TestLightSettingsVerbsTraverseDaemonOverUDP` PASSES already (routing exists); the latch test is the failing driver.

- [ ] **Step 3: Add the minimal production implementation**

In `internal/daemon/daemon.go`, extend the Daemon struct latches (keep gofmt field alignment):

```go
	lastStatusReply       string
	lastLightStatusReply  string // like lastStatusReply, for the "light status" poller
	lastSettingsListReply string // like lastStatusReply, for the tray's "light settings list" poller
```

Update the logCommand doc comment ("two resident-poller commands" → three) and add the third case:

```go
// logCommand logs one served UDP command. Non-poll commands always log.
// Resident-poller commands ("status", "light status", "light settings
// list" — each polled on a fixed interval by key/tray) log only when the
// reply changes: rotation runs only at daemon start, so unconditional
// logging would grow the log unbounded. Called only from the single
// serveUDP goroutine, so the latches need no lock.
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
		if reply == d.lastSettingsListReply {
			return
		}
		d.lastSettingsListReply = reply
	}
	d.Logger.Printf("command %q -> %q", cmd, reply)
}
```

- [ ] **Step 4: Run the focused tests**

Run: `go test ./internal/daemon/ -count=1`

Expected: PASS

- [ ] **Step 5: Refactor while green**

No refactor: the latch is one exact-match case beside its two siblings; the harness refactor de-duplicated startDaemon. Add the README docs now (in this task): in the "Light commands" table, insert after the `unname` row:

```markdown
  | `mutastic light settings save <name>` | snapshot every connected light's look under `<name>` (persisted in `light-settings.json`) |
  | `mutastic light settings list` | saved names, sorted, one per line — an EMPTY reply means none saved |
  | `mutastic light settings apply <name>` | re-apply a saved setting, one reply line per light |
```

and append after the notes paragraph's "A light's identity is its COM port ... defaults apply." this paragraph:

```markdown
  Saved settings persist in `%LOCALAPPDATA%\mutastic\light-settings.json`
  (plain JSON, overwrite-by-name; missing/corrupt file = none saved).
  `save` snapshots every connected light — on/brightness/temp, keyed by
  registry name when named, else COM port — and replies `saved "<name>" (N
  lights)` (`error: invalid settings name` for an empty or
  newline-containing name; `error: settings persistence disabled` with no
  state directory). `list` replies with the sorted names, one per line;
  the empty reply is the wire contract for "none saved" (a zero-length UDP
  datagram). `apply` resolves each entry by name first, then COM port,
  writes on → brightness → temp in that order, and replies in the usual
  fan-out shape (`COM4 desk-right: on 47% 2900K`); unreachable entries
  reply `error: light "<key>": unreachable, skipped`, an unknown name
  replies `error: unknown setting "<name>"`, and zero connected lights
  reply `error: no lights connected`. Quoted multi-word names work:
  `mutastic.exe light settings save "movie mode"` — every argument after
  `light` joins into one daemon command verbatim. No delete/rename verb,
  deliberately (YAGNI): overwrite with a fresh `save`, or delete
  `light-settings.json` to drop all.
```

- [ ] **Step 6: Run impacted-test verification**

Latch + harness touch only `internal/daemon`; README is unpinned prose. Impacted set = all internal packages.

Run: `go test ./internal/... -count=1`

Expected: PASS

- [ ] **Step 7: Commit the task**

```bash
git add internal/daemon/daemon.go internal/daemon/daemon_test.go README.md && git commit -m "daemon: dedupe settings-list polls; loopback coverage + README for light settings verbs"
```

### Task 3: Web UI audio (mic) card + /api/mic routes

**Files:**
- Modify: `ui.go` (newDaemonDispatcher 96-102, ServeHTTP 134-220, new handlers near handleLights 257)
- Modify: `internal/lightui/index.html` (CSS ~108-126, markup after gang section ~230, JS ~393-496)
- Modify: `ui_test.go` (new tests; mirrors TestEmbeddedLightUICardsUseTheirOwnIdentityAsTarget 132-168)
- Modify: `README.md` (`mutastic ui` bullet ~66-72)

**Interfaces:**
- Consumes: daemon mic verbs `mute|unmute|toggle` + tri-state `status` (`daemon.go:96-116`); `daemonCall`/`daemonDispatcher.sequence` (`ui.go:86-115`); the `clientCommand` timeout split (main.go:89-99: mic 1 s, light 6 s); index.html's mutation queue + 750 ms cadence; `newTestDaemonDispatcher`/`newUIServer` seams.
- Produces: GET/POST `/api/mic` with the contract §5 shape `{"state":"muted|unmuted|unknown|unreachable"}`; types `uiMicStatus`, `uiMicRequest`; `uiDaemonTimeout(command string) time.Duration`; index.html `refreshMic()`, `updateMic(state)`, `bindMicControls()`.

- [ ] **Step 1: Write the failing tests**

Add to `ui_test.go` (no import changes needed):

```go
func TestUIMicStatusReportsDaemonState(t *testing.T) {
	cases := []struct{ name, reply string; err error; want string }{
		{name: "muted", reply: "muted", want: "muted"},
		{name: "unmuted", reply: "unmuted", want: "unmuted"},
		{name: "unknown", reply: "unknown", want: "unknown"},
		{name: "daemon error reply", reply: "error: no device", want: "unreachable"},
		{name: "transport error", err: errors.New("connection refused"), want: "unreachable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var commands []string
			server := newUIServer(42815, newTestDaemonDispatcher(func(command string) (string, error) {
				commands = append(commands, command)
				return tc.reply, tc.err
			}))
			req := httptest.NewRequest(http.MethodGet, "/api/mic", nil)
			req.Host = "127.0.0.1:42815"
			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body.String())
			}
			if !reflect.DeepEqual(commands, []string{"status"}) {
				t.Fatalf("daemon commands = %v, want [status]", commands)
			}
			wantBody := `{"state":"` + tc.want + `"}`
			if got := strings.TrimSpace(recorder.Body.String()); got != wantBody {
				t.Fatalf("body = %q, want %q", got, wantBody)
			}
		})
	}
}

func TestUIMicPostRunsVerbThenFreshStatus(t *testing.T) {
	var commands []string
	call := func(command string) (string, error) {
		commands = append(commands, command)
		switch command {
		case "mute", "status":
			return "muted", nil
		default:
			return "error: unexpected command", nil
		}
	}
	server := newUIServer(42815, newTestDaemonDispatcher(call))
	req := httptest.NewRequest(http.MethodPost, "/api/mic", strings.NewReader(`{"action":"mute"}`))
	req.Host = "127.0.0.1:42815"
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	if got := strings.TrimSpace(recorder.Body.String()); got != `{"state":"muted"}` {
		t.Fatalf("body = %q, want the fresh state %q", got, `{"state":"muted"}`)
	}
	if !reflect.DeepEqual(commands, []string{"mute", "status"}) {
		t.Fatalf("daemon commands = %v, want [mute status] (verb then fresh status)", commands)
	}
}

func TestUIMicPostValidatesActionHonorsGuardsAndMethods(t *testing.T) {
	calls := 0
	server := newUIServer(42815, newTestDaemonDispatcher(func(string) (string, error) {
		calls++
		return "unknown", nil
	}))
	post := func(body, origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/mic", strings.NewReader(body))
		req.Host = "127.0.0.1:42815"
		req.Header.Set("Content-Type", "application/json")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, req)
		return recorder
	}
	// Bad actions are 400 and never reach the daemon.
	for _, body := range []string{`{"action":"explode"}`, `{"action":""}`} {
		if rec := post(body, ""); rec.Code != http.StatusBadRequest {
			t.Fatalf("body %s status = %d, want 400", body, rec.Code)
		}
	}
	if calls != 0 {
		t.Fatalf("daemon received %d calls for rejected actions, want 0", calls)
	}
	// Origin guard posture matches the other mutating endpoints.
	if rec := post(`{"action":"mute"}`, "http://evil.example"); rec.Code != http.StatusForbidden {
		t.Fatalf("foreign origin status = %d, want 403", rec.Code)
	}
	// Wrong methods: rejected like the existing routes.
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/api/mic", nil)
		req.Host = "127.0.0.1:42815"
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != "GET, POST" {
			t.Fatalf("%s /api/mic = %d (Allow %q), want 405 Allow: GET, POST", method, recorder.Code, recorder.Header().Get("Allow"))
		}
	}
}

// TestEmbeddedLightUIMicCardUsesTheMicEndpoints mirrors the pinned-fragment
// approach of TestEmbeddedLightUICardsUseTheirOwnIdentityAsTarget.
func TestEmbeddedLightUIMicCardUsesTheMicEndpoints(t *testing.T) {
	source := string(lightUIHTML)
	for _, fragment := range []string{
		`id="mic-badge"`,
		`id="mic-status-line"`,
		`data-mic-action="mute"`,
		`data-mic-action="unmute"`,
		`data-mic-action="toggle"`,
		`fetch("/api/mic", {cache: "no-store"})`,
		"enqueueMutation(`mic:${button.dataset.micAction}`, \"/api/mic\", {action: button.dataset.micAction}, false)",
		`window.setInterval(() => { refreshLights(true); refreshMic(); }, 750);`,
	} {
		if !strings.Contains(source, fragment) {
			t.Fatalf("embedded UI is missing mic card fragment %q", fragment)
		}
	}
	if strings.Contains(source, `fetch("/api/mic", {method: "POST"`) {
		t.Fatal("mic actions must go through the shared mutation queue, not a direct fetch")
	}
}
```

- [ ] **Step 2: Run the tests and verify the intended failure**

Run: `go test . -run 'TestUIMic|TestEmbeddedLightUIMicCard' -count=1`

Expected: FAIL — `/api/mic` 404s and the embedded HTML lacks every mic card fragment.

- [ ] **Step 3: Add the minimal production implementation**

In `ui.go`:

1. New types after `uiActionRequest` (~line 82):

```go
// uiMicStatus is the wire shape of GET/POST /api/mic: the daemon's mic
// state word, or "unreachable" when the daemon errors or can't be asked.
type uiMicStatus struct {
	State string `json:"state"`
}

type uiMicRequest struct {
	Action string `json:"action"`
}
```

2. Mirror clientCommand's timeout split; newDaemonDispatcher's askDaemon call uses it:

```go
// uiDaemonTimeout mirrors clientCommand's split (main.go): mic verbs 1 s;
// light and shutdown verbs keep lightClientTimeout.
func uiDaemonTimeout(command string) time.Duration {
	switch command {
	case "status", "mute", "unmute", "toggle":
		return time.Second
	}
	return lightClientTimeout
}
```

```go
		roundTrip: func(command string) (string, error) {
			return askDaemon(command, udpAddr, uiDaemonTimeout(command))
		},
```

3. Route in ServeHTTP's switch after the `/api/lights` case (top-of-ServeHTTP guards already ran; POST origin-checks exactly like /api/light):

```go
	case "/api/mic":
		switch r.Method {
		case http.MethodGet:
			s.handleMic(w)
		case http.MethodPost:
			if !s.validPostOrigin(r) {
				writeUIJSON(w, http.StatusForbidden, uiResponse{Error: "origin is not allowed"})
				return
			}
			s.handleMicAction(w, r)
		default:
			w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
			writeUIJSON(w, http.StatusMethodNotAllowed, uiResponse{Error: "method not allowed"})
		}
```

4. Handlers and parser, after handleLights:

```go
func (s *uiServer) handleMic(w http.ResponseWriter) {
	state := "unreachable"
	_ = s.dispatcher.sequence(func(call daemonCall) error {
		got, err := queryUIMic(call)
		if err == nil {
			state = got
		}
		return nil
	})
	writeUIJSON(w, http.StatusOK, uiMicStatus{State: state})
}

func (s *uiServer) handleMicAction(w http.ResponseWriter, r *http.Request) {
	var req uiMicRequest
	if err := decodeUIJSON(w, r, &req); err != nil {
		writeUIJSON(w, http.StatusBadRequest, uiResponse{Error: err.Error()})
		return
	}
	switch req.Action {
	case "mute", "unmute", "toggle":
	default:
		writeUIJSON(w, http.StatusBadRequest, uiResponse{Error: fmt.Sprintf("unknown mic action %q", req.Action)})
		return
	}
	state := "unreachable"
	_ = s.dispatcher.sequence(func(call daemonCall) error {
		reply, err := call(req.Action)
		if err != nil || strings.HasPrefix(strings.TrimSpace(reply), "error:") {
			return nil
		}
		got, queryErr := queryUIMic(call)
		if queryErr == nil {
			state = got
		}
		return nil
	})
	writeUIJSON(w, http.StatusOK, uiMicStatus{State: state})
}

// queryUIMic asks the daemon for the mic state; transport errors and
// "error:" replies both collapse to "unreachable".
func queryUIMic(call daemonCall) (string, error) {
	reply, err := call("status")
	if err != nil {
		return "unreachable", err
	}
	state := parseUIMicState(reply)
	if state == "unreachable" {
		return state, fmt.Errorf("unexpected mic status reply %q", strings.TrimSpace(reply))
	}
	return state, nil
}

func parseUIMicState(reply string) string {
	switch trimmed := strings.TrimSpace(reply); trimmed {
	case "muted", "unmuted", "unknown":
		return trimmed
	default:
		return "unreachable"
	}
}
```

5. In `internal/lightui/index.html`, CSS — after the `.status-badge[data-state="disconnected"]` rule (~line 112):

```css
    .status-badge[data-state="unmuted"] { border-color: rgba(114, 213, 160, .35); color: var(--green); }
    .status-badge[data-state="muted"] { border-color: rgba(255, 143, 134, .4); color: var(--red); }
    .status-badge[data-state="unreachable"] { border-color: rgba(255, 143, 134, .4); color: var(--red); }
    #mic-status-line { margin: 14px 0 0; }
    #mic-status-line[data-state="unreachable"] { color: var(--red); }
```

6. Markup — new panel between the gang-controls `</section>` (~line 230) and the `.section-title` div; unreachable is visibly distinct (red badge/line, disabled buttons):

```html
    <section class="panel" aria-labelledby="mic-title">
      <div class="panel-inner">
        <div class="panel-head">
          <div>
            <p class="eyebrow">Microphone</p>
            <h2 id="mic-title">Yeti X mute</h2>
            <p class="subtle">The mic's own hardware mute, tracked by the daemon. Panel mutes never fire the meeting-app sweep (physical button, tray, and Stream Deck mutes do).</p>
          </div>
          <span id="mic-badge" class="status-badge" data-state="unknown">unknown</span>
        </div>
        <div class="power-row" role="group" aria-label="Microphone mute">
          <button type="button" data-mic-action="mute">Mute</button>
          <button type="button" data-mic-action="unmute">Unmute</button>
          <button class="button-quiet" type="button" data-mic-action="toggle">Toggle</button>
        </div>
        <p id="mic-status-line" class="subtle" data-state="unknown" role="status">Asking the daemon…</p>
      </div>
    </section>
```

7. JS — after `bindCards()` (~line 414), add:

```js
      function updateMic(micState) {
        const labels = {
          muted: "Mic is muted",
          unmuted: "Mic is live",
          unknown: "Mute state unknown until the first mute command or button press",
          unreachable: "Daemon unreachable — controls disabled"
        };
        const state = Object.prototype.hasOwnProperty.call(labels, micState) ? micState : "unreachable";
        const badge = $("mic-badge");
        badge.dataset.state = state;
        badge.textContent = state;
        const line = $("mic-status-line");
        line.dataset.state = state;
        line.textContent = labels[state];
        document.querySelectorAll("[data-mic-action]").forEach((button) => { button.disabled = state === "unreachable"; });
      }

      async function refreshMic() {
        try {
          const response = await fetch("/api/mic", {cache: "no-store"});
          const data = await response.json();
          updateMic(data.state || "unreachable");
        } catch (error) {
          updateMic("unreachable");
        }
      }

      function bindMicControls() {
        document.querySelectorAll("[data-mic-action]").forEach((button) => {
          button.addEventListener("click", () => enqueueMutation(`mic:${button.dataset.micAction}`, "/api/mic", {action: button.dataset.micAction}, false));
        });
      }
```

8. Queue `post` callback: after the `if (data.lights) {...}` block add `if (data.state) { updateMic(data.state); }`; in `onFailure` add `refreshMic();` after `refreshLights(true);`. Replace the final boot lines with:

```js
      bindTopControls();
      bindMicControls();
      refreshLights(false);
      refreshMic();
      window.setInterval(() => { refreshLights(true); refreshMic(); }, 750);
```

- [ ] **Step 4: Run the focused tests**

Run: `go test . -run 'TestUIMic|TestEmbeddedLightUIMicCard' -count=1`

Expected: PASS

- [ ] **Step 5: Refactor while green**

No structural refactor: handlers reuse the existing guards/dispatcher/JSON helpers, and the card reuses `.panel`/`.status-badge`/`.power-row` styling. Add the README paragraph now (same task as the docs it describes): in the `mutastic ui` bullet, after the `/api/shutdown` sentences:

```markdown
  The panel's Microphone card mirrors the daemon-tracked Yeti X mute state
  (polled with the lights' 750 ms refresh via `GET /api/mic`: `live`,
  `muted`, `unknown` after a daemon restart, or a distinct `daemon
  unreachable` state that disables the buttons), and its Mute / Unmute /
  Toggle buttons `POST /api/mic` with the daemon's absolute mic verbs
  (`mute`, `unmute`, `toggle`; 1 s timeout class like the CLI — light verbs
  keep 6 s). Panel mutes never fire the F24 meeting-app sweep — exactly
  like the CLI — only a physical press of the mic's own button (or the
  tray / Stream Deck mute actions) does.
```

- [ ] **Step 6: Run impacted-test verification**

The changes touch only root-package UI code plus the embedded HTML; the impacted set is the UI/embedded-HTML contract tests plus the Node-backed mutation queue test:

Run: `go test . -run 'TestUI|TestEmbeddedLightUI|TestLight' -count=1`
Expected: PASS

Then the full root package, since `ui.go` is `package main` shared with main/deckplugin/traystate tests:

Run: `go test . -count=1`
Expected: PASS

- [ ] **Step 7: Commit the task**

```bash
git add ui.go internal/lightui/index.html ui_test.go README.md && git commit -m "ui: mic audio card with /api/mic routes (mute/unmute/toggle + status)"
```
### Task 4: Web UI saved-settings section + /api/settings routes

**Files:**
- Modify: `ui.go` (route case after the `/api/group` case in `ServeHTTP`, ~ui.go:195; response/request types after `uiResponse` ~ui.go:76; handlers + parser + builder after `handleGroup` ~ui.go:311)
- Modify: `internal/lightui/index.html` (styles after the `.trim-row` rules; markup between the gang-controls `</section>` and the "Individual controls" section-title; JS inside the existing IIFE)
- Test: `ui_test.go`
- Modify: `README.md` (`mutastic ui` component bullet)

**Interfaces:**
- Consumes: Task 1/3's daemon wire verbs `light settings save <name>` (reply `saved "<name>" (N lights)`), `light settings apply <name>` (fleet fan-out lines), `light settings list` (newline-joined names; EMPTY string when the store is empty; `error: settings persistence disabled` possible). Names may contain spaces (`"movie mode"`), never newlines. Reuses `daemonCall` (`ui.go:86`), `newTestDaemonDispatcher` (`ui.go:104`), `daemonDispatcher.sequence` (`ui.go:108`, 6 s `lightClientTimeout` for settings verbs — the light/verb budget, not the 1 s mic budget), `decodeUIJSON`, `writeUIJSON`, `writeUIMethodError` (its `method` arg is just the `Allow` header string, so `"GET, POST"` is valid), `validPostOrigin`, and the `newUIServer(port, dispatcher)` + `httptest` harness pattern (`req.Host = "127.0.0.1:42815"`, `Content-Type: application/json` on POSTs).
- Produces: `GET /api/settings` → `{"names":[...]}` (HTTP 200 always; daemon transport failure OR `error:` reply degrades in-band to `{"names":[],"error":"unreachable"}` — the light polling loop already carries the big banner, so the settings list never hard-errors the page); `POST /api/settings` `{"action":"save|apply","name":"..."}` → daemon verb then one list refresh, replying HTTP 200 `{"names":[...]}`; missing/whitespace name, newline name, or unknown action → HTTP 400; daemon `error:` reply or transport failure on the verb → HTTP 502 `{"names":[],"error":"<daemon reply verbatim|transport error>"}` (same 502-with-details mapping `mutate` uses). Wording notes: "list refetched on load and after save/apply" is satisfied by the POST reply itself carrying the refreshed names (contract §5) — the page does NOT re-GET after a POST.

- [ ] **Step 1: Write the failing behavioral tests**

Append these four tests to `ui_test.go` (file already imports `bytes`, `encoding/json`, `errors`, `net/http`, `net/http/httptest`, `reflect`, `strings`, `testing` — no import changes):

```go
func TestUIAPISettingsList(t *testing.T) {
	var commands []string
	server := newUIServer(42815, newTestDaemonDispatcher(func(command string) (string, error) {
		commands = append(commands, command)
		if command != "light settings list" {
			return "error: unexpected command", nil
		}
		return "movie mode\ndesk night", nil
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	req.Host = "127.0.0.1:42815"
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /api/settings status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Names []string `json:"names"`
		Error string   `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(response.Names, []string{"movie mode", "desk night"}) || response.Error != "" {
		t.Fatalf("GET /api/settings body = %s, want names [movie mode, desk night] and no error", recorder.Body.String())
	}
	if !reflect.DeepEqual(commands, []string{"light settings list"}) {
		t.Fatalf("daemon commands = %v, want exactly one light settings list", commands)
	}

	// An empty store replies with an empty string (the wire contract); the
	// JSON must still be a real empty array, not null.
	empty := newUIServer(42815, newTestDaemonDispatcher(func(string) (string, error) {
		return "", nil
	}))
	recorder = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	req.Host = "127.0.0.1:42815"
	empty.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK || !bytes.Contains(recorder.Body.Bytes(), []byte(`"names":[]`)) {
		t.Fatalf("empty store body = %d/%s, want 200 with \"names\":[]", recorder.Code, recorder.Body.String())
	}

	// Unreachable daemon OR a daemon error: reply degrades in-band to an
	// empty names array plus the fixed "unreachable" marker at HTTP 200
	// (the 750 ms light poll already owns the error banner).
	for name, outcome := range map[string]struct {
		reply string
		err   error
	}{
		"transport error": {err: errors.New("connection refused")},
		"daemon error":    {reply: "error: settings persistence disabled"},
	} {
		t.Run(name, func(t *testing.T) {
			down := newUIServer(42815, newTestDaemonDispatcher(func(string) (string, error) {
				return outcome.reply, outcome.err
			}))
			recorder := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
			req.Host = "127.0.0.1:42815"
			down.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (the failure is in-band); body %s", recorder.Code, recorder.Body.String())
			}
			var body struct {
				Names []string `json:"names"`
				Error string   `json:"error"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if len(body.Names) != 0 || body.Error != "unreachable" {
				t.Fatalf("body = %s, want names [] and error \"unreachable\"", recorder.Body.String())
			}
		})
	}
}

func TestUIAPISettingsSaveAndApplyRefreshTheList(t *testing.T) {
	names := []string{"before"}
	var commands []string
	call := func(command string) (string, error) {
		commands = append(commands, command)
		switch command {
		case "light settings save movie mode":
			names = []string{"before", "movie mode"}
			return `saved "movie mode" (3 lights)`, nil
		case "light settings apply before":
			return "COM4 desk-right: on 47% 2900K", nil
		case "light settings list":
			return strings.Join(names, "\n"), nil
		}
		return "error: unexpected command", nil
	}
	server := newUIServer(42815, newTestDaemonDispatcher(call))
	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/settings", strings.NewReader(body))
		req.Host = "127.0.0.1:42815"
		req.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, req)
		return recorder
	}

	recorder := post(`{"action":"save","name":"movie mode"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("save status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Names []string `json:"names"`
		Error string   `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(response.Names, []string{"before", "movie mode"}) || response.Error != "" {
		t.Fatalf("save reply = %s, want the refreshed names list", recorder.Body.String())
	}
	want := []string{"light settings save movie mode", "light settings list"}
	if !reflect.DeepEqual(commands, want) {
		t.Fatalf("save daemon commands = %v, want %v (verb first, then exactly one list refresh)", commands, want)
	}

	commands = nil
	recorder = post(`{"action":"apply","name":"before"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("apply status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	want = []string{"light settings apply before", "light settings list"}
	if !reflect.DeepEqual(commands, want) {
		t.Fatalf("apply daemon commands = %v, want %v (verb first, then exactly one list refresh)", commands, want)
	}
}

func TestUIAPISettingsValidatesAndPassesDaemonErrorsThrough(t *testing.T) {
	server := newUIServer(42815, newTestDaemonDispatcher(func(string) (string, error) {
		return "", nil
	}))
	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/settings", strings.NewReader(body))
		req.Host = "127.0.0.1:42815"
		req.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, req)
		return recorder
	}
	// Client-side validation: these must never reach the daemon.
	for _, body := range []string{
		`{"action":"save"}`,
		`{"action":"save","name":"   "}`,
		`{"action":"apply"}`,
		`{"action":"save","name":"movie\nnight"}`,
		`{"action":"explode","name":"x"}`,
	} {
		if got := post(body).Code; got != http.StatusBadRequest {
			t.Fatalf("POST /api/settings %s status = %d, want 400", body, got)
		}
	}
	// Method guard: the route pair takes GET and POST only.
	req := httptest.NewRequest(http.MethodPut, "/api/settings", nil)
	req.Host = "127.0.0.1:42815"
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != "GET, POST" {
		t.Fatalf("PUT /api/settings = %d (Allow %q), want 405 Allow: GET, POST", recorder.Code, recorder.Header().Get("Allow"))
	}
	// Origin guard: same posture as the panel's other mutating endpoints.
	originReq := httptest.NewRequest(http.MethodPost, "/api/settings", strings.NewReader(`{"action":"save","name":"x"}`))
	originReq.Host = "127.0.0.1:42815"
	originReq.Header.Set("Content-Type", "application/json")
	originReq.Header.Set("Origin", "http://evil.example")
	recorder = httptest.NewRecorder()
	server.ServeHTTP(recorder, originReq)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("POST /api/settings with foreign Origin = %d, want 403", recorder.Code)
	}

	// A daemon "error:" reply is a bad gateway; the body carries it verbatim.
	errServer := newUIServer(42815, newTestDaemonDispatcher(func(command string) (string, error) {
		if command == "light settings apply ghost" {
			return `error: unknown setting "ghost"`, nil
		}
		return "", nil
	}))
	recorder = httptest.NewRecorder()
	errReq := httptest.NewRequest(http.MethodPost, "/api/settings", strings.NewReader(`{"action":"apply","name":"ghost"}`))
	errReq.Host = "127.0.0.1:42815"
	errReq.Header.Set("Content-Type", "application/json")
	errServer.ServeHTTP(recorder, errReq)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("daemon error status = %d, want 502; body %s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error != `error: unknown setting "ghost"` {
		t.Fatalf("error body = %q, want the verbatim daemon reply", body.Error)
	}

	// A transport failure on the verb maps to 502 too (mutate's mapping).
	dead := newUIServer(42815, newTestDaemonDispatcher(func(string) (string, error) {
		return "", errors.New("connection refused")
	}))
	recorder = httptest.NewRecorder()
	deadReq := httptest.NewRequest(http.MethodPost, "/api/settings", strings.NewReader(`{"action":"save","name":"x"}`))
	deadReq.Host = "127.0.0.1:42815"
	deadReq.Header.Set("Content-Type", "application/json")
	dead.ServeHTTP(recorder, deadReq)
	if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "connection refused") {
		t.Fatalf("transport failure = %d/%s, want 502 carrying the transport error", recorder.Code, recorder.Body.String())
	}
}

func TestEmbeddedLightUIHasSavedSettingsSection(t *testing.T) {
	source := string(lightUIHTML)
	for _, fragment := range []string{
		`<h2 id="saved-settings-title">Saved settings</h2>`,
		`<form id="settings-save" class="settings-save-row">`,
		`id="settings-name"`,
		`<ul id="settings-list" class="settings-list"`,
		`data-apply="${escapeHTML(name)}"`,
		`enqueueMutation("settings:save", "/api/settings", {action: "save", name}, false)`,
		"/api/settings", {action: "apply", name: button.dataset.apply}, false)`,
		`renderSettings(data.names || [])`,
		"if (mutation.body.action === \"apply\") refreshLights(true);",
	} {
		if !strings.Contains(source, fragment) {
			t.Fatalf("embedded UI is missing saved-settings fragment %q", fragment)
		}
	}
}
```

- [ ] **Step 2: Run the tests and verify the intended failure**

Run: `go test . -run 'TestUIAPISettings|TestEmbeddedLightUIHasSavedSettingsSection' -count=1`

Expected: FAIL because `ServeHTTP` has no `/api/settings` case (the `/api/` fallthrough answers 404 for GET/POST/PUT, so the 200/400/502 assertions fail with 404/405… actually 404 for every sub-case) and the embedded page contains none of the pinned fragments ("embedded UI is missing saved-settings fragment"). The failure is missing route behavior and missing markup, not a harness accident.

- [ ] **Step 3: Add the production implementation**

`ui.go` — (a) add the types right after the `uiResponse` declaration:

```go
// uiSettingsResponse is the stable JSON shape for /api/settings. Names is
// always a real array ([] rather than null) so the page can render it
// blindly; a daemon failure on the read path degrades in-band through
// Error ("unreachable"), matching the tray's degrade-don't-blank posture.
type uiSettingsResponse struct {
	Names []string `json:"names"`
	Error string   `json:"error,omitempty"`
}

type uiSettingsRequest struct {
	Action string `json:"action"`
	Name   string `json:"name"`
}
```

(b) add the route case inside `ServeHTTP`'s path switch, immediately after the `/api/group` case:

```go
	case "/api/settings":
		switch r.Method {
		case http.MethodGet:
			s.handleSettingsList(w)
		case http.MethodPost:
			if !s.validPostOrigin(r) {
				writeUIJSON(w, http.StatusForbidden, uiResponse{Error: "origin is not allowed"})
				return
			}
			s.handleSettingsAction(w, r)
		default:
			writeUIMethodError(w, "GET, POST")
		}
```

(c) add handlers + parser + builder immediately after `handleGroup`:

```go
func (s *uiServer) handleSettingsList(w http.ResponseWriter) {
	var (
		names     []string
		daemonErr bool
	)
	err := s.dispatcher.sequence(func(call daemonCall) error {
		var fatal error
		names, daemonErr, fatal = queryUISettings(call)
		return fatal
	})
	if err != nil || daemonErr {
		writeUIJSON(w, http.StatusOK, uiSettingsResponse{Names: []string{}, Error: "unreachable"})
		return
	}
	writeUIJSON(w, http.StatusOK, uiSettingsResponse{Names: names})
}

func (s *uiServer) handleSettingsAction(w http.ResponseWriter, r *http.Request) {
	var req uiSettingsRequest
	if err := decodeUIJSON(w, r, &req); err != nil {
		writeUIJSON(w, http.StatusBadRequest, uiSettingsResponse{Names: []string{}, Error: err.Error()})
		return
	}
	command, err := buildSettingsCommand(req)
	if err != nil {
		writeUIJSON(w, http.StatusBadRequest, uiSettingsResponse{Names: []string{}, Error: err.Error()})
		return
	}
	var names []string
	sequenceErr := s.dispatcher.sequence(func(call daemonCall) error {
		reply, err := call(command)
		if err != nil {
			return err
		}
		reply = strings.TrimSpace(reply)
		if strings.HasPrefix(reply, "error:") {
			return errors.New(reply) // 502 below carries the daemon reply verbatim
		}
		var daemonErr bool
		names, daemonErr, err = queryUISettings(call)
		if daemonErr && err == nil {
			names = []string{} // saved, but the list refresh is degraded
		}
		return err
	})
	if sequenceErr != nil {
		writeUIJSON(w, http.StatusBadGateway, uiSettingsResponse{Names: []string{}, Error: sequenceErr.Error()})
		return
	}
	writeUIJSON(w, http.StatusOK, uiSettingsResponse{Names: names})
}

// queryUISettings reads the saved-names list. The wire contract is one name
// per line (names may contain spaces: "movie mode") and an EMPTY string when
// the store is empty; an error: reply is reported as daemonErr, not parsed.
func queryUISettings(call daemonCall) (names []string, daemonErr bool, fatal error) {
	reply, err := call("light settings list")
	if err != nil {
		return nil, false, err
	}
	if strings.HasPrefix(strings.TrimSpace(reply), "error:") {
		return nil, true, nil
	}
	names = parseUISettingsNames(reply)
	if names == nil {
		names = []string{}
	}
	return names, false, nil
}

func parseUISettingsNames(reply string) []string {
	var names []string
	for _, rawLine := range strings.Split(reply, "\n") {
		name := strings.TrimSpace(rawLine)
		if name == "" || strings.HasPrefix(name, "error:") {
			continue
		}
		names = append(names, name)
	}
	return names
}

func buildSettingsCommand(req uiSettingsRequest) (string, error) {
	if req.Action != "save" && req.Action != "apply" {
		return "", fmt.Errorf("unknown settings action %q", req.Action)
	}
	name := strings.TrimSpace(req.Name) // leading/trailing space is never significant
	if name == "" {
		return "", errors.New("settings name is required")
	}
	if strings.ContainsAny(name, "\r\n") {
		return "", errors.New("settings name must not contain a newline")
	}
	return "light settings " + req.Action + " " + name, nil
}
```

`internal/lightui/index.html` — (a) styles, after the `.trim-row` rules (~line 97):

```css
    .settings-save-row { display: flex; gap: 8px; margin-bottom: 14px; }
    .settings-save-row input[type="text"] { flex: 1 1 auto; min-width: 0; min-height: 44px; border: 1px solid var(--line-strong); border-radius: 10px; padding: 9px 13px; background: var(--panel-2); color: var(--text); }
    .settings-list { display: grid; gap: 8px; margin: 0; padding: 0; list-style: none; }
    .settings-item { display: flex; align-items: center; justify-content: space-between; gap: 12px; border: 1px solid var(--line); border-radius: 10px; padding: 8px 12px; background: rgba(255,255,255,.015); }
    .settings-item .settings-name { overflow-wrap: anywhere; }
    .settings-empty { margin: 0 0 4px; }
```

(b) markup, between the gang-controls `</section>` (line 230) and the `<div class="section-title">` for "Individual controls" (line 232):

```html
    <section class="panel" aria-labelledby="saved-settings-title">
      <div class="panel-inner">
        <div class="panel-head">
          <div>
            <p class="eyebrow">Snapshots</p>
            <h2 id="saved-settings-title">Saved settings</h2>
            <p class="subtle">Save the current look of every connected light under one name; Apply restores it on the lights that are connected now.</p>
          </div>
        </div>
        <form id="settings-save" class="settings-save-row">
          <input id="settings-name" type="text" placeholder="movie mode" maxlength="32" autocomplete="off" aria-label="Name for the saved setting">
          <button class="button-primary" type="submit">Save</button>
        </form>
        <p id="settings-empty" class="settings-empty" hidden>No saved settings yet — the daemon keeps them.</p>
        <ul id="settings-list" class="settings-list" aria-live="polite"></ul>
      </div>
    </section>
```

(c) JS inside the existing IIFE — add after `bindCards()`:

```js
      function renderSettings(names) {
        const list = $("settings-list");
        list.innerHTML = names.map((name) => `<li class="settings-item"><span class="settings-name">${escapeHTML(name)}</span><button class="button-quiet" type="button" data-apply="${escapeHTML(name)}">Apply</button></li>`).join("");
        $("settings-empty").hidden = names.length !== 0;
        list.querySelectorAll("[data-apply]").forEach((button) => {
          button.addEventListener("click", () => enqueueMutation(`settings:apply:${button.dataset.apply}`, "/api/settings", {action: "apply", name: button.dataset.apply}, false));
        });
      }

      async function refreshSettings() {
        try {
          const response = await fetch("/api/settings", {cache: "no-store"});
          const data = await response.json();
          renderSettings(data.names || []);
        } catch (error) {
          renderSettings([]);
        }
      }

      function bindSettingsControls() {
        $("settings-save").addEventListener("submit", (event) => {
          event.preventDefault();
          const name = $("settings-name").value.trim();
          if (!name) return;
          enqueueMutation("settings:save", "/api/settings", {action: "save", name}, false);
          $("settings-name").value = "";
        });
      }
```

and extend the existing `mutationQueue` config's `onSuccess` (settings posts go through the same mutation_queue.js discipline as every light control):

```js
        onSuccess: (data, mutation) => {
          if (mutation.endpoint === "/api/settings") {
            renderSettings(data.names || []);
            if (mutation.body.action === "apply") refreshLights(true);
          }
          clearError();
        },
```

and the boot lines become `bindTopControls(); bindSettingsControls(); refreshSettings(); refreshLights(false); window.setInterval(() => refreshLights(true), 750);`. The `data-apply` attribute round-trips the exact name (HTML entities are decoded into `dataset`), so Apply sends the verbatim saved name. Apply triggering `refreshLights(true)` is the existing 750 ms light-status refresh path.

`README.md` — in the **`mutastic ui`** bullet, after the "…the tray's Quit uses it." sentence, append: "The **Saved settings** section snapshots every connected light's current look under a name and restores it with one click: **Save** sends `light settings save <name>`, a name's **Apply** button sends `light settings apply <name>`, and the listed names come from `light settings list` — the daemon owns and persists the store, so the panel and the tray see the same set."

- [ ] **Step 4: Run the focused tests**

Run: `gofmt -l ui.go ui_test.go && go test . -run 'TestUIAPISettings|TestEmbeddedLightUIHasSavedSettingsSection' -count=1`

Expected: PASS (and gofmt prints nothing).

- [ ] **Step 5: Refactor while green**

No refactor needed: the handlers reuse `sequence`/`decodeUIJSON`/`writeUIJSON` verbatim, the page reuses `escapeHTML` and the queue (no new post path was cloned), and `parseUISettingsNames` stays separate only because the empty-reply contract makes it independently unittable.

- [ ] **Step 6: Run impacted-test verification**

The changes live in the root package (`ui.go`, embedded `index.html`, `ui_test.go`, `README.md`). The fragment test pins pinned-fragment style shared with `TestEmbeddedLightUICardsUseTheirOwnIdentityAsTarget`, and the mutation queue config was extended (its Node test runs under `go test .`), so the impacted set is the whole root-package suite: `go test . -count=1`.

Expected: PASS (the whole suite, including `TestMuteAllMeetingsHotkeyContract`, whose stale assertion was already fixed at commit f05b23b in this run before this plan began; nothing here may regress it).

- [ ] **Step 7: Commit the task**

```bash
git add ui.go internal/lightui/index.html ui_test.go README.md && git commit -m "ui: saved light settings section with save/apply via /api/settings"
```

---

### Task 5: Tray dynamic Mute/Unmute menu item

**Files:**
- Modify: `traystate.go` (replace `trayMutedChecked` at :63; rename `trayMicEnabled` at :81; add `trayMuteTitle` after `trayTitle`; `onMicToggle` at :180 → `onMicSet`; `mutedClick` at :199 → `muteClick`)
- Modify: `tray_windows.go` (menu construction at :108, click registration at :186, refresh loop at :219-272; add `sync/atomic` import)
- Test: `traystate_test.go` (rewrite the four pinned tests in Step 3 — full new bodies below)
- Modify: `README.md` (mute path #3 at :25-27; tray bullet prose at :80-84 and :92-94)

**Interfaces:**
- Consumes: the existing `traySpy` harness (`traystate_test.go:94-128`, call-order strings + per-command `script map[string]scriptOutcome`, `levelRecorder` at :384-394 — both unchanged), `trayStateFor`/`trayTitle`/`trayActionsEnabled`/`trayIconFor` (all unchanged), and the fork's per-item `SetTitle`/`Enable`/`Disable` (retitle-in-place precedent: `header.SetTitle(trayTitle(state))` every tick).
- Produces (contract §6, exact signatures):
  ```go
  func trayMuteTitle(s trayState) string   // "Mute" when unmuted, "Unmute" when muted, "Mute/Unmute" when unknown/down
  func trayMuteEnabled(s trayState) bool   // true only for trayStateMuted|trayStateUnmuted (replaces trayMicEnabled)
  func (a *trayActions) onMicSet(verb string)      // absolute "mute"/"unmute" verb + F24 sweep + refresh (replaces onMicToggle)
  func (a *trayActions) muteClick(armed trayState) // probe-gated click (replaces mutedClick)
  ```
  `trayMutedChecked` is DELETED; the item is an action item, never checkable. Click spy orders: armed Mute + matching probe → `ask:status,ask:mute,inject,refresh`; armed Unmute + matching probe → `ask:status,ask:unmute,inject,refresh`; definitive-but-opposite probe or unknown/err probe → WARN log + no-op (`ask:status` only).

- [ ] **Step 1: Rewrite the production code first (the existing pins are the failing driver)**

Contrary to the usual test-first order, here the OLD pinned tests define what currently holds and the NEW pinned tests (Step 3) are the contract being installed; the production rewrite lands first so the Step 2 failure is caused by production behavior/order changes, not by test-authored syntax. In `traystate.go`: delete `trayMutedChecked` entirely; rename `trayMicEnabled` → `trayMuteEnabled` (semantics identical; update its doc comment's "Muted action" → "Mute/Unmute action" and the reference `(mutedClick)` → `(muteClick)`); insert after `trayTitle`:

```go
// trayMuteTitle is the absolute verb the Mute/Unmute action item displays.
// It is always the OPPOSITE of the definitive state: a live mic gets Mute,
// a muted mic gets Unmute. At unknown-or-down the item is also disabled by
// trayMuteEnabled (a disabled item still shows its last-set title), so the
// neutral "Mute/Unmute" keeps a stale directional verb off the screen.
func trayMuteTitle(s trayState) string {
	switch s {
	case trayStateMuted:
		return "Unmute"
	case trayStateUnmuted:
		return "Mute"
	default:
		return "Mute/Unmute"
	}
}
```

Replace `onMicToggle` with the absolute-direction version (same attempted-pair + refresh contract):

```go
// onMicSet is the tray's mute-everything path for an ABSOLUTE direction,
// mirroring the Stream Deck mute key (README): one daemon verb ("mute" or
// "unmute") AND one F24 meeting-app sweep, both attempted even when the
// other fails. (The daemon injects F24 only for physical button presses,
// so a verb-only tray would mute the mic while leaving meeting apps live.)
// Loop-free for the same reasons as before: the AHK sweep never calls a
// mic verb, and the mic's host-command echo (0x20) is ignored by the
// daemon's injector gate.
func (a *trayActions) onMicSet(verb string) {
	reply, askErr := a.ask(verb)
	sweepErr := a.injectSweep()
	if askErr != nil || sweepErr != nil || strings.HasPrefix(reply, "error:") {
		a.logger.Error("mic "+verb+" failed", "daemon_reply", reply, "ask_err", errString(askErr), "sweep_err", errString(sweepErr))
	} else {
		a.logger.Info("mic "+verb, "daemon_reply", reply)
	}
	// The daemon applied its state change before replying; on failure the
	// refresh restores the truthful display within one poll.
	a.signalRefresh()
}
```

Replace `mutedClick` with `muteClick` (armed = the state the title was last drawn from; the Windows glue keeps it, see below):

```go
// muteClick is the Mute/Unmute menu's click entry point. The item's title
// is only the last completed poll's snapshot ("armed"), so a flip or daemon
// restart landing between that poll and the user's click must not fire the
// armed direction against the new truth (toggle defaulted unknown to
// set-mute; absolute verbs always force ONE side). The click re-probes and
// fires the ABSOLUTE verb only when the probe reproduces the armed state;
// a definitive-but-opposite probe, an unknown state, and an unreachable
// daemon all decline with a WARN (the next poll redraws the truthful verb).
func (a *trayActions) muteClick(armed trayState) {
	state := trayStateFor(a.ask("status"))
	if state != armed {
		a.logger.Warn("mute click declined: mic state changed since the menu was drawn", "armed", trayTitle(armed), "probed", trayTitle(state))
		return
	}
	switch state {
	case trayStateUnmuted:
		a.onMicSet("mute")
	case trayStateMuted:
		a.onMicSet("unmute")
	default:
		// armed == state but not definitive (the menu item should have
		// been disabled by trayMuteEnabled): decline rather than guess.
		a.logger.Warn("mute click declined: mic state not definitive", "state", trayTitle(state))
	}
}
```

In `tray_windows.go` (add `"sync/atomic"` to imports): construction line 108 becomes

```go
	mute := systray.AddMenuItem("Mute/Unmute", "mute-everything: mic mute/unmute + F24 meeting-app sweep (same flow as the Stream Deck mute key)")
	systray.AddSeparator()
```

the disable loop at :119-121 lists `mute` instead of `muted`; before the click registrations add

```go
	// muteArmed is the mic state the Mute/Unmute title was last drawn
	// from. The refresh loop stores it every tick; the click handler loads
	// it, and muteClick revalidates it against a fresh status probe.
	muteArmed := &atomic.Int32{}
	muteArmed.Store(int32(trayStateUnknown))
```

click registration becomes `mute.Click(func() { go actions.muteClick(trayState(muteArmed.Load())) })`, and the `trayRefreshLoop` call gains the extra argument. `trayRefreshLoop`'s signature becomes

```go
func trayRefreshLoop(logger *slog.Logger, refreshCh <-chan struct{}, header, mute, lights, brightness, preset *systray.MenuItem, muteArmed *atomic.Int32) {
```

and inside the tick REPLACE the whole `if trayMutedChecked(state) { muted.Check() } else { muted.Uncheck() }` block with

```go
		mute.SetTitle(trayMuteTitle(state))
		muteArmed.Store(int32(state))
```

keep the lights loop unchanged, and replace the final `trayMicEnabled` gate with

```go
		if trayMuteEnabled(state) {
			mute.Enable()
		} else {
			mute.Disable()
		}
```

Also update the loop's header comment: "tooltip, header, checkbox, and enabled states" → "tooltip, header, item titles, and enabled states" (no checkbox remains). Call site: `go trayRefreshLoop(logger, refreshCh, header, mute, lights, brightness, preset, muteArmed)`.

- [ ] **Step 2: Run the focused suite and verify the old pins fail**

Run: `go test . -run 'TestTrayDisplayDecisions|TestMuteClickRevalidates|TestTrayMicToggleIsMuteEverything' -count=1`

Expected: FAIL driven by the production change — the test package no longer builds because the rewritten production code intentionally deleted `trayMutedChecked` and renamed `trayMicEnabled`/`mutedClick`/`onMicToggle` which `TestTrayDisplayDecisions`, `TestMutedClickRevalidates`, and `TestTrayMicToggleIsMuteEverything` still reference, AND the surviving old expectations pin the toggle-based order `ask:status,ask:toggle,inject,refresh` where the new behavior fires an absolute verb (`ask:status,ask:unmute,inject,refresh` for the muted-probe case). This is the designed signal: the four rewritten tests in Step 3 now define the contract. (The regex intentionally names `TestMuteClickRevalidates` — the renamed test installed in Step 3.)

- [ ] **Step 3: Rewrite the four pinned tests (complete new bodies)**

In `traystate_test.go`: replace `TestTrayDisplayDecisions` with

```go
func TestTrayDisplayDecisions(t *testing.T) {
	if trayTitle(trayStateMuted) != "Mutastic — muted" {
		t.Errorf("muted title = %q", trayTitle(trayStateMuted))
	}
	if trayTitle(trayStateUnmuted) != "Mutastic — live" {
		t.Errorf("unmuted title = %q", trayTitle(trayStateUnmuted))
	}
	if trayTitle(trayStateUnknown) != "Mutastic — mic state unknown" {
		t.Errorf("unknown title = %q", trayTitle(trayStateUnknown))
	}
	if trayTitle(trayStateDown) != "Mutastic — daemon unreachable" {
		t.Errorf("down title = %q", trayTitle(trayStateDown))
	}
	// The action item's verb is the OPPOSITE of the definitive state; the
	// neutral fallback covers the states the enabled gate refuses (a
	// disabled item still displays its last-set title).
	if trayMuteTitle(trayStateUnmuted) != "Mute" {
		t.Errorf("mute title for a live mic = %q, want %q", trayMuteTitle(trayStateUnmuted), "Mute")
	}
	if trayMuteTitle(trayStateMuted) != "Unmute" {
		t.Errorf("mute title for a muted mic = %q, want %q", trayMuteTitle(trayStateMuted), "Unmute")
	}
	for _, s := range []trayState{trayStateUnknown, trayStateDown} {
		if trayMuteTitle(s) != "Mute/Unmute" {
			t.Errorf("mute title in state %v = %q, want %q", s, trayMuteTitle(s), "Mute/Unmute")
		}
	}
	if trayActionsEnabled(trayStateDown) {
		t.Error("actions must be disabled while the daemon is unreachable")
	}
	// Light actions arm on any daemon answer, including unknown (unknown is
	// a mic-state concept, not a reachability one).
	for _, s := range []trayState{trayStateMuted, trayStateUnmuted, trayStateUnknown} {
		if !trayActionsEnabled(s) {
			t.Errorf("actions must be enabled in state %v", s)
		}
	}
	// The mic action arms only on definitive answers; light actions arm on
	// any daemon answer including unknown.
	if trayMuteEnabled(trayStateUnknown) || trayMuteEnabled(trayStateDown) {
		t.Error("mute action must stay disabled at unknown/down (a stale directional verb can desync the F24 sweep)")
	}
	if !trayMuteEnabled(trayStateMuted) || !trayMuteEnabled(trayStateUnmuted) {
		t.Error("mute action must be armed at definitive answers")
	}
}
```

replace `TestTrayMicToggleIsMuteEverything` with

```go
// TestTrayMicToggleIsMuteEverything mirrors the Stream Deck mute key: one
// daemon verb AND the F24 meeting-app sweep, then a refresh - and the
// sweep must fire even when the daemon round trip fails (a verb-only path
// would mute the mic while leaving meeting apps live). The verb is an
// ABSOLUTE direction (mute/unmute picked from the armed title), never the
// ambiguous toggle.
func TestTrayMicToggleIsMuteEverything(t *testing.T) {
	for _, verb := range []string{"mute", "unmute"} {
		spy := &traySpy{}
		spy.actions().onMicSet(verb)
		if got := spy.order(); got != "ask:"+verb+",inject,refresh" {
			t.Fatalf("onMicSet(%q) side effects = %q, want %q", verb, got, "ask:"+verb+",inject,refresh")
		}

		failing := &traySpy{askErr: errors.New("daemon dead")}
		failing.actions().onMicSet(verb)
		if got := failing.order(); got != "ask:"+verb+",inject,refresh" {
			t.Fatalf("onMicSet(%q) with a dead daemon = %q, want %q (the sweep must still fire)", verb, got, "ask:"+verb+",inject,refresh")
		}
	}
}
```

replace `TestMutedClickRevalidates` with the renamed `TestMuteClickRevalidates`:

```go
// TestMuteClickRevalidates pins the action-time revalidation of the
// Mute/Unmute click. The item's title is only the last completed poll's
// snapshot ("armed"), so a flip or daemon restart landing between that
// poll and the user's click must not fire the armed direction against the
// new truth: the click re-probes and fires the ABSOLUTE verb only when the
// probe reproduces the armed state, running the full mute-everything pair
// exactly once (verb, F24 sweep, refresh - no extra probes).
func TestMuteClickRevalidates(t *testing.T) {
	// Armed Mute (live mic at the last poll) and the probe agrees.
	spy := &traySpy{script: map[string]scriptOutcome{
		"status": {reply: "unmuted"},
		"mute":   {reply: "muted"},
	}}
	spy.actions().muteClick(trayStateUnmuted)
	if got := spy.order(); got != "ask:status,ask:mute,inject,refresh" {
		t.Fatalf("muteClick armed Mute with a matching probe = %q, want %q", got, "ask:status,ask:mute,inject,refresh")
	}

	// Armed Unmute (muted mic at the last poll) and the probe agrees.
	unmuted := &traySpy{script: map[string]scriptOutcome{
		"status": {reply: "muted"},
		"unmute": {reply: "unmuted"},
	}}
	unmuted.actions().muteClick(trayStateMuted)
	if got := unmuted.order(); got != "ask:status,ask:unmute,inject,refresh" {
		t.Fatalf("muteClick armed Unmute with a matching probe = %q, want %q", got, "ask:status,ask:unmute,inject,refresh")
	}

	// Definitive-but-opposite probe (the mic flipped between poll and
	// click): decline with a WARN - the next poll draws the new title and
	// the user's next click fires it.
	flipLevels := &levelRecorder{}
	flipped := &traySpy{script: map[string]scriptOutcome{
		"status": {reply: "muted"},
	}}
	a := flipped.actions()
	a.logger = slog.New(flipLevels)
	a.muteClick(trayStateUnmuted)
	if got := flipped.order(); got != "ask:status" {
		t.Fatalf("muteClick with an opposite probe = %q, want %q (probe only, no verb/sweep)", got, "ask:status")
	}
	if len(flipLevels.levels) != 1 || flipLevels.levels[0] != slog.LevelWarn {
		t.Fatalf("levels = %v, want [WARN]", flipLevels.levels)
	}

	// Unknown probe answer: decline with a WARN, no verb, no sweep.
	levels := &levelRecorder{}
	unknown := &traySpy{script: map[string]scriptOutcome{
		"status": {reply: "unknown"},
	}}
	a = unknown.actions()
	a.logger = slog.New(levels)
	a.muteClick(trayStateUnmuted)
	if got := unknown.order(); got != "ask:status" {
		t.Fatalf("muteClick with an unknown probe = %q, want %q (probe only, no verb/sweep)", got, "ask:status")
	}
	if len(levels.levels) != 1 || levels.levels[0] != slog.LevelWarn {
		t.Fatalf("levels = %v, want [WARN] (a declined definitive-check is not an error)", levels.levels)
	}

	// Daemon down (its UDP port refused the datagram: nothing is
	// listening): decline, again without touching the verb or the sweep.
	downLevels := &levelRecorder{}
	down := &traySpy{script: map[string]scriptOutcome{
		"status": {err: fmt.Errorf("%w: %w", errNoReply, syscall.ECONNREFUSED)},
	}}
	a = down.actions()
	a.logger = slog.New(downLevels)
	a.muteClick(trayStateUnmuted)
	if got := down.order(); got != "ask:status" {
		t.Fatalf("muteClick with the daemon down = %q, want %q (probe attempt only)", got, "ask:status")
	}
	if len(downLevels.levels) != 1 || downLevels.levels[0] != slog.LevelWarn {
		t.Fatalf("levels = %v, want [WARN]", downLevels.levels)
	}
}
```

and in `TestLogSeverityClassifiesFailures`, change the line `a.onMicToggle()` to `a.onMicSet("mute")` and the final message tail `(toggle error + per-line fleet error)` to `(mute error + per-line fleet error)` — nothing else in that test changes.

- [ ] **Step 4: Run the focused tests**

Run: `gofmt -l traystate.go traystate_test.go && go test . -run 'TestTrayDisplayDecisions|TestMuteClickRevalidates|TestTrayMicToggleIsMuteEverything' -count=1`

Expected: PASS (gofmt silent). Also confirm no stragglers: `rg -n "mutedClick|trayMutedChecked|trayMicEnabled|onMicToggle" --glob '!docs/**' .` prints nothing outside `docs/` (historical plan docs stay as history).

- [ ] **Step 5: Refactor while green (docs ship with the behavior)**

`README.md`, mute path #3 (:25-27): replace the `**Tray icon Muted menu**` entry with: `3. **Tray icon Mute/Unmute menu** — the tray's Mute/Unmute action runs the same mute-everything pair through the daemon (always the absolute verb the item currently displays), and only after re-checking that the mic still sits in the state that armed that verb (it declines rather than guess at `+"`unknown`"+` or fire a verb whose direction flipped since the last poll).` In the tray bullet (:80-84) replace "a synced **Muted** check item (mute-everything — mic toggle plus the F24 meeting-app sweep, the same in-process flow as the Stream Deck mute key; both halves are attempted on every click and any failure is logged)," with: "a dynamic **Mute**/**Unmute** action item whose label is always the absolute verb for the last definitive mic state (**Mute** when live, **Unmute** when muted; the click re-checks the state before firing, so a stale label can never fire the wrong direction — mute-everything: the mic verb plus the F24 meeting-app sweep, the same in-process flow as the Stream Deck mute key; both halves are attempted on every click and any failure is logged)," and (:92-94) replace "while the mic state is *unknown* the **Muted** item stays gray" with "while the mic state is *unknown* the item reads **Mute/Unmute** and stays gray". No code refactor: the pure/glue split already matches the repo pattern (`trayMuteTitle`/`trayMuteEnabled` tested on Linux; titles/enable re-asserted every tick like `header.SetTitle`).

- [ ] **Step 6: Run impacted-test verification**

`tray_windows.go` is Windows-only and never compiled by the Linux gate, so `bash build.sh` is its only compile gate and is mandatory here; the Linux suite covers the pure side plus every root-package consumer (`traySpy` harness users, `TestRunTrayUnsupportedOffWindows`). Run:

```bash
bash build.sh && go build ./... && go vet ./... && go test . ./internal/... -count=1
```

Expected: `bin/mutastic.exe` produced (windows/amd64, CGO), and the full suite PASSes with zero exceptions (the formerly red `TestMuteAllMeetingsHotkeyContract` was fixed at f05b23b before this plan began and must stay green).

- [ ] **Step 7: Commit the task**

```bash
git add traystate.go traystate_test.go tray_windows.go README.md && git commit -m "tray: dynamic Mute/Unmute action item (absolute verb per state, sweep preserved)"
```

### Task 6: Tray Saved settings submenu (polled, diff-rendered)

**Files:**
- Modify: `traystate.go` (append the new pure API after `trayIconFor`, before the `trayActions` block)
- Test: `traystate_test.go` (add `"reflect"` to the import list; append three tests)
- Modify: `tray_windows.go` (submenu construction after the `preset := …` line at :113; call site at :200; `trayRefreshLoop` body + new `syncSavedSettingsMenu` helper)
- Modify: `README.md` (menu enumeration in the `mutastic tray` bullet, :84-85)

**Interfaces:**
- Consumes: `askDaemon(command, udpAddr, lightClientTimeout)` (deckplugin.go:40-56; 2048-byte buffer, multi-line replies normal); the systray fork's `(*MenuItem).AddSubMenuItem/Hide/SetTitle/Click/Enable/Disable` (children append in creation-id order, no remove/insert API — verified in systray.go:191-195, systray_windows.go:783-793); Task 2's `logCommand` latch that keeps `"light settings list"` quiet in daemon.log (contract §4); Task 1's wire contract `settings list` → sorted names newline-joined, `""` = none (contract §3); Task 5's rewritten `trayRefreshLoop` mic-item handling (`trayMuteTitle`/`trayMuteEnabled`, no Check/Uncheck — contract §6).
- Produces:
  ```go
  type trayMenuSpec struct { Title string; Enabled bool }
  func traySavedSettings(names []string, daemonOK bool) []trayMenuSpec
  func trayParseSettingsList(reply string, err error) (names []string, daemonOK bool)
  const traySavedSettingsListCmd = "light settings list"
  func traySameMenuSpecs(a, b []trayMenuSpec) bool
  func syncSavedSettingsMenu(parent *systray.MenuItem, children []*systray.MenuItem, cached, want []trayMenuSpec, lightCmd func(string) func()) ([]*systray.MenuItem, []trayMenuSpec) // Windows-only, tray_windows.go
  ```

- [ ] **Step 1: Write the failing tests**

Append to `traystate_test.go` (and add `"reflect"` to its import block):

```go
// TestTraySavedSettingsMenuSpec pins the three display regimes of the
// Saved settings submenu: daemon down (one disabled unreachable
// placeholder), daemon up with an empty store (one disabled none-saved
// placeholder), and saved names (one enabled item per name, input order).
// The placeholder strings are tray-side UI text; the daemon never sends
// them (the wire contract for "none" is the empty reply).
func TestTraySavedSettingsMenuSpec(t *testing.T) {
	down := traySavedSettings(nil, false)
	if want := []trayMenuSpec{{Title: "(daemon unreachable)", Enabled: false}}; !reflect.DeepEqual(down, want) {
		t.Errorf("down spec = %+v, want %+v", down, want)
	}
	empty := traySavedSettings(nil, true)
	if want := []trayMenuSpec{{Title: "(no saved settings)", Enabled: false}}; !reflect.DeepEqual(empty, want) {
		t.Errorf("empty-store spec = %+v, want %+v", empty, want)
	}
	named := traySavedSettings([]string{"desk day", "movie mode"}, true)
	want := []trayMenuSpec{{Title: "desk day", Enabled: true}, {Title: "movie mode", Enabled: true}}
	if !reflect.DeepEqual(named, want) {
		t.Errorf("named spec = %+v, want %+v (one enabled item per name, input order)", named, want)
	}
}

// TestTrayParseSettingsList pins the wire contract of the daemon's
// "light settings list" reply: an empty string means "none saved" (NOT
// an error and NOT unreachable), names are newline-joined, and any ask
// error means the daemon is unreachable for menu purposes.
func TestTrayParseSettingsList(t *testing.T) {
	names, ok := trayParseSettingsList("", nil)
	if !ok || len(names) != 0 {
		t.Errorf(`trayParseSettingsList("", nil) = (%v, %v), want (empty, true): the empty reply is the wire contract for none`, names, ok)
	}
	names, ok = trayParseSettingsList("a\nb", nil)
	if !ok || !reflect.DeepEqual(names, []string{"a", "b"}) {
		t.Errorf("trayParseSettingsList(\"a\\nb\", nil) = (%v, %v), want ([a b], true)", names, ok)
	}
	names, ok = trayParseSettingsList("", errors.New("no reply"))
	if ok || names != nil {
		t.Errorf("trayParseSettingsList with an ask error = (%v, %v), want (nil, false)", names, ok)
	}
}

// TestTraySameMenuSpecs pins the compare-by-title rule the refresh loop
// uses to decide a submenu rebuild: within every regime equal titles
// imply equal enabled bits, so titles fully determine the rendering.
func TestTraySameMenuSpecs(t *testing.T) {
	a := []trayMenuSpec{{Title: "one", Enabled: true}, {Title: "two", Enabled: true}}
	if !traySameMenuSpecs(a, a) {
		t.Error("same list must compare equal")
	}
	if !traySameMenuSpecs(nil, nil) {
		t.Error("two empty lists (state before the first poll) must compare equal")
	}
	b := []trayMenuSpec{{Title: "one", Enabled: true}, {Title: "renamed", Enabled: true}}
	if traySameMenuSpecs(a, b) {
		t.Error("a changed title must compare different (triggers a rebuild)")
	}
	if traySameMenuSpecs(a, []trayMenuSpec{{Title: "one", Enabled: true}}) {
		t.Error("different lengths must compare different")
	}
}
```

- [ ] **Step 2: Run the tests and verify the intended failure**

Run: `go test . -run 'TestTraySavedSettingsMenuSpec|TestTrayParseSettingsList|TestTraySameMenuSpecs' -count=1`

Expected: FAIL to compile with `undefined: traySavedSettings`, `undefined: trayMenuSpec`, `undefined: trayParseSettingsList`, `undefined: traySameMenuSpecs` — the pure functions do not exist yet.

- [ ] **Step 3: Add the production implementation**

Append to `traystate.go` after `trayIconFor` (`strings` is already imported):

```go
// trayMenuSpec is one rendered menu-entry decision: title plus enabled
// bit. Pure data so the Windows glue (tray_windows.go) can reconcile a
// submenu without any decision logic of its own.
type trayMenuSpec struct {
	Title   string
	Enabled bool
}

// traySavedSettingsListCmd is the daemon command the tray's refresh loop
// polls once per tick to learn the saved setting names. daemon.go's
// logCommand latch (Task 2) keeps this resident poll out of daemon.log.
const traySavedSettingsListCmd = "light settings list"

// trayParseSettingsList turns one daemon round trip for
// traySavedSettingsListCmd into (names, daemonOK). The wire contract:
// an EMPTY reply means "no settings saved" (still daemon-OK), names are
// newline-joined (the daemon emits them sorted), and ANY ask error means
// the daemon is unreachable for menu purposes.
func trayParseSettingsList(reply string, err error) ([]string, bool) {
	if err != nil {
		return nil, false
	}
	if reply == "" {
		return nil, true
	}
	return strings.Split(reply, "\n"), true
}

// traySavedSettings decides the Saved settings submenu's children from
// the freshly polled name list. The placeholder strings are tray-side UI
// text, never daemon replies: a down daemon shows one disabled
// "(daemon unreachable)", an empty store shows one disabled
// "(no saved settings)", and real names become enabled items in input
// order.
func traySavedSettings(names []string, daemonOK bool) []trayMenuSpec {
	if !daemonOK {
		return []trayMenuSpec{{Title: "(daemon unreachable)", Enabled: false}}
	}
	if len(names) == 0 {
		return []trayMenuSpec{{Title: "(no saved settings)", Enabled: false}}
	}
	specs := make([]trayMenuSpec, len(names))
	for i, name := range names {
		specs[i] = trayMenuSpec{Title: name, Enabled: true}
	}
	return specs
}

// traySameMenuSpecs compares two spec lists by TITLE only: within every
// regime equal titles imply equal enabled bits, so titles fully
// determine what the submenu renders. The refresh loop rebuilds the
// submenu children exactly when this reports false.
func traySameMenuSpecs(a, b []trayMenuSpec) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Title != b[i].Title {
			return false
		}
	}
	return true
}
```

- [ ] **Step 4: Run the focused tests**

Run: `go test . -run 'TestTraySavedSettingsMenuSpec|TestTrayParseSettingsList|TestTraySameMenuSpecs' -count=1`

Expected: PASS

- [ ] **Step 5: Wire the Windows glue and the README menu enumeration**

No refactor needed on the pure side — the four new symbols are each one screenful with one job. This step applies on top of Task 5's rewrite of the mic item (`trayMutedChecked` and the Check/Uncheck block are already gone; the mic item is re-titled and re-gated each tick). Keep Task 5's binding names for items it owns; the only new names introduced here are `saved`, `savedChildren`, `savedSpecs`, and `syncSavedSettingsMenu`.

In `tray_windows.go`, in `trayOnReady`, insert immediately after the `preset := systray.AddMenuItem("Light preset", "apply a preset on all lights")` line (top-level items render in creation order, so this lands between "Light preset" and the separator before "Light panel…"):

```go
	saved := systray.AddMenuItem("Saved settings", "saved named light settings, polled from the daemon every 2 s — click a name to apply it")
```

Do NOT add `saved` to the startup `Disable()` loop or to the refresh loop's `trayActionsEnabled` gating: the parent stays clickable so the grayed `(daemon unreachable)`/`(no saved settings)` placeholder children are visible; the children carry the enabled state.

Update the refresh-loop call site (was :200) to pass the new item and the command factory: `go trayRefreshLoop(logger, refreshCh, header, <Task 5's mic item>, lights, brightness, preset, saved, lightCmd)`. Replace `trayRefreshLoop` with this complete version (the mic lines shown are Task 5's contract §6 handling; the new lines are the settings poll + reconcile right after the status ask):

```go
// trayRefreshLoop owns all tray-visible state. Each signal triggers one
// daemon status round trip plus one "light settings list" round trip;
// every display decision comes from the pure traystate.go mappings.
// Convergence is intentional: tooltip, header, mic title, enabled
// states, and the settings submenu carry no handle cost and are
// re-asserted on every signal, so a transient failure heals on the next
// tick - the transition gate only decides logging and icon reapplies
// (SetIcon leaks a GDI handle per call; see trayIconReapplyEvery). The
// icon switches only on definitive answers (trayIconFor): unknown or
// unreachable keeps the last icon.
func trayRefreshLoop(logger *slog.Logger, refreshCh <-chan struct{}, header, mic, lights, brightness, preset, saved *systray.MenuItem, lightCmd func(string) func()) {
	first := true
	last := trayStateUnknown
	tick := 0
	// lastShown is the icon currently displayed (see the pre-Task-6
	// comment block above the loop for the full self-heal rationale).
	lastShown := trayIconUnknown
	// Saved settings submenu reconciliation state: the fork appends
	// children in creation-id order and has no remove/insert, so the glue
	// keeps an ordered slice of child pointers plus the spec list they
	// render, and rebuilds only when titles differ.
	var savedChildren []*systray.MenuItem
	var savedSpecs []trayMenuSpec
	for range refreshCh {
		tick++
		reply, err := askDaemon("status", udpAddr, lightClientTimeout)
		state := trayStateFor(reply, err)
		listReply, listErr := askDaemon(traySavedSettingsListCmd, udpAddr, lightClientTimeout)
		savedNames, savedOK := trayParseSettingsList(listReply, listErr)
		savedChildren, savedSpecs = syncSavedSettingsMenu(saved, savedChildren, savedSpecs, traySavedSettings(savedNames, savedOK), lightCmd)
		change := first || state != last
		if change {
			first = false
			last = state
			logger.Info("status display", "title", trayTitle(state))
		}
		if change || tick <= 3 || tick%trayIconReapplyEvery == 0 {
			switch trayIconFor(state) {
			case trayIconMutedMic:
				lastShown = trayIconMuted
			case trayIconLiveMic:
				lastShown = trayIconLive
			} // trayIconKeep: reassert the currently displayed icon
			systray.SetIcon(lastShown)
		}
		systray.SetTooltip(trayTitle(state))
		header.SetTitle(trayTitle(state))
		// Task 5: the mic item is an action item - retitle it to the exact
		// action a click performs, and gate it on definitive answers only.
		mic.SetTitle(trayMuteTitle(state))
		if trayMuteEnabled(state) {
			mic.Enable()
		} else {
			mic.Disable()
		}
		// Light actions only need a reachable daemon (unknown is a
		// mic-state concept, not a reachability one).
		for _, it := range []*systray.MenuItem{lights, brightness, preset} {
			if trayActionsEnabled(state) {
				it.Enable()
			} else {
				it.Disable()
			}
		}
	}
}

// syncSavedSettingsMenu reconciles the Saved settings submenu against
// the freshly polled spec list. The fork has no remove/insert (only
// Hide/Show, and children append in creation-id order), so the
// reconciliation is a full rebuild whenever titles differ: hide every
// existing child, then add the wanted entries in spec order. Clicks on
// enabled names enqueue "light settings apply <name>" on the same
// ordered light-command channel the other light menu items use (drained
// into actions.onLight, which logs "light command"); placeholders are
// disabled. Hidden orphans keep their ids and click bindings but are
// never displayed again; they accumulate only on name-list churn and are
// the deliberate price of the fork having no delete API.
func syncSavedSettingsMenu(parent *systray.MenuItem, children []*systray.MenuItem, cached, want []trayMenuSpec, lightCmd func(string) func()) ([]*systray.MenuItem, []trayMenuSpec) {
	if traySameMenuSpecs(cached, want) {
		return children, cached
	}
	for _, child := range children {
		child.Hide()
	}
	children = children[:0]
	for _, spec := range want {
		title := spec.Title
		child := parent.AddSubMenuItem(title, "")
		if spec.Enabled {
			child.Click(lightCmd("light settings apply " + title))
		} else {
			child.Disable()
		}
		children = append(children, child)
	}
	return children, want
}
```

README.md: in the `mutastic tray` bullet's menu enumeration, replace the exact substring `**Light preset**, **Light panel…**` with:

```text
**Light preset**, **Saved settings** (the daemon's saved named light settings, polled every 2 s; click a name to apply it), **Light panel…**
```

Glue verification note: Linux cannot drive systray (the fork's message loop is Windows-only and `tray_windows.go` is behind `//go:build windows`), so the menu-object reconciliation is verified by construction — every decision (spec computation, parse, title-compare) is a tested pure function, and `syncSavedSettingsMenu` is mechanical Hide/AddSubMenuItem/Click over the tested spec list — plus the `build.sh` cross-compile in Step 6, which is the only compile gate for `*_windows.go`.

- [ ] **Step 6: Run impacted-test verification**

`traystate.go` is root-package, so the impacted set is the root package tests; `tray_windows.go` is covered only by the cross-build; internal packages are untouched but cheap, so run them too per the task gate.

Run: `go test . ./internal/... -count=1`

Expected: PASS with zero exceptions (`TestMuteAllMeetingsHotkeyContract` was fixed at f05b23b before this plan began; it must remain green via its new `#NoTrayIcon` assertion).

Run: `bash build.sh`

Expected: prints `built bin/mutastic.exe` (proves the modified `tray_windows.go` cross-compiles).

- [ ] **Step 7: Commit the task**

```bash
git add traystate.go traystate_test.go tray_windows.go README.md && git commit -m "tray: Saved settings submenu polled from daemon (apply on click)"
```

---

### Task 7: Docs + smoke runbook + final gate

**Files:**
- Modify: `README.md` (final consolidated user docs: saved named light settings from web UI + tray + CLI, mic card, dynamic Mute/Unmute item)
- Modify: `docs/plans/2026-08-14-tray-icon.md` (append steps 12-18 to `## Windows smoke verification (manual, on deploy)`, after step 11)

**Interfaces:**
- Consumes: Task 1's CLI verbs (`settings save|list|apply`, pass-through, no main.go change); Tasks 3-4's web UI cards (Mic card + Settings card and their routes); Task 5's dynamic tray mute item prose anchors; Task 6's Saved settings submenu. Earlier tasks left their own README fragments — this task's replacements are FINAL wording: where a Task 3-6 edit touched the same sentence, replace that fragment rather than duplicating.
- Produces: no code interfaces; terminal user documentation and the manual smoke gate.

- [ ] **Step 1: Consolidate the README user docs**

Three edits. (a) In the Light commands table, immediately after the `` `mutastic light list` `` row, insert:

```markdown
  | `mutastic light settings save <name>` | snapshot every connected light's on/brightness/temp under `<name>` (quote names containing spaces: `mutastic.exe light settings save "movie mode"`); saving an existing name overwrites it |
  | `mutastic light settings list` | saved setting names, one per line; an empty reply means none are saved |
  | `mutastic light settings apply <name>` | restore a saved setting onto the lights it names (unknown name → `error: unknown setting "<name>"`; a saved light that is no longer connected is reported and skipped) |
```

In the notes sentence after the table that reads `Names persist in `%LOCALAPPDATA%\mutastic\light-names.json`; per-light state in `light-state-<COMx>.json`.` append: ` Saved named settings persist in `light-settings.json` (same directory; delete it to clear all saved names).`

(b) In the `mutastic tray` bullet, the menu enumeration's final wording is (replace the Task 5/6 fragments covering the same items): `a dynamic **Mute**/**Unmute** action item (mute-everything — the mic plus the F24 meeting-app sweep, the same in-process flow as the Stream Deck mute key; the label always names the exact action a click performs, the click re-checks the mic state first, and at unknown state the item stays gray), **Toggle lights**, **Brightness** (applied in click order), **Light preset**, **Saved settings** (the daemon's saved named light settings — the same names the web UI saves — refreshed every 2 s; click a name to apply it; a grayed `(no saved settings)`/`(daemon unreachable)` placeholder appears when appropriate), **Light panel…**, and **Quit**`. Also remove any remaining "**Muted** check item" phrasing so the tray bullet describes the action item only.

(c) In the `mutastic ui` bullet, after the `/api/shutdown` sentence, append: ` The page also has a **Mic** card (live muted/unmuted status with **Mute**/**Unmute**/**Toggle** buttons; these change the mic only — no F24 meeting-app sweep, unlike the tray item and the Stream Deck mute key) and a **Settings** card (the daemon's saved named light settings: type a name and **Save** to snapshot every connected light's current on/brightness/temp; click a listed name to apply it).`

There is no failing test to write first: no Go test pins README prose (deploy_test.go pins only the startup command set and AHK contents; ui_test.go pins embedded index.html fragments, which Task 7 does not touch) — the verification of these docs is the manual runbook of Step 2 plus the final gate.

- [ ] **Step 2: Append the Windows smoke runbook steps**

Append exactly this block to `docs/plans/2026-08-14-tray-icon.md`, continuing the numbered list under `## Windows smoke verification (manual, on deploy)` after step 11 (daemons are running again from step 11's relaunch):

```markdown
12. **Save a named setting in the web UI:** set the lights to a distinctive look (e.g. tray **Light preset → candle**), open the panel (left-click the tray icon), type `smoke test` into the **Settings** card's name field, and click **Save** — `smoke test` appears in the card's saved list.
13. **The tray picks the name up:** right-click the tray icon and open **Saved settings** — within ~2 s (the tray polls `light settings list` on its normal tick) the submenu lists **smoke test** as an enabled item.
14. **Apply from the tray menu:** change the lights (tray **Toggle lights** or another preset), then click **Saved settings → smoke test** — the lights return to the saved look, and `%LOCALAPPDATA%\mutastic\tray.log` gains an INFO line with `"msg":"light command"` and `"cmd":"light settings apply smoke test"`.
15. **Apply from the web UI:** change the lights again, then click **smoke test** in the panel's **Settings** card — the lights restore and the light cards refresh on the same poll.
16. **Mic card:** the panel's **Mic** card shows the current muted/unmuted status; **Mute** mutes, **Unmute** unmutes, **Toggle** flips, and the status follows within one poll. These change the mic ONLY — no F24 meeting-app sweep — so with a meeting app open, confirm the apps do NOT change (the tray item and Stream Deck key remain the mute-everything paths).
17. **Dynamic tray mute item:** with the mic live the tray item reads **Mute** — click it and the mic (plus any open meeting app) mutes; once muted the item reads **Unmute** — click it and the mic (and apps) unmute. The label always names the exact action the click performs.
18. **Quit cascade:** menu **Quit** — the icon disappears; `C:\Users\dan\code\mutastic-deploy\mutastic.exe status` prints `error: no daemon reachable`; `curl http://127.0.0.1:42815/api/health` is refused (`curl: (7) Failed to connect`); `tasklist /FI "IMAGENAME eq mutastic.exe"` prints `INFO: No tasks are running which match the specified criteria.` (daemon, UI server, and tray all stopped — one click, everything down). Relaunch via the `Mutastic Daemon` startup shortcut and confirm the icon returns.
```

- [ ] **Step 3: Final gate — build**

Run: `go build ./...` (from the worktree root)

Expected: PASS (no output, exit 0)

- [ ] **Step 4: Final gate — full test suite**

Run: `go test ./... -count=1` (from the worktree root)

Expected: PASS for every package with zero exceptions (`TestMuteAllMeetingsHotkeyContract` was already fixed at f05b23b in this run before this plan began; its new `#NoTrayIcon` assertion must stay green). Any failure must be fixed before committing.

- [ ] **Step 5: Final gate — vet**

Run: `go vet ./...`

Expected: PASS (silent, exit 0)

- [ ] **Step 6: Final gate — Windows cross-build**

Run: `bash build.sh`

Expected: prints `built bin/mutastic.exe` — the only compile gate covering the Windows-only tray/mic/inject/browser files.

- [ ] **Step 7: Commit the task**

```bash
git add README.md docs/plans/2026-08-14-tray-icon.md && git commit -m "docs: saved settings, mic card, dynamic mute item; extend Windows smoke runbook"
```
