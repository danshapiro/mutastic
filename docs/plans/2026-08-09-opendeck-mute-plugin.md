# OpenDeck Native Mute Plugin Implementation Plan

> **For agentic workers:** This plan is executed task-by-task by the
> workflow's execute stage: a fresh implementer per task, with a spec +
> quality review after each task. Steps use checkbox (`- [ ]`) syntax
> for tracking.

**Goal:** Build a native OpenDeck plugin into the existing `mutastic` binary so the Stream Deck mute key shows the TRUE mic mute state from ALL sources (physical Yeti X button, pedal, CLI, deck press) instead of blindly auto-toggling its icon.

**Architecture:** The single `mutastic.exe` gains a plugin mode (`deckplugin` subcommand, auto-detected when OpenDeck launches it with a leading `-port` flag). It speaks the Elgato Stream Deck SDK WebSocket protocol against OpenDeck, handles `willAppear`/`willDisappear`/`keyDown`, performs the full mute-everything flow in-process (UDP `toggle` to the daemon + `SendInput` F24), and drives the key icon exclusively via `setState` from a ~750ms poll of the daemon's UDP `status`. Platform-free logic lives in a new `internal/deckplugin` package behind tiny interfaces (mirroring `internal/daemon`'s `Device`/`KeyInjector` pattern); the real WebSocket, UDP client, and injector are wired in `package main`.

**Tech Stack:** Go 1.26 (existing repo, stdlib `testing` only), `github.com/gorilla/websocket` v1.5.3 (pure Go, zero deps — safe under the mingw cgo cross-compile), Windows batch + PowerShell 5.1 for deployment, OpenDeck v2.13.1 (fork build) as the plugin host.

## Global Constraints

Every task's requirements implicitly include this section. Values are copied verbatim from the spec.

- **Repo root for every command:** `/home/dan/code/mutastic/.worktrees/opendeck-mute-plugin` (the isolated worktree). All relative paths in this plan are relative to it. Every subagent dispatch must state this path explicitly and use `git -C` / path-prefixed access.
- **Do NOT modify the OpenDeck fork** at `/home/dan/code/OpenDeck` — read-only reference. The plugin must work against stock v2.13.1 behavior.
- **Single binary:** the plugin ships inside `mutastic.exe` (a plugin mode of the existing binary), not a second executable. `build.sh` keeps its single `GOOS=windows GOARCH=amd64 CGO_ENABLED=1 CC=x86_64-w64-mingw32-gcc` target; any new dependency must be pure Go.
- **Quality gate for every code task:** `go test -race ./... && go vet ./...` clean, plus `GOOS=windows GOARCH=amd64 CGO_ENABLED=1 CC=x86_64-w64-mingw32-gcc go vet .` for Windows-tagged files. ALL existing mic/light/daemon tests keep passing. Existing daemon/CLI behavior unchanged (including exact `runClient` output strings).
- **Action identity:** plugin directory (= plugin UUID) `com.danshapiro.mutastic.sdPlugin`; action UUID `com.danshapiro.mutastic.mute`; action name `Mutastic Mute`; 2 states — state 0 = live mic, state 1 = muted; `DisableAutomaticStates: true` in the manifest AND `disable_automatic_states: true` in the profile instance (the plugin alone drives state).
- **keyDown behavior:** daemon UDP `toggle` (127.0.0.1:42814) + in-process F24 injection via the existing `newKeyInjector()` SendInput path (`dwExtraInfo=0`, proven to fire `MuteAllMeetings.ahk`'s `*F24` sweep). No cmd/AHK spawning. Both actions run even if the other fails (mirrors `mute-everything.cmd`'s two unconditional lines). Never inject F24 in reaction to a state change (loop hazard).
- **State sync:** poll daemon `status` every ~750ms while any instance is visible; on `muted`/`unmuted` change, `setState(1|0)` for all visible instances; `unknown` or daemon-unreachable → leave current state, log.
- **Plugin log:** `%LOCALAPPDATA%\mutastic\deckplugin.log` — log every `setState` (the E2E log contract greps these lines).
- **Icons:** the two existing 144×144 PNGs on pure black remain the visuals — `deck/icons/mutastic-mic.png` (state 0) and `deck/icons/mutastic-mic-muted.png` (state 1).
- **Keep `deploy/mute-everything.cmd`** (still a useful CLI entry point); the deck no longer uses it.
- **Profile edit:** `C:\Users\dan\AppData\Roaming\opendeck\profiles\sd-A00DA6141I07PW\Default.json` `keys[5]` (context `Keypad.5.0`), edited only with OpenDeck STOPPED, with a `.bak` kept before editing.
- **Windows paths:** deployed daemon dir `C:\Users\dan\code\mutastic-deploy\`; OpenDeck exe `C:\Users\dan\AppData\Local\OpenDeck\opendeck.exe`; OpenDeck config `C:\Users\dan\AppData\Roaming\opendeck\`; OpenDeck logs `C:\Users\dan\AppData\Local\OpenDeck\logs\`.
- **README.md is the only end-user markdown doc** (this plan under `docs/plans/` is a working/agent doc). Keep commits focused and atomic.
- **WSL interop is historically flaky:** for live Windows steps, retry over a couple of minutes before declaring a blocker.
- **End state after E2E:** mic UNMUTED, light untouched (issue no `light` commands).

---

## Scope Check

One subsystem, one repo, one deliverable (the plugin mode + its deployment). No split needed; a single plan with end-to-end coverage is right.

## Context for the Implementer (read once, referenced by every task)

You are extending an existing, working system. Facts you need (verified against the OpenDeck v2.13.1 source and the live Windows install):

**The mutastic repo** (module `mutastic`, Go 1.26.3): root `package main` + `internal/{daemon,light,proto}`. Subcommand dispatch is hand-rolled `os.Args` inspection in `main.go` (no flag package, no cobra): `daemon` is special-cased inline; everything else maps to a one-shot UDP client via the pure function `clientCommand`. The daemon serves plain-text commands on UDP `127.0.0.1:42814` (`const udpAddr`, `main.go:24`). Replies for mic verbs are exactly `"muted"`, `"unmuted"`, `"unknown"`, or `"error: <reason>"`. `"unknown"` is normal after a daemon restart; `toggle` on unknown state mutes. There is NO push channel — a live icon must poll `status`. The daemon logs every UDP command; the poll will add periodic `command "status" -> ...` lines to `%LOCALAPPDATA%\mutastic\mutastic.log` (its 5 MB rotation already handles growth). F24 injection: `newKeyInjector() daemon.KeyInjector` (root `package main`, `inject_windows.go`) returns a SendInput-based injector with `Inject() error`; on non-Windows it returns `nil` and callers must nil-check. Its `dwExtraInfo=0` is load-bearing — visible to AHK hook hotkeys without `SendLevel`.

**OpenDeck plugin hosting** (from `/home/dan/code/OpenDeck/src-tauri/src/plugins/` — read-only): plugins live at `%APPDATA%\opendeck\plugins\<dirname>.sdPlugin\` and **the directory name IS the plugin UUID** (there is no top-level UUID manifest field). `manifest.json` sits at the plugin dir root; required top-level fields: `Name`, `Author`, `Version`, `Icon`, `Actions`, `OS`. `PropertyInspectorPath` is optional — omitted means the plugin simply never gets PI events; nothing breaks. On Windows, `CodePathWin` (a path relative to the plugin dir) selects the binary; a flat `"CodePathWin": "mutastic.exe"` is correct. Manifest image paths are **extensionless** (OpenDeck appends `.svg` → `@2x.png` → `.png`; writing `icons/x.png` would resolve `icons/x.png.png`). OpenDeck spawns the binary with argv `-port <N> -pluginUUID <dirname> -registerEvent registerPlugin -info <json>` (single dash, values as separate argv entries, port base 57116 — always use the given value), working directory = plugin dir, `CREATE_NO_WINDOW`, stdout+stderr redirected to `%LOCALAPPDATA%\opendeck\logs\plugins\<dirname>.log`. The plugin connects to `ws://127.0.0.1:<port>` and MUST send, as its very first text frame, `{"event":"registerPlugin","uuid":"<dirname>"}` — a malformed register is a **silent** failure (socket never registered, you receive nothing). Events queued before registration are flushed on register. No acknowledgement is sent; silence after registering is normal. OpenDeck kills plugin processes when it exits and does not restart them.

