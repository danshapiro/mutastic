# Mic Button → F24 Meeting-App Sweep Implementation Plan

> **For agentic workers:** This plan is executed task-by-task by the
> workflow's execute stage: a fresh implementer per task, with a spec +
> quality review after each task. Steps use checkbox (`- [ ]`) syntax
> for tracking.

**Goal:** Make the Yeti X microphone's physical mute button trigger the same meeting-app mute sweep as the middle foot pedal, with no double-toggling in either direction.

**Architecture:** The daemon already decodes the mic's `0x21` DeviceMute HID event, which fires ONLY on physical button presses (host-initiated mute commands echo `0x20` instead, and those echoes carry no state). We add a debounced hook in the daemon's session loop that, on `0x21`, injects a synthetic `F24` keystroke via user32 `SendInput`; the AHK script gets a new `F24::` hotkey that runs only the meeting-app sweep (no `mutastic toggle` — the mic already toggled its own hardware mute). The pedal path (`F14`) is untouched.

**Tech Stack:** Go 1.26 (stdlib `syscall.NewLazyDLL` — no new deps), AutoHotkey v1.1 script, WSL2 cross-compile via mingw (`build.sh`), Windows deployment via `deploy/deploy.cmd` over WSL interop.

## Global Constraints

- **Worktree root:** `/home/dan/code/mutastic/.worktrees/mic-button-f24-sweep` — run every command from there (or `git -C` / absolute paths). Never rely on cwd.
- `ahk/MuteAllMeetings.ahk` is **UTF-8 with BOM (`EF BB BF`) + CRLF line endings, AHK v1.1 syntax**. Every edit must preserve both (verify with `file` + `git diff`). Edit via byte-level Python (`read_bytes`/`replace`/`write_bytes` with count==1 asserts). **Never hand-rewrite the file.**
- **F14 behavior stays exactly as-is** (sweep + `mutastic toggle`). F13 untouched. **F15 belongs to Winpepper — never touch it.** F24 is confirmed unused on this machine (pedal firmware emits only F13/F14/F15; winpepper uses F15 and RightCtrl+RightShift+Space).
- `internal/daemon`, `internal/light`, `internal/proto` stay platform-free (zero build tags). All OS-specific code lives in package `main` at the repo root as `<concern>_windows.go` + `<concern>_other.go` pairs, each with BOTH the filename suffix AND an explicit `//go:build` line.
- No new direct dependencies: `go.mod` stays `module mutastic / go 1.26.3` with only `go-hid v0.15.0` + `go.bug.st/serial v1.8.0`. Use raw `syscall.NewLazyDLL` (house precedent: `hideConsoleIfOwned`, `hid_windows.go:77-93`), not `golang.org/x/sys/windows`.
- No `flag` package anywhere — argument handling is manual `os.Args` slicing (house style).
- The injection trigger must gate on `ev.Op == proto.EvtDeviceMute` (0x21) **independently of `Tracker.Apply`'s result** — `docs/yeti-x-hid-protocol.md:109-115` flags the 0x21 value byte as unverified on some firmware, and `Tracker.Apply` erases the op.
- Debounce: ignore further 0x21 events within **400 ms** of the last one acted on; implement as a package-level `var muteInjectDebounce = 400 * time.Millisecond` with a comment justifying the value (house convention). Timing-var hazard (commit `eebd6c7`): the session goroutine reads the var, so tests must register the shrink/restore `t.Cleanup` BEFORE calling the daemon harness (t.Cleanup is LIFO — the harness's stop-and-join cleanup must run first).
- Log lines: stdlib `*log.Logger`, `Printf`, lowercase, no prefix. Each injection logs `mic button -> F24 app sweep`; failures log the error and are non-fatal.
- Gates for every code task: `go test -race ./... && go vet ./...` clean; `./build.sh` green before any deploy. All existing mic + light tests keep passing.
- UDP inbound buffer stays 64 bytes; do not change unrelated daemon behavior.
- WSL→Windows interop is intermittently flaky (vsock errors): retry any `cmd.exe`/`powershell.exe`/`*.exe` invocation up to 3 times over ~2 minutes before surfacing a blocker. Pre-flight with `cmd.exe /c echo interop-ok`.
- Deploy from WSL only via `timeout 90 cmd.exe /c '<UNC>\deploy\deploy.cmd' '<UNC>' > /tmp/<file>.log 2>&1` — single quotes are load-bearing; success = transcript ends `Deploy complete.`, NOT the exit code.
- Live E2E end state: **mic UNMUTED**, light state untouched (no light commands beyond `status`/`list`).
- `README.md` is the only end-user markdown doc to edit. Commits: conventional prefixes (`feat:`/`fix:`/`docs:`), lowercase subjects, focused and atomic.

---

## Scope Check

Single subsystem: one repo, one trigger→effect flow, touching the AHK script, the Go daemon, and docs that describe them. The pieces are useless independently (an F24 hotkey nothing presses; an injector nothing listens to), so this is one plan with one live E2E at the end — not multiple plans.

## Background facts (validated; do not re-derive)

- Physical button presses arrive as DeviceMute events `op=0x21` carrying the RESULTING state (`value=0x00 -> muted=false`, `value=0x01 -> muted=true`), one event per press. Validated from the deployed daemon's real log 2026-08-08. The daemon's decode path already logs them at `internal/daemon/daemon.go:200` (`event op=0x%02x value=0x%02x -> muted=%v`).
- Host-initiated mute commands produce only `op=0x20` zero-payload echoes whose value byte is a constant `0x0b` tag — `proto.MutedFromValue(0x0b)` fails, so they land on the `(ignored)` log branch. **0x21 fires ONLY for physical presses, never for daemon-sent commands.** No loop risk from that direction.
- Loop analysis (encode in tests and README):
  - Pedal path: `F14` → AHK sweeps apps + runs `mutastic toggle` → daemon writes `0x20` → mic echoes `0x20` (ignored) → no `0x21` → no re-trigger.
  - Physical path: firmware toggles mic + emits `0x21` → daemon injects `F24` → AHK sweeps apps only → no `mutastic` call → no further events.
- The meeting-app sweep is ALREADY a self-contained AHK function `ToggleAllMeetings()` (`ahk/MuteAllMeetings.ahk:35-70`); the `mutastic.exe toggle` Run lives outside it in the F14 hotkey body (line 27). So the "shared subroutine" the design asks for already exists — the AHK change is purely additive.
- `Tracker.Apply` (`internal/daemon/tracker.go`) treats 0x20 and 0x21 identically; the only place the raw op is distinguishable is `(*Daemon).session` (`internal/daemon/daemon.go:169-206`).
- `daemon.Run` signature today: `Run(ctx context.Context, open OpenFunc, light CommandHandler, pc net.PacketConn, logger *log.Logger) error` — exactly two call sites: `main.go:139` and `startDaemon` in `internal/daemon/daemon_test.go`.
- Fake-HID test infra (`internal/daemon/daemon_test.go`, internal test package): `newFakeDevice()`, `inputReport(op, value byte) []byte`, `startDaemon(t, open) (addr, ask)`, `waitFor(t, what, cond)`, `testLogger()`. Events are fed with `dev.events <- inputReport(0x21, 0x01)`.
- SendInput succeeds whether or not the AHK script is running — absence of a listener is undetectable and harmless by design.

## File Structure

| File | Action | Responsibility |
|---|---|---|
| `ahk/MuteAllMeetings.ahk` | Modify (byte-level) | New `F24::` hotkey → `ToggleAllMeetings()` only |
| `internal/daemon/inject.go` | Create | `KeyInjector` interface, `muteInjectDebounce` var, `injectGate` debounce/decision logic (platform-free) |
| `internal/daemon/inject_test.go` | Create | Pure unit tests for the gate (explicit `now`, no timing races) |
| `internal/daemon/daemon.go` | Modify | `Daemon.Inject` field + `gate`, `Run` signature gains `inject KeyInjector`, session-loop hook + logging |
| `internal/daemon/daemon_test.go` | Modify | `startDaemonInject` harness variant, `fakeInjector`, integration tests via fake HID |
| `inject_windows.go` | Create | `SendInput`-based `f24Injector` (VK_F24 down+up), `newKeyInjector()` |
| `inject_other.go` | Create | `newKeyInjector()` returning nil (non-Windows builds keep compiling) |
| `main.go` | Modify | Wire `newKeyInjector()` into `daemon.Run`; hidden `mutastic daemon --test-inject` smoke command (`runTestInject`) |
| `main_test.go` | Modify | Test for `runTestInject` off-Windows behavior |
| `README.md` | Modify | Document both flows + loop analysis + troubleshooting bullet |

---

### Task 1: AHK — add the F24 sweep-only hotkey

**Files:**
- Modify: `ahk/MuteAllMeetings.ahk` (insert after the F13 block, lines 31-33; file is 98 lines, 3363 bytes today)

**Interfaces:**
- Consumes: existing `ToggleAllMeetings()` function (`ahk/MuteAllMeetings.ahk:35-70`).
- Produces: an `F24::` hotkey that runs ONLY the meeting-app sweep. The daemon (Task 4) sends VK_F24 (0x87). F14/F13 byte-for-byte unchanged.

- [ ] **Step 1: Record pre-edit byte facts**

```bash
cd /home/dan/code/mutastic/.worktrees/mic-button-f24-sweep
file ahk/MuteAllMeetings.ahk
head -c 3 ahk/MuteAllMeetings.ahk | xxd
wc -l < ahk/MuteAllMeetings.ahk && grep -c $'\r' ahk/MuteAllMeetings.ahk
```

Expected: `Unicode text, UTF-8 (with BOM) text, with CRLF line terminators`; `efbb bf`; both counts `98`. If anything differs, STOP — the file drifted from what this plan recorded; read the actual bytes and adapt Step 2's anchors before proceeding.

- [ ] **Step 2: Insert the F24 block via byte-level Python**

```bash
cd /home/dan/code/mutastic/.worktrees/mic-button-f24-sweep
python3 - <<'PYEOF'
from pathlib import Path

p = Path("ahk/MuteAllMeetings.ahk")
data = p.read_bytes()
assert data.startswith(b"\xef\xbb\xbf"), "BOM missing before edit"
assert b"F24::" not in data, "F24 already present - edit already applied?"

old = (b"F13::\r\n"
       b"Run, \"%A_ScriptDir%\\mutastic.exe\" light toggle, %A_ScriptDir%, Hide UseErrorLevel\r\n"
       b"return\r\n")
new = old + (b"\r\n"
       b"; F24 is injected by the mutastic daemon when the mic's own mute\r\n"
       b"; button is pressed (0x21 DeviceMute event). Sweep the meeting apps\r\n"
       b"; only - the mic has already toggled its own hardware mute, so\r\n"
       b"; running mutastic.exe toggle here would undo it.\r\n"
       b"F24::\r\n"
       b"ToggleAllMeetings()\r\n"
       b"return\r\n")
assert data.count(old) == 1, "F13 block not found exactly once"
data = data.replace(old, new)

p.write_bytes(data)
print("edit ok")
PYEOF
```

Expected: `edit ok`. If any assertion fails, run `git checkout -- ahk/MuteAllMeetings.ahk`, read the actual file bytes, adapt the exact old-byte strings, and retry. **Never hand-rewrite the file.**

- [ ] **Step 3: Verify encoding invariants and the diff**

```bash
cd /home/dan/code/mutastic/.worktrees/mic-button-f24-sweep
file ahk/MuteAllMeetings.ahk
wc -l < ahk/MuteAllMeetings.ahk && grep -c $'\r' ahk/MuteAllMeetings.ahk
git diff --stat ahk/MuteAllMeetings.ahk
grep -a -A2 'F24::' ahk/MuteAllMeetings.ahk
grep -a -A3 'F14::' ahk/MuteAllMeetings.ahk
```

Expected: `file` still reports `UTF-8 (with BOM) ... CRLF`; both counts `106` (98 + 8 inserted lines); diff stat shows ~8 insertions, 0 deletions in 1 file (if EVERY line shows changed, line endings were destroyed — `git checkout -- ahk/MuteAllMeetings.ahk` and redo); `F24::` is followed by `ToggleAllMeetings()` and `return`; the F14 block still shows `Run, "%A_ScriptDir%\mutastic.exe" toggle ...` then `ToggleAllMeetings()` then `return` (unchanged). Note the `-a` flag — grep may treat the BOM'd file as binary without it.

- [ ] **Step 4: Syntax-check with the real AHK v1 interpreter (parse-only, WSL interop)**

```bash
cmd.exe /c echo interop-ok
"/mnt/c/Program Files/AutoHotkey/AutoHotkeyU64.exe" /ErrorStdOut /iLib nul \
  "$(wslpath -w /home/dan/code/mutastic/.worktrees/mic-button-f24-sweep/ahk/MuteAllMeetings.ahk)"; echo "exit=$?"
```

Expected: `interop-ok`, then no output and `exit=0` (`/iLib nul` parses without executing; `/ErrorStdOut` prevents a modal dialog that would hang interop). Contingencies: interop vsock failures → retry up to 3 times over ~2 minutes; if the interpreter rejects the UNC path, `cp` the .ahk to `/mnt/c/Users/dan/AppData/Local/Temp/` and check it there. If the check reports a syntax error, fix the edit — do not commit a script that fails to parse. If interop is down entirely after retries, HALT and surface the blocker — do not skip the check silently.

- [ ] **Step 5: Commit**

```bash
cd /home/dan/code/mutastic/.worktrees/mic-button-f24-sweep
git add ahk/MuteAllMeetings.ahk
git commit -m "feat: add F24 hotkey that runs the meeting-app sweep alone"
```

---

### Task 2: Debounced injection gate (platform-free, TDD)

**Files:**
- Create: `internal/daemon/inject.go`
- Test: `internal/daemon/inject_test.go`

**Interfaces:**
- Consumes: `proto.Event`, `proto.EvtDeviceMute`, `proto.EvtSoftwareMute` (from `mutastic/internal/proto`).
- Produces (Tasks 3 and 4 rely on these exact names):
  - `type KeyInjector interface { Inject() error }`
  - `var muteInjectDebounce = 400 * time.Millisecond`
  - `type injectGate struct { last time.Time }`
  - `func (g *injectGate) shouldInject(ev proto.Event, now time.Time) bool`

- [ ] **Step 1: Write the failing tests**

Create `internal/daemon/inject_test.go` (package `daemon` — internal test package, matching `tracker_test.go`'s style: `proto.Event` literals, explicit times, no fakes, no goroutines):

```go
package daemon

import (
	"testing"
	"time"

	"mutastic/internal/proto"
)

func TestGateFiresOnDeviceMute(t *testing.T) {
	var g injectGate
	if !g.shouldInject(proto.Event{Op: proto.EvtDeviceMute, Value: 0x01}, time.Unix(1000, 0)) {
		t.Fatal("first 0x21 event should fire")
	}
}

func TestGateIgnoresSoftwareMute(t *testing.T) {
	var g injectGate
	now := time.Unix(1000, 0)
	if g.shouldInject(proto.Event{Op: proto.EvtSoftwareMute, Value: '0'}, now) {
		t.Fatal("0x20 software echo must never fire (loop risk: F14 -> toggle -> 0x20 -> F24 -> ...)")
	}
	// A rejected 0x20 must not consume the debounce window either.
	if !g.shouldInject(proto.Event{Op: proto.EvtDeviceMute, Value: 0x00}, now) {
		t.Fatal("0x21 right after a 0x20 should still fire")
	}
}

func TestGateFiresOnUndecodableValue(t *testing.T) {
	// The 0x21 value byte is unverified on some firmware
	// (docs/yeti-x-hid-protocol.md:109-115); the gate must fire on the op
	// alone, independent of whether the value decodes.
	var g injectGate
	if !g.shouldInject(proto.Event{Op: proto.EvtDeviceMute, Value: 0x0b}, time.Unix(1000, 0)) {
		t.Fatal("0x21 with an undecodable value byte must still fire")
	}
}

func TestGateDebounce(t *testing.T) {
	var g injectGate
	base := time.Unix(1000, 0)
	press := proto.Event{Op: proto.EvtDeviceMute, Value: 0x01}

	if !g.shouldInject(press, base) {
		t.Fatal("first press should fire")
	}
	if g.shouldInject(press, base.Add(muteInjectDebounce-time.Millisecond)) {
		t.Fatal("chatter just inside the debounce window must be suppressed")
	}
	// Suppressed chatter must NOT extend the window: a press exactly at
	// base+window still fires.
	if !g.shouldInject(press, base.Add(muteInjectDebounce)) {
		t.Fatal("press at the window boundary should fire")
	}
	if g.shouldInject(press, base.Add(muteInjectDebounce+time.Millisecond)) {
		t.Fatal("the boundary press must start a fresh window")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd /home/dan/code/mutastic/.worktrees/mic-button-f24-sweep && go test ./internal/daemon/ -run 'TestGate' -v`
Expected: FAIL to compile with `undefined: injectGate` (and `muteInjectDebounce`).

- [ ] **Step 3: Write the implementation**

Create `internal/daemon/inject.go`:

```go
package daemon

import (
	"time"

	"mutastic/internal/proto"
)

// KeyInjector delivers one synthetic keystroke to the OS. The daemon uses
// it to fire the AHK script's meeting-app sweep (F24) when the mic's own
// mute button is pressed. The Windows implementation (SendInput) lives in
// package main, keeping this package platform-free — same pattern as
// Daemon.Light/CommandHandler.
type KeyInjector interface {
	Inject() error
}

// muteInjectDebounce is how long after an acted-on 0x21 DeviceMute event
// further 0x21 events are ignored. The mic's capacitive button can
// chatter; 400ms comfortably outlasts chatter while staying shorter than
// any intentional repeat press. Var (not const) so tests can shrink it.
var muteInjectDebounce = 400 * time.Millisecond

// injectGate decides whether a decoded event should trigger a keystroke
// injection. Only physical-press events (0x21 DeviceMute) qualify: 0x20
// SoftwareMute echoes are host-initiated, and injecting on them would
// loop (F14 -> mutastic toggle -> 0x20 echo -> F24 -> sweep -> ...). The
// gate fires on the op alone, independent of whether the value byte
// decodes — 0x21 value semantics are unverified on some firmware
// (docs/yeti-x-hid-protocol.md).
//
// Not safe for concurrent use: only the daemon's session goroutine calls
// shouldInject.
type injectGate struct {
	last time.Time
}

// shouldInject reports whether ev, observed at now, should trigger an
// injection, recording now as the last firing time when it does.
// Suppressed events do NOT extend the debounce window, so a genuine
// second press still fires as soon as the window from the last ACTED-ON
// event lapses.
func (g *injectGate) shouldInject(ev proto.Event, now time.Time) bool {
	if ev.Op != proto.EvtDeviceMute {
		return false
	}
	if !g.last.IsZero() && now.Sub(g.last) < muteInjectDebounce {
		return false
	}
	g.last = now
	return true
}
```

- [ ] **Step 4: Run the tests and the full gate**

Run: `cd /home/dan/code/mutastic/.worktrees/mic-button-f24-sweep && go test ./internal/daemon/ -run 'TestGate' -v && go test -race ./... && go vet ./...`
Expected: the four `TestGate*` tests PASS; full suite `ok` for all 4 packages; vet silent.

- [ ] **Step 5: Commit**

```bash
cd /home/dan/code/mutastic/.worktrees/mic-button-f24-sweep
git add internal/daemon/inject.go internal/daemon/inject_test.go
git commit -m "feat: add debounced injection gate for physical mute-button events"
```

---

### Task 3: Hook the gate into the daemon session loop (TDD via fake HID)

**Files:**
- Modify: `internal/daemon/daemon.go` (Daemon struct ~line 39; `Run` ~line 129; `session` ~lines 194-203)
- Modify: `internal/daemon/daemon_test.go` (harness + new tests)
- Modify: `main.go:139` (call-site signature fix only; real injector wired in Task 4)

**Interfaces:**
- Consumes (from Task 2): `KeyInjector`, `injectGate`, `shouldInject(ev, now)`, `muteInjectDebounce`.
- Produces (Task 4 relies on these):
  - `func Run(ctx context.Context, open OpenFunc, light CommandHandler, inject KeyInjector, pc net.PacketConn, logger *log.Logger) error` (param added between `light` and `pc`)
  - `Daemon.Inject KeyInjector` exported field (nil = no injection wired)
  - Log lines: `mic button -> F24 app sweep` on success, `mic button -> F24 app sweep: inject failed: <err>` on error
  - Test helper: `startDaemonInject(t *testing.T, open OpenFunc, inject KeyInjector) (addr string, ask func(cmd string) string)`

- [ ] **Step 1: Write the failing integration tests**

Add to `internal/daemon/daemon_test.go` (ensure `errors` and `sync/atomic` are in the import block — `sync/atomic` already is):

```go
// fakeInjector implements KeyInjector: counts calls, returns err.
type fakeInjector struct {
	calls atomic.Int32
	err   error
}

func (f *fakeInjector) Inject() error {
	f.calls.Add(1)
	return f.err
}

func TestDeviceMuteEventInjectsSweepKey(t *testing.T) {
	dev := newFakeDevice()
	inj := &fakeInjector{}
	_, _ = startDaemonInject(t, func() (Device, error) { return dev, nil }, inj)
	waitFor(t, "handshake", func() bool { return dev.writeCount() >= 2 })

	dev.events <- inputReport(0x21, 0x01)
	waitFor(t, "one injection", func() bool { return inj.calls.Load() == 1 })
}

func TestSoftwareMuteEchoDoesNotInject(t *testing.T) {
	dev := newFakeDevice()
	inj := &fakeInjector{}
	_, ask := startDaemonInject(t, func() (Device, error) { return dev, nil }, inj)
	waitFor(t, "handshake", func() bool { return dev.writeCount() >= 2 })

	// Decodable host-echo shape (ASCII value); the status round-trip
	// proves the event was consumed before we assert zero injections.
	dev.events <- inputReport(0x20, '1')
	waitFor(t, "echo tracked", func() bool { return ask("status") == "muted" })
	if got := inj.calls.Load(); got != 0 {
		t.Fatalf("software echo injected: calls = %d, want 0", got)
	}
}

func TestDeviceMuteChatterIsDebounced(t *testing.T) {
	// Registered BEFORE startDaemonInject: t.Cleanup is LIFO, so the
	// harness's stop-and-join cleanup runs first and Run's goroutine is
	// gone before the var is restored (commit eebd6c7 discipline).
	old := muteInjectDebounce
	muteInjectDebounce = time.Hour // the window cannot lapse mid-test
	t.Cleanup(func() { muteInjectDebounce = old })

	dev := newFakeDevice()
	inj := &fakeInjector{}
	_, ask := startDaemonInject(t, func() (Device, error) { return dev, nil }, inj)
	waitFor(t, "handshake", func() bool { return dev.writeCount() >= 2 })

	dev.events <- inputReport(0x21, 0x01)
	waitFor(t, "first injection", func() bool { return inj.calls.Load() == 1 })

	dev.events <- inputReport(0x21, 0x00) // chatter, inside the window
	waitFor(t, "chatter tracked", func() bool { return ask("status") == "unmuted" })
	if got := inj.calls.Load(); got != 1 {
		t.Fatalf("chatter injected: calls = %d, want 1", got)
	}
}

func TestDeviceMuteFiresAgainAfterDebounceWindow(t *testing.T) {
	old := muteInjectDebounce
	muteInjectDebounce = time.Millisecond
	t.Cleanup(func() { muteInjectDebounce = old })

	dev := newFakeDevice()
	inj := &fakeInjector{}
	_, _ = startDaemonInject(t, func() (Device, error) { return dev, nil }, inj)
	waitFor(t, "handshake", func() bool { return dev.writeCount() >= 2 })

	dev.events <- inputReport(0x21, 0x01)
	waitFor(t, "first injection", func() bool { return inj.calls.Load() == 1 })

	time.Sleep(5 * time.Millisecond) // let the 1ms window lapse
	dev.events <- inputReport(0x21, 0x00)
	waitFor(t, "second injection", func() bool { return inj.calls.Load() == 2 })
}

func TestInjectFailureIsNonFatal(t *testing.T) {
	dev := newFakeDevice()
	inj := &fakeInjector{err: errors.New("sendinput exploded")}
	_, ask := startDaemonInject(t, func() (Device, error) { return dev, nil }, inj)
	waitFor(t, "handshake", func() bool { return dev.writeCount() >= 2 })

	dev.events <- inputReport(0x21, 0x01)
	waitFor(t, "failed injection attempted", func() bool { return inj.calls.Load() == 1 })
	// The daemon must keep tracking and serving after the failure.
	waitFor(t, "daemon still serves status", func() bool { return ask("status") == "muted" })
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd /home/dan/code/mutastic/.worktrees/mic-button-f24-sweep && go test ./internal/daemon/ -run 'Inject|Debounce' -v`
Expected: FAIL to compile with `undefined: startDaemonInject`.

- [ ] **Step 3: Implement the hook**

In `internal/daemon/daemon.go`, make three edits:

(a) Add the field and gate to the `Daemon` struct (currently `Track`, `Logger`, `Light`, then `mu`/`dev`):

```go
type Daemon struct {
	Track  Tracker
	Logger *log.Logger
	Light  CommandHandler // nil when no light support is wired in
	Inject KeyInjector    // nil when no key injection is wired in (non-Windows builds)

	gate injectGate // debounces physical mute-button injections; session goroutine only

	mu  sync.Mutex
	dev Device
}
```

(b) Extend `Run`'s signature (param between `light` and `pc`) and set the field right after `d.Light = light`:

```go
func Run(ctx context.Context, open OpenFunc, light CommandHandler, inject KeyInjector, pc net.PacketConn, logger *log.Logger) error {
	d := New(logger)
	d.Light = light
	d.Inject = inject
	// ... rest of Run unchanged ...
```

(c) In `session`, insert the hook between the `DecodeEvent` ok-check and the `d.Track.Apply(ev)` branch, so the region reads:

```go
		ev, ok := proto.DecodeEvent(buf[:n])
		if !ok {
			continue
		}
		// Physical mute-button press: fire the AHK meeting-app sweep.
		// Gated on the op (0x21) alone, NOT on Apply's result — see
		// injectGate's doc comment.
		if d.Inject != nil && d.gate.shouldInject(ev, time.Now()) {
			if err := d.Inject.Inject(); err != nil {
				d.Logger.Printf("mic button -> F24 app sweep: inject failed: %v", err)
			} else {
				d.Logger.Printf("mic button -> F24 app sweep")
			}
		}
		if d.Track.Apply(ev) {
			muted, _ := d.Track.Status()
			d.Logger.Printf("event op=0x%02x value=0x%02x -> muted=%v", ev.Op, ev.Value, muted)
		} else {
			d.Logger.Printf("event op=0x%02x value=0x%02x (ignored)", ev.Op, ev.Value)
		}
```

In `internal/daemon/daemon_test.go`, refactor `startDaemon` to delegate (existing tests keep passing UNCHANGED and now double as nil-injector safety proof — `TestDeviceEventsDriveStatusOverUDP` feeds 0x21 with `Inject == nil`):

```go
func startDaemon(t *testing.T, open OpenFunc) (addr string, ask func(cmd string) string) {
	t.Helper()
	return startDaemonInject(t, open, nil)
}

// startDaemonInject is startDaemon with a KeyInjector wired into Run.
// It is startDaemon's previous body moved verbatim; the ONLY change is
// the added inject argument in the Run call.
func startDaemonInject(t *testing.T, open OpenFunc, inject KeyInjector) (addr string, ask func(cmd string) string) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Run(ctx, open, nil, inject, pc, testLogger())
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done // ensure Run has exited before later cleanups (e.g. restoring handshakeLiveness)
	})

	addr = pc.LocalAddr().String()
	ask = func(cmd string) string {
		conn, err := net.Dial("udp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := conn.Write([]byte(cmd)); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 256)
		n, err := conn.Read(buf)
		if err != nil {
			t.Fatalf("no reply to %q: %v", cmd, err)
		}
		return string(buf[:n])
	}
	return addr, ask
}
```

In `main.go:139`, fix the call site to the new signature (the real injector arrives in Task 4):

```go
	return daemon.Run(ctx, open, lights, nil, pc, logger)
```

(Adapt to the exact existing statement shape at main.go:139 — only the added `nil` argument between `lights` and `pc` is the change.)

- [ ] **Step 4: Run the tests and the full gate**

Run: `cd /home/dan/code/mutastic/.worktrees/mic-button-f24-sweep && go test ./internal/daemon/ -run 'Inject|Debounce' -v && go test -race ./... && go vet ./... && go build ./...`
Expected: the five new tests PASS; full suite `ok` for all 4 packages (all pre-existing tests untouched and green); vet and build silent.

- [ ] **Step 5: Commit**

```bash
cd /home/dan/code/mutastic/.worktrees/mic-button-f24-sweep
git add internal/daemon/daemon.go internal/daemon/daemon_test.go main.go
git commit -m "feat: fire key injector on 0x21 device-mute events in the daemon session"
```

---

### Task 4: Windows SendInput injector, non-Windows stub, wiring, and `--test-inject`

**Files:**
- Create: `inject_windows.go` (repo root, package `main`)
- Create: `inject_other.go` (repo root, package `main`)
- Modify: `main.go` (dispatch + `runTestInject` + wire real injector)
- Test: `main_test.go`

**Interfaces:**
- Consumes (from Tasks 2-3): `daemon.KeyInjector`, `daemon.Run(..., inject, ...)`.
- Produces:
  - `func newKeyInjector() daemon.KeyInjector` — Windows: `f24Injector{}` (SendInput VK_F24 0x87 down+up); non-Windows: `nil`.
  - `func runTestInject(out io.Writer) int` — hidden smoke command, exit 0 on success, 1 on failure/unsupported; prints `injected F24` on success.
  - Hidden CLI: `mutastic daemon --test-inject` (not listed in `usage()`).

- [ ] **Step 1: Write the failing test**

Add to `main_test.go` (add `bytes`, `runtime`, `strings` to imports if missing):

```go
func TestRunTestInjectUnsupportedOffWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exercises the non-Windows stub")
	}
	var out bytes.Buffer
	if got := runTestInject(&out); got != 1 {
		t.Fatalf("runTestInject() = %d, want 1 on non-Windows builds", got)
	}
	if !strings.Contains(out.String(), "only supported on Windows") {
		t.Fatalf("runTestInject() output = %q, want the platform error", out.String())
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd /home/dan/code/mutastic/.worktrees/mic-button-f24-sweep && go test . -run TestRunTestInject -v`
Expected: FAIL to compile with `undefined: runTestInject`.

- [ ] **Step 3: Write the implementation**

Create `inject_other.go`:

```go
//go:build !windows

package main

import "mutastic/internal/daemon"

// newKeyInjector returns the platform key injector. Non-Windows builds
// have none: the daemon treats a nil KeyInjector as "not wired" and
// skips the mic-button hook entirely (same spirit as openYetiX's stub).
func newKeyInjector() daemon.KeyInjector { return nil }
```

Create `inject_windows.go`:

```go
//go:build windows

package main

import (
	"fmt"
	"syscall"
	"unsafe"

	"mutastic/internal/daemon"
)

// Win32 constants for SendInput.
const (
	inputKeyboard  = 1      // INPUT.type: keyboard event
	keyeventfKeyup = 0x0002 // KEYBDINPUT.dwFlags: key release
	vkF24          = 0x87   // VK_F24 — free on this machine (pedal firmware uses F13/F14/F15)
)

// input mirrors the Win32 INPUT struct carrying a KEYBDINPUT on 64-bit
// Windows: DWORD type at offset 0, 4 bytes padding (the union is 8-byte
// aligned), KEYBDINPUT at offset 8, then trailing padding out to the
// union's largest member (MOUSEINPUT, 32 bytes). Total: 40 bytes.
type input struct {
	inputType   uint32  // INPUT.type
	_           uint32  // padding before the 8-aligned union
	wVk         uint16  // KEYBDINPUT.wVk: virtual-key code
	wScan       uint16  // KEYBDINPUT.wScan: hardware scan code (unused)
	dwFlags     uint32  // KEYBDINPUT.dwFlags: 0 = down, KEYEVENTF_KEYUP = up
	time        uint32  // KEYBDINPUT.time: 0 = system-supplied timestamp
	_           uint32  // padding: dwExtraInfo is 8-byte aligned
	dwExtraInfo uintptr // KEYBDINPUT.dwExtraInfo
	_           [8]byte // pad the union out to MOUSEINPUT's 32 bytes
}

// Compile-time size guard: each line fails to compile (negative untyped
// constant) if input drifts from the 40-byte x64 INPUT layout in either
// direction.
const (
	_ = uint64(unsafe.Sizeof(input{}) - 40)
	_ = uint64(40 - unsafe.Sizeof(input{}))
)

var procSendInput = syscall.NewLazyDLL("user32.dll").NewProc("SendInput")

// f24Injector delivers a synthetic F24 press (down then up) to the active
// desktop via user32 SendInput, firing the AHK script's F24:: meeting-app
// sweep. SendInput succeeds whether or not the AHK script is running —
// with no listener the keystroke lands nowhere, which is harmless by
// design.
type f24Injector struct{}

func (f24Injector) Inject() error {
	events := [2]input{
		{inputType: inputKeyboard, wVk: vkF24},
		{inputType: inputKeyboard, wVk: vkF24, dwFlags: keyeventfKeyup},
	}
	n, _, callErr := procSendInput.Call(
		uintptr(len(events)),
		uintptr(unsafe.Pointer(&events[0])),
		unsafe.Sizeof(events[0]),
	)
	if n != uintptr(len(events)) {
		return fmt.Errorf("SendInput inserted %d of %d events: %v", n, len(events), callErr)
	}
	return nil
}

// newKeyInjector returns the platform key injector (see inject_other.go
// for the non-Windows counterpart).
func newKeyInjector() daemon.KeyInjector { return f24Injector{} }
```

In `main.go`, make three edits:

(a) In `main()`, extend the daemon branch (currently `if os.Args[1] == "daemon" { os.Exit(runDaemon()) }`):

```go
	if os.Args[1] == "daemon" {
		if len(os.Args) > 2 && os.Args[2] == "--test-inject" {
			os.Exit(runTestInject(os.Stdout))
		}
		os.Exit(runDaemon())
	}
```

(b) Add `runTestInject` near `runClient` (keep it out of `usage()` — it is a hidden debug command):

```go
// runTestInject exercises the SendInput plumbing once and exits: a hidden
// smoke command for live verification (`mutastic daemon --test-inject`).
// With the AHK script running, this fires the F24 meeting-app sweep
// exactly as a physical mic-button press would (harmless when no meeting
// windows are open: the sweep finds nothing).
func runTestInject(out io.Writer) int {
	inj := newKeyInjector()
	if inj == nil {
		fmt.Fprintln(out, "error: key injection is only supported on Windows")
		return 1
	}
	if err := inj.Inject(); err != nil {
		fmt.Fprintln(out, "error:", err)
		return 1
	}
	fmt.Fprintln(out, "injected F24")
	return 0
}
```

(c) In `runDaemon` (the `daemon.Run` call updated in Task 3), replace the `nil` injector with the real one:

```go
	return daemon.Run(ctx, open, lights, newKeyInjector(), pc, logger)
```

- [ ] **Step 4: Run the tests and both build targets**

Run:

```bash
cd /home/dan/code/mutastic/.worktrees/mic-button-f24-sweep
go test . -run TestRunTestInject -v
go test -race ./... && go vet ./...
./build.sh
GOOS=windows GOARCH=amd64 CGO_ENABLED=1 CC=x86_64-w64-mingw32-gcc go vet .
```

Expected: `TestRunTestInjectUnsupportedOffWindows` PASS; full suite + vet clean; `built bin/mutastic.exe` (this compiles `inject_windows.go` including the compile-time size guards); the cross-vet is silent. If the cross-vet fails for cgo/toolchain reasons while `./build.sh` passes, treat `./build.sh` as the authoritative Windows gate and note it in the commit body.

- [ ] **Step 5: Commit**

```bash
cd /home/dan/code/mutastic/.worktrees/mic-button-f24-sweep
git add inject_windows.go inject_other.go main.go main_test.go
git commit -m "feat: inject synthetic F24 via SendInput on physical mic-button presses"
```

---

### Task 5: Document both flows in README

**Files:**
- Modify: `README.md` (intro lines 1-19, `## Components` bullets, `## Troubleshooting`)

**Interfaces:**
- Consumes: log line `mic button -> F24 app sweep` (Task 3), hidden command `mutastic daemon --test-inject` (Task 4), 400 ms debounce (Task 2).
- Produces: user-facing documentation of the pedal path and the physical path, including the loop analysis.

- [ ] **Step 1: Read the current README and apply the edits**

First read `README.md` in full. The anchors below were verbatim as of this plan's writing; if any anchor has drifted, adapt the anchor while preserving the new content exactly.

(a) Replace the opening line (README.md:3):

Old:
```markdown
One pedal press mutes everything: meeting apps AND the microphone itself.
```
New:
```markdown
One press mutes everything — foot pedal or the mic's own mute button:
meeting apps AND the microphone itself.
```

(b) Insert a new trigger paragraph AFTER the F13/light paragraph (README.md:17-19, ends `...light over its CH340 USB-serial port.`), separated by blank lines:

```markdown
Pressing the **mute button on the Yeti X itself** keeps the meeting apps
in sync too: the daemon sees the mic's `0x21` DeviceMute event (emitted
only for physical presses — host-initiated commands echo `0x20` instead)
and injects a synthetic `F24` keystroke; the AHK script's `F24::` hotkey
runs the same meeting-app sweep, but does NOT run `mutastic toggle` — the
mic has already toggled its own hardware mute. Both directions are
loop-free:

- **Pedal (`F14`):** AHK sweeps the apps and runs `mutastic toggle` → the
  daemon writes a `0x20` mute command → the mic echoes `0x20` (stateless,
  ignored) → no `0x21` is emitted → nothing re-triggers.
- **Mic button:** the firmware toggles the mic and emits `0x21` → the
  daemon injects `F24` (debounced, 400 ms) → AHK sweeps the apps only →
  nothing runs `mutastic toggle` → no further events.
```

(c) Extend the `mutastic daemon` bullet in `## Components` — append after the sentence `Reconnects automatically if the mic disappears.`:

```markdown
  On a physical mute-button press (`0x21` DeviceMute event), it injects a
  synthetic `F24` keystroke via `SendInput` so the AHK script sweeps the
  meeting apps; injections are debounced (400 ms) and logged as
  `mic button -> F24 app sweep`.
```

(d) Extend the `ahk/MuteAllMeetings.ahk` bullet at the end of `## Components` — after `...the F13 handler runs mutastic.exe light toggle the same way.` append:

```markdown
  The F24 handler — triggered only by the daemon's synthetic keystroke on
  a physical mic-button press — runs the meeting-app sweep alone, with no
  `mutastic.exe` call, so nothing loops back.
```

(e) Add a bullet to `## Troubleshooting` (matching the existing `**<symptom>:** <explanation>` shape):

```markdown
- **Mic button mutes the mic but the meeting apps don't follow:** check
  the log for `mic button -> F24 app sweep` right after the press's
  `event op=0x21 ...` line. Line absent → the daemon didn't see the event
  (or the 400 ms debounce suppressed a double-fire). Line present but the
  apps didn't toggle → the AHK script isn't running (`SendInput` succeeds
  regardless); relaunch it via its Startup shortcut.
  `mutastic.exe daemon --test-inject` fires one synthetic F24 to exercise
  the injection path without touching the mic.
```

- [ ] **Step 2: Verify**

```bash
cd /home/dan/code/mutastic/.worktrees/mic-button-f24-sweep
grep -c 'F24' README.md
grep -n 'mic button -> F24 app sweep' README.md
git diff --stat README.md
```

Expected: `F24` appears at least 6 times; the log line appears in both the Components bullet and the Troubleshooting bullet; diff touches only README.md.

- [ ] **Step 3: Commit**

```bash
cd /home/dan/code/mutastic/.worktrees/mic-button-f24-sweep
git add README.md
git commit -m "docs: document mic-button -> F24 meeting sweep and loop analysis"
```

---

### Task 6: Live E2E — build, deploy, verify (software-verifiable parts)

**Files:**
- No repo changes. Evidence goes to `/home/dan/code/mutastic/.worktrees/.the-usual-logs/mic-button-f24-sweep/e2e-results.md` (outside the repo — nothing to commit).

**Interfaces:**
- Consumes: everything above, `deploy/deploy.cmd` (unchanged — it already copies `bin/mutastic.exe` + `ahk/MuteAllMeetings.ahk` to `C:\Users\dan\code\mutastic-deploy\` and restarts both processes).
- Produces: a deployed, log-verified system + an evidence file + the recorded human question.

**Reminder:** you CANNOT fake a physical press in software — `0x21` only comes from hardware. The physical-press end-to-end test is the recorded human question at the bottom of this plan; everything below is the software-verifiable part.

- [ ] **Step 1: Interop pre-flight**

Run: `cmd.exe /c echo interop-ok`
Expected: `interop-ok`. On vsock/interop errors retry up to 3 times over ~2 minutes; if still failing, HALT and surface the blocker.

- [ ] **Step 2: Local gates + fresh build**

```bash
cd /home/dan/code/mutastic/.worktrees/mic-button-f24-sweep
go test -race ./... && go vet ./... && ./build.sh
```

Expected: all packages `ok`, vet silent, `built bin/mutastic.exe`. Do not deploy on any failure.

- [ ] **Step 3: Deploy via deploy.cmd (documented hang workaround)**

```bash
timeout 90 cmd.exe /c '\\wsl.localhost\Ubuntu\home\dan\code\mutastic\.worktrees\mic-button-f24-sweep\deploy\deploy.cmd' '\\wsl.localhost\Ubuntu\home\dan\code\mutastic\.worktrees\mic-button-f24-sweep' > /tmp/deploy-f24.log 2>&1
cat /tmp/deploy-f24.log
tasklist.exe | grep -i -E 'mutastic|AutoHotkey'
```

Expected: the invocation MAY hit the 90 s timeout (normal — the started daemon inherits the interop console). Success evidence is the transcript ending `Deploy complete.` (failure prints `DEPLOY FAILED`), then both `mutastic.exe` and `AutoHotkeyU64.exe` in the tasklist. The UNC paths must be single-quoted (double quotes collapse `\\` to `\`); a benign `UNC paths are not supported` cwd warning is expected. Defender contingency: if `mutastic.exe` is not running, check `powershell.exe -NoProfile -Command "Get-MpThreatDetection | Select-Object -First 5 | Format-List"` FIRST — unsigned stripped cgo binaries are a known false-positive class; the recorded fix is rebuilding without `-ldflags "-s -w"`.

- [ ] **Step 4: Verify the deployed AHK has the F24 hotkey**

```bash
grep -a -A2 'F24::' /mnt/c/Users/dan/code/mutastic-deploy/MuteAllMeetings.ahk
```

Expected: `F24::` followed by `ToggleAllMeetings()` and `return`.

- [ ] **Step 5: Verify the daemon log — fresh start, mic opened, COM4 light session up**

```bash
LOG=/mnt/c/Users/dan/AppData/Local/mutastic/mutastic.log
tail -40 "$LOG"
START=$(grep -n 'mutastic daemon starting' "$LOG" | tail -1 | cut -d: -f1)
tail -n +"$START" "$LOG" | grep -c 'device opened'      # expect: 1
tail -n +"$START" "$LOG" | grep -c 'starting session'   # expect: 1 (COM4, once)
tail -n +"$START" "$LOG" | grep -c 'mic button'         # expect: 0 (no physical press yet)
```

Expected: fresh `mutastic daemon starting` tail with HID enumeration, `device opened` = 1, light `starting session` = 1, and ZERO `mic button` lines (proves the hook is quiet without a physical press — no spurious injections from the handshake or light traffic).

- [ ] **Step 6: CLI round-trip (also seeds the required end state)**

```bash
D=/mnt/c/Users/dan/code/mutastic-deploy/mutastic.exe
$D unmute        # expect: unmuted   (seeds known state; required end state anyway)
$D status        # expect: unmuted
$D light list    # expect: one line per known light, e.g. "COM4 <name|-> connected <state>"
```

Expected: exactly those reply shapes, exit 0. Do NOT send any light command other than `list`/`status` — light state must be left untouched.

- [ ] **Step 7: SendInput plumbing smoke test**

```bash
/mnt/c/Users/dan/code/mutastic-deploy/mutastic.exe daemon --test-inject; echo "exit=$?"
```

Expected: `injected F24`, `exit=0`. This proves the SendInput path end-to-end on the real OS. With the AHK script running and no meeting windows open the sweep finds nothing (tooltip "No meeting windows found" on the Windows desktop) — harmless by design. Retry per the interop flakiness rule if the invocation itself fails with vsock errors.

- [ ] **Step 8: Final state check + write the evidence file**

```bash
D=/mnt/c/Users/dan/code/mutastic-deploy/mutastic.exe
$D status        # expect: unmuted  (mandatory end state)
$D light list    # expect: COM4 line unchanged from Step 6
```

Then write `/home/dan/code/mutastic/.worktrees/.the-usual-logs/mic-button-f24-sweep/e2e-results.md` recording: deploy transcript tail, tasklist evidence, the Step 5 grep counts, Step 6/7/8 command outputs, and the explicit note that the physical-press test is deferred to the human (see Recorded human questions). Nothing in this task is committed to the repo.

---

## Recorded human questions (surface to the user at the end of the run)

The physical mute button cannot be pressed by software — `0x21` events only come from hardware. The following require the human:

1. **Physical press, live:** press the Yeti X's mute button (ideally during a real meeting, or with a test meeting window open). Expected: the mic's own mute LED toggles once (firmware), every meeting app's mute follows within ~a second, and the daemon log shows `event op=0x21 ... -> muted=...` immediately followed by `mic button -> F24 app sweep`. Nothing double-toggles: the mic toggles exactly once per press, each app exactly once.
2. **Debounce, live:** two very fast presses (within 400 ms). Expected: the log shows two `op=0x21` events but only ONE `mic button -> F24 app sweep` line (note: the firmware itself still toggles the mic per press — the debounce guards only the app sweep).
3. **Pedal regression:** press the middle pedal (F14) during the same meeting. Expected: unchanged behavior — apps sweep AND the mic hardware toggles, log shows the `command "toggle"` line and a `0x20` echo on the `(ignored)` branch, and NO `mic button -> F24 app sweep` line.
