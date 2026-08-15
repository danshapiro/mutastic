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
- Saved-settings list size is uncapped by design (YAGNI): at the 43-byte name cap at most ~46 newline-joined names fit one 2048-byte `askDaemon` reply buffer (120+ at realistic name lengths), and a settings menu that long is unusable anyway — no `settings list` chunking verb is planned.
- The daemon replies use exact wire shapes documented in the contract.

---

## Plan status

This plan is scaffolding: it fixes intent, interfaces, wire contracts, and test coverage — not implementation. After this plan's independent review passes, the code and tests landed in the per-task commits are the sole source of truth. Intent-level deviations discovered during execution are recorded in this section; the plan is not byte-synced to shipped code.

### Task 1: Daemon-side saved light settings store and verbs

Both new surfaces (web UI, tray) are pure UDP clients, so the daemon owns the store: a `SettingsStore` beside the `Registry` inside `MultiManager`, reached through three new `settings` sub-verbs on the existing `light` command path. CLI verbatim light-args pass-through needs no `main.go` change; a `settings delete` verb is deliberately out of scope (YAGNI).

**Files:**
- Create: `internal/light/settings.go`
- Test: `internal/light/settings_test.go` (uses the existing `fakeFleet` + `newTestMulti` harnesses)
- Modify: `internal/light/multi.go` (add the `settings` field; construct the store from `stateDir` inside `NewMultiManager`; add the `settings` case to the `HandleCommand` switch; append the new handlers after `handleDelta`)

**Interfaces:**
- Consumes: `Registry.Resolve`/`Registry.NameFor` (names.go); per-Manager `state.Status()`/`state.TargetOn()` (state.go); `Manager.HandleCommand` (manager.go); fleet helpers `callLight` + `label` and the `handleAll` fan-out pattern (multi.go); `ByteToKelvin` (frame.go); the `Registry.saveLocked` persistence idiom (non-atomic `json.Marshal` + `os.MkdirAll` + `os.WriteFile(.., 0o644)`, tolerate-corrupt load).
- Produces (fixed by contract §1):

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

- Produces (wire verbs, contract §3 — literal replies):
  - `settings save <name>` → `saved "<name>" (N lights)`
  - `settings list` → sorted names newline-joined; empty store → `""` (the wire contract for "none")
  - `settings apply <name>` → one line per entry in the fleet fan-out shape `COM4 desk: on 47% 2900K`; failure shapes `error: light "<key>": unreachable, skipped`, `error: unknown setting "<name>"`, `error: no lights connected`
  - Validation/disabled: `error: invalid settings name`, `error: settings name too long (max 43 bytes)`, `error: settings persistence disabled`, plus a usage error for malformed sub-verbs.

**Behavior:**
1. Persistence: the store lives at `<stateDir>/light-settings.json` beside the existing registry/state files, mirroring names.go conventions (plain `json.Marshal` + `MkdirAll` + `os.WriteFile` 0644, non-atomic, in-memory mutex); a missing or corrupt file silently defaults to empty; an empty `stateDir` disables the store, wired inside `NewMultiManager` with NO signature change.
2. Save: `settings save <name>` snapshots every currently-connected light from memory-only state reads (`Status()`/`TargetOn()`) so a wedged light cannot stall the UDP loop, keying each entry by the registry name when the light is named else its COM port path; an off light saves its restore-target brightness, not 0. Saving an existing name overwrites it; reply `saved "<name>" (N lights)`.
3. Name validation (identical for save and apply; the name is re-read from the RAW command after the sub-verb so an embedded newline is not collapsed into a space by field-splitting): empty/whitespace names, names containing a newline, and names case-insensitively starting with `error:` are rejected with `error: invalid settings name`; names over 43 BYTES are rejected with `error: settings name too long (max 43 bytes)` — `maxSettingsNameLen = 43`, derived as the daemon's 64 B UDP receive buffer minus the 21 B longest verb prefix `light settings apply ` (a longer name would save fine, then silently truncate on apply and could never be applied). A disabled store short-circuits save/apply with `error: settings persistence disabled` (list still reads empty).
4. List: `settings list` replies with the sorted names newline-joined, or the literal empty string `""` when the store is empty — `""` IS the wire contract for "none" (a zero-length UDP datagram).
5. Apply fan-out: `settings apply <name>` replies `error: unknown setting "<name>"` for an unknown name and `error: no lights connected` when ZERO lights are connected; otherwise entries are applied in PARALLEL exactly like the `handleAll` fan-out — goroutine per key, `wg.Wait`, each key's whole on→brightness→temp sequence inside ONE `callLight` so a wedged light costs one 2 s `CallTimeout` once (contract §3).
6. Apply resolution and reply shape: each key resolves via `reg.Resolve(name)` first, then direct port path; unreachable or unresolvable keys yield `error: light "<key>": unreachable, skipped` while the rest still apply; reply lines are written into a slice PREALLOCATED in keys-sorted order so the reply is deterministic regardless of goroutine completion order, mirroring the fleet fan-out shape (`COM4 desk: on 47% 2900K`).
7. Apply playback per key: power, then brightness, then temp through the existing per-light command path (`Manager.HandleCommand`); an off entry is a single `off` (which keeps the current temp byte); the stored temp byte renders through `ByteToKelvin`, re-quantizing to the same step.