**Wire shapes** (verbatim from OpenDeck's serializers): inbound events share one envelope —

```json
{"event":"willAppear","action":"com.danshapiro.mutastic.mute","context":"sd-A00DA6141I07PW.Default.Keypad.5.0","device":"sd-A00DA6141I07PW","payload":{"settings":{},"coordinates":{"row":1,"column":2},"controller":"Keypad","state":0,"isInMultiAction":false}}
```

`willDisappear`, `keyDown`, `keyUp` are identical except the `event` value. `context` is a dotted string `"<device>.<profile>.<controller>.<position>.<index>"` — treat it as opaque and echo it back verbatim. Every `willAppear` is immediately followed by a `titleParametersDidChange` for the same context — tolerate and ignore it. `keyUp` may be suppressed (profile switch between press and release), so act on `keyDown`. Outbound: `{"event":"setState","context":"<echoed ctx>","payload":{"state":N}}` — bounds-checked (a 2-state action accepts only 0/1), authorized (the instance's `action.plugin` must equal your registered uuid), silently dropped on any mismatch. A successful `setState` updates `current_state`, re-renders the key image from the instance's `states[N].image`, and persists the profile.

**Automatic state toggling** (what we disable): with exactly 2 states and `disable_automatic_states == false`, OpenDeck flips `current_state` on every keyUp *before* sending the event — the lying-icon behavior this feature removes. **The profile snapshots the action**: changing the manifest does not fix already-placed keys, which is why the deploy step edits the profile instance directly.

**The live profile** (`C:\Users\dan\AppData\Roaming\opendeck\profiles\sd-A00DA6141I07PW\Default.json`, 6-key deck, lower-right = `keys[5]`, context `Keypad.5.0`): top level is `{"infobars":[],"keys":[...6 slots, 0-4 null...],"sliders":[]}`. A key instance has exactly six fields: `action` (12-field snapshot), `children` (null), `context`, `current_state`, `settings`, `states` (the instance-level `states` is what renders). State objects have 14 fields (`alignment`, `background_colour`, `colour`, `family`, `image`, `image_scale`, `name`, `show`, `size`, `stroke_colour`, `stroke_size`, `style`, `text`, `underline`). Profile image paths are relative to the OpenDeck config root WITH extension, e.g. `plugins/com.danshapiro.mutastic.sdPlugin/icons/mutastic-mic.png`.

**Test conventions:** stdlib `testing` only, hand-written `if got != want { t.Fatalf(...) }` with explanatory messages. Interfaces in `internal/`, fakes in `_test.go` mirroring `fakeDevice` (mutex-guarded `writes [][]byte`, buffered channels, accessor methods). Timing knobs are package `var`s so tests can shrink them, restored via `t.Cleanup` registered BEFORE any harness whose cleanup must run first (LIFO). Silent logger: `log.New(io.Discard, "", 0)`.

## File Structure

| Path | Responsibility |
|---|---|
| `internal/deckplugin/protocol.go` (create) | Elgato wire types: `ParseArgs`, `DecodeEvent`, `EncodeRegister`, `EncodeSetState`. Pure, platform-free. |
| `internal/deckplugin/protocol_test.go` (create) | Exact-JSON encode/decode + argv parsing tests. |
| `internal/deckplugin/plugin.go` (create) | `Plugin`: visibility tracking, status→state decision, keyDown flow, poll loop, `Run`. Single-goroutine confined. |
| `internal/deckplugin/plugin_test.go` (create) | Fakes (`fakeConn`, `fakeDaemon`, `fakeInjector`) + behavior tests. |
| `deckplugin.go` (create, root `package main`) | Real wiring: `askDaemon` UDP helper, gorilla `wsConn` adapter, `udpDaemonClient`, `runDeckPlugin`. |
| `deckplugin_test.go` (create, root) | `askDaemon` tests over real UDP (repo idiom). |
| `deck_manifest_test.go` (create, root) | Guard test pinning the manifest contract + icon presence. |
| `main.go` (modify) | Dispatch `deckplugin` / leading `-port`; `usage()` line; `openLogFile` → `openNamedLogFile` refactor; `runClient` reuses `askDaemon` with output preserved. |
| `deck/com.danshapiro.mutastic.sdPlugin/manifest.json` (create) | The OpenDeck plugin manifest (source of truth; deploy assembles the installed dir). |
| `deploy/set-mute-key.ps1` (create) | Idempotent profile editor: points `keys[5]` at the plugin action, keeps a `.bak`. |
| `deploy/deploy.cmd` (modify) | Kill OpenDeck, assemble plugin dir into `%APPDATA%\opendeck\plugins\`, run profile editor, relaunch OpenDeck. |
| `README.md` (modify) | Deck section describes the plugin instead of the Run Command button. |
| `go.mod` / `go.sum` (modify) | Add `github.com/gorilla/websocket v1.5.3`. |

Design decisions locked in here:

- **Library:** `github.com/gorilla/websocket` — pure Go, zero transitive deps, stable blocking `ReadMessage`/`WriteMessage` API that maps 1:1 onto the tiny `Conn` interface. Cross-compiles under the existing mingw cgo build untouched.
- **Launch detection (documented choice):** both `mutastic deckplugin -port ...` (manual/diagnostic) and auto-detection of a leading `-port` first argument (how OpenDeck actually launches it — CodePath args are fixed by OpenDeck and cannot include a subcommand word).
- **Concurrency model:** one reader goroutine feeds a channel; `Run`'s single loop consumes frames and ticker ticks, so `Plugin` state needs no locks and `-race` stays clean by construction.
- **`runClient` reuse:** a new `askDaemon(cmd, addr, timeout) (string, error)` becomes the shared UDP round-trip; `runClient` is reimplemented on top of it with its exact historical output preserved (dial/write errors print `error: no daemon reachable: <err>`; read errors print the bare `error: no daemon reachable`), distinguished via an `errNoReply` sentinel.
- **Poller sends `setState` only on change** (every `setState` makes OpenDeck re-render AND persist the profile to disk — pushing every 750ms would thrash disk). New instances get the last-known state pushed on `willAppear`.
- **Two `mutastic.exe` processes will run** (daemon + plugin child of OpenDeck). Safe: only the daemon binds UDP 42814 (the single-instance lock); the plugin is a pure client. The plugin logs to its own file (`deckplugin.log`) because two processes racing `mutastic.log`'s rename-rotation would be a real hazard.

---

### Task 1: Elgato protocol types and argv parsing (`internal/deckplugin/protocol.go`)

**Files:**
- Create: `internal/deckplugin/protocol.go`
- Test: `internal/deckplugin/protocol_test.go`

**Interfaces:**
- Consumes: nothing (stdlib only).
- Produces (later tasks rely on these exact signatures):
  - `type Config struct { Port int; PluginUUID, RegisterEvent, Info string }`
  - `func ParseArgs(args []string) (Config, error)`
  - `type Event struct { Event, Action, Context, Device string }`
  - `func DecodeEvent(data []byte) (Event, error)`
  - `func EncodeRegister(event, uuid string) []byte`
  - `func EncodeSetState(context string, state int) []byte`

- [ ] **Step 1: Write the failing tests**

Create `internal/deckplugin/protocol_test.go`:

```go
package deckplugin

import (
	"strings"
	"testing"
)

func TestParseArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    Config
		wantErr bool
	}{
		{
			// Exact argv OpenDeck passes (plugins/mod.rs): single-dash flags,
			// values as separate elements, -info last with raw JSON.
			name: "real OpenDeck argv",
			args: []string{"-port", "57116", "-pluginUUID", "com.danshapiro.mutastic.sdPlugin", "-registerEvent", "registerPlugin", "-info", `{"application":{"version":"2.13.1"}}`},
			want: Config{Port: 57116, PluginUUID: "com.danshapiro.mutastic.sdPlugin", RegisterEvent: "registerPlugin", Info: `{"application":{"version":"2.13.1"}}`},
		},
		{
			name: "info is optional",
			args: []string{"-port", "57117", "-pluginUUID", "x.sdPlugin", "-registerEvent", "registerPlugin"},
			want: Config{Port: 57117, PluginUUID: "x.sdPlugin", RegisterEvent: "registerPlugin"},
		},
		{name: "flag without value", args: []string{"-port"}, wantErr: true},
		{name: "bad port", args: []string{"-port", "nope", "-pluginUUID", "x", "-registerEvent", "e"}, wantErr: true},
		{name: "unknown flag", args: []string{"-port", "1", "-pluginUUID", "x", "-registerEvent", "e", "-bogus", "v"}, wantErr: true},
		{name: "missing required flags", args: []string{"-port", "57116"}, wantErr: true},
		{name: "empty argv", args: nil, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseArgs(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseArgs(%v) = %+v, want error", tt.args, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseArgs(%v): unexpected error: %v", tt.args, err)
			}
			if got != tt.want {
				t.Fatalf("ParseArgs(%v) = %+v, want %+v", tt.args, got, tt.want)
			}
		})
	}
}

func TestDecodeEventWillAppear(t *testing.T) {
	// Verbatim wire shape from OpenDeck's outbound/will_appear.rs serializer.
	frame := `{"event":"willAppear","action":"com.danshapiro.mutastic.mute","context":"sd-A00DA6141I07PW.Default.Keypad.5.0","device":"sd-A00DA6141I07PW","payload":{"settings":{},"coordinates":{"row":1,"column":2},"controller":"Keypad","state":0,"isInMultiAction":false}}`
	ev, err := DecodeEvent([]byte(frame))
	if err != nil {
		t.Fatalf("DecodeEvent: %v", err)
	}
	if ev.Event != "willAppear" {
		t.Errorf("Event = %q, want willAppear", ev.Event)
	}
	if ev.Action != "com.danshapiro.mutastic.mute" {
		t.Errorf("Action = %q, want com.danshapiro.mutastic.mute", ev.Action)
	}
	if ev.Context != "sd-A00DA6141I07PW.Default.Keypad.5.0" {
		t.Errorf("Context = %q, want the verbatim dotted string", ev.Context)
	}
	if ev.Device != "sd-A00DA6141I07PW" {
		t.Errorf("Device = %q, want sd-A00DA6141I07PW", ev.Device)
	}
}

func TestDecodeEventKeyDown(t *testing.T) {
	frame := `{"event":"keyDown","action":"com.danshapiro.mutastic.mute","context":"sd-A00DA6141I07PW.Default.Keypad.5.0","device":"sd-A00DA6141I07PW","payload":{"settings":{},"coordinates":{"row":1,"column":2},"controller":"Keypad","state":0,"isInMultiAction":false}}`
	ev, err := DecodeEvent([]byte(frame))
	if err != nil {
		t.Fatalf("DecodeEvent: %v", err)
	}
	if ev.Event != "keyDown" {
		t.Errorf("Event = %q, want keyDown", ev.Event)
	}
}

func TestDecodeEventRejectsGarbage(t *testing.T) {
	if _, err := DecodeEvent([]byte(`not json`)); err == nil {
		t.Error("DecodeEvent(not json) succeeded, want error")
	}
	if _, err := DecodeEvent([]byte(`{"payload":{}}`)); err == nil {
		t.Error(`DecodeEvent without "event" field succeeded, want error`)
	}
}

func TestEncodeRegister(t *testing.T) {
	got := string(EncodeRegister("registerPlugin", "com.danshapiro.mutastic.sdPlugin"))
	want := `{"event":"registerPlugin","uuid":"com.danshapiro.mutastic.sdPlugin"}`
	if got != want {
		t.Fatalf("EncodeRegister = %s, want %s", got, want)
	}
}