**Test scenarios** (fail-first: `go test ./internal/light/ -run TestSavedSettings -count=1` fails at COMPILE — `undefined: NewSettingsStore`/`SavedSetting`/`SavedLightState` and `mm.settings undefined` — the intended missing-behavior failure, not a syntax accident):
- `TestSavedSettingsStoreSaveListGetRoundTrip` — save/list/get round-trip with sorted names, a `Get` miss, and overwrite-by-exact-name keeping a single entry.
- `TestSavedSettingsStorePersistedAcrossReload` — a second `NewSettingsStore` on the same path sees the snapshot; a corrupt file silently lists empty; `NewSettingsStore("")` is disabled and a no-op.
- `TestSavedSettingsSaveAndListWireVerbs` — the wire contract through `HandleCommand`: empty list is `""`, the save reply is literal, the list is sorted and newline-joined, overwrite keeps one entry, and the on-disk JSON holds both names.
- `TestSavedSettingsSnapshotKeysByNameElsePort` — a named light snapshots under its registry name, an unnamed light under its COM port, an off light saves its restore-target brightness, and the reloaded file matches.
- `TestSavedSettingsApplyRestoresLiveStateInOrder` — apply restores the disturbed fleet with exactly on→brightness→temp frames per light in order (cross-light interleaving deliberately NOT asserted because the fan-out is parallel), and the reply is the deterministic `COM4: on 47% 2900K\nCOM7: off`.
- `TestSavedSettingsApplySkipsUnreachableKeys` — a key saved under a name that no longer resolves, and a COM-literal key with no live session, each yield `error: light "<key>": unreachable, skipped` while resolvable entries apply.
- `TestSavedSettingsApplyErrorShapes` — unknown name → `error: unknown setting "nope"`; apply with zero lights connected → `error: no lights connected`.
- `TestSavedSettingsNameValidation` — empty, newline-containing, and `error:`-prefixed (both cases) names are rejected for save AND apply; a 44-byte name → `error: settings name too long (max 43 bytes)` while a 43-byte name is accepted (the cap is inclusive); rejected saves never land in the list.
- `TestSavedSettingsDisabledStore` — with `""` stateDir the store is disabled: save/apply → `error: settings persistence disabled`, list → `""`.