func TestEncodeSetState(t *testing.T) {
	got := string(EncodeSetState("sd-A00DA6141I07PW.Default.Keypad.5.0", 1))
	want := `{"event":"setState","context":"sd-A00DA6141I07PW.Default.Keypad.5.0","payload":{"state":1}}`
	if got != want {
		t.Fatalf("EncodeSetState = %s, want %s", got, want)
	}
	if !strings.Contains(string(EncodeSetState("a.b.c.5.0", 0)), `"state":0`) {
		t.Fatal("EncodeSetState must carry state 0 explicitly (omitempty would silently drop it)")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/deckplugin/ -v`
Expected: FAIL to build — `undefined: ParseArgs`, `undefined: Config`, etc. (build errors are the correct RED state for a not-yet-existing implementation).

- [ ] **Step 3: Write the implementation**

Create `internal/deckplugin/protocol.go`:

```go
// Package deckplugin implements the OpenDeck (Elgato Stream Deck SDK)
// plugin protocol for the mutastic mute button: registration over a
// WebSocket, inbound event decoding, and outbound setState encoding,
// plus the state machine that keeps the key icon in sync with the
// daemon's true mute state. The package is platform-free; the real
// WebSocket, UDP client, F24 injector, and log file are injected from
// package main (deckplugin.go) — the same pattern as internal/daemon's
// Device/CommandHandler/KeyInjector.
package deckplugin

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// Config is the launch configuration OpenDeck passes on the command line:
//
//	mutastic.exe -port 57116 -pluginUUID com.danshapiro.mutastic.sdPlugin -registerEvent registerPlugin -info {...}
//
// PluginUUID is the plugin DIRECTORY name (OpenDeck's plugin identity);
// RegisterEvent is the event name to send in the register frame (always
// "registerPlugin" today — use the given value, never hardcode). Info is
// captured for completeness and unused.
type Config struct {
	Port          int
	PluginUUID    string
	RegisterEvent string
	Info          string
}

// ParseArgs parses Elgato-style plugin argv: single-dash flags with the
// value as the NEXT argv element (never -port=N). args excludes the
// program name (and the optional "deckplugin" subcommand word). Unknown
// flags are errors so a mangled launch fails loudly in the log instead
// of half-working.
func ParseArgs(args []string) (Config, error) {
	var cfg Config
	for i := 0; i < len(args); i += 2 {
		if i+1 >= len(args) {
			return Config{}, fmt.Errorf("flag %s has no value", args[i])
		}
		val := args[i+1]
		switch args[i] {
		case "-port":
			p, err := strconv.Atoi(val)
			if err != nil {
				return Config{}, fmt.Errorf("bad -port %q: %v", val, err)
			}
			cfg.Port = p
		case "-pluginUUID":
			cfg.PluginUUID = val
		case "-registerEvent":
			cfg.RegisterEvent = val
		case "-info":
			cfg.Info = val
		default:
			return Config{}, fmt.Errorf("unknown flag %q", args[i])
		}
	}
	if cfg.Port == 0 || cfg.PluginUUID == "" || cfg.RegisterEvent == "" {
		return Config{}, fmt.Errorf("missing required flags: need -port, -pluginUUID, -registerEvent (got port=%d uuid=%q event=%q)", cfg.Port, cfg.PluginUUID, cfg.RegisterEvent)
	}
	return cfg, nil
}

// Event is the envelope of one inbound frame from OpenDeck. The payload
// is deliberately not modeled — this plugin only needs the envelope.
// Context is an opaque token ("<device>.<profile>.<controller>.<pos>.<index>")
// that MUST be echoed back verbatim in setState: OpenDeck re-parses it
// and silently drops messages whose context doesn't round-trip.
type Event struct {
	Event   string `json:"event"`
	Action  string `json:"action"`
	Context string `json:"context"`
	Device  string `json:"device"`
}

// DecodeEvent decodes one inbound frame. Events the caller doesn't
// handle still decode fine; a missing "event" field is an error.
func DecodeEvent(data []byte) (Event, error) {
	var ev Event
	if err := json.Unmarshal(data, &ev); err != nil {
		return Event{}, fmt.Errorf("decode event: %w", err)
	}
	if ev.Event == "" {
		return Event{}, fmt.Errorf("decode event: missing \"event\" field in %.120s", data)
	}
	return ev, nil
}

type registerMsg struct {
	Event string `json:"event"`
	UUID  string `json:"uuid"`
}

// EncodeRegister builds the FIRST frame the plugin must send after the
// WebSocket connects: {"event":"registerPlugin","uuid":"<dir name>"}.
// A malformed or wrong-uuid register is OpenDeck's #1 silent failure
// mode: the socket is never added to its registry and the plugin
// receives nothing, with no error anywhere.
func EncodeRegister(event, uuid string) []byte {
	data, _ := json.Marshal(registerMsg{Event: event, UUID: uuid}) // marshal of plain strings cannot fail
	return data
}

type setStatePayload struct {
	State int `json:"state"`
}

type setStateMsg struct {
	Event   string          `json:"event"`
	Context string          `json:"context"`
	Payload setStatePayload `json:"payload"`
}

// EncodeSetState builds the setState frame that drives the key icon:
// {"event":"setState","context":C,"payload":{"state":N}}. OpenDeck
// bounds-checks state against the instance's states (a 2-state action
// accepts only 0 and 1) and authorizes context against the registered
// uuid; failures are silent no-ops on its side.
func EncodeSetState(context string, state int) []byte {
	data, _ := json.Marshal(setStateMsg{Event: "setState", Context: context, Payload: setStatePayload{State: state}}) // cannot fail
	return data
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/deckplugin/ -v`
Expected: PASS (all `TestParseArgs` subtests, `TestDecodeEvent*`, `TestEncode*`).

- [ ] **Step 5: Run the full gate**

Run: `go test -race ./... && go vet ./...`
Expected: all packages PASS, vet clean.

- [ ] **Step 6: Commit**

```bash
git add internal/deckplugin/protocol.go internal/deckplugin/protocol_test.go
git commit -m "feat(deckplugin): Elgato plugin protocol types and arg parsing"
```

---

### Task 2: Plugin core — visibility tracking and status-driven icon (`internal/deckplugin/plugin.go`)

**Files:**
- Create: `internal/deckplugin/plugin.go`
- Test: `internal/deckplugin/plugin_test.go`

**Interfaces:**
- Consumes (from Task 1): `DecodeEvent`, `EncodeSetState`, `Event`.
- Produces (later tasks rely on these exact signatures):
  - `type Conn interface { ReadMessage() ([]byte, error); WriteMessage(data []byte) error }`
  - `type DaemonClient interface { Command(cmd string) (string, error) }`
  - `type Injector interface { Inject() error }`
  - `func New(conn Conn, daemonClient DaemonClient, inject Injector, logger *log.Logger) *Plugin`
  - `func (p *Plugin) HandleMessage(data []byte)`
  - `func (p *Plugin) PollOnce()`
  - unexported: `desiredState(reply string) (int, bool)`, `stateLive = 0`, `stateMuted = 1`
- Test fakes produced here and reused by Tasks 3–4: `fakeConn` (with `newFakeConn()`, fields `frames chan []byte`, `readErr chan error`, methods `writeCount() int`, `write(i int) string`), `fakeDaemon` (fields `replies map[string]string`, `err error`, `calls []string`; methods `setReply`, `setErr`, `callCount() int`, `call(i int) string`), `testLogger()`, `waitFor(t, what, cond)`, `const willAppearFrame`, `frameFor(event, ctx string) []byte`.

- [ ] **Step 1: Write the failing tests**

Create `internal/deckplugin/plugin_test.go`:

```go
package deckplugin

import (
	"fmt"
	"io"
	"log"
	"sync"
	"testing"
	"time"
)

// willAppearFrame is the verbatim wire shape for the real deck's
// lower-right key (device sd-X stands in for sd-A00DA6141I07PW).
const willAppearFrame = `{"event":"willAppear","action":"com.danshapiro.mutastic.mute","context":"sd-X.Default.Keypad.5.0","device":"sd-X","payload":{"settings":{},"coordinates":{"row":1,"column":2},"controller":"Keypad","state":0,"isInMultiAction":false}}`

// frameFor builds an event frame for an arbitrary context.
func frameFor(event, ctx string) []byte {
	return fmt.Appendf(nil, `{"event":%q,"action":"com.danshapiro.mutastic.mute","context":%q,"device":"sd-X","payload":{"settings":{},"coordinates":{"row":1,"column":2},"controller":"Keypad","state":0,"isInMultiAction":false}}`, event, ctx)
}

func testLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// waitFor polls cond every 5ms for up to 2s (mirrors daemon_test.go).
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// fakeConn implements Conn: writes are recorded; reads block on channels
// (mirrors daemon_test.go's fakeDevice shape).
type fakeConn struct {
	mu      sync.Mutex
	writes  [][]byte
	frames  chan []byte
	readErr chan error
}

func newFakeConn() *fakeConn {
	return &fakeConn{frames: make(chan []byte, 8), readErr: make(chan error, 1)}
}

func (f *fakeConn) ReadMessage() ([]byte, error) {
	select {
	case fr := <-f.frames:
		return fr, nil
	case err := <-f.readErr:
		return nil, err
	}
}

func (f *fakeConn) WriteMessage(data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := make([]byte, len(data))
	copy(c, data)
	f.writes = append(f.writes, c)
	return nil
}

func (f *fakeConn) writeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.writes)
}

func (f *fakeConn) write(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return string(f.writes[i])
}

// fakeDaemon implements DaemonClient with scripted replies per command.
type fakeDaemon struct {
	mu      sync.Mutex
	replies map[string]string
	err     error
	calls   []string
}

func (f *fakeDaemon) Command(cmd string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, cmd)
	if f.err != nil {
		return "", f.err
	}
	return f.replies[cmd], nil
}

func (f *fakeDaemon) setReply(cmd, reply string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies[cmd] = reply
}

func (f *fakeDaemon) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeDaemon) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeDaemon) call(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[i]
}

func TestDesiredState(t *testing.T) {
	tests := []struct {
		reply string
		state int
		ok    bool
	}{
		{"muted", stateMuted, true},
		{"unmuted", stateLive, true},
		{"unknown", 0, false},          // normal after daemon restart: keep current icon
		{"error: no device", 0, false}, // daemon error replies carry no state
		{"", 0, false},
	}
	for _, tt := range tests {
		st, ok := desiredState(tt.reply)
		if ok != tt.ok || (ok && st != tt.state) {
			t.Errorf("desiredState(%q) = (%d, %v), want (%d, %v)", tt.reply, st, ok, tt.state, tt.ok)
		}
	}
}

func TestWillAppearPushesKnownState(t *testing.T) {
	conn := newFakeConn()
	fd := &fakeDaemon{replies: map[string]string{"status": "muted"}}
	p := New(conn, fd, nil, testLogger())
	p.HandleMessage([]byte(willAppearFrame))
	if got := conn.writeCount(); got != 1 {
		t.Fatalf("writes = %d, want 1 setState after willAppear", got)
	}
	want := `{"event":"setState","context":"sd-X.Default.Keypad.5.0","payload":{"state":1}}`
	if got := conn.write(0); got != want {
		t.Fatalf("frame = %s, want %s", got, want)
	}
}

func TestWillAppearUnknownStatusPushesNothing(t *testing.T) {
	conn := newFakeConn()
	fd := &fakeDaemon{replies: map[string]string{"status": "unknown"}}
	p := New(conn, fd, nil, testLogger())
	p.HandleMessage([]byte(willAppearFrame))
	if got := conn.writeCount(); got != 0 {
		t.Fatalf("writes = %d, want 0: unknown state must leave the icon alone", got)
	}
}

func TestSecondInstanceGetsStateOnAppear(t *testing.T) {
	conn := newFakeConn()
	fd := &fakeDaemon{replies: map[string]string{"status": "muted"}}
	p := New(conn, fd, nil, testLogger())
	p.HandleMessage([]byte(willAppearFrame))
	p.HandleMessage(frameFor("willAppear", "sd-X.Other.Keypad.2.0"))
	if got := conn.writeCount(); got != 2 {
		t.Fatalf("writes = %d, want 2: each appearing instance gets the known state", got)
	}
	want := `{"event":"setState","context":"sd-X.Other.Keypad.2.0","payload":{"state":1}}`
	if got := conn.write(1); got != want {
		t.Fatalf("second frame = %s, want %s", got, want)
	}
}

func TestPollOncePushesOnlyOnChange(t *testing.T) {
	conn := newFakeConn()
	fd := &fakeDaemon{replies: map[string]string{"status": "unmuted"}}
	p := New(conn, fd, nil, testLogger())
	p.HandleMessage([]byte(willAppearFrame)) // pushes state 0
	base := conn.writeCount()

	p.PollOnce() // unchanged: no push
	if got := conn.writeCount(); got != base {
		t.Fatalf("writes after unchanged poll = %d, want %d (setState persists the profile; only push on change)", got, base)
	}

	fd.setReply("status", "muted")
	p.PollOnce()
	if got := conn.writeCount(); got != base+1 {
		t.Fatalf("writes after changed poll = %d, want %d", got, base+1)
	}
	want := `{"event":"setState","context":"sd-X.Default.Keypad.5.0","payload":{"state":1}}`
	if got := conn.write(base); got != want {
		t.Fatalf("frame = %s, want %s", got, want)
	}

	p.PollOnce() // still muted: no new push
	if got := conn.writeCount(); got != base+1 {
		t.Fatalf("writes after second unchanged poll = %d, want %d", got, base+1)
	}
}

func TestPollOnceUnreachableDaemonLeavesIcon(t *testing.T) {
	conn := newFakeConn()
	fd := &fakeDaemon{replies: map[string]string{"status": "muted"}}
	p := New(conn, fd, nil, testLogger())
	p.HandleMessage([]byte(willAppearFrame))
	base := conn.writeCount()
	fd.setErr(fmt.Errorf("no reply from daemon"))
	p.PollOnce()
	p.PollOnce()
	if got := conn.writeCount(); got != base {
		t.Fatalf("writes = %d, want %d: unreachable daemon must not change the icon", got, base)
	}
}

func TestPollOnceSkipsWhenNothingVisible(t *testing.T) {
	conn := newFakeConn()
	fd := &fakeDaemon{replies: map[string]string{"status": "muted"}}
	p := New(conn, fd, nil, testLogger())
	p.PollOnce()
	if got := fd.callCount(); got != 0 {
		t.Fatalf("daemon calls = %d, want 0: no visible instance means no polling", got)
	}
}

func TestWillDisappearStopsPushes(t *testing.T) {
	conn := newFakeConn()
	fd := &fakeDaemon{replies: map[string]string{"status": "unmuted"}}
	p := New(conn, fd, nil, testLogger())
	p.HandleMessage([]byte(willAppearFrame))
	p.HandleMessage(frameFor("willDisappear", "sd-X.Default.Keypad.5.0"))
	base := conn.writeCount()
	fd.setReply("status", "muted")
	p.PollOnce()
	if got := conn.writeCount(); got != base {
		t.Fatalf("writes = %d, want %d: no visible instances, nothing to push", got, base)
	}
}

func TestUnknownEventsAreIgnored(t *testing.T) {
	conn := newFakeConn()
	fd := &fakeDaemon{replies: map[string]string{}}
	p := New(conn, fd, nil, testLogger())
	// titleParametersDidChange follows every willAppear; garbage must not crash.
	p.HandleMessage([]byte(`{"event":"titleParametersDidChange","context":"sd-X.Default.Keypad.5.0","payload":{}}`))
	p.HandleMessage([]byte(`not json at all`))
	if got := conn.writeCount(); got != 0 {
		t.Fatalf("writes = %d, want 0", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/deckplugin/ -v`
Expected: FAIL to build — `undefined: New`, `undefined: desiredState`, `undefined: stateMuted`, etc. (Task 1 code compiles; the new test file references symbols that don't exist yet.)

- [ ] **Step 3: Write the implementation**

Create `internal/deckplugin/plugin.go`:

```go
package deckplugin

import (
	"log"
)

// The manifest's two state indices. The plugin alone drives these via
// setState: DisableAutomaticStates is true in the manifest AND
// disable_automatic_states is true in the profile instance, so OpenDeck
// never flips the icon on its own.
const (
	stateLive  = 0 // state 0: live mic (icons/mutastic-mic)
	stateMuted = 1 // state 1: muted (icons/mutastic-mic-muted)
)

// Conn is the minimal WebSocket surface the plugin needs. Implemented by
// package main's gorilla/websocket adapter; tests use a channel-backed
// fake (mirrors daemon.Device / light.Port).
type Conn interface {
	ReadMessage() ([]byte, error) // blocks until one text frame arrives
	WriteMessage(data []byte) error
}

// DaemonClient sends one plain-text command to the mutastic daemon
// (UDP 127.0.0.1:42814) and returns the trimmed reply: "muted",
// "unmuted", "unknown", or "error: <reason>". A non-nil error means the
// daemon was unreachable (no reply arrived).
type DaemonClient interface {
	Command(cmd string) (string, error)
}

// Injector delivers one synthetic F24 keystroke. Structurally identical
// to daemon.KeyInjector so package main's newKeyInjector() satisfies it;
// redeclared here to keep this brick free of daemon imports.
type Injector interface {
	Inject() error
}

// Plugin is one running plugin session. All methods are called from a
// single goroutine (Run's select loop feeds HandleMessage and PollOnce),
// so there is no internal locking by design.
type Plugin struct {
	conn   Conn
	daemon DaemonClient
	inject Injector // may be nil (non-Windows): keyDown skips the F24 sweep
	logger *log.Logger

	visible   map[string]bool // context -> instance currently on a visible key
	lastKnown int             // last state observed/pushed; -1 = never known
	pollDown  bool            // daemon was unreachable at the last poll (log transitions, not every 750ms)
}

// New builds a Plugin. inject may be nil (no key injection on this
// platform); logger must not be nil (tests pass log.New(io.Discard,"",0)).
func New(conn Conn, daemonClient DaemonClient, inject Injector, logger *log.Logger) *Plugin {
	return &Plugin{
		conn:      conn,
		daemon:    daemonClient,
		inject:    inject,
		logger:    logger,
		visible:   make(map[string]bool),
		lastKnown: -1,
	}
}

// desiredState maps a daemon status/toggle reply to the OpenDeck state
// index. ok=false means the reply carries no usable state ("unknown" is
// normal after a daemon restart; "error: ..." likewise) and the caller
// must leave the current icon alone.
func desiredState(reply string) (state int, ok bool) {
	switch reply {
	case "muted":
		return stateMuted, true
	case "unmuted":
		return stateLive, true
	}
	return 0, false
}

// HandleMessage processes one inbound frame. Events the plugin doesn't
// handle are ignored by design: titleParametersDidChange follows every
// willAppear, and deviceDidConnect / systemDidWakeUp / keyUp arrive
// unrequested (keyUp can be suppressed by OpenDeck on profile switches,
// which is why the mute flow acts on keyDown alone).
func (p *Plugin) HandleMessage(data []byte) {
	ev, err := DecodeEvent(data)
	if err != nil {
		p.logger.Printf("ignoring undecodable frame: %v", err)
		return
	}
	switch ev.Event {
	case "willAppear":
		p.visible[ev.Context] = true
		p.logger.Printf("willAppear %s (visible: %d)", ev.Context, len(p.visible))
		// Correct this key's icon immediately instead of waiting a tick.
		if reply, err := p.daemon.Command("status"); err != nil {
			p.logger.Printf("willAppear %s: status failed: %v", ev.Context, err)
		} else if st, ok := desiredState(reply); ok {
			p.lastKnown = st
		}
		if p.lastKnown >= 0 {
			p.sendSetState(ev.Context, p.lastKnown)
		}
	case "willDisappear":
		delete(p.visible, ev.Context)
		p.logger.Printf("willDisappear %s (visible: %d)", ev.Context, len(p.visible))
	}
}

// PollOnce queries the daemon's status once and, when the mute state is
// known and has CHANGED, pushes setState to every visible instance.
// Pushing only on change matters: OpenDeck persists the profile to disk
// on every setState. Unknown or unreachable leaves the icons untouched.
func (p *Plugin) PollOnce() {
	if len(p.visible) == 0 {
		return
	}
	reply, err := p.daemon.Command("status")
	if err != nil {
		if !p.pollDown {
			p.pollDown = true
			p.logger.Printf("status poll: daemon unreachable, keeping icon: %v", err)
		}
		return
	}
	if p.pollDown {
		p.pollDown = false
		p.logger.Printf("status poll: daemon reachable again")
	}
	st, ok := desiredState(reply)
	if !ok {
		return // "unknown" or "error: ...": keep the current icon
	}
	if st == p.lastKnown {
		return
	}
	p.lastKnown = st
	p.pushAll()
}

// pushAll sends the last-known state to every visible instance.
func (p *Plugin) pushAll() {
	for ctx := range p.visible {
		p.sendSetState(ctx, p.lastKnown)
	}
}

// sendSetState writes one setState frame and logs it. The log line is
// load-bearing: the live E2E greps deckplugin.log for
// "setState <context> -> <state>".
func (p *Plugin) sendSetState(ctx string, state int) {
	if err := p.conn.WriteMessage(EncodeSetState(ctx, state)); err != nil {
		p.logger.Printf("setState %s -> %d: write failed: %v", ctx, state, err)
		return
	}
	p.logger.Printf("setState %s -> %d", ctx, state)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/deckplugin/ -v`
Expected: PASS — all Task 1 + Task 2 tests.

- [ ] **Step 5: Run the full gate**

Run: `go test -race ./... && go vet ./...`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add internal/deckplugin/plugin.go internal/deckplugin/plugin_test.go
git commit -m "feat(deckplugin): track visible instances and sync icon from daemon status"
```

---

### Task 3: keyDown — the full mute-everything flow in-process

**Files:**
- Modify: `internal/deckplugin/plugin.go` (add `keyDown` case + `handleKeyDown`)
- Test: `internal/deckplugin/plugin_test.go` (append)

**Interfaces:**
- Consumes (Task 2): `Plugin`, `Injector`, `desiredState`, `pushAll`, fakes `fakeConn`/`fakeDaemon`, `frameFor`.
- Produces: `keyDown` handling inside `HandleMessage`; test fake `fakeInjector` (fields `calls atomic.Int32`, `err error`) reused by later tasks.

- [ ] **Step 1: Write the failing tests**

Append to `internal/deckplugin/plugin_test.go` (add `"errors"` and `"sync/atomic"` to its imports):

```go
// fakeInjector mirrors internal/daemon/daemon_test.go's fakeInjector:
// counts calls, returns err.
type fakeInjector struct {
	calls atomic.Int32
	err   error
}

func (f *fakeInjector) Inject() error {
	f.calls.Add(1)
	return f.err
}

func TestKeyDownTogglesAndInjects(t *testing.T) {
	conn := newFakeConn()
	fd := &fakeDaemon{replies: map[string]string{"status": "unmuted", "toggle": "muted"}}
	inj := &fakeInjector{}
	p := New(conn, fd, inj, testLogger())
	p.HandleMessage([]byte(willAppearFrame)) // establishes state 0
	base := conn.writeCount()

	p.HandleMessage(frameFor("keyDown", "sd-X.Default.Keypad.5.0"))

	if got := fd.call(fd.callCount() - 1); got != "toggle" {
		t.Fatalf("last daemon command = %q, want toggle", got)
	}
	if got := inj.calls.Load(); got != 1 {
		t.Fatalf("injections = %d, want exactly 1 F24 sweep per keyDown", got)
	}
	// The toggle reply IS the new state: the icon updates immediately,
	// without waiting for the next poll.
	if got := conn.writeCount(); got != base+1 {
		t.Fatalf("writes = %d, want %d (one setState from the toggle reply)", got, base+1)
	}
	want := `{"event":"setState","context":"sd-X.Default.Keypad.5.0","payload":{"state":1}}`
	if got := conn.write(base); got != want {
		t.Fatalf("frame = %s, want %s", got, want)
	}
}

func TestKeyDownInjectsEvenWhenDaemonDown(t *testing.T) {
	// mute-everything.cmd runs its two lines unconditionally (not &&);
	// the plugin mirrors that: a dead daemon must not stop the app sweep.
	conn := newFakeConn()
	fd := &fakeDaemon{replies: map[string]string{}}
	fd.setErr(errors.New("no reply from daemon"))
	inj := &fakeInjector{}
	p := New(conn, fd, inj, testLogger())
	p.HandleMessage(frameFor("keyDown", "sd-X.Default.Keypad.5.0"))
	if got := inj.calls.Load(); got != 1 {
		t.Fatalf("injections = %d, want 1 even with the daemon down", got)
	}
	if got := conn.writeCount(); got != 0 {
		t.Fatalf("writes = %d, want 0: no state to show", got)
	}
}

func TestKeyDownNilInjectorStillToggles(t *testing.T) {
	// Non-Windows: newKeyInjector() returns nil. The daemon toggle must
	// still run; only the F24 sweep is skipped.
	conn := newFakeConn()
	fd := &fakeDaemon{replies: map[string]string{"toggle": "muted"}}
	p := New(conn, fd, nil, testLogger())
	p.HandleMessage(frameFor("keyDown", "sd-X.Default.Keypad.5.0"))
	if got := fd.callCount(); got != 1 || fd.call(0) != "toggle" {
		t.Fatalf("daemon calls = %d, want exactly one toggle call", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/deckplugin/ -run TestKeyDown -v`
Expected: FAIL — `TestKeyDownTogglesAndInjects` reports 0 injections / no toggle call (the `keyDown` case doesn't exist yet, so the event is silently ignored).

- [ ] **Step 3: Write the implementation**

In `internal/deckplugin/plugin.go`, add a case to `HandleMessage`'s switch (after the `willDisappear` case):

```go
	case "keyDown":
		p.handleKeyDown(ev)
```

And add the handler at the end of the file:

```go
// handleKeyDown runs the full mute-everything flow in-process, mirroring
// deploy/mute-everything.cmd's two unconditional lines: daemon toggle
// FIRST, then exactly one F24 injection for the meeting-app sweep. Each
// half runs even if the other fails. LOOP HAZARD: never inject F24 in
// reaction to a state change or the daemon's own injection — F24 must
// only ever mean "sweep the meeting apps once for this key press".
func (p *Plugin) handleKeyDown(ev Event) {
	reply, err := p.daemon.Command("toggle")
	if err != nil {
		p.logger.Printf("keyDown %s: toggle failed: %v", ev.Context, err)
	} else {
		p.logger.Printf("keyDown %s: toggle -> %q", ev.Context, reply)
		// The toggle reply is the NEW state — update the icon now
		// instead of waiting for the next poll tick.
		if st, ok := desiredState(reply); ok && st != p.lastKnown {
			p.lastKnown = st
			p.pushAll()
		}
	}
	if p.inject == nil {
		p.logger.Printf("keyDown %s: no key injector on this platform, skipping F24 sweep", ev.Context)
	} else if err := p.inject.Inject(); err != nil {
		p.logger.Printf("keyDown %s: F24 inject failed: %v", ev.Context, err)
	} else {
		p.logger.Printf("keyDown %s: injected F24 app sweep", ev.Context)
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/deckplugin/ -v`
Expected: PASS (all package tests).

- [ ] **Step 5: Run the full gate**

Run: `go test -race ./... && go vet ./...`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add internal/deckplugin/plugin.go internal/deckplugin/plugin_test.go
git commit -m "feat(deckplugin): keyDown runs the full mute-everything flow in-process"
```

---

### Task 4: The `Run` event loop (register, read, poll)

**Files:**
- Modify: `internal/deckplugin/plugin.go` (add `PollInterval`, `Run`)
- Test: `internal/deckplugin/plugin_test.go` (append)

**Interfaces:**
- Consumes (Tasks 1–3): `EncodeRegister`, `HandleMessage`, `PollOnce`, fakes.
- Produces (Task 5 relies on): `var PollInterval = 750 * time.Millisecond` and
  `func (p *Plugin) Run(ctx context.Context, registerEvent, pluginUUID string) error` — returns `nil` when OpenDeck closes the socket (normal end of life), `ctx.Err()` on cancellation, non-nil error if the register write fails.

- [ ] **Step 1: Write the failing tests**

Append to `internal/deckplugin/plugin_test.go` (add `"context"` to its imports):

```go
func TestRunSendsRegisterFirst(t *testing.T) {
	conn := newFakeConn()
	p := New(conn, &fakeDaemon{replies: map[string]string{}}, nil, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx, "registerPlugin", "com.danshapiro.mutastic.sdPlugin") }()

	waitFor(t, "register frame", func() bool { return conn.writeCount() >= 1 })
	want := `{"event":"registerPlugin","uuid":"com.danshapiro.mutastic.sdPlugin"}`
	if got := conn.write(0); got != want {
		t.Fatalf("first frame = %s, want %s (register MUST be the very first frame)", got, want)
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("Run after cancel = nil, want context error")
	}
	conn.readErr <- errors.New("unblock the reader goroutine")
}

func TestRunReturnsNilOnSocketClose(t *testing.T) {
	conn := newFakeConn()
	p := New(conn, &fakeDaemon{replies: map[string]string{}}, nil, testLogger())
	conn.readErr <- errors.New("connection closed by OpenDeck")
	if err := p.Run(context.Background(), "registerPlugin", "x.sdPlugin"); err != nil {
		t.Fatalf("Run = %v, want nil: a closed socket is the normal end of life", err)
	}
}

func TestRunHandlesEventsAndPolls(t *testing.T) {
	old := PollInterval
	PollInterval = 5 * time.Millisecond
	t.Cleanup(func() { PollInterval = old })

	conn := newFakeConn()
	fd := &fakeDaemon{replies: map[string]string{"status": "muted"}}
	p := New(conn, fd, nil, testLogger())
	done := make(chan error, 1)
	go func() { done <- p.Run(context.Background(), "registerPlugin", "com.danshapiro.mutastic.sdPlugin") }()

	conn.frames <- []byte(willAppearFrame)
	// write 0 = register, write 1 = setState(1) from willAppear.
	waitFor(t, "setState from willAppear", func() bool { return conn.writeCount() >= 2 })

	// Flip the daemon state out-of-band (models the physical mic button);
	// the ticker poll must observe it and push the change.
	fd.setReply("status", "unmuted")
	waitFor(t, "setState from poll", func() bool { return conn.writeCount() >= 3 })
	want := `{"event":"setState","context":"sd-X.Default.Keypad.5.0","payload":{"state":0}}`
	if got := conn.write(2); got != want {
		t.Fatalf("poll frame = %s, want %s", got, want)
	}

	conn.readErr <- errors.New("closing")
	if err := <-done; err != nil {
		t.Fatalf("Run = %v, want nil on socket close", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/deckplugin/ -run TestRun -v`
Expected: FAIL to build — `undefined: PollInterval`, `p.Run undefined`.

- [ ] **Step 3: Write the implementation**

In `internal/deckplugin/plugin.go`, extend the import block to `"context"`, `"fmt"`, `"log"`, `"time"`. Add near the state constants:

```go
// PollInterval is how often the plugin polls the daemon's status while
// at least one instance is visible. ~750ms keeps the icon honest within
// a blink of a physical mic-button press. A var so tests can shrink it
// (restore via t.Cleanup, registered before the loop starts — same
// discipline as daemon_test.go's timing knobs).
var PollInterval = 750 * time.Millisecond
```

Add at the end of the file:

```go
// Run registers with OpenDeck and processes events until the WebSocket
// closes or ctx is cancelled. A read error is the NORMAL end of life
// (OpenDeck kills or closes plugins when it exits and never restarts
// them), so it returns nil. One reader goroutine feeds the select loop;
// HandleMessage and PollOnce only ever run on this goroutine, which is
// what makes the lock-free Plugin state safe.
func (p *Plugin) Run(ctx context.Context, registerEvent, pluginUUID string) error {
	if err := p.conn.WriteMessage(EncodeRegister(registerEvent, pluginUUID)); err != nil {
		return fmt.Errorf("register: %w", err)
	}
	p.logger.Printf("registered as %s (event %s)", pluginUUID, registerEvent)

	frames := make(chan []byte)
	readErrs := make(chan error, 1)
	go func() {
		for {
			data, err := p.conn.ReadMessage()
			if err != nil {
				readErrs <- err
				return
			}
			select {
			case frames <- data:
			case <-ctx.Done():
				return
			}
		}
	}()

	tick := time.NewTicker(PollInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-readErrs:
			p.logger.Printf("websocket closed: %v", err)
			return nil
		case data := <-frames:
			p.HandleMessage(data)
		case <-tick.C:
			p.PollOnce()
		}
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/deckplugin/ -race -v`
Expected: PASS, race-clean (the single-goroutine confinement is the thing under test here).

- [ ] **Step 5: Run the full gate**

Run: `go test -race ./... && go vet ./...`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add internal/deckplugin/plugin.go internal/deckplugin/plugin_test.go
git commit -m "feat(deckplugin): event loop with register-first and status polling"
```

---

### Task 5: Wire the plugin mode into the mutastic binary

**Files:**
- Create: `deckplugin.go` (root `package main`)
- Create: `deckplugin_test.go` (root)
- Modify: `main.go` — `main()` dispatch (after the `daemon` special case, currently `main.go:42-47`), `usage()` (`main.go:75-81`), `openLogFile` → `openNamedLogFile` (`main.go:165-185`), `runClient` (`main.go:83-109`)
- Modify: `go.mod`, `go.sum` (gorilla/websocket)

**Interfaces:**
- Consumes: `deckplugin.ParseArgs/Config/New/Run/Conn/DaemonClient/Injector` (Tasks 1–4); existing `newKeyInjector() daemon.KeyInjector` (root, `inject_windows.go` / `inject_other.go` — nil off-Windows), `const udpAddr = "127.0.0.1:42814"` (main.go:24), `nopWriteCloser` (main.go).
- Produces:
  - `func askDaemon(cmd, addr string, timeout time.Duration) (string, error)` + `var errNoReply` (in `deckplugin.go`)
  - `func runDeckPlugin(args []string) int`
  - `func openNamedLogFile(name string) (io.WriteCloser, string, error)` (with `openLogFile()` delegating to it — daemon behavior unchanged)
  - `mutastic deckplugin ...` and auto-detected `-port` launch modes.

- [ ] **Step 1: Record the client baseline (existing behavior must not change)**

Run: `go test . -v`
Expected: PASS. Note the `TestRunClient*` / `TestClientCommand*` test names — they pin `runClient`'s exact output and must pass identically at the end of this task.

- [ ] **Step 2: Add the dependency**

```bash
go get github.com/gorilla/websocket@v1.5.3 && go mod tidy
```

Expected: `go.mod` gains `require github.com/gorilla/websocket v1.5.3`; `go.sum` updated; no other requirement changes. (gorilla/websocket is pure Go with zero deps — the mingw cgo cross-compile is unaffected; Step 9 proves it.)

- [ ] **Step 3: Write the failing tests**

Create `deckplugin_test.go` (root, `package main`):

```go
package main

import (
	"net"
	"testing"
	"time"
)

// TestAskDaemon exercises the reply-returning UDP round trip against a
// scripted fake daemon on an ephemeral port (same idiom as
// main_test.go's runClient tests).
func TestAskDaemon(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 64)
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		if string(buf[:n]) != "status" {
			pc.WriteTo([]byte("error: unknown command"), addr)
			return
		}
		pc.WriteTo([]byte("muted\n"), addr) // trailing newline: reply must be trimmed
	}()
	reply, err := askDaemon("status", pc.LocalAddr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("askDaemon: %v", err)
	}
	if reply != "muted" {
		t.Fatalf("reply = %q, want %q (trimmed)", reply, "muted")
	}
}

func TestAskDaemonUnreachable(t *testing.T) {
	// Bind then close: guarantees nothing listens on the port.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := pc.LocalAddr().String()
	pc.Close()
	if _, err := askDaemon("status", addr, 200*time.Millisecond); err == nil {
		t.Fatal("askDaemon to a dead port succeeded, want error")
	}
}
```

- [ ] **Step 4: Run tests to verify they fail**

Run: `go test . -run TestAskDaemon -v`
Expected: FAIL to build — `undefined: askDaemon`.

- [ ] **Step 5: Write `deckplugin.go`**

Create `deckplugin.go` (root):

```go
// deckplugin.go wires the OpenDeck (Elgato Stream Deck SDK) plugin mode
// into the mutastic binary. OpenDeck spawns this exe from
// %APPDATA%\opendeck\plugins\com.danshapiro.mutastic.sdPlugin\ with
//
//	mutastic.exe -port <N> -pluginUUID <dir name> -registerEvent registerPlugin -info <json>
//
// (working directory = the plugin dir, CREATE_NO_WINDOW, stdout/stderr
// redirected to OpenDeck's per-plugin log). main() detects either the
// explicit "deckplugin" subcommand or that leading -port flag. The
// platform-free protocol + state machine live in internal/deckplugin;
// this file supplies the real WebSocket, UDP client, F24 injector, and
// log file.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"mutastic/internal/deckplugin"
)

// errNoReply distinguishes "daemon reached but no reply arrived" from
// dial/write failures, so runClient can preserve its exact historical
// output for each case.
var errNoReply = errors.New("no reply from daemon")

// askDaemon sends one UDP command to the daemon and returns the trimmed
// reply. It is the reply-returning core of runClient (which prints);
// both share the daemon's plain-text protocol on udpAddr.
func askDaemon(cmd, addr string, timeout time.Duration) (string, error) {
	conn, err := net.Dial("udp", addr)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write([]byte(cmd)); err != nil {
		return "", err
	}
	buf := make([]byte, 2048) // multi-light list/fan-out replies exceed 256 bytes
	n, err := conn.Read(buf)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errNoReply, err)
	}
	return strings.TrimSpace(string(buf[:n])), nil
}

// wsConn adapts gorilla/websocket to deckplugin.Conn.
type wsConn struct{ c *websocket.Conn }

func (w wsConn) ReadMessage() ([]byte, error) {
	_, data, err := w.c.ReadMessage()
	return data, err
}

func (w wsConn) WriteMessage(data []byte) error {
	return w.c.WriteMessage(websocket.TextMessage, data)
}

// udpDaemonClient implements deckplugin.DaemonClient: one UDP round trip
// per call with the mic-verb timeout (1s, same as the CLI client).
type udpDaemonClient struct {
	addr    string
	timeout time.Duration
}

func (u udpDaemonClient) Command(cmd string) (string, error) {
	return askDaemon(cmd, u.addr, u.timeout)
}

// runDeckPlugin is the plugin-mode entry point. args excludes the
// program name and the optional "deckplugin" word. Exit codes: 0 clean
// shutdown (OpenDeck closed the socket), 1 runtime failure, 2 bad usage.
func runDeckPlugin(args []string) int {
	cfg, err := deckplugin.ParseArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "deckplugin:", err)
		return 2
	}

	logw, logPath, err := openNamedLogFile("deckplugin.log")
	if err != nil {
		fmt.Fprintln(os.Stderr, "deckplugin: cannot open log file:", err)
		logw = nopWriteCloser{}
	}
	defer logw.Close()
	// Logfile FIRST: io.MultiWriter aborts on the first destination
	// error, and stderr here is OpenDeck's redirected pipe, which dies
	// with OpenDeck. Same invariant as the daemon logger in main.go.
	logger := log.New(io.MultiWriter(logw, os.Stderr), "", log.LstdFlags)
	logger.Printf("deckplugin starting: port=%d uuid=%s (log: %s)", cfg.Port, cfg.PluginUUID, logPath)

	// OpenDeck binds its WebSocket server before spawning plugins, but a
	// short retry makes startup races and slow boots harmless.
	url := fmt.Sprintf("ws://127.0.0.1:%d", cfg.Port)
	var ws *websocket.Conn
	for attempt := 1; ; attempt++ {
		ws, _, err = websocket.DefaultDialer.Dial(url, nil)
		if err == nil {
			break
		}
		if attempt >= 5 {
			logger.Printf("dial %s failed after %d attempts: %v", url, attempt, err)
			return 1
		}
		logger.Printf("dial %s (attempt %d): %v -- retrying in 1s", url, attempt, err)
		time.Sleep(time.Second)
	}
	defer ws.Close()

	var inject deckplugin.Injector
	if ki := newKeyInjector(); ki != nil {
		inject = ki // nil on non-Windows: keyDown still toggles the daemon, skips F24
	}
	p := deckplugin.New(wsConn{ws}, udpDaemonClient{udpAddr, time.Second}, inject, logger)
	if err := p.Run(context.Background(), cfg.RegisterEvent, cfg.PluginUUID); err != nil {
		logger.Printf("deckplugin exiting: %v", err)
		return 1
	}
	logger.Printf("deckplugin exiting (socket closed)")
	return 0
}
```

- [ ] **Step 6: Refactor `main.go` — log file, dispatch, usage, runClient**

Four edits, all in `main.go`:

**(a)** Replace `openLogFile` (currently at `main.go:165-185`) with a parameterized core plus a delegating wrapper — the daemon's path and rotation behavior stay byte-identical:

```go
// openNamedLogFile opens %LOCALAPPDATA%\mutastic\<name> (os.UserCacheDir
// is %LOCALAPPDATA% on Windows), rotating to <name>.old above 5 MB.
// The daemon and the deckplugin use SEPARATE files: two processes racing
// the rename-rotation on one file would be a real hazard.
func openNamedLogFile(name string) (io.WriteCloser, string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return nil, "", err
	}
	logDir := filepath.Join(dir, "mutastic")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, "", err
	}
	path := filepath.Join(logDir, name)
	if fi, err := os.Stat(path); err == nil && fi.Size() > 5<<20 {
		os.Rename(path, path+".old")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, "", err
	}
	return f, path, nil
}

// openLogFile opens the daemon's log (%LOCALAPPDATA%\mutastic\mutastic.log).
func openLogFile() (io.WriteCloser, string, error) {
	return openNamedLogFile("mutastic.log")
}
```

**(b)** In `main()` (currently `main.go:37-54`), insert after the `daemon` special case and before the `clientCommand` fallthrough:

```go
	// Stream Deck plugin mode. OpenDeck launches the binary from the
	// plugin directory with Elgato-style args and NO subcommand word
	// (mutastic.exe -port N -pluginUUID ... -registerEvent ... -info ...),
	// so a leading -port flag IS the plugin mode; the explicit
	// "deckplugin" word exists for manual/diagnostic launches.
	if os.Args[1] == "deckplugin" {
		os.Exit(runDeckPlugin(os.Args[2:]))
	}
	if os.Args[1] == "-port" {
		os.Exit(runDeckPlugin(os.Args[1:]))
	}
```

(Do NOT add anything to `clientCommand` — that is the one-shot request/reply UDP path and would be wrong for a resident plugin process.)

**(c)** In `usage()` (currently `main.go:75-81`), add one line after the first `Fprintln`:

```go
	fmt.Fprintln(os.Stderr, "       mutastic deckplugin -port <N> -pluginUUID <uuid> -registerEvent <event> [-info <json>]  (OpenDeck plugin mode)")
```

**(d)** Reimplement `runClient` (currently `main.go:83-109`) on top of `askDaemon`, preserving its EXACT output strings and exit codes (dial/write errors print the error; read errors print the bare line — that is what `errNoReply` encodes):

```go
// runClient sends one UDP command to the daemon and prints the reply.
// Exit codes: 0 = ok, 1 = "error:" reply from the daemon, 2 = no daemon.
func runClient(cmd, addr string, timeout time.Duration, out io.Writer) int {
	reply, err := askDaemon(cmd, addr, timeout)
	switch {
	case errors.Is(err, errNoReply):
		fmt.Fprintln(out, "error: no daemon reachable")
		return 2
	case err != nil:
		fmt.Fprintln(out, "error: no daemon reachable:", err)
		return 2
	}
	fmt.Fprintln(out, reply)
	if strings.HasPrefix(reply, "error:") {
		return 1
	}
	return 0
}
```

Add `"errors"` to `main.go`'s imports if not already present; remove `"net"` from `main.go`'s imports ONLY if the compiler reports it unused (other functions may still use it).

- [ ] **Step 7: Run the new tests and the baseline**

Run: `go test . -v`
Expected: PASS — including every pre-existing `TestRunClient*` / `TestClientCommand*` from Step 1, unchanged, plus `TestAskDaemon` and `TestAskDaemonUnreachable`.

- [ ] **Step 8: Run the full gate**

Run: `go test -race ./... && go vet ./...`
Expected: clean.

- [ ] **Step 9: Prove the cross-compile still works**

```bash
GOOS=windows GOARCH=amd64 CGO_ENABLED=1 CC=x86_64-w64-mingw32-gcc go vet .
./build.sh
```

Expected: vet clean; `built bin/mutastic.exe`. (This is the proof that gorilla/websocket survives the mingw cgo build.)

- [ ] **Step 10: Commit**

```bash
git add deckplugin.go deckplugin_test.go main.go go.mod go.sum
git commit -m "feat: wire OpenDeck plugin mode into the mutastic binary"
```

---

### Task 6: Plugin manifest and its guard test

**Files:**
- Create: `deck/com.danshapiro.mutastic.sdPlugin/manifest.json`
- Test: `deck_manifest_test.go` (root `package main` — the package dir is the repo root, so relative paths reach `deck/`)

**Interfaces:**
- Consumes: nothing from code; consumed later by `deploy/deploy.cmd` (copies the manifest verbatim) and by OpenDeck at runtime.
- Produces: the manifest contract other tasks depend on — plugin dir name `com.danshapiro.mutastic.sdPlugin`, action UUID `com.danshapiro.mutastic.mute`, `CodePathWin: mutastic.exe`, extensionless state images `icons/mutastic-mic` / `icons/mutastic-mic-muted`.

- [ ] **Step 1: Write the failing test**

Create `deck_manifest_test.go`:

```go
package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestDeckPluginManifest pins the OpenDeck manifest contract: required
// top-level fields (Name/Author/Version/Icon/Actions/OS — a missing one
// makes OpenDeck skip the plugin with only a warn log), the action
// identity that the profile edit and the runtime both depend on, and the
// EXTENSIONLESS image paths OpenDeck requires (its convert_icon appends
// .svg/@2x.png/.png itself; "icons/x.png" would resolve icons/x.png.png).
func TestDeckPluginManifest(t *testing.T) {
	raw, err := os.ReadFile("deck/com.danshapiro.mutastic.sdPlugin/manifest.json")
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m struct {
		Name, Author, Version, Icon, CodePathWin string
		OS                                       []struct{ Platform string }
		Actions                                  []struct {
			Name, UUID             string
			DisableAutomaticStates bool
			Controllers            []string
			States                 []struct{ Image string }
		}
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	for field, val := range map[string]string{"Name": m.Name, "Author": m.Author, "Version": m.Version, "Icon": m.Icon} {
		if val == "" {
			t.Errorf("manifest %s is empty (OpenDeck requires it)", field)
		}
	}
	if m.CodePathWin != "mutastic.exe" {
		t.Errorf("CodePathWin = %q, want mutastic.exe (flat layout, binary at plugin dir root)", m.CodePathWin)
	}
	if len(m.OS) != 1 || m.OS[0].Platform != "windows" {
		t.Errorf("OS = %+v, want exactly one windows entry", m.OS)
	}
	if len(m.Actions) != 1 {
		t.Fatalf("Actions has %d entries, want 1", len(m.Actions))
	}
	a := m.Actions[0]
	if a.UUID != "com.danshapiro.mutastic.mute" {
		t.Errorf("action UUID = %q, want com.danshapiro.mutastic.mute", a.UUID)
	}
	if a.Name != "Mutastic Mute" {
		t.Errorf("action Name = %q, want Mutastic Mute", a.Name)
	}
	if !a.DisableAutomaticStates {
		t.Error("DisableAutomaticStates must be true: the plugin alone drives the icon")
	}
	if len(a.States) != 2 {
		t.Fatalf("States has %d entries, want 2 (0 = live, 1 = muted)", len(a.States))
	}
	wantImages := []string{"icons/mutastic-mic", "icons/mutastic-mic-muted"}
	for i, st := range a.States {
		if st.Image != wantImages[i] {
			t.Errorf("States[%d].Image = %q, want %q", i, st.Image, wantImages[i])
		}
		if strings.Contains(st.Image, ".png") {
			t.Errorf("States[%d].Image = %q must be extensionless", i, st.Image)
		}
	}
	// The PNGs deploy.cmd installs next to this manifest must exist.
	for _, p := range []string{"deck/icons/mutastic-mic.png", "deck/icons/mutastic-mic-muted.png"} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("icon missing: %s: %v", p, err)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test . -run TestDeckPluginManifest -v`
Expected: FAIL — `read manifest: open deck/com.danshapiro.mutastic.sdPlugin/manifest.json: no such file or directory`.

- [ ] **Step 3: Write the manifest**

Create `deck/com.danshapiro.mutastic.sdPlugin/manifest.json`:

```json
{
	"Name": "Mutastic",
	"Author": "Dan Shapiro",
	"Version": "1.0.0",
	"Icon": "icons/mutastic-mic",
	"Category": "Custom",
	"CodePathWin": "mutastic.exe",
	"OS": [
		{ "Platform": "windows", "MinimumVersion": "10" }
	],
	"Actions": [
		{
			"Name": "Mutastic Mute",
			"UUID": "com.danshapiro.mutastic.mute",
			"Tooltip": "Toggle mic mute everywhere; icon tracks the true mic state",
			"Icon": "icons/mutastic-mic",
			"DisableAutomaticStates": true,
			"Controllers": ["Keypad"],
			"SupportedInMultiActions": false,
			"States": [
				{ "Image": "icons/mutastic-mic", "Name": "Live", "ShowTitle": false },
				{ "Image": "icons/mutastic-mic-muted", "Name": "Muted", "ShowTitle": false }
			]
		}
	]
}
```

Notes pinned by this design (do not "fix" them): NO `PropertyInspectorPath` anywhere — OpenDeck fully supports its absence (the per-action field defaults to `""` and the only consumer early-returns); the plugin simply never receives PI events. NO top-level `UUID` field — the installed directory name IS the plugin UUID. `SupportedInMultiActions: false` because the setState-driven icon model doesn't compose with multi-action children.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test . -run TestDeckPluginManifest -v`
Expected: PASS.

- [ ] **Step 5: Run the full gate**

Run: `go test -race ./... && go vet ./...`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add deck/com.danshapiro.mutastic.sdPlugin/manifest.json deck_manifest_test.go
git commit -m "feat(deck): OpenDeck plugin manifest for Mutastic Mute"
```

---

### Task 7: Profile editor script (`deploy/set-mute-key.ps1`)

**Files:**
- Create: `deploy/set-mute-key.ps1`

**Interfaces:**
- Consumes: the live profile shape (six-field key instance, 14-field state objects — see Context) and Task 6's identity constants.
- Produces: an idempotent script `set-mute-key.ps1 [-ProfilePath <path>]` (default `$env:APPDATA\opendeck\profiles\sd-A00DA6141I07PW\Default.json`) that backs up the profile to `<path>.bak-deckplugin` and replaces `keys[5]` with a Mutastic Mute instance. Exit 0 on success or no-op; throws (nonzero exit) on failure. Task 8's `deploy.cmd` calls it with the default path.

- [ ] **Step 1: Write the script**

Create `deploy/set-mute-key.ps1`:

```powershell
# set-mute-key.ps1 -- point keys[5] (context Keypad.5.0, the lower-right
# key) of the OpenDeck profile at the Mutastic Mute plugin action.
# Idempotent: if keys[5] is already the plugin action, exits without
# touching the file. Otherwise backs up the profile to
# <profile>.bak-deckplugin first. MUST run with OpenDeck STOPPED --
# OpenDeck persists profiles on exit and would clobber this edit.
#
# The instance-level "states" array is what OpenDeck renders (the
# "action" object is the manifest-derived template snapshot). Image
# paths in the profile are relative to the OpenDeck config root and
# INCLUDE the extension -- unlike the manifest, which is extensionless.
param(
    [string]$ProfilePath = "$env:APPDATA\opendeck\profiles\sd-A00DA6141I07PW\Default.json"
)
$ErrorActionPreference = 'Stop'

$json = Get-Content -Raw -LiteralPath $ProfilePath | ConvertFrom-Json

while ($json.keys.Count -lt 6) { $json.keys += $null }

if ($json.keys[5] -and $json.keys[5].action.uuid -eq 'com.danshapiro.mutastic.mute') {
    Write-Output "keys[5] already Mutastic Mute; no change"
    exit 0
}

Copy-Item -LiteralPath $ProfilePath -Destination "$ProfilePath.bak-deckplugin" -Force

function New-MuteState([string]$image, [string]$name) {
    # All 14 fields of an OpenDeck profile state object, matching the
    # defaults observed in the live profile. show=false suppresses the
    # title overlay (the icon is the whole message).
    [ordered]@{
        alignment = 'middle'; background_colour = '#000000'; colour = '#FFFFFF'
        family = 'Liberation Sans'; image = $image; image_scale = 100
        name = $name; show = $false; size = 16; stroke_colour = '#000000'
        stroke_size = 3; style = 'Regular'; text = ''; underline = $false
    }
}
$live  = New-MuteState 'plugins/com.danshapiro.mutastic.sdPlugin/icons/mutastic-mic.png' 'Live'
$muted = New-MuteState 'plugins/com.danshapiro.mutastic.sdPlugin/icons/mutastic-mic-muted.png' 'Muted'

$json.keys[5] = [ordered]@{
    action = [ordered]@{
        controllers = @('Keypad')
        disable_automatic_states = $true   # the plugin alone drives state
        encoder = $null
        icon = 'plugins/com.danshapiro.mutastic.sdPlugin/icons/mutastic-mic.png'
        name = 'Mutastic Mute'
        plugin = 'com.danshapiro.mutastic.sdPlugin'
        property_inspector = ''
        states = @($live, $muted)
        supported_in_multi_actions = $false
        tooltip = 'Toggle mic mute everywhere; icon tracks the true mic state'
        uuid = 'com.danshapiro.mutastic.mute'
        visible_in_action_list = $true
    }
    children = $null
    context = 'Keypad.5.0'
    current_state = 0
    settings = [ordered]@{}
    states = @($live, $muted)
}

# ASCII, not UTF8: Windows PowerShell 5.1's UTF8 writes a BOM, which
# serde_json (OpenDeck's parser) rejects. All content here is ASCII.
$json | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $ProfilePath -Encoding ASCII
Write-Output "keys[5] set to Mutastic Mute (backup: $ProfilePath.bak-deckplugin)"
```

- [ ] **Step 2: Run it against a fixture copy of the real profile (never the live file)**

```bash
cp /home/dan/code/mutastic/.worktrees/opendeck-mute-plugin/deploy/set-mute-key.ps1 /mnt/c/Users/dan/AppData/Local/Temp/set-mute-key.ps1
cp /mnt/c/Users/dan/AppData/Roaming/opendeck/profiles/sd-A00DA6141I07PW/Default.json /mnt/c/Users/dan/AppData/Local/Temp/mutastic-fixture.json
powershell.exe -NoProfile -ExecutionPolicy Bypass -Command "& 'C:\Users\dan\AppData\Local\Temp\set-mute-key.ps1' -ProfilePath 'C:\Users\dan\AppData\Local\Temp\mutastic-fixture.json'"
```

Expected output: `keys[5] set to Mutastic Mute (backup: C:\Users\dan\AppData\Local\Temp\mutastic-fixture.json.bak-deckplugin)`.
(WSL interop is flaky: if `powershell.exe` fails to launch or hangs, retry after ~15s, up to ~8 attempts over 2 minutes.)

- [ ] **Step 3: Assert the edited fixture**

```bash
powershell.exe -NoProfile -Command "\$j = Get-Content -Raw 'C:\Users\dan\AppData\Local\Temp\mutastic-fixture.json' | ConvertFrom-Json; if (\$j.keys[5].action.uuid -ne 'com.danshapiro.mutastic.mute') { throw 'action uuid wrong' }; if (\$j.keys[5].action.plugin -ne 'com.danshapiro.mutastic.sdPlugin') { throw 'plugin wrong' }; if (-not \$j.keys[5].action.disable_automatic_states) { throw 'automatic states not disabled' }; if (\$j.keys[5].context -ne 'Keypad.5.0') { throw 'context wrong' }; if (\$j.keys[5].current_state -ne 0) { throw 'current_state wrong' }; if (\$j.keys[5].states.Count -ne 2) { throw 'states count wrong' }; if (\$j.keys[5].states[0].image -ne 'plugins/com.danshapiro.mutastic.sdPlugin/icons/mutastic-mic.png') { throw 'live image wrong' }; if (\$j.keys[5].states[1].image -ne 'plugins/com.danshapiro.mutastic.sdPlugin/icons/mutastic-mic-muted.png') { throw 'muted image wrong' }; if (\$j.keys[0]) { throw 'keys[0] should still be null' }; 'FIXTURE OK'"
```

Expected: `FIXTURE OK`. Also confirm no BOM snuck in (the file's first byte must be `{`):

```bash
head -c 3 /mnt/c/Users/dan/AppData/Local/Temp/mutastic-fixture.json | od -c | head -1
```

Expected: first character `{` (NOT `357 273 277`, which would be a UTF-8 BOM).

- [ ] **Step 4: Verify idempotency**

```bash
powershell.exe -NoProfile -ExecutionPolicy Bypass -Command "& 'C:\Users\dan\AppData\Local\Temp\set-mute-key.ps1' -ProfilePath 'C:\Users\dan\AppData\Local\Temp\mutastic-fixture.json'"
```

Expected output: `keys[5] already Mutastic Mute; no change`.

- [ ] **Step 5: Clean up the fixtures and commit**

```bash
rm -f /mnt/c/Users/dan/AppData/Local/Temp/set-mute-key.ps1 /mnt/c/Users/dan/AppData/Local/Temp/mutastic-fixture.json /mnt/c/Users/dan/AppData/Local/Temp/mutastic-fixture.json.bak-deckplugin
git add deploy/set-mute-key.ps1
git commit -m "feat(deploy): profile editor points keys[5] at the Mutastic Mute plugin"
```

---

### Task 8: Extend `deploy/deploy.cmd` — install plugin, edit profile, cycle OpenDeck

**Files:**
- Modify: `deploy/deploy.cmd` (a CRLF file — keep CRLF line endings when editing)

**Interfaces:**
- Consumes: `deck/com.danshapiro.mutastic.sdPlugin/manifest.json` (Task 6), `deck/icons/*.png`, `bin/mutastic.exe` (built by `build.sh`), `deploy/set-mute-key.ps1` (Task 7).
- Produces: a deploy that assembles `%APPDATA%\opendeck\plugins\com.danshapiro.mutastic.sdPlugin\{manifest.json, icons\*.png, mutastic.exe}`, edits the profile with OpenDeck stopped, and relaunches OpenDeck. Everything the old script did is preserved (daemon, AHK, shortcuts, `mute-everything.cmd` still copied).

- [ ] **Step 1: Apply the edits**

Four edit points in `deploy/deploy.cmd` (anchors below are unique existing lines):

**(a)** After the line `set "OLD_DEPLOY=C:\Users\dan\code\mute-unmute-meetings"`, add:

```bat
set "ODPLUGDIR=%APPDATA%\opendeck\plugins\com.danshapiro.mutastic.sdPlugin"
set "OPENDECK_EXE=C:\Users\dan\AppData\Local\OpenDeck\opendeck.exe"
```

**(b)** Replace the stop section's first two lines:

```bat
echo == Stopping running instances ==
taskkill /F /IM mutastic.exe >nul 2>&1
```

with (OpenDeck must die FIRST: `taskkill /F` orphans its plugin children, and the following `mutastic.exe` kill catches both the daemon and the orphaned plugin instance, releasing the exe file locks; the `ping` is the non-interactive ~2s sleep — `timeout /t` fails when stdin is redirected, which it is under WSL interop):

```bat
echo == Stopping running instances ==
taskkill /F /IM opendeck.exe >nul 2>&1
taskkill /F /IM mutastic.exe >nul 2>&1
ping -n 3 127.0.0.1 >nul
```

**(c)** After the `mic_mute_light.ico` block (the line `if not exist "%DEST%\mic_mute_light.ico" echo WARNING: mic_mute_light.ico not found - tray icon will be missing`), add:

```bat
echo == Installing OpenDeck plugin ==
if not exist "%ODPLUGDIR%\icons" mkdir "%ODPLUGDIR%\icons"
copy /Y "%SRC%\deck\com.danshapiro.mutastic.sdPlugin\manifest.json" "%ODPLUGDIR%\manifest.json" >nul || goto :fail
copy /Y "%SRC%\deck\icons\mutastic-mic.png" "%ODPLUGDIR%\icons\mutastic-mic.png" >nul || goto :fail
copy /Y "%SRC%\deck\icons\mutastic-mic-muted.png" "%ODPLUGDIR%\icons\mutastic-mic-muted.png" >nul || goto :fail
copy /Y "%SRC%\bin\mutastic.exe" "%ODPLUGDIR%\mutastic.exe" >nul || goto :fail
copy /Y "%SRC%\deploy\set-mute-key.ps1" "%DEST%\set-mute-key.ps1" >nul || goto :fail

echo == Pointing profile keys[5] at the plugin ==
powershell -NoProfile -ExecutionPolicy Bypass -File "%DEST%\set-mute-key.ps1" || goto :fail
```

**(d)** In the relaunch section, after the line `start "" "%AHK_EXE%" "%DEST%\MuteAllMeetings.ahk"`, add:

```bat
start "" "%OPENDECK_EXE%"
```

- [ ] **Step 2: Static sanity check of the edited script**

```bash
grep -n "ODPLUGDIR\|OPENDECK_EXE\|set-mute-key\|opendeck.exe\|ping -n 3" deploy/deploy.cmd
grep -n "mute-everything.cmd" deploy/deploy.cmd
file deploy/deploy.cmd
```

Expected: all additions present in order stop → copy → plugin install → profile edit → shortcuts → relaunch (OpenDeck relaunch after the AHK relaunch); the existing `mute-everything.cmd` copy line untouched (spec: keep it); `file` still reports CRLF line terminators.

- [ ] **Step 3: Commit**

```bash
git add deploy/deploy.cmd
git commit -m "feat(deploy): install OpenDeck plugin, edit profile, cycle OpenDeck"
```

(The live execution of this script is Task 10 — deploying half-built state earlier would leave the Windows box inconsistent.)

---

### Task 9: README — the deck section describes the plugin

**Files:**
- Modify: `README.md`

**Interfaces:**
- Consumes: the shipped behavior of Tasks 1–8.
- Produces: accurate end-user documentation (README is the only end-user markdown doc).

- [ ] **Step 1: Locate the stale content**

Run: `grep -n -i "opendeck\|stream deck\|mute-everything" README.md`
Expected: hits describing the deck button as a "Run Command" action invoking `C:\Users\dan\code\mutastic-deploy\mute-everything.cmd` with two auto-toggling states (plus unrelated hits in the F24-flow and deploy sections, which stay).

- [ ] **Step 2: Rewrite the deck-button description**

Replace the prose that describes the Run Command button / auto-toggling icon (keep surrounding sections intact, match the file's heading level) with:

```markdown
### Stream Deck (OpenDeck plugin)

The deck's lower-right key is a native OpenDeck plugin action, **Mutastic
Mute** (`com.danshapiro.mutastic.mute`), served by the plugin mode built
into `mutastic.exe` itself. OpenDeck launches the copy installed at
`%APPDATA%\opendeck\plugins\com.danshapiro.mutastic.sdPlugin\mutastic.exe`
with Elgato-style args (`-port N -pluginUUID ... -registerEvent ... -info ...`);
the binary auto-detects the leading `-port` flag as plugin mode
(`mutastic deckplugin -port ...` works for manual launches).

- **Press** = the full mute-everything flow, in-process: `toggle` to the
  daemon over UDP 42814 plus one SendInput F24 for the meeting-app sweep
  (no cmd/AHK hop; both halves run even if the other fails).
- **Icon** = the TRUE mic state. The plugin polls the daemon's `status`
  every 750ms and drives the icon via `setState`, so physical mic-button
  presses, the pedal, and the CLI all show up on the deck. `unknown`
  (fresh daemon) keeps the last icon.
- **Log:** `%LOCALAPPDATA%\mutastic\deckplugin.log` (every `setState` is
  logged).

`deploy\deploy.cmd` installs the plugin directory, points the profile's
`keys[5]` at the action (backup kept at `Default.json.bak-deckplugin`),
and restarts OpenDeck. `deploy\mute-everything.cmd` remains as a CLI
entry point but the deck no longer uses it.
```

- [ ] **Step 3: Proofread against behavior**

Run: `grep -n -i "run command\|auto-toggl" README.md`
Expected: no remaining claims that the deck button is a Run Command action or that its icon auto-toggles. The F24/AHK flow section and troubleshooting entries stay (they still describe the daemon and physical button accurately).

- [ ] **Step 4: Commit**

```bash
git add README.md
git commit -m "docs: README describes the native OpenDeck mute plugin"
```

---

### Task 10: Live E2E on Windows (build → deploy → verify), full regression

**Files:** none created (fixes, if any, land as focused `fix:` commits with their own test-first cycle).

**Interfaces:**
- Consumes: everything above; the live Windows box (daemon deployed at `C:\Users\dan\code\mutastic-deploy\mutastic.exe`, OpenDeck v2.13.1 at `C:\Users\dan\AppData\Local\OpenDeck\opendeck.exe`, deck serial `sd-A00DA6141I07PW`).
- Produces: verified live deployment + the recorded human questions.

**Flakiness policy (applies to every step here):** WSL→Windows interop (`cmd.exe`, `powershell.exe`, `/mnt/c` exes) is historically flaky. On "exec format error", a hang, or empty output: wait ~15s and retry the same command, up to ~8 attempts over 2–3 minutes, before declaring a blocker.

- [ ] **Step 1: Full gate + build**

```bash
go test -race ./... && go vet ./...
GOOS=windows GOARCH=amd64 CGO_ENABLED=1 CC=x86_64-w64-mingw32-gcc go vet .
./build.sh
```

Expected: all clean; `built bin/mutastic.exe`.

- [ ] **Step 2: Deploy**

```bash
cd /home/dan/code/mutastic/.worktrees/opendeck-mute-plugin
timeout 120 cmd.exe /c '\\wsl.localhost\Ubuntu\home\dan\code\mutastic\.worktrees\opendeck-mute-plugin\deploy\deploy.cmd' '\\wsl.localhost\Ubuntu\home\dan\code\mutastic\.worktrees\opendeck-mute-plugin' > /tmp/deploy.log 2>&1
cat /tmp/deploy.log
```

Expected: transcript contains `== Installing OpenDeck plugin ==`, `keys[5] set to Mutastic Mute` (or `already Mutastic Mute; no change` on re-runs), and ends with `Deploy complete.`. Per the repo's documented deploy-from-WSL gotcha, trust the transcript, not the exit code (the `start`ed processes can inherit the interop console and stall the return; cmd.exe's UNC-path CWD warning is harmless). UNC paths must stay single-quoted.

- [ ] **Step 3: OpenDeck restarted and registered the plugin**

```bash
sleep 20
tail -40 /mnt/c/Users/dan/AppData/Local/OpenDeck/logs/OpenDeck.log
```

Expected in the fresh tail (the log is append-only across launches — check timestamps after the deploy): a `Registered plugin com.danshapiro.mutastic.sdPlugin` line and NO `Failed to initialise plugin` for our directory. Also check the per-plugin stdout log:

```bash
ls -la /mnt/c/Users/dan/AppData/Local/OpenDeck/logs/plugins/com.danshapiro.mutastic.sdPlugin.log
tail -20 /mnt/c/Users/dan/AppData/Local/OpenDeck/logs/plugins/com.danshapiro.mutastic.sdPlugin.log
```

Expected: the file exists and mirrors the plugin's stderr (`deckplugin starting: ...`).

- [ ] **Step 4: Plugin's own log shows startup, registration, and the key appearing**

```bash
tail -30 /mnt/c/Users/dan/AppData/Local/mutastic/deckplugin.log
```

Expected lines (timestamps after the deploy): `deckplugin starting: port=...`, `registered as com.danshapiro.mutastic.sdPlugin (event registerPlugin)`, and `willAppear sd-A00DA6141I07PW.Default.Keypad.5.0`. willAppear only fires with the deck connected and the Default profile active — if absent, verify the deck is plugged in and the profile edit applied:

```bash
powershell.exe -NoProfile -Command "(Get-Content -Raw 'C:\Users\dan\AppData\Roaming\opendeck\profiles\sd-A00DA6141I07PW\Default.json' | ConvertFrom-Json).keys[5].action.uuid"
```

Expected: `com.danshapiro.mutastic.mute`.

- [ ] **Step 5: Daemon healthy**

```bash
/mnt/c/Users/dan/code/mutastic-deploy/mutastic.exe status
```

Expected: `muted`, `unmuted`, or `unknown` (all healthy; `unknown` is normal right after the deploy restarted the daemon). `error: no daemon reachable` = blocker → check the `taskkill`/relaunch lines in the deploy transcript and `/mnt/c/Users/dan/AppData/Local/mutastic/mutastic.log`.

- [ ] **Step 6: The plugin observes CLI-driven state changes (the core promise)**

```bash
/mnt/c/Users/dan/code/mutastic-deploy/mutastic.exe unmute && sleep 3
grep -c "setState sd-A00DA6141I07PW.Default.Keypad.5.0 -> 0" /mnt/c/Users/dan/AppData/Local/mutastic/deckplugin.log
/mnt/c/Users/dan/code/mutastic-deploy/mutastic.exe mute && sleep 3
grep -c "setState sd-A00DA6141I07PW.Default.Keypad.5.0 -> 1" /mnt/c/Users/dan/AppData/Local/mutastic/deckplugin.log
/mnt/c/Users/dan/code/mutastic-deploy/mutastic.exe unmute && sleep 3
/mnt/c/Users/dan/code/mutastic-deploy/mutastic.exe status
```

Expected: the first grep ≥ 1 (setState → 0 observed after `unmute`), the second grep ≥ 1 (setState → 1 after `mute`), and the final status prints `unmuted`. This proves the poll loop turns out-of-band state changes into `setState` — the same daemon-tracked path a physical mic-button press takes (its `0x21` event updates the same status; only a human can confirm the pixels, Step 8). **Do not issue any `light` commands** — the light must be left untouched.

- [ ] **Step 7: Profile backup + regression sweep**

```bash
ls -la "/mnt/c/Users/dan/AppData/Roaming/opendeck/profiles/sd-A00DA6141I07PW/Default.json.bak-deckplugin"
go test -race ./... && go vet ./...
```

Expected: backup exists (rollback path: stop OpenDeck, copy the `.bak-deckplugin` back over `Default.json`, restart OpenDeck); full test suite still green.

- [ ] **Step 8: Record the human-verification questions**

These CANNOT be verified programmatically; record them as the final open questions for the user:

1. Press the **physical mic mute button** on the Yeti X — does the deck key icon flip to the muted/live icon within ~1 second, both directions?
2. Press the **deck key** itself — do the mic LED, the meeting-app sweep, and the deck icon all toggle together (icon driven by real state, not an optimistic flip)?
3. After a full **OpenDeck restart or reboot**, does the key come up showing the correct current state (or the last icon if the daemon reports `unknown`)?

---

## Verification Summary (what proves each spec requirement)

| Spec requirement | Proof |
|---|---|
| Plugin mode in the single binary, Elgato argv | Task 1 `TestParseArgs` (real argv), Task 5 dispatch + `./build.sh` |
| Registration protocol correctness | Task 1 `TestEncodeRegister` (exact JSON), Task 4 `TestRunSendsRegisterFirst`, Task 10 Step 3 (`Registered plugin` in OpenDeck.log — live) |
| willAppear/willDisappear/keyDown handling | Tasks 2–3 tests (verbatim wire frames), Task 10 Step 4 (`willAppear` logged live) |
| keyDown = daemon toggle + in-process F24, no cmd/AHK | Task 3 `TestKeyDownTogglesAndInjects` / `TestKeyDownInjectsEvenWhenDaemonDown`, injector reused from `inject_windows.go` (Task 5 wiring); live keypress is human question 2 |
| Icon from polled true state; unknown → no-op | Task 2 `TestDesiredState` / `TestPollOnce*`, Task 4 `TestRunHandlesEventsAndPolls`, Task 10 Step 6 (CLI mute/unmute observed as setState live — covers physical button/pedal/CLI, which all flow through the same daemon-tracked state) |
| `DisableAutomaticStates` / `disable_automatic_states` | Task 6 manifest + guard test; Task 7 profile edit + fixture assertions |
| Packaging mirrors OpenDeck's real conventions | Task 6 (dirname-as-UUID, `CodePathWin`, extensionless images, no PI), Task 8 assembly, Task 10 Step 3 (loads without error) |
| Deployment (kill/relaunch OpenDeck, profile `.bak`) | Tasks 7–8, Task 10 Steps 2–3 and 7 |
| Existing behavior unchanged; `-race`+vet clean | Task 5 Steps 1/7 baseline, every task's gate, Task 10 Steps 1/7 |
| Mic left UNMUTED, light untouched | Task 10 Step 6 (final `unmute` + status check; no light commands anywhere) |
| Deck icon flip on physical press / real deck keypress | Task 10 Step 8 — recorded human questions (only a human can confirm pixels and a physical press) |