**Verification:**
- Fail-first: `go test ./internal/light/ -run TestSavedSettings -count=1` → FAILS at compile with the undefined symbols above.
- `go test ./internal/light/ -run TestSavedSettings -count=1` → PASS.
- `go test ./internal/light/ ./internal/daemon/ -count=1` → PASS (the daemon's `light`-prefix routing tests keep pinning that `settings ...` arrives unchanged).
- Gate: `go build ./... && go vet ./...` → PASS (silent, exit 0).

**Commit:**
`git add internal/light/settings.go internal/light/settings_test.go internal/light/multi.go && git commit -m "light: daemon-owned named saved settings (save/list/apply verbs with persisted store)"`

### Task 2: Daemon log dedupe for settings list + loopback coverage + protocol docs

**Files:**
- Modify: `internal/daemon/daemon.go` (Daemon struct latches, `logCommand`)
- Modify: `internal/daemon/daemon_test.go` (startDaemon harness refactor, new tests)
- Modify: `README.md` (light commands table + notes paragraph)

**Interfaces:**
- Consumes: Task 1's `settings save|list|apply` verbs in `MultiManager.HandleCommand`; the daemon's untouched `light` CutPrefix routing; contract §3 wire shapes; contract §4's latch mandate.
- Produces: a third `logCommand` reply latch for the exact command `light settings list`; the harness variant `startDaemonLight(t, open, light CommandHandler)` (with a single shared `startDaemonAll(t, open, light, inject)` body behind all three helpers); loopback proof the three verbs traverse the daemon unchanged; README protocol docs.

**Behavior:**
1. Log dedupe: `logCommand` reply-latches `light settings list` exactly like `status` and `light status` — the tray polls it every 2 s and log rotation runs only at daemon start, so unconditional logging would grow the log unbounded; non-poll verbs (settings save/apply) always log, even identical repeats; each latch stays independent; no lock (called only from the single serveUDP goroutine).
2. Loopback coverage: `light settings ...` datagrams reach the light handler verbatim and replies round-trip byte-for-byte over real UDP, including the empty-string list reply (a zero-length UDP datagram).
3. The test harness is de-duplicated: `startDaemon`, `startDaemonInject`, and `startDaemonLight` all delegate to one `startDaemonAll` (no copied bodies, no stale comments).
4. README protocol/CLI docs enumerate the three verbs, the name rules and BOTH validation errors (`error: invalid settings name` for empty/newline/`error:`-prefixed names — case-insensitive; `error: settings name too long (max 43 bytes)` past the byte cap, with the 64-byte-receive-buffer rationale), the `saved "<name>" (N lights)`/sorted-list/empty-reply-is-none shapes, the apply fan-out line shape with `unreachable, skipped` per-key errors, `error: unknown setting "<name>"`, `error: no lights connected`, persistence in `%LOCALAPPDATA%\mutastic\light-settings.json`, quoted multi-word names (`mutastic.exe light settings save "movie mode"`), and the deliberate no-delete/rename-verb (YAGNI) note.

**Test scenarios** (fixture: `scriptedSettingsFleet`, a scripted two-light stand-in owning the settings verbs):
- `TestLogCommandSuppressesRepeatedSettingsList` — identical `light settings list` replies log once until the reply changes (first + change = 2 log lines), `settings save` repeats always log, and the `status` latch stays independent — FAILS FIRST: with no latch the list is logged 4 times instead of 2 (this is the failing driver of the task).
- `TestLightSettingsVerbsTraverseDaemonOverUDP` — save/list/apply (including the unknown-name error and the `""` empty-list reply) traverse the daemon over UDP with the handler receiving the exact verbatim command sequence — characterization coverage: routing already exists, so this PASSES ON FIRST RUN; it guards regressions, e.g. a colliding top-level `settings` verb.

**Verification:**
- Fail-first: `go test ./internal/daemon/ -run 'TestLogCommandSuppressesRepeatedSettingsList|TestLightSettingsVerbsTraverseDaemonOverUDP' -count=1` → the latch test FAILS (list logged 4×, want 2); the traverse test passes already.
- `go test ./internal/daemon/ -count=1` → PASS.
- `go test ./internal/... -count=1` → PASS.
- Gate: `go build ./... && go vet ./...` → PASS.

**Commit:**
`git add internal/daemon/daemon.go internal/daemon/daemon_test.go README.md && git commit -m "daemon: dedupe settings-list polls; loopback coverage + README for light settings verbs"`

### Task 3: Web UI audio (mic) card + /api/mic routes

**Files:**
- Modify: `ui.go` (route case in `ServeHTTP`; `uiMicStatus`/`uiMicRequest` types; handlers + mic-status parser after `handleLights`)
- Modify: `internal/lightui/index.html` (mic card CSS, markup between the gang controls and the individual-controls section, JS inside the existing IIFE)
- Modify: `ui_test.go` (new tests; mirrors the pinned-fragment style of `TestEmbeddedLightUICardsUseTheirOwnIdentityAsTarget`)
- Modify: `README.md` (`mutastic ui` bullet)

**Interfaces:**
- Consumes: daemon mic verbs `mute|unmute|toggle` + tri-state `status`; `daemonCall`/`daemonDispatcher.sequence`; the existing route guards (CSP/XFO headers, Sec-Fetch-Site rejection, loopback host+exact-port check, origin check on POSTs); index.html's mutation queue + 750 ms polling cadence; the `newTestDaemonDispatcher`/`newUIServer` test seams.
- Produces (contract §5): `GET /api/mic` → `{"state":"muted|unmuted|unknown|unreachable"}`; `POST /api/mic` `{"action":"mute|unmute|toggle"}` → `{"state":...}`; JS `refreshMic()`, `updateMic(state)`, `bindMicControls()`.

**Behavior:**
1. `/api/mic` sits behind the same guards as the existing UI routes, and wrong methods answer 405 with `Allow: GET, POST`.
2. GET replies the daemon's mic state word; a daemon `error:` reply AND a transport error both collapse to `"unreachable"`, always at HTTP 200.
3. POST validates the action (`mute|unmute|toggle`; anything else → HTTP 400 and NEVER reaches the daemon), sends the daemon verb, then issues a FRESH `status` query and replies with that state.
4. ALL daemon calls from the UI — mic AND light — use the single 6 s `lightClientTimeout`-class budget, because `serveUDP` is strictly serial and a wedged light call occupies ~2 s: a 1 s budget would flap the mic card to "unreachable" mid light-operation and could report failure for mutes the daemon still dequeued and executed (LB-3 ruling; tray_windows.go already uses 6 s for every ask for exactly this reason). NO timeout split is introduced; a comment at the dispatcher states this.
5. The mic card is a status indicator (badge + status line; `unreachable` visibly distinct) plus Mute/Unmute/Toggle buttons, wired through the existing mutation queue (never a direct fetch) and the shared 750 ms poll; buttons disable while unreachable. Panel mutes change the mic ONLY — no F24 meeting-app sweep (unlike the physical button, tray, and Stream Deck paths).
6. README `mutastic ui` bullet documents the card, its endpoints/absolute verbs, the single-timeout ruling, and the no-F24-sweep caveat.

**Test scenarios** (fail-first: `/api/mic` 404s and the embedded HTML lacks every mic fragment):
- `TestUIMicStatusReportsDaemonState` — table: `muted`/`unmuted`/`unknown` replies map through, a daemon `error:` reply and a transport error both map to `"unreachable"`, each at HTTP 200 with exactly one `status` daemon call.
- `TestUIMicPostRunsVerbThenFreshStatus` — POST mute issues exactly `["mute", "status"]` (verb then fresh status) and replies `{"state":"muted"}`.
- `TestUIMicPostValidatesActionHonorsGuardsAndMethods` — bad/empty actions → 400 with ZERO daemon calls, foreign Origin → 403, PUT/DELETE → 405 with `Allow: GET, POST`.
- `TestEmbeddedLightUIMicCardUsesTheMicEndpoints` — pinned fragments: the badge/status-line ids, the three `data-mic-action` buttons, the `enqueueMutation("mic:...", "/api/mic", ...)` queue string, the shared 750 ms interval line, AND the absence of any direct `fetch("/api/mic", {method: "POST"` (mutations must go through the queue).

**Verification:**
- Fail-first: `go test . -run 'TestUIMic|TestEmbeddedLightUIMicCard' -count=1` → FAIL (404s + missing fragments).
- `go test . -run 'TestUIMic|TestEmbeddedLightUIMicCard' -count=1` → PASS.
- `go test . -run 'TestUI|TestEmbeddedLightUI|TestLight' -count=1` → PASS.
- `go test . -count=1` → PASS (whole root package).
- Gate: `go build ./... && go vet ./...` → PASS.

**Commit:**
`git add ui.go internal/lightui/index.html ui_test.go README.md && git commit -m "ui: mic audio card with /api/mic routes (mute/unmute/toggle + status)"`

### Task 4: Web UI saved-settings section + /api/settings routes

**Files:**
- Modify: `ui.go` (`/api/settings` route case immediately after the `/api/group` case in `ServeHTTP`; types after `uiResponse`; handlers + parser + builder after `handleGroup`)
- Modify: `internal/lightui/index.html` (styles after the `.trim-row` rules; markup between the gang-controls section and "Individual controls"; JS inside the existing IIFE)
- Test: `ui_test.go`
- Modify: `README.md` (`mutastic ui` bullet)

**Interfaces:**
- Consumes: Task 1's wire verbs (`light settings save <name>` → `saved "<name>" (N lights)`; `light settings apply <name>` → fleet fan-out lines; `light settings list` → newline-joined names, `""` when the store is empty, possibly `error: settings persistence disabled`); `daemonCall`/`daemonDispatcher.sequence` at the single 6 s `lightClientTimeout` budget per Task 3's LB-3 ruling (no 1 s split); `decodeUIJSON`/`writeUIJSON`/`writeUIMethodError`/`validPostOrigin`; the `newUIServer` + `httptest` harness pattern.
- Produces (contract §5): `GET /api/settings` → `{"names":[...]}`; `POST /api/settings` `{"action":"save|apply","name":"..."}` → `{"names":[...]}` refreshed; types `uiSettingsResponse{Names []string, Error string}` / `uiSettingsRequest{Action, Name string}`; helpers `queryUISettings`, `parseUISettingsNames`, `buildSettingsCommand`; JS `renderSettings`, `refreshSettings`, `bindSettingsControls`.

**Behavior:**
1. `/api/settings` sits behind the same guards as the existing routes (CSP/XFO, Sec-Fetch-Site rejection, loopback host+exact-port, origin check on POST); wrong methods → 405 with `Allow: GET, POST`.
2. GET always answers HTTP 200 with `{"names":[...]}` — a REAL array, never null (an empty store's `""` reply parses to `[]`); a daemon transport failure OR an `error:` reply degrades in-band to `{"names":[],"error":"unreachable"}` (the light polling loop already owns the page's error banner, so the settings list never hard-errors the page).
3. POST runs the daemon verb `light settings <action> <name>`, then exactly ONE `light settings list` refresh, and replies HTTP 200 `{"names":[...]}`; the POST reply itself carries the refreshed names — the page does NOT re-GET after a POST.
4. POST maps to HTTP 502 `{"names":[],"error":"<verbatim daemon reply|transport error>"}` ONLY on a transport error OR on a reply that is EXACTLY ONE line starting with `error:` (whole-reply errors: unknown setting, invalid name, persistence disabled, too-long name); a MULTI-LINE apply reply whose first line is an inline `error: light ...` skip is PARTIAL SUCCESS — the resolvable lights did apply — and answers HTTP 200 + refreshed names (LB-4 ruling).
5. Client-side validation runs before any daemon call: action must be `save`|`apply` (else 400), and the name is required — save/apply with an empty or whitespace-only name, or a name containing a newline, → HTTP 400 and never reaches the daemon.
6. index.html gains a "Saved settings" section between the gang controls and "Individual controls": a name input + Save form and a per-name Apply list, all through the existing mutation queue and `escapeHTML`; the list is refetched on load and after mutations (the queue's onSuccess consumes the POST reply's names), and an APPLY additionally triggers the existing light-status refresh (`refreshLights(true)`); the `data-apply` attribute round-trips the verbatim saved name.
7. README `mutastic ui` bullet documents the section: the daemon owns and persists the store, so the panel and the tray see the same set.

**Test scenarios** (fail-first: `ServeHTTP` has no `/api/settings` case — every 200/400/502 sub-assertion fails with 404 — and the embedded page contains none of the pinned fragments; missing route behavior + missing markup, not a harness accident):
- `TestUIAPISettingsList` — GET returns per-line names from exactly one `light settings list` call; an empty-store `""` reply still yields `"names":[]` (not null); a transport error AND a daemon `error:` reply each degrade in-band to `{"names":[],"error":"unreachable"}` at HTTP 200.
- `TestUIAPISettingsSaveAndApplyRefreshTheList` — POST save then apply each issue exactly `[light settings <verb> <name>, light settings list]` (verb first, then exactly one list refresh) and reply HTTP 200 with the refreshed names.
- `TestUIAPISettingsValidatesAndPassesDaemonErrorsThrough` — invalid bodies (missing/whitespace/newline name, unknown action) → 400 without daemon calls; PUT → 405 `Allow: GET, POST`; foreign Origin → 403; a ONE-line `error:` daemon reply → 502 carrying it verbatim; a MULTI-LINE apply whose first line is an inline `error: light "COM9": unreachable, skipped` → 200 with refreshed names, NOT 502 (LB-4); a transport failure on the verb → 502 carrying the transport error.
- `TestEmbeddedLightUIHasSavedSettingsSection` — pinned fragments for the section title, the save form, the name input, the list, the `data-apply` round-trip, both queued mutations (`settings:save` / `settings:apply:<name>`), the `renderSettings(data.names || [])` call, and the apply→`refreshLights(true)` hook.

**Verification:**
- Fail-first: `go test . -run 'TestUIAPISettings|TestEmbeddedLightUIHasSavedSettingsSection' -count=1` → FAIL (404 route + missing fragments).
- `gofmt -l ui.go ui_test.go && go test . -run 'TestUIAPISettings|TestEmbeddedLightUIHasSavedSettingsSection' -count=1` → PASS (gofmt prints nothing).
- `go test . -count=1` → PASS for the whole root suite (including `TestMuteAllMeetingsHotkeyContract`, already fixed at f05b23b before this plan began — nothing here may regress it).
- Gate: `go build ./... && go vet ./...` → PASS.

**Commit:**
`git add ui.go internal/lightui/index.html ui_test.go README.md && git commit -m "ui: saved light settings section with save/apply via /api/settings"`

### Task 5: Tray dynamic Mute/Unmute menu item

**Files:**
- Modify: `traystate.go` (delete `trayMutedChecked`; rename `trayMicEnabled` → `trayMuteEnabled`; add `trayMuteTitle` after `trayTitle`; replace `onMicToggle` with `onMicSet`; replace `mutedClick` with `muteClick`)
- Modify: `tray_windows.go` (menu construction, click registration, refresh-loop signature/body; add `sync/atomic`)
- Test: `traystate_test.go` (rewrite the four pinned tests named below)
- Modify: `README.md` (mute path #3; tray bullet prose)

**Interfaces:**
- Consumes: the existing `traySpy` harness (call-order strings + per-command `script map[string]scriptOutcome`) and `levelRecorder` — both unchanged; `trayStateFor`/`trayTitle`/`trayActionsEnabled`/`trayIconFor` — unchanged; the fork's per-item `SetTitle`/`Enable`/`Disable` (retitle-in-place precedent: `header.SetTitle(trayTitle(state))` every tick).
- Produces (contract §6, exact signatures):

```go
func trayMuteTitle(s trayState) string   // "Mute" when unmuted, "Unmute" when muted, "Mute/Unmute" when unknown/down
func trayMuteEnabled(s trayState) bool   // true only for trayStateMuted|trayStateUnmuted (replaces trayMicEnabled)
func (a *trayActions) onMicSet(verb string)      // absolute "mute"/"unmute" verb + F24 sweep + refresh (replaces onMicToggle)
func (a *trayActions) muteClick(armed trayState) // probe-gated click (replaces mutedClick)
```

`trayMutedChecked` is DELETED; tray_windows.go drops Check/Uncheck for this item — it is an ACTION item, never checkable.

**Behavior:**
1. Title: the item's displayed verb is always the OPPOSITE of the last definitive mic state — a live mic shows "Mute", a muted mic shows "Unmute"; at unknown-or-down it reads the neutral "Mute/Unmute", because a disabled item still displays its last-set title and the neutral text keeps a stale directional verb off the screen (contract §6).
2. Enable: the item arms only at definitive answers — `trayMuteEnabled` true exactly for muted|unmuted (the rename of `trayMicEnabled`, semantics identical); light actions keep arming on any daemon answer via `trayActionsEnabled` (unknown is a mic-state concept, not a reachability one).
3. `trayMutedChecked` is deleted outright; the tray glue never calls Check/Uncheck on this item; title and enabled bit are re-asserted every tick, and an `atomic.Int32` records the armed state the title was last drawn from (the refresh loop stores it, the click handler loads it).
4. Click (`muteClick`): first re-probe via `ask("status")` — then ONLY when the probe state reproduces the armed state, fire the ABSOLUTE verb matching the armed label (`ask("mute")`/`ask("unmute")`), then `injectSweep()`, then refresh: spy orders `ask:status,ask:mute,inject,refresh` / `ask:status,ask:unmute,inject,refresh`. A definitive-but-opposite probe (the mic flipped between the last poll and the click) → WARN + NO-OP; an unknown probe or a daemon-down/err probe → WARN + NO-OP — in all no-op cases the spy order is exactly `ask:status` and the next poll redraws the truthful verb (contract §6).
5. `onMicSet(verb)` replaces `onMicToggle`: the absolute daemon verb AND the F24 meeting-app sweep are BOTH attempted even when the other fails, then a refresh — the same mute-everything flow as the Stream Deck mute key (a verb-only path would mute the mic while leaving meeting apps live); loop-free as before (the AHK sweep never calls a mic verb, and the daemon's injector gate ignores the mic's host-command echo).
6. tray_windows.go: the item is constructed with the neutral "Mute/Unmute" title and the mute-everything tooltip, replaces `muted` in the startup disable loop, its click registration dispatches `go actions.muteClick(trayState(muteArmed.Load()))`, and `trayRefreshLoop` gains the mic item + armed-state parameters, retitling and re-gating the item each tick (the loop's header comment drops the word "checkbox" — no checkbox remains).
7. `bash build.sh` runs IN THIS TASK — it is the ONLY compile gate for the Windows-only `tray_windows.go`, which the Linux gate skips.
8. README: mute path #3 becomes "Tray icon Mute/Unmute menu" (mute-everything pair through the daemon, always the absolute verb the item displays, re-checking the arming state first — declining at `unknown` or on a flipped direction); the tray bullet's "Muted check item" phrasing is replaced by the dynamic action item description; at unknown state the item reads "Mute/Unmute" and stays gray.

**Test scenarios** (fail-first ordering is INVERTED on purpose: the PRODUCTION rewrite lands first — deleting `trayMutedChecked` and renaming `trayMicEnabled`/`mutedClick`/`onMicToggle` — so the old pinned tests fail to COMPILE and pin stale toggle-based orders like `ask:status,ask:toggle,inject,refresh`; the four rewritten tests below then define the contract):
- `TestTrayDisplayDecisions` (rewritten) — pins the four tooltip/header titles, `trayMuteTitle` for every state (opposite verb at definitive states, neutral at unknown/down), `trayMuteEnabled` gating (disabled at unknown/down, armed at definitive answers), and `trayActionsEnabled` (disabled at down, enabled at muted/unmuted/unknown).
- `TestTrayMicToggleIsMuteEverything` (rewritten) — for BOTH `mute` and `unmute`: spy order `ask:<verb>,inject,refresh`, and the sweep still fires when the daemon is dead (mute-everything; the verb is an ABSOLUTE direction, never the ambiguous toggle).
- `TestMuteClickRevalidates` (renamed from `TestMutedClickRevalidates`) — armed Mute + matching probe fires the full pair exactly once (`ask:status,ask:mute,inject,refresh`); armed Unmute + matching probe fires `ask:status,ask:unmute,inject,refresh`; a definitive-but-opposite probe, an unknown probe, and a daemon-down probe each produce `ask:status` ONLY plus exactly one WARN log (no verb, no sweep; a declined definitive-check is not an error).
- `TestLogSeverityClassifiesFailures` (touched, not rewritten) — only its call site changes from `a.onMicToggle()` to `a.onMicSet("mute")` and the message tail becomes "(mute error + per-line fleet error)"; nothing else in that test changes.

**Verification:**
- Fail-first (after the production rewrite, before the test rewrite): `go test . -run 'TestTrayDisplayDecisions|TestMuteClickRevalidates|TestTrayMicToggleIsMuteEverything' -count=1` → FAIL because the test package no longer compiles (deleted/renamed symbols) — the designed signal that the rewritten tests now define the contract.
- `gofmt -l traystate.go traystate_test.go && go test . -run 'TestTrayDisplayDecisions|TestMuteClickRevalidates|TestTrayMicToggleIsMuteEverything' -count=1` → PASS (gofmt silent).
- `rg -n "mutedClick|trayMutedChecked|trayMicEnabled|onMicToggle" --glob '!docs/**' .` → prints nothing outside `docs/` (historical plan docs stay as history).
- `bash build.sh && go build ./... && go vet ./... && go test . ./internal/... -count=1` → `bin/mutastic.exe` produced (windows/amd64, CGO) and the full suite PASSes with zero exceptions (`TestMuteAllMeetingsHotkeyContract` stays green).

**Commit:**
`git add traystate.go traystate_test.go tray_windows.go README.md && git commit -m "tray: dynamic Mute/Unmute action item (absolute verb per state, sweep preserved)"`

### Task 6: Tray Saved settings submenu (polled, diff-rendered)

**Files:**
- Modify: `traystate.go` (append the new pure API after `trayIconFor`, before the `trayActions` block)
- Test: `traystate_test.go` (append three tests; add `"reflect"` to the imports)
- Modify: `tray_windows.go` (submenu construction right after the "Light preset" item; refresh-loop call site; settings poll + reconcile inside `trayRefreshLoop`; new `syncSavedSettingsMenu` helper)
- Modify: `README.md` (menu enumeration in the `mutastic tray` bullet)

**Interfaces:**
- Consumes: `askDaemon(command, udpAddr, lightClientTimeout)` (2048-byte buffer; multi-line replies normal); the systray fork's `(*MenuItem).AddSubMenuItem`/`Hide`/`SetTitle`/`Click`/`Enable`/`Disable` (children append in creation-id order; NO remove/insert API); Task 2's `logCommand` latch keeping `light settings list` quiet in daemon.log; Task 1's `settings list` wire contract (`""` = none); Task 5's rewritten mic-item handling in `trayRefreshLoop`.
- Produces (contract §7, exact signatures):

```go
type trayMenuSpec struct { Title string; Enabled bool }
func traySavedSettings(names []string, daemonOK bool) []trayMenuSpec
func trayParseSettingsList(reply string, err error) (names []string, daemonOK bool)
const traySavedSettingsListCmd = "light settings list"
func traySameMenuSpecs(a, b []trayMenuSpec) bool
func syncSavedSettingsMenu(parent *systray.MenuItem, children []*systray.MenuItem, cached, want []trayMenuSpec, lightCmd func(string) func()) ([]*systray.MenuItem, []trayMenuSpec) // Windows-only, tray_windows.go
```

**Behavior:**
1. Parse: `trayParseSettingsList` treats an EMPTY reply as (no names, daemonOK=true) — the wire contract for "none saved", NOT an error and NOT unreachable; newline-joined names split in order (the daemon emits them sorted); ANY ask error → not-ok; and a reply starting with `error:` (e.g. `error: settings persistence disabled` — single-line is the only error shape this verb sends) is treated as NOT-OK (LB-4 ruling: without the guard the error text would render as an ENABLED menu item whose click always fails). `(nil, false)` renders via `traySavedSettings` as the single disabled "(daemon unreachable)" item.
2. Spec: `traySavedSettings` renders three regimes — daemonOK==false → one DISABLED `(daemon unreachable)` placeholder; names empty → one DISABLED `(no saved settings)` placeholder; else one `{name, ENABLED}` per name in input order. Both placeholder strings are tray-side UI text, NEVER sent by the daemon (contract §7).
3. Diff: `traySameMenuSpecs` compares spec lists by TITLE only (within every regime equal titles imply equal enabled bits, so titles fully determine the rendering); the refresh loop rebuilds the submenu children exactly when it reports false.
4. Glue construction: tray_windows.go adds the top-level "Saved settings" submenu immediately after "Light preset" (before the separator + "Light panel…" — top-level items render in creation order) and does NOT add it to the startup `Disable()` loop or the refresh loop's `trayActionsEnabled` gating: the parent stays clickable so the grayed placeholder children stay visible; children carry the enabled state.
5. Poll + reconcile: the existing 2 s refresh loop also sends `light settings list` once per tick (hushed in daemon.log by Task 2's latch) and reconciles children via `syncSavedSettingsMenu`: on a title difference, Hide every existing child, then `AddSubMenuItem` each wanted spec in order (with `SetTitle`/enable state per spec) — a full rebuild each time because the fork has no remove/insert, only Hide; hidden orphans keep their ids and click bindings but are never displayed again, accumulating only on name-list churn — the deliberate price of the fork having no delete API.
6. Click on an enabled name sends `light settings apply <name>` through the existing light-command flow (the same ordered channel the other light menu items use, drained into `actions.onLight` and logged "light command"); placeholder items are disabled and unbound.
7. `bash build.sh` runs IN THIS TASK — the only compile gate for `*_windows.go`; on Linux the menu reconciliation is verified by construction (every decision — spec computation, parse, title-compare — is a tested pure function, and the glue is mechanical Hide/AddSubMenuItem/Click over the tested spec list) plus the cross-build.
8. README `mutastic tray` bullet menu enumeration gains `**Saved settings** (the daemon's saved named light settings, polled every 2 s; click a name to apply it)` between `**Light preset**` and `**Light panel…**`.

**Test scenarios** (fail-first: `go test . -run 'TestTraySavedSettingsMenuSpec|TestTrayParseSettingsList|TestTraySameMenuSpecs' -count=1` fails at COMPILE — `undefined: traySavedSettings`/`trayMenuSpec`/`trayParseSettingsList`/`traySameMenuSpecs`):
- `TestTraySavedSettingsMenuSpec` — the three display regimes produce exactly `[{(daemon unreachable) false}]` when down, `[{(no saved settings) false}]` when the store is empty, and one `{name, true}` per name in input order when saved names exist.
- `TestTrayParseSettingsList` — `("", nil)` → (empty, TRUE): the empty reply is the wire contract for none, not an error; `("a\nb", nil)` → ([a b], true); an ask error → (nil, false); an `error:`-prefixed reply → (nil, false) per LB-4.
- `TestTraySameMenuSpecs` — equal lists (including two nils, the state before the first poll) compare equal; a changed title or a different length compares different and triggers a rebuild.

**Verification:**
- Fail-first: `go test . -run 'TestTraySavedSettingsMenuSpec|TestTrayParseSettingsList|TestTraySameMenuSpecs' -count=1` → FAILS at compile (undefined pure functions).
- `go test . -run 'TestTraySavedSettingsMenuSpec|TestTrayParseSettingsList|TestTraySameMenuSpecs' -count=1` → PASS.
- `go test . ./internal/... -count=1` → PASS with zero exceptions (`TestMuteAllMeetingsHotkeyContract` remains green via its new `#NoTrayIcon` assertion).
- `bash build.sh` → prints `built bin/mutastic.exe` (proves the modified tray_windows.go cross-compiles).

**Commit:**
`git add traystate.go traystate_test.go tray_windows.go README.md && git commit -m "tray: Saved settings submenu polled from daemon (apply on click)"`

### Task 7: Docs + smoke runbook + final gate

**Files:**
- Modify: `README.md` (final consolidated user docs: saved named light settings from web UI + tray + CLI, mic card, dynamic Mute/Unmute item)
- Modify: `docs/plans/2026-08-14-tray-icon.md` (append the runbook block below under `## Windows smoke verification (manual, on deploy)`, after step 11 — daemons running again from step 11's relaunch)

**Interfaces:**
- Consumes: Task 1's CLI verbs (`settings save|list|apply`, pass-through, no main.go change); Tasks 3–4's web UI cards; Task 5's dynamic tray mute item; Task 6's Saved settings submenu. Earlier tasks left their own README fragments — this task's wordings are FINAL: where a Task 3–6 edit touched the same sentence, REPLACE that fragment rather than duplicating it.
- Produces: no code interfaces; terminal user documentation and the manual smoke gate.

**Behavior:**
1. README, Light commands table (immediately after the `mutastic light list` row): three rows covering `settings save <name>` (snapshot every connected light's on/brightness/temp; quote names containing spaces, e.g. `mutastic.exe light settings save "movie mode"`; saving an existing name overwrites it), `settings list` (names one per line; empty reply means none saved), and `settings apply <name>` (unknown name → `error: unknown setting "<name>"`; a saved light no longer connected is reported and skipped); the notes sentence after the table gains the `light-settings.json` persistence note (same directory as `light-names.json`; delete the file to clear all saved names).
2. README, `mutastic tray` bullet: the final menu enumeration is the dynamic **Mute**/**Unmute** action item (mute-everything — mic plus F24 sweep, same in-process flow as the Stream Deck mute key; the label always names the exact action a click performs, the click re-checks the mic state first, at unknown state the item stays gray), **Toggle lights**, **Brightness** (applied in click order), **Light preset**, **Saved settings** (the daemon's saved named light settings — the same names the web UI saves — refreshed every 2 s; click a name to apply it; grayed `(no saved settings)`/`(daemon unreachable)` placeholders appear when appropriate), **Light panel…**, and **Quit**; remove any remaining "**Muted** check item" phrasing.
3. README, `mutastic ui` bullet: the **Mic** card (live muted/unmuted status with Mute/Unmute/Toggle buttons; these change the mic ONLY — no F24 meeting-app sweep, unlike the tray item and the Stream Deck mute key) and the **Settings** card (type a name and Save to snapshot every connected light's current on/brightness/temp; click a listed name to apply it).
4. Append EXACTLY the runbook block below to `docs/plans/2026-08-14-tray-icon.md`, continuing the numbered list after step 11 (steps 16–18 are the LB-1 menu-robustness additions: rebuild across name-set changes, placeholder regimes, mid-refresh glitch self-heal):

```markdown
12. **Save a named setting in the web UI:** set the lights to a distinctive look (e.g. tray **Light preset → candle**), open the panel (left-click the tray icon), type `smoke test` into the **Settings** card's name field, and click **Save** — `smoke test` appears in the card's saved list.
13. **The tray picks the name up:** right-click the tray icon and open **Saved settings** — within ~2 s (the tray polls `light settings list` on its normal tick) the submenu lists **smoke test** as an enabled item.
14. **Apply from the tray menu:** change the lights (tray **Toggle lights** or another preset), then click **Saved settings → smoke test** — the lights return to the saved look, and `%LOCALAPPDATA%\mutastic\tray.log` gains an INFO line with `"msg":"light command"` and `"cmd":"light settings apply smoke test"`.
15. **Apply from the web UI:** change the lights again, then click **smoke test** in the panel's **Settings** card — the lights restore and the light cards refresh on the same poll.
16. **Submenu rebuild across name-set changes:** in the panel's **Settings** card save two more names (`smoke b`, then `smoke c`) and overwrite `smoke test` with a fresh **Save** under the same name (there is no delete verb, so the set only grows and overwrites — rename/shrink coverage comes from overwriting and, in step 17, from clearing the file). Open the tray **Saved settings** submenu 5 times spread across these changes: every open renders exactly the current names, in the daemon's sorted order, with no duplicates and no phantom items left from earlier name sets, and clicking any listed name applies it.
17. **Placeholder regimes:** stop the daemon (step 21's Quit, or taskkill) — on the next 2 s poll the **Saved settings** submenu collapses to a single grayed **(daemon unreachable)** item; relaunch with the store empty (stop the daemon, delete `%LOCALAPPDATA%\mutastic\light-settings.json`, relaunch) and the submenu shows a single grayed **(no saved settings)** item; save a name again and it returns on the next poll.
18. **Mid-refresh glitch (best-effort):** hold the **Saved settings** submenu open across a poll while a save lands from the web UI, so the 2 s tick rebuilds the menu underneath the open popup (a documented cosmetic window — the fork mutates a displayed TrackPopupMenu from another thread). Whatever transient rendering occurs must self-heal: close and reopen the submenu and it shows exactly the current name list, no stuck duplicates or phantoms, and committed state is never affected.
19. **Mic card:** the panel's **Mic** card shows the current muted/unmuted status; **Mute** mutes, **Unmute** unmutes, **Toggle** flips, and the status follows within one poll. These change the mic ONLY — no F24 meeting-app sweep — so with a meeting app open, confirm the apps do NOT change (the tray item and Stream Deck key remain the mute-everything paths).
20. **Dynamic tray mute item:** with the mic live the tray item reads **Mute** — click it and the mic (plus any open meeting app) mutes; once muted the item reads **Unmute** — click it and the mic (and apps) unmute. The label always names the exact action the click performs.
21. **Quit cascade:** menu **Quit** — the icon disappears; `C:\Users\dan\code\mutastic-deploy\mutastic.exe status` prints `error: no daemon reachable`; `curl http://127.0.0.1:42815/api/health` is refused (`curl: (7) Failed to connect`); `tasklist /FI "IMAGENAME eq mutastic.exe"` prints `INFO: No tasks are running which match the specified criteria.` (daemon, UI server, and tray all stopped — one click, everything down). Relaunch via the `Mutastic Daemon` startup shortcut and confirm the icon returns.
```

5. There is no failing test to write first in this task: no Go test pins README prose (`deploy_test.go` pins only the startup command set and AHK contents; `ui_test.go` pins embedded index.html fragments, which this task does not touch) — the verification of these docs is the manual runbook above plus the final gate.

**Test scenarios:**
- None (docs-only task; see Behavior 5). Coverage of the documented behavior lives in Tasks 1–6's tests; this task's gate is the runbook plus the commands below.

**Verification (final gate):**
- `go build ./...` (from the worktree root) → PASS (no output, exit 0).
- `go test ./... -count=1` → PASS for every package with zero exceptions (`TestMuteAllMeetingsHotkeyContract` was fixed at f05b23b before this plan began; its `#NoTrayIcon` assertion must stay green). Any failure is fixed before committing.
- `go vet ./...` → PASS (silent, exit 0).
- `bash build.sh` → prints `built bin/mutastic.exe` — the only compile gate covering the Windows-only tray/mic/inject/browser files.

**Commit:**
`git add README.md docs/plans/2026-08-14-tray-icon.md && git commit -m "docs: saved settings, mic card, dynamic mute item; extend Windows smoke runbook"`
