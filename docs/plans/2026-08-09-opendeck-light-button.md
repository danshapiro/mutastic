# OpenDeck Lights Button Implementation Plan

> **For agentic workers:** This plan is executed task-by-task by the
> workflow's execute stage: a fresh implementer per task, with a spec +
> quality review after each task. Steps use checkbox (`- [ ]`) syntax
> for tracking.

**Goal:** Add a second action to the existing OpenDeck plugin — a LIGHTS button on the top-right Stream Deck key (`keys[2]`, context `Keypad.2.0`) that collectively toggles ALL connected NEEWER desk lights and whose icon tracks whether any light is on.

**Architecture:** The plugin (`internal/deckplugin/`) currently assumes every instance is a mute button. This plan refactors its state into a per-action table keyed by action UUID (`Event.Action` is already decoded on every inbound frame but never read), so mute and light instances each get their own visible-set, last-known state, poll command, reply parser, and press behavior — all sharing the one 750ms poll tick and the one WebSocket. Deployment gains new PIL-generated icons, a second manifest action, and a parallel profile-editing script for `keys[2]`.

**Tech Stack:** Go 1.26.3 (module `mutastic`), gorilla/websocket, UDP text protocol to the daemon on 127.0.0.1:42814, Python 3 + Pillow (icon generation, WSL), PowerShell 5.1 (profile edit), DOS batch (deploy), OpenDeck v2.13.1 fork.

## Global Constraints

- Worktree root (all paths relative to it): `/home/dan/code/mutastic/.worktrees/opendeck-light-button`
- New action UUID exactly `com.danshapiro.mutastic.light`, display name exactly `Mutastic Lights`, 2 states: state 0 = lights OFF (dim gray outline of a light panel), state 1 = lights ON (bright warm-lit panel with rays)
- Icons: 144x144 RGB PNG, PURE BLACK background `(0,0,0)`, hard-edged flat colors matching the mic icons' visual language; repo paths `deck/icons/mutastic-light-on.png` and `deck/icons/mutastic-light-off.png`
- Manifest image paths are EXTENSIONLESS (`icons/mutastic-light-on`); profile image paths INCLUDE `.png` (`plugins/com.danshapiro.mutastic.sdPlugin/icons/mutastic-light-on.png`) — never mix these up
- `DisableAutomaticStates: true` in the manifest AND `disable_automatic_states = $true` in the profile instance — the plugin alone drives state
- Button state rule: ANY connected light reporting on -> state 1; otherwise (all off / zero lights attached) -> state 0; all-unknown or daemon unreachable -> hold current state and log
- One 750ms poll tick for both actions (`PollInterval` ticker in `Run`): a visible light key costs one extra UDP round trip, never a second timer
- setState only on change (OpenDeck persists the profile to disk on every setState); the log line format `setState <context> -> <state>` is load-bearing (the live E2E greps it)
- Profile edit targets `keys[2]` / context `Keypad.2.0` of `C:\Users\dan\AppData\Roaming\opendeck\profiles\sd-A00DA6141I07PW\Default.json`; do NOT disturb `keys[5]` (Mutastic Mute) — the only other populated key: keys[2] and keys[4] are currently `null`/empty in the live profile (an earlier note calling keys[4] "Device Brightness" was stale; the fixture test still snapshots keys[4]+keys[5] to prove neighbors untouched); keep a timestamped backup; edit only while opendeck.exe is stopped (deploy.cmd's kill ordering guarantees this)
- `deploy/deploy.cmd` is CRLF — every edit must preserve CRLF line endings
- `deploy/*.ps1` are ASCII, LF, written with `Set-Content -Encoding ASCII` and a non-ASCII guard (a UTF-8 BOM breaks OpenDeck's serde_json parser)
- `go test -race ./...` + `go vet ./...` clean; ALL existing tests keep passing; mute-button behavior unchanged
- README.md is the only end-user markdown doc; Windows paths in it use SINGLE backslashes inside backticks
- Do NOT modify the OpenDeck fork at `/home/dan/code/OpenDeck` (read-only reference)
- WSL interop is flaky today (vsock errors that recover in ~30-60s): wrap EVERY interop call (`*.exe`, `cmd.exe`, `powershell.exe`) in retry-up-to-3-with-45s-waits before declaring a blocker; filesystem reads via `/mnt/c` always work and are the preferred evidence channel
- Deployed tree `C:\Users\dan\code\mutastic-deploy\`; OpenDeck at `C:\Users\dan\AppData\Local\OpenDeck\opendeck.exe`; plugin installed as `com.danshapiro.mutastic.sdPlugin`; plugin log `%LOCALAPPDATA%\mutastic\deckplugin.log`
- The single currently-connected light is COM4, named `desk-right` (verified live 2026-08-09: attached, responsive, last observed `off` with stored brightness 30 / temp 2900K); the live E2E must restore its pre-test state (query first, restore whatever is found) and leave the mic unmuted

## File Structure

| File | Change | Responsibility |
|---|---|---|
| `internal/deckplugin/plugin.go` | Modify | Add `lightAnyOn` parser + per-action refactor (`actionSpec`/`actionState`, routing on `Event.Action`) |
| `internal/deckplugin/plugin_test.go` | Modify | New parser table test, per-action routing/poll/dedupe tests, `frameForAction` helper |
| `deckplugin.go` (repo root) | Modify | Per-verb UDP timeout (`commandTimeout`, `lightPluginTimeout`) |
| `deckplugin_test.go` (repo root) | Modify | Timeout selection + invariant tests |
| `internal/daemon/daemon.go` | Modify | `logCommand` suppression extended to `light status` (new latch field) |
| `internal/daemon/daemon_test.go` | Modify | Suppression test for the lights poller |
| `deck/icons/gen-light-icons.py` | Create | Committed PIL generator for the two light icons |
| `deck/icons/mutastic-light-on.png`, `deck/icons/mutastic-light-off.png` | Create | The shipped icons (144px, pure black bg) |
| `deck/com.danshapiro.mutastic.sdPlugin/manifest.json` | Modify | Second action entry |
| `deck_manifest_test.go` | Modify | Guard test rewritten to look actions up by UUID (2 actions) |
| `deploy/set-light-key.ps1` | Create | Profile editor for `keys[2]` (parallel to `set-mute-key.ps1`, distinct backup suffix) |
| `deploy/deploy.cmd` | Modify | Copy new icons + copy/invoke the new ps1 (CRLF preserved) |
| `README.md` | Modify | Plugin section documents the second action |

Reference material (read-only): the prior feature's plan `docs/plans/2026-08-09-opendeck-mute-plugin.md` documents the profile JSON shape, deploy transcript markers, and WSL-interop verification recipes this plan reuses.

---

### Task 1: Light-reply parser `lightAnyOn`

**Files:**
- Modify: `internal/deckplugin/plugin.go` (add constants + one function; nothing else changes yet)
- Test: `internal/deckplugin/plugin_test.go`

**Interfaces:**
- Consumes: nothing new (pure function over the daemon's documented reply grammar)
- Produces: `lightAnyOn(reply string) (state int, ok bool)` and constants `stateLightsOff = 0`, `stateLightsOn = 1` — Task 2 wires `lightAnyOn` into the light action's spec as its `replyToState`

Background (daemon reply grammar, from `internal/light/multi.go` + `state.go`): `light status` and `light toggle` reply with one line per attached light, `\n`-joined, `<COMx>[ <name>]: <status>` where `<status>` is exactly `on <N>% <K>K`, `off`, `unknown`, or `error: <reason>`. Labels (ports and names) never contain `:`. Zero lights attached replies `error: no light` (single line, no label). The fleet `toggle` treats `unknown` and `error: timeout` as off, so this parser mirrors that: unknown counts as off when anything else IS known, but an all-unknown reply carries no usable state.

- [ ] **Step 1: Write the failing test**

Append to `internal/deckplugin/plugin_test.go`:

```go
// TestLightAnyOn pins the light-reply -> state mapping against the
// daemon's REAL output strings (fixtures copied verbatim from
// internal/light/multi_test.go and main_test.go). ok=false means "no
// usable state: hold the current icon" — same contract as desiredState.
func TestLightAnyOn(t *testing.T) {
	cases := []struct {
		name  string
		reply string
		state int
		ok    bool
	}{
		{"single on", "COM4: on 30% 2900K", stateLightsOn, true},
		{"single named on", "COM4 desk-right: on 30% 2900K", stateLightsOn, true},
		{"single off", "COM4: off", stateLightsOff, true},
		{"all off", "COM4: off\nCOM7: off", stateLightsOff, true},
		{"mixed off and on", "COM4: off\nCOM7: on 100% 4950K", stateLightsOn, true},
		{"all on", "COM4: on 50% 4950K\nCOM7: on 50% 4950K\nCOM12: on 50% 4950K", stateLightsOn, true},
		{"on plus wedged light", "COM4: on 40% 4950K\nCOM7: error: timeout", stateLightsOn, true},
		{"off plus wedged light", "COM4: off\nCOM7: error: timeout", stateLightsOff, true},
		{"off plus unknown counts as off", "COM4: off\nCOM7: unknown", stateLightsOff, true},
		{"zero lights attached", "error: no light", stateLightsOff, true},
		{"single unknown holds", "COM4: unknown", 0, false},
		{"all unknown holds", "COM4: unknown\nCOM7: unknown", 0, false},
		{"all wedged holds", "COM4: error: timeout", 0, false},
		{"no light support holds", "error: no light support", 0, false},
		{"unknown command holds", "error: unknown light command", 0, false},
		{"empty reply holds", "", 0, false},
		{"mic reply is not a light reply", "muted", 0, false},
	}
	for _, c := range cases {
		st, ok := lightAnyOn(c.reply)
		if ok != c.ok || (ok && st != c.state) {
			t.Errorf("%s: lightAnyOn(%q) = (%d, %v), want (%d, %v)", c.name, c.reply, st, ok, c.state, c.ok)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/dan/code/mutastic/.worktrees/opendeck-light-button && go test ./internal/deckplugin/ -run TestLightAnyOn -v`
Expected: FAIL to compile with `undefined: lightAnyOn` (and `stateLightsOn`/`stateLightsOff`)

- [ ] **Step 3: Write the implementation**

In `internal/deckplugin/plugin.go`, extend the existing state-index const block (currently `stateLive`/`stateMuted` at the top of the file) to:

```go
const (
	stateLive  = 0 // mute action state 0: live mic (icons/mutastic-mic)
	stateMuted = 1 // mute action state 1: muted (icons/mutastic-mic-muted)

	stateLightsOff = 0 // light action state 0: all lights off (icons/mutastic-light-off)
	stateLightsOn  = 1 // light action state 1: any light on (icons/mutastic-light-on)
)
```

Add `"strings"` to the import block (it currently imports only `context`, `fmt`, `log`, `time`), then add directly below the existing `desiredState` function:

```go
// lightAnyOn maps a "light status" (or "light toggle") fan-out reply to
// the light action's state index. Per-line grammar (multi.go handleAll):
// "<COMx>[ <name>]: <status>" where status is "on <N>% <K>K", "off",
// "unknown", or "error: <reason>"; labels never contain ':' so the
// first ": " split is unambiguous even for "COM7: error: timeout".
// Rules: ANY light on -> stateLightsOn; else any light known-off — or
// the zero-lights reply "error: no light" (nothing attached, nothing
// is on) -> stateLightsOff; else (all unknown/errors/unparseable) ->
// ok=false: no usable state, the caller holds the current icon. This
// mirrors the fleet toggle's own predicate, which counts unknown and
// timed-out lights as off.
func lightAnyOn(reply string) (state int, ok bool) {
	if reply == "error: no light" {
		return stateLightsOff, true
	}
	if strings.HasPrefix(reply, "error:") {
		return 0, false
	}
	sawOff := false
	for _, line := range strings.Split(reply, "\n") {
		_, status, found := strings.Cut(line, ": ")
		if !found {
			continue
		}
		if status == "on" || strings.HasPrefix(status, "on ") {
			return stateLightsOn, true
		}
		if status == "off" {
			sawOff = true
		}
	}
	if sawOff {
		return stateLightsOff, true
	}
	return 0, false
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/deckplugin/ -run TestLightAnyOn -v`
Expected: PASS (all 17 cases)

- [ ] **Step 5: Run the whole package + commit**

```bash
go test ./internal/deckplugin/ && go vet ./internal/deckplugin/
git -C /home/dan/code/mutastic/.worktrees/opendeck-light-button add internal/deckplugin/plugin.go internal/deckplugin/plugin_test.go
git -C /home/dan/code/mutastic/.worktrees/opendeck-light-button commit -m "feat(deckplugin): lightAnyOn maps light fan-out replies to a collective on/off state"
```

---

### Task 2: Per-action instance routing and polling

**Files:**
- Modify: `internal/deckplugin/plugin.go` (replace the single-action state with a per-action table)
- Test: `internal/deckplugin/plugin_test.go`

**Interfaces:**
- Consumes: `lightAnyOn` + `stateLights*` from Task 1; existing `desiredState`, `Event.Action` (already decoded by `DecodeEvent` in `protocol.go` — no protocol changes needed), fakes `fakeConn`/`fakeDaemon`/`fakeInjector`
- Produces: constants `actionMute = "com.danshapiro.mutastic.mute"`, `actionLight = "com.danshapiro.mutastic.light"`; types `actionSpec`, `actionState`; `Plugin.actions map[string]*actionState` replacing `Plugin.visible`/`lastKnown`/`pollDown`; `pushAll(st *actionState)` and `handleKeyDown(ev Event, st *actionState)` signatures; test helper `frameForAction(event, action, ctx string) []byte` and const `lightWillAppearFrame`. `New`, `Run`, `HandleMessage`, `PollOnce`, `sendSetState` keep their current signatures.

The public API (`New`, `Run`, `HandleMessage`, `PollOnce`) is unchanged, so all 22 existing tests must keep passing untouched. The daemon commands the light action speaks are the literal strings `light status` (poll/probe) and `light toggle` (press); the daemon routes any `light`-prefixed command to the light fleet manager (`internal/daemon/daemon.go` `HandleCommand`).

- [ ] **Step 1: Write the failing tests**

First add the frame helpers next to the existing `frameFor` in `internal/deckplugin/plugin_test.go` — and REPLACE the body of the existing `frameFor` with the delegating version so mute tests keep working unchanged:

```go
// frameForAction builds an event frame for an arbitrary action + context.
func frameForAction(event, action, ctx string) []byte {
	return fmt.Appendf(nil, `{"event":%q,"action":%q,"context":%q,"device":"sd-X","payload":{"settings":{},"coordinates":{"row":0,"column":2},"controller":"Keypad","state":0,"isInMultiAction":false}}`, event, action, ctx)
}

// lightWillAppearFrame mirrors willAppearFrame for the lights action on
// the real deck's top-right key (Keypad.2.0).
const lightWillAppearFrame = `{"event":"willAppear","action":"com.danshapiro.mutastic.light","context":"sd-X.Default.Keypad.2.0","device":"sd-X","payload":{"settings":{},"coordinates":{"row":0,"column":2},"controller":"Keypad","state":0,"isInMultiAction":false}}`
```

New body for the existing `frameFor` (keep its doc comment):

```go
func frameFor(event, ctx string) []byte {
	return frameForAction(event, "com.danshapiro.mutastic.mute", ctx)
}
```

Then append the behavior tests:

```go
func TestLightWillAppearProbesLightStatus(t *testing.T) {
	conn := newFakeConn()
	fd := &fakeDaemon{replies: map[string]string{"light status": "COM4 desk-right: on 30% 2900K"}}
	p := New(conn, fd, nil, testLogger())
	p.HandleMessage([]byte(lightWillAppearFrame))
	if got := fd.call(0); got != "light status" {
		t.Fatalf("willAppear probe = %q, want %q", got, "light status")
	}
	want := `{"event":"setState","context":"sd-X.Default.Keypad.2.0","payload":{"state":1}}`
	if got := conn.write(0); got != want {
		t.Fatalf("frame = %s, want %s", got, want)
	}
}

func TestLightKeyDownSendsLightToggleAndNeverInjects(t *testing.T) {
	conn := newFakeConn()
	fd := &fakeDaemon{replies: map[string]string{"light status": "COM4: off", "light toggle": "COM4: on 30% 2900K"}}
	inj := &fakeInjector{}
	p := New(conn, fd, inj, testLogger())
	p.HandleMessage([]byte(lightWillAppearFrame)) // probe pushes state 0
	base := conn.writeCount()

	p.HandleMessage(frameForAction("keyDown", "com.danshapiro.mutastic.light", "sd-X.Default.Keypad.2.0"))

	if got := fd.call(fd.callCount() - 1); got != "light toggle" {
		t.Fatalf("keyDown daemon command = %q, want %q", got, "light toggle")
	}
	if n := inj.calls.Load(); n != 0 {
		t.Fatalf("F24 injections = %d, want 0 (lights have nothing to do with meetings)", n)
	}
	// The toggle reply is the NEW fleet state: icon updates immediately.
	want := `{"event":"setState","context":"sd-X.Default.Keypad.2.0","payload":{"state":1}}`
	if got := conn.write(base); got != want {
		t.Fatalf("frame = %s, want %s", got, want)
	}
}

func TestMuteKeyDownRoutingUnchanged(t *testing.T) {
	conn := newFakeConn()
	fd := &fakeDaemon{replies: map[string]string{"status": "unmuted", "toggle": "muted"}}
	inj := &fakeInjector{}
	p := New(conn, fd, inj, testLogger())
	p.HandleMessage([]byte(willAppearFrame))

	p.HandleMessage(frameFor("keyDown", "sd-X.Default.Keypad.5.0"))

	if got := fd.call(fd.callCount() - 1); got != "toggle" {
		t.Fatalf("keyDown daemon command = %q, want %q (mute must not speak light verbs)", got, "toggle")
	}
	if n := inj.calls.Load(); n != 1 {
		t.Fatalf("F24 injections = %d, want exactly 1", n)
	}
}

func TestPollOncePollsPerVisibleAction(t *testing.T) {
	conn := newFakeConn()
	fd := &fakeDaemon{replies: map[string]string{"status": "unmuted", "light status": "COM4: off"}}
	p := New(conn, fd, nil, testLogger())

	// Only a mute key visible: the tick sends ONLY the mic status.
	p.HandleMessage([]byte(willAppearFrame))
	base := fd.callCount()
	p.PollOnce()
	if got := fd.callCount(); got != base+1 {
		t.Fatalf("daemon calls after mute-only poll = %d, want %d", got, base+1)
	}
	if got := fd.call(base); got != "status" {
		t.Fatalf("poll command = %q, want %q", got, "status")
	}

	// A light key appears too: the SAME tick now costs one extra round trip.
	p.HandleMessage([]byte(lightWillAppearFrame))
	base = fd.callCount()
	p.PollOnce()
	if got := fd.callCount(); got != base+2 {
		t.Fatalf("daemon calls after both-actions poll = %d, want %d", got, base+2)
	}
	if a, b := fd.call(base), fd.call(base+1); a != "status" || b != "light status" {
		t.Fatalf("poll commands = %q, %q; want %q then %q", a, b, "status", "light status")
	}
}

func TestLightPollPushesOnlyOnChange(t *testing.T) {
	conn := newFakeConn()
	fd := &fakeDaemon{replies: map[string]string{"light status": "COM4: off"}}
	p := New(conn, fd, nil, testLogger())
	p.HandleMessage([]byte(lightWillAppearFrame)) // probe pushes state 0
	base := conn.writeCount()

	p.PollOnce() // unchanged: no push
	if got := conn.writeCount(); got != base {
		t.Fatalf("writes after unchanged poll = %d, want %d (setState persists the profile; only push on change)", got, base)
	}

	fd.setReply("light status", "COM4: on 30% 2900K")
	p.PollOnce()
	if got := conn.writeCount(); got != base+1 {
		t.Fatalf("writes after changed poll = %d, want %d", got, base+1)
	}
	want := `{"event":"setState","context":"sd-X.Default.Keypad.2.0","payload":{"state":1}}`
	if got := conn.write(base); got != want {
		t.Fatalf("frame = %s, want %s", got, want)
	}

	p.PollOnce() // still on: no new push
	if got := conn.writeCount(); got != base+1 {
		t.Fatalf("writes after second unchanged poll = %d, want %d", got, base+1)
	}
}

func TestLightPollUnknownOrUnreachableHoldsState(t *testing.T) {
	conn := newFakeConn()
	fd := &fakeDaemon{replies: map[string]string{"light status": "COM4: on 30% 2900K"}}
	p := New(conn, fd, nil, testLogger())
	p.HandleMessage([]byte(lightWillAppearFrame)) // probe pushes state 1
	base := conn.writeCount()

	fd.setReply("light status", "COM4: unknown") // all-unknown: hold
	p.PollOnce()
	if got := conn.writeCount(); got != base {
		t.Fatalf("writes after all-unknown poll = %d, want %d (hold current state)", got, base)
	}

	fd.setErr(errors.New("daemon down")) // unreachable: hold
	p.PollOnce()
	if got := conn.writeCount(); got != base {
		t.Fatalf("writes after unreachable poll = %d, want %d (hold current state)", got, base)
	}
}

func TestActionsDoNotCrossContaminate(t *testing.T) {
	conn := newFakeConn()
	fd := &fakeDaemon{replies: map[string]string{"status": "unmuted", "light status": "COM4: off"}}
	p := New(conn, fd, nil, testLogger())
	p.HandleMessage([]byte(willAppearFrame))      // mute key: state 0 pushed
	p.HandleMessage([]byte(lightWillAppearFrame)) // light key: state 0 pushed
	base := conn.writeCount()

	// Mic flips: ONLY the mute context gets a setState.
	fd.setReply("status", "muted")
	p.PollOnce()
	if got := conn.writeCount(); got != base+1 {
		t.Fatalf("writes after mic flip = %d, want %d (one push, mute key only)", got, base+1)
	}
	want := `{"event":"setState","context":"sd-X.Default.Keypad.5.0","payload":{"state":1}}`
	if got := conn.write(base); got != want {
		t.Fatalf("frame = %s, want %s", got, want)
	}

	// Lights flip: ONLY the light context gets a setState.
	fd.setReply("light status", "COM4: on 30% 2900K")
	p.PollOnce()
	if got := conn.writeCount(); got != base+2 {
		t.Fatalf("writes after light flip = %d, want %d", got, base+2)
	}
	want = `{"event":"setState","context":"sd-X.Default.Keypad.2.0","payload":{"state":1}}`
	if got := conn.write(base + 1); got != want {
		t.Fatalf("frame = %s, want %s", got, want)
	}
}

func TestUnknownActionEventsAreIgnored(t *testing.T) {
	conn := newFakeConn()
	fd := &fakeDaemon{replies: map[string]string{}}
	p := New(conn, fd, nil, testLogger())
	p.HandleMessage(frameForAction("willAppear", "com.danshapiro.mutastic.nonesuch", "sd-X.Default.Keypad.1.0"))
	p.HandleMessage(frameForAction("keyDown", "com.danshapiro.mutastic.nonesuch", "sd-X.Default.Keypad.1.0"))
	if got := conn.writeCount(); got != 0 {
		t.Fatalf("writes = %d, want 0 (unknown action must be ignored)", got)
	}
	if got := fd.callCount(); got != 0 {
		t.Fatalf("daemon calls = %d, want 0 (unknown action must not probe or toggle)", got)
	}
	p.PollOnce() // and it must not have joined any visible set
	if got := fd.callCount(); got != 0 {
		t.Fatalf("daemon calls after poll = %d, want 0", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/deckplugin/ -v 2>&1 | tail -40`
Expected: the new tests FAIL (e.g. `TestLightWillAppearProbesLightStatus` gets probe command `"status"` instead of `"light status"`; `TestLightKeyDownSendsLightToggleAndNeverInjects` sees command `"toggle"` and 1 injection) because `HandleMessage` ignores `ev.Action` entirely. Existing tests still pass.

- [ ] **Step 3: Implement the per-action refactor**

In `internal/deckplugin/plugin.go`, add below the state constants:

```go
// Action UUIDs served by this plugin (manifest Actions[].UUID). Every
// inbound willAppear/willDisappear/keyDown carries one in its "action"
// field; all routing keys off it.
const (
	actionMute  = "com.danshapiro.mutastic.mute"
	actionLight = "com.danshapiro.mutastic.light"
)

// actionOrder is the deterministic per-tick polling order (map
// iteration order would make tests and logs flap).
var actionOrder = []string{actionMute, actionLight}
```

Add below the `Injector` interface:

```go
// actionSpec is one action's behavior table: the daemon commands it
// speaks, how its replies map to state indices, and whether a key
// press also injects F24 (the mute action's meeting-app sweep; never
// for lights).
type actionSpec struct {
	statusCmd     string
	toggleCmd     string
	replyToState  func(string) (int, bool)
	injectOnPress bool
}

// actionState is one action's worth of runtime state — exactly what
// used to be the whole Plugin state back when mute was the only action.
type actionState struct {
	spec      actionSpec
	visible   map[string]bool // context -> instance currently on a visible key
	lastKnown int             // last state observed/pushed; -1 = never known
	pollDown  bool            // daemon was unreachable at the last poll (log transitions, not every 750ms)
	noState   bool            // last poll reply carried no usable state (log transitions, not every 750ms)
}
```

Replace the `Plugin` struct's three per-action fields (`visible`, `lastKnown`, `pollDown`) with one map, and rebuild `New`:

```go
// Plugin is one running plugin session. All methods are called from a
// single goroutine (Run's select loop feeds HandleMessage and PollOnce),
// so there is no internal locking by design.
type Plugin struct {
	conn   Conn
	daemon DaemonClient
	inject Injector // may be nil (non-Windows): mute keyDown skips the F24 sweep
	logger *log.Logger

	actions map[string]*actionState // action UUID -> that action's state
}

// New builds a Plugin serving both actions. inject may be nil (no key
// injection on this platform); logger must not be nil (tests pass
// log.New(io.Discard,"",0)).
func New(conn Conn, daemonClient DaemonClient, inject Injector, logger *log.Logger) *Plugin {
	return &Plugin{
		conn:   conn,
		daemon: daemonClient,
		inject: inject,
		logger: logger,
		actions: map[string]*actionState{
			actionMute: {
				spec:      actionSpec{statusCmd: "status", toggleCmd: "toggle", replyToState: desiredState, injectOnPress: true},
				visible:   make(map[string]bool),
				lastKnown: -1,
			},
			actionLight: {
				spec:      actionSpec{statusCmd: "light status", toggleCmd: "light toggle", replyToState: lightAnyOn, injectOnPress: false},
				visible:   make(map[string]bool),
				lastKnown: -1,
			},
		},
	}
}
```

Replace `HandleMessage` (keep its existing doc comment about unhandled events falling through silently):

```go
func (p *Plugin) HandleMessage(data []byte) {
	ev, err := DecodeEvent(data)
	if err != nil {
		p.logger.Printf("ignoring undecodable frame: %v", err)
		return
	}
	switch ev.Event {
	case "willAppear", "willDisappear", "keyDown":
	default:
		return // titleParametersDidChange etc.: ignored by design
	}
	st, ok := p.actions[ev.Action]
	if !ok {
		p.logger.Printf("%s %s: unknown action %q, ignoring", ev.Event, ev.Context, ev.Action)
		return
	}
	switch ev.Event {
	case "willAppear":
		st.visible[ev.Context] = true
		p.logger.Printf("willAppear %s %s (visible: %d)", ev.Action, ev.Context, len(st.visible))
		// Correct this key's icon immediately instead of waiting a tick.
		if reply, err := p.daemon.Command(st.spec.statusCmd); err != nil {
			p.logger.Printf("willAppear %s: %s failed: %v", ev.Context, st.spec.statusCmd, err)
		} else if s, ok := st.spec.replyToState(reply); ok && s != st.lastKnown {
			// The probe observed a state change: every visible instance
			// of THIS action is stale, not just this one. Recording
			// lastKnown without pushing to all would make the next poll
			// see "no change" and leave the older keys wrong. pushAll
			// covers ev.Context too (just added).
			st.lastKnown = s
			p.pushAll(st)
			return
		}
		// Unchanged, or unknown/unreachable with a prior known state:
		// correct only the appearing key.
		if st.lastKnown >= 0 {
			p.sendSetState(ev.Context, st.lastKnown)
		}
	case "willDisappear":
		delete(st.visible, ev.Context)
		p.logger.Printf("willDisappear %s %s (visible: %d)", ev.Action, ev.Context, len(st.visible))
	case "keyDown":
		p.handleKeyDown(ev, st)
	}
}
```

Replace `PollOnce` and `pushAll`:

```go
// PollOnce polls the daemon once per action that has visible instances
// and, when an action's state is known and has CHANGED, pushes setState
// to that action's visible instances. Pushing only on change matters:
// OpenDeck persists the profile to disk on every setState. Unknown or
// unreachable leaves the icons untouched. Both actions share the one
// 750ms tick — a visible light key costs one extra UDP round trip per
// tick, never a second timer.
func (p *Plugin) PollOnce() {
	for _, action := range actionOrder {
		st := p.actions[action]
		if len(st.visible) == 0 {
			continue
		}
		reply, err := p.daemon.Command(st.spec.statusCmd)
		if err != nil {
			if !st.pollDown {
				st.pollDown = true
				p.logger.Printf("%s poll: daemon unreachable, keeping icon: %v", st.spec.statusCmd, err)
			}
			continue
		}
		if st.pollDown {
			st.pollDown = false
			p.logger.Printf("%s poll: daemon reachable again", st.spec.statusCmd)
		}
		s, ok := st.spec.replyToState(reply)
		if !ok {
			// Unknown / error reply: hold the current icon, and log the
			// TRANSITION into this condition (all-unknown lights after a
			// daemon restart would otherwise spam a line every 750ms).
			if !st.noState {
				st.noState = true
				p.logger.Printf("%s poll: no usable state in %q, keeping icon", st.spec.statusCmd, reply)
			}
			continue
		}
		st.noState = false
		if s == st.lastKnown {
			continue
		}
		st.lastKnown = s
		p.pushAll(st)
	}
}

// pushAll sends the action's last-known state to its visible instances.
func (p *Plugin) pushAll(st *actionState) {
	for ctx := range st.visible {
		p.sendSetState(ctx, st.lastKnown)
	}
}
```

Replace `handleKeyDown` (preserve the LOOP HAZARD warning — it is load-bearing):

```go
// handleKeyDown routes a key press to its action: send the action's
// toggle command, update the icon from the reply (the toggle reply IS
// the new state — don't wait a tick), and — for the mute action only —
// inject exactly one F24 for the meeting-app sweep. Each half runs even
// if the other fails. LOOP HAZARD: never inject F24 in reaction to a
// state change or the daemon's own injection — F24 must only ever mean
// "sweep the meeting apps once for this key press". The light action
// never injects: lights have nothing to do with meetings.
func (p *Plugin) handleKeyDown(ev Event, st *actionState) {
	reply, err := p.daemon.Command(st.spec.toggleCmd)
	if err != nil {
		p.logger.Printf("keyDown %s: %s failed: %v", ev.Context, st.spec.toggleCmd, err)
	} else {
		p.logger.Printf("keyDown %s: %s -> %q", ev.Context, st.spec.toggleCmd, reply)
		if s, ok := st.spec.replyToState(reply); ok && s != st.lastKnown {
			st.lastKnown = s
			p.pushAll(st)
		}
	}
	if !st.spec.injectOnPress {
		return
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

`sendSetState`, `desiredState`, `Run`, and everything in `protocol.go` stay byte-for-byte unchanged.

- [ ] **Step 4: Run the full package suite**

Run: `go test ./internal/deckplugin/ -v 2>&1 | tail -50`
Expected: PASS — all new tests AND every pre-existing test (the mute path must behave identically; if any existing test fails, the refactor is wrong, not the test).

- [ ] **Step 5: Race + vet + commit**

```bash
go test -race ./internal/deckplugin/ && go vet ./internal/deckplugin/
git -C /home/dan/code/mutastic/.worktrees/opendeck-light-button add internal/deckplugin/plugin.go internal/deckplugin/plugin_test.go
git -C /home/dan/code/mutastic/.worktrees/opendeck-light-button commit -m "feat(deckplugin): route instances per action UUID; lights key polls light status and sends light toggle"
```

---

### Task 3: Per-verb UDP timeout in the plugin's daemon client

**Files:**
- Modify: `deckplugin.go` (repo root, package main)
- Test: `deckplugin_test.go` (repo root)

**Interfaces:**
- Consumes: `light.CallTimeout` (`mutastic/internal/light`, = 2s), existing `askDaemon(cmd, addr string, timeout time.Duration)`
- Produces: `commandTimeout(cmd string) time.Duration` and `const lightPluginTimeout`; `udpDaemonClient` loses its `timeout` field (constructed as `udpDaemonClient{udpAddr}`)

Why: the plugin's UDP client is hard-wired to 1s, but the daemon's per-light stall bound (`light.CallTimeout`) is 2s — a wedged light's degraded reply (healthy lines + per-line `error: timeout`) lands just AFTER 2s, so a 1s budget would misread partial success as "daemon unreachable" on every poll. The CLI uses 6s, but the plugin cannot: `Command` blocks the single event-loop goroutine, so the budget also caps how long a wedged light can stall mute-key handling. Resolution: `light.CallTimeout + 500ms` (2.5s) — enough headroom for a localhost UDP round trip after the daemon's internal deadline, small enough that a wedged light degrades the plugin gracefully. A missed reply is harmless: the poll holds the current icon and self-heals on a later tick. Two verified caveats (daemon code traced): the daemon serves UDP serially, so a command queued behind ANOTHER client's wedged mutating light command (e.g. a pedal-driven `light toggle`) can reply at ~4s > 2.5s — bounded, one missed reply, self-heals next tick, no code change needed. And NEVER retry a timed-out toggle: the daemon executes the command even when the reply is lost, so a retry double-toggles — the plugin sends exactly one `Command` per event; keep it that way.

- [ ] **Step 1: Write the failing test**

Append to `deckplugin_test.go` (add `"mutastic/internal/light"` to its imports; `"time"` is already imported):

```go
// TestCommandTimeout pins the per-verb UDP budgets: mic verbs stay
// snappy at 1s; light verbs get light.CallTimeout+500ms so a wedged
// light's degraded-mode reply (which lands just after CallTimeout) is
// still readable. The prefix rule mirrors the daemon's own routing:
// "light" + end-of-string, space, or '@' — "lightning" is a mic-side
// (unknown) command.
func TestCommandTimeout(t *testing.T) {
	wantLight := light.CallTimeout + 500*time.Millisecond
	cases := []struct {
		cmd  string
		want time.Duration
	}{
		{"status", time.Second},
		{"toggle", time.Second},
		{"light status", wantLight},
		{"light toggle", wantLight},
		{"light", wantLight},
		{"light@desk-right brightness 30", wantLight},
		{"lightning", time.Second},
	}
	for _, c := range cases {
		if got := commandTimeout(c.cmd); got != c.want {
			t.Errorf("commandTimeout(%q) = %v, want %v", c.cmd, got, c.want)
		}
	}
}

// TestLightPluginTimeoutExceedsDaemonStallBound mirrors
// TestLightClientTimeoutExceedsDaemonStallBound in main_test.go: a
// budget at or below light.CallTimeout deterministically misses the
// degraded-mode reply and masks partial success as daemon failure.
func TestLightPluginTimeoutExceedsDaemonStallBound(t *testing.T) {
	if lightPluginTimeout <= light.CallTimeout {
		t.Fatalf("lightPluginTimeout = %v, want > light.CallTimeout (%v)", lightPluginTimeout, light.CallTimeout)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./ -run 'TestCommandTimeout|TestLightPluginTimeout' -v`
Expected: FAIL to compile with `undefined: commandTimeout` / `undefined: lightPluginTimeout`

- [ ] **Step 3: Implement**

In `deckplugin.go` (repo root): add `"mutastic/internal/light"` to the imports (`"strings"` and `"time"` are already imported), then replace the `udpDaemonClient` type and method (currently `struct { addr string; timeout time.Duration }`) with:

```go
// lightPluginTimeout is the plugin's UDP read budget for light verbs.
// It must exceed the daemon's per-light stall bound (light.CallTimeout):
// a wedged light's degraded reply lands just after that bound. Unlike
// the CLI's 6s lightClientTimeout, the plugin adds only 500ms of
// headroom because this call blocks the plugin's single event-loop
// goroutine — the budget also caps how long a wedged light can stall
// mute-key handling. A missed reply just holds the icon one tick.
const lightPluginTimeout = light.CallTimeout + 500*time.Millisecond

// commandTimeout picks the UDP read budget for one plugin->daemon
// command. The light-prefix rule mirrors the daemon's routing in
// daemon.HandleCommand: "light" + end-of-string, space, or '@'.
func commandTimeout(cmd string) time.Duration {
	if rest, ok := strings.CutPrefix(cmd, "light"); ok && (rest == "" || rest[0] == ' ' || rest[0] == '@') {
		return lightPluginTimeout
	}
	return time.Second
}

// udpDaemonClient implements deckplugin.DaemonClient: one UDP round
// trip per call with a per-verb timeout (see commandTimeout).
type udpDaemonClient struct {
	addr string
}

func (u udpDaemonClient) Command(cmd string) (string, error) {
	return askDaemon(cmd, u.addr, commandTimeout(cmd))
}
```

Then update the single construction site in `runDeckPlugin` (currently `udpDaemonClient{udpAddr, time.Second}`):

```go
	p := deckplugin.New(wsConn{ws}, udpDaemonClient{udpAddr}, inject, logger)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./ -v 2>&1 | tail -30`
Expected: PASS (including the pre-existing `TestAskDaemon` and `TestLightClientTimeoutExceedsDaemonStallBound`)

- [ ] **Step 5: Commit**

```bash
go vet ./...
git -C /home/dan/code/mutastic/.worktrees/opendeck-light-button add deckplugin.go deckplugin_test.go
git -C /home/dan/code/mutastic/.worktrees/opendeck-light-button commit -m "feat(deckplugin): per-verb UDP budget — light verbs get CallTimeout+500ms, mic verbs stay 1s"
```

---

### Task 4: Daemon log suppression for the lights poller

**Files:**
- Modify: `internal/daemon/daemon.go` (the `Daemon` struct + `logCommand`)
- Test: `internal/daemon/daemon_test.go`

**Interfaces:**
- Consumes: existing `Daemon.Logger`, `Daemon.lastStatusReply`, `logCommand(cmd, reply string)`
- Produces: new unexported field `Daemon.lastLightStatusReply string`; no signature changes

Why: `logCommand` suppresses repeated identical replies only for the literal command `"status"`. The new lights key polls `light status` every 750ms; without suppression the daemon log grows one line per tick forever (rotation runs only at daemon start).

- [ ] **Step 1: Write the failing test**

Append to `internal/daemon/daemon_test.go` (add `"bytes"`, `"log"`, and `"strings"` to its imports if not already present):

```go
// TestLogCommandSuppressesRepeatedLightStatus: the lights key polls
// "light status" every ~750ms, exactly like the mute key polls
// "status" — both need the repeated-reply latch or the log grows
// unbounded (rotation runs only at daemon start).
func TestLogCommandSuppressesRepeatedLightStatus(t *testing.T) {
	var buf bytes.Buffer
	d := &Daemon{Logger: log.New(&buf, "", 0)}
	d.logCommand("light status", "COM4: off")
	d.logCommand("light status", "COM4: off")          // identical: suppressed
	d.logCommand("light status", "COM4: on 30% 2900K") // changed: logs
	d.logCommand("status", "muted")                    // separate latch, separate bookkeeping
	d.logCommand("light toggle", "COM4: off")          // non-poll verbs always log
	if got := strings.Count(buf.String(), `"light status"`); got != 2 {
		t.Fatalf("light status logged %d times, want 2 (first + change):\n%s", got, buf.String())
	}
	if got := strings.Count(buf.String(), `"light toggle"`); got != 1 {
		t.Fatalf("light toggle logged %d times, want 1:\n%s", got, buf.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/daemon/ -run TestLogCommandSuppressesRepeatedLightStatus -v`
Expected: FAIL with `light status logged 3 times, want 2` (every tick currently logs)

- [ ] **Step 3: Implement**

In `internal/daemon/daemon.go`: find the `lastStatusReply` field in the `Daemon` struct (`grep -n lastStatusReply internal/daemon/daemon.go`) and add directly below it:

```go
	lastLightStatusReply string // like lastStatusReply, for the lights key's "light status" poller
```

Replace `logCommand`:

```go
// logCommand logs one served UDP command. Non-poll commands always log.
// The two resident-poller commands ("status" from the mute key,
// "light status" from the lights key, each every ~750ms) log only when
// their reply differs from the previously logged reply for that
// command: rotation runs only at daemon start, so unconditional logging
// would grow the log unbounded. Called only from the single serveUDP
// goroutine, so the latches need no lock.
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
	}
	d.Logger.Printf("command %q -> %q", cmd, reply)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/daemon/`
Expected: PASS (all existing daemon tests plus the new one)

- [ ] **Step 5: Commit**

```bash
git -C /home/dan/code/mutastic/.worktrees/opendeck-light-button add internal/daemon/daemon.go internal/daemon/daemon_test.go
git -C /home/dan/code/mutastic/.worktrees/opendeck-light-button commit -m "feat(daemon): suppress repeated identical 'light status' log lines (lights-key poller)"
```

---

### Task 5: Light icons (PIL generator + PNGs)

**Files:**
- Create: `deck/icons/gen-light-icons.py`
- Create: `deck/icons/mutastic-light-on.png`, `deck/icons/mutastic-light-off.png` (generated output, committed)

**Interfaces:**
- Consumes: Pillow (available in WSL: `python3 -c "import PIL"` — version 12.3.0)
- Produces: the two PNG paths that Task 6's manifest/guard-test and Task 7's deploy.cmd/profile edit reference by exact name

Visual language to match (measured from the mic icons): 144x144, mode RGB (no alpha), pure-black background, bold hard-edged glyph roughly inside x 44..100 / y 26..112, very few flat colors, no anti-aliasing to speak of. No committed generator exists for the mic icons (verified across git history) — this is the repo's first, which is fine.

- [ ] **Step 1: Write the generator**

Create `deck/icons/gen-light-icons.py`:

```python
#!/usr/bin/env python3
"""Generate the Mutastic Lights key icons.

Writes deck/icons/mutastic-light-on.png and mutastic-light-off.png,
matching the mic icons' visual language: 144x144 RGB PNG, pure-black
background, bold hard-edged glyph, few flat colours. State 0 (OFF) is a
dim gray OUTLINE of a desk light panel; state 1 (ON) is the panel filled
bright warm with short rays.

Run from the repo root:  python3 deck/icons/gen-light-icons.py
"""
from PIL import Image, ImageDraw

SIZE = 144
BLACK = (0, 0, 0)
GRAY = (128, 128, 128)
WHITE = (255, 255, 255)
WARM = (255, 228, 138)  # warm lit panel fill
RAY = (255, 240, 200)   # slightly paler warm rays

# Panel geometry (sits in the mic glyph's footprint: x 44..100, y 26..112)
PANEL = (38, 44, 106, 92)  # rounded-rect light panel
RADIUS = 10
STEM = (68, 92, 76, 104)   # stand stem below the panel
BASE = (52, 104, 92, 112)  # stand base bar

RAYS = [  # short rays for the ON state: (x0, y0, x1, y1), width 8
    (72, 34, 72, 16),    # straight up
    (46, 38, 32, 24),    # up-left diagonal
    (98, 38, 112, 24),   # up-right diagonal
    (28, 68, 12, 68),    # left
    (116, 68, 132, 68),  # right
]


def off_icon() -> Image.Image:
    img = Image.new("RGB", (SIZE, SIZE), BLACK)
    d = ImageDraw.Draw(img)
    # Dim gray OUTLINE only: the panel is dark.
    d.rounded_rectangle(PANEL, radius=RADIUS, outline=GRAY, width=8)
    d.rectangle(STEM, fill=GRAY)
    d.rectangle(BASE, fill=GRAY)
    return img


def on_icon() -> Image.Image:
    img = Image.new("RGB", (SIZE, SIZE), BLACK)
    d = ImageDraw.Draw(img)
    for x0, y0, x1, y1 in RAYS:
        d.line((x0, y0, x1, y1), fill=RAY, width=8)
    # Bright warm-lit panel with a white frame.
    d.rounded_rectangle(PANEL, radius=RADIUS, fill=WARM, outline=WHITE, width=6)
    d.rectangle(STEM, fill=WHITE)
    d.rectangle(BASE, fill=WHITE)
    return img


def main() -> None:
    off_icon().save("deck/icons/mutastic-light-off.png")
    on_icon().save("deck/icons/mutastic-light-on.png")
    print("wrote deck/icons/mutastic-light-off.png and mutastic-light-on.png")


if __name__ == "__main__":
    main()
```

- [ ] **Step 2: Generate the icons**

Run: `cd /home/dan/code/mutastic/.worktrees/opendeck-light-button && python3 deck/icons/gen-light-icons.py`
Expected: `wrote deck/icons/mutastic-light-off.png and mutastic-light-on.png`

- [ ] **Step 3: Verify the icons match the visual contract**

```bash
python3 - <<'PYEOF'
from PIL import Image
checks = [
    ("deck/icons/mutastic-light-off.png", (128, 128, 128)),
    ("deck/icons/mutastic-light-on.png", (255, 228, 138)),
]
for name, must_have in checks:
    img = Image.open(name)
    assert img.size == (144, 144), (name, img.size)
    assert img.mode == "RGB", (name, img.mode)
    assert img.getpixel((0, 0)) == (0, 0, 0), (name, "corner not pure black")
    colours = {c for _, c in img.getcolors(4096)}
    assert must_have in colours, (name, "expected colour missing", sorted(colours))
    print("ok", name, "colours:", len(colours))
PYEOF
```

Expected: `ok` for both files; the colour count stays small (single digits — rounded corners add a few blend pixels, which is acceptable; the overall look must remain hard-edged flat shapes).

- [ ] **Step 4: Commit**

```bash
git -C /home/dan/code/mutastic/.worktrees/opendeck-light-button add deck/icons/gen-light-icons.py deck/icons/mutastic-light-on.png deck/icons/mutastic-light-off.png
git -C /home/dan/code/mutastic/.worktrees/opendeck-light-button commit -m "feat(deck): light panel icons (PIL-generated, 144px pure black) + committed generator"
```

---

### Task 6: Manifest second action + guard-test rewrite

**Files:**
- Modify: `deck_manifest_test.go` (lines 45-76: single-action assertions + the function's final `}` -> lookup-by-UUID over 2 actions)
- Modify: `deck/com.danshapiro.mutastic.sdPlugin/manifest.json`

**Interfaces:**
- Consumes: icon PNGs from Task 5 (the test `os.Stat`s them)
- Produces: manifest action `com.danshapiro.mutastic.light` that OpenDeck reads at plugin registration; state order (0 = off, 1 = on) that Task 7's profile edit and Task 2's `stateLights*` constants both assume

- [ ] **Step 1: Rewrite the guard test (the RED step)**

In `deck_manifest_test.go`, replace everything from `if len(m.Actions) != 1 {` through the function's final `}` (currently lines 45-76: the single-action assertions, the icon-stat loop, AND the function's closing `}` on line 76) with the block below. The block's last line is a column-0 `}` that becomes the function's new final brace — the old line-76 `}` must not survive the edit, or the file gains a duplicate top-level brace and fails to compile:

```go
	wantActions := map[string]struct {
		name   string
		images []string
	}{
		"com.danshapiro.mutastic.mute":  {"Mutastic Mute", []string{"icons/mutastic-mic", "icons/mutastic-mic-muted"}},
		"com.danshapiro.mutastic.light": {"Mutastic Lights", []string{"icons/mutastic-light-off", "icons/mutastic-light-on"}},
	}
	if len(m.Actions) != len(wantActions) {
		t.Fatalf("Actions has %d entries, want %d (mute + lights)", len(m.Actions), len(wantActions))
	}
	for _, a := range m.Actions {
		want, ok := wantActions[a.UUID]
		if !ok {
			t.Errorf("unexpected action UUID %q", a.UUID)
			continue
		}
		delete(wantActions, a.UUID)
		if a.Name != want.name {
			t.Errorf("action %s Name = %q, want %q", a.UUID, a.Name, want.name)
		}
		if !a.DisableAutomaticStates {
			t.Errorf("action %s: DisableAutomaticStates must be true: the plugin alone drives the icon", a.UUID)
		}
		if len(a.States) != len(want.images) {
			t.Fatalf("action %s: States has %d entries, want %d", a.UUID, len(a.States), len(want.images))
		}
		for i, st := range a.States {
			if st.Image != want.images[i] {
				t.Errorf("action %s States[%d].Image = %q, want %q", a.UUID, i, st.Image, want.images[i])
			}
			if strings.Contains(st.Image, ".png") {
				t.Errorf("action %s States[%d].Image = %q must be extensionless", a.UUID, i, st.Image)
			}
		}
	}
	for uuid := range wantActions {
		t.Errorf("manifest is missing action %s", uuid)
	}
	// The PNGs deploy.cmd installs next to this manifest must exist.
	for _, p := range []string{
		"deck/icons/mutastic-mic.png", "deck/icons/mutastic-mic-muted.png",
		"deck/icons/mutastic-light-on.png", "deck/icons/mutastic-light-off.png",
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("icon missing: %s: %v", p, err)
		}
	}
}
```

(The lines above 45 — top-level field checks, `CodePathWin`, `OS` — stay exactly as they are.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./ -run TestDeckPluginManifest -v`
Expected: FAIL with `Actions has 1 entries, want 2 (mute + lights)`

- [ ] **Step 3: Add the second manifest action**

In `deck/com.danshapiro.mutastic.sdPlugin/manifest.json` (tab-indented — keep tabs), add a comma after the mute action's closing `}` and add this second entry inside the `"Actions"` array:

```json
		{
			"Name": "Mutastic Lights",
			"UUID": "com.danshapiro.mutastic.light",
			"Tooltip": "Toggle all NEEWER lights; icon tracks whether any light is on",
			"Icon": "icons/mutastic-light-on",
			"DisableAutomaticStates": true,
			"Controllers": ["Keypad"],
			"SupportedInMultiActions": false,
			"States": [
				{ "Image": "icons/mutastic-light-off", "Name": "Off", "ShowTitle": false },
				{ "Image": "icons/mutastic-light-on", "Name": "On", "ShowTitle": false }
			]
		}
```

(Image paths extensionless — OpenDeck's convert_icon appends `.png` itself. Do NOT add a top-level `UUID` or `PropertyInspectorPath`; their absence is intentional.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./ -run TestDeckPluginManifest -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git -C /home/dan/code/mutastic/.worktrees/opendeck-light-button add deck_manifest_test.go deck/com.danshapiro.mutastic.sdPlugin/manifest.json
git -C /home/dan/code/mutastic/.worktrees/opendeck-light-button commit -m "feat(deck): Mutastic Lights action in the plugin manifest; guard test looks actions up by UUID"
```

---

### Task 7: Profile wiring — `set-light-key.ps1` + `deploy.cmd`

**Files:**
- Create: `deploy/set-light-key.ps1`
- Modify: `deploy/deploy.cmd` (icon copies, script copy, script invocation — CRLF preserved)

**Interfaces:**
- Consumes: the icon filenames from Task 5 and action identity from Task 6
- Produces: `keys[2]`/`Keypad.2.0` profile wiring; transcript line `keys[2] set to Mutastic Lights (backup: ...)` (or `keys[2] already Mutastic Lights; no change`) that Task 9's E2E greps for; timestamped backup file `Default.json.bak-deckplugin-light-<yyyyMMdd-HHmmss>`

Design notes: a parallel script (not a generalization of `set-mute-key.ps1`) keeps the proven mute path untouched. The backup name MUST differ from the mute script's `.bak-deckplugin` AND be unique per run (timestamped): a fixed name lets a later edit-run clobber the earlier good snapshot (validated falsification — the live dir already shows `.bak-deckplugin` diverging from the OpenDeck-rewritten profile, i.e. a fixed-name backup degrades into the only-and-overwritable snapshot), so every run writes its own `.bak-deckplugin-light-<timestamp>` file. Deploy ordering is load-bearing: OpenDeck prunes placed profile keys whose action is missing from the registered plugin's manifest at profile load (fork `store/profiles.rs`), so deploy.cmd must keep installing the plugin dir (manifest+binary+icons) BEFORE the profile edit, restarting OpenDeck last — its current ordering already does this; never reorder. Profile image paths INCLUDE `.png` (opposite of the manifest). The instance-level `states` array is what OpenDeck renders; `action` is the manifest-derived snapshot. There is no programmatic "OpenDeck stopped" check — deploy.cmd's kill ordering enforces it; the fixture test below never touches the live profile so it is safe while OpenDeck runs.

- [ ] **Step 1: Write the script**

Create `deploy/set-light-key.ps1` (LF line endings, pure ASCII — no smart quotes or dashes):

```powershell
# set-light-key.ps1 -- point keys[2] (context Keypad.2.0, the top-right
# key) of the OpenDeck profile at the Mutastic Lights plugin action.
# Idempotent: if keys[2] is already the plugin action, exits without
# touching the file. Otherwise backs up the profile to
# <profile>.bak-deckplugin-light-<timestamp> first (unique per run: a
# fixed name would let a later edit-run clobber the earlier good
# snapshot; also distinct from set-mute-key.ps1's .bak-deckplugin).
# MUST run with OpenDeck STOPPED -- OpenDeck persists profiles on exit
# and would clobber this edit. Never touches keys[5] (Mutastic Mute) or
# any other key.
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

if ($json.keys[2] -and $json.keys[2].action.uuid -eq 'com.danshapiro.mutastic.light') {
    Write-Output "keys[2] already Mutastic Lights; no change"
    exit 0
}

$BackupPath = "$ProfilePath.bak-deckplugin-light-$(Get-Date -Format yyyyMMdd-HHmmss)"
Copy-Item -LiteralPath $ProfilePath -Destination $BackupPath

function New-LightState([string]$image, [string]$name) {
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
$off = New-LightState 'plugins/com.danshapiro.mutastic.sdPlugin/icons/mutastic-light-off.png' 'Off'
$on  = New-LightState 'plugins/com.danshapiro.mutastic.sdPlugin/icons/mutastic-light-on.png' 'On'

$json.keys[2] = [ordered]@{
    action = [ordered]@{
        controllers = @('Keypad')
        disable_automatic_states = $true   # the plugin alone drives state
        encoder = $null
        icon = 'plugins/com.danshapiro.mutastic.sdPlugin/icons/mutastic-light-on.png'
        name = 'Mutastic Lights'
        plugin = 'com.danshapiro.mutastic.sdPlugin'
        property_inspector = ''
        states = @($off, $on)
        supported_in_multi_actions = $false
        tooltip = 'Toggle all NEEWER lights; icon tracks whether any light is on'
        uuid = 'com.danshapiro.mutastic.light'
        visible_in_action_list = $true
    }
    children = $null
    context = 'Keypad.2.0'
    current_state = 0
    settings = [ordered]@{}
    states = @($off, $on)
}

# ASCII, not UTF8: Windows PowerShell 5.1's UTF8 writes a BOM, which
# serde_json (OpenDeck's parser) rejects. ASCII is only safe while the
# content IS ASCII (-Encoding ASCII silently mangles non-ASCII to '?'),
# so guard and fail loudly instead of corrupting silently.
$out = $json | ConvertTo-Json -Depth 12
if ($out -match '[^\x00-\x7F]') { throw 'profile serialization contains non-ASCII; refusing -Encoding ASCII write' }
# -Depth 12 covers today's profile (measured depth 6), but ConvertTo-Json
# silently stringifies anything past the cutoff -- guard the symptom.
if ($out -match '"@\{') { throw 'profile serialization hit -Depth truncation (nested object stringified); refusing write' }
$out | Set-Content -LiteralPath $ProfilePath -Encoding ASCII
Write-Output "keys[2] set to Mutastic Lights (backup: $BackupPath)"
```

- [ ] **Step 2: Fixture test — set up (never the live profile)**

```bash
PROFILE=/mnt/c/Users/dan/AppData/Roaming/opendeck/profiles/sd-A00DA6141I07PW/Default.json
FIX=/mnt/c/Users/dan/AppData/Local/Temp/lightkey-fixture.json
cp "$PROFILE" "$FIX"
python3 - <<'PYEOF' > /tmp/keys45-before.json
import json
p = json.load(open("/mnt/c/Users/dan/AppData/Local/Temp/lightkey-fixture.json"))
print(json.dumps([p["keys"][4], p["keys"][5]], sort_keys=True))
PYEOF
wc -c /tmp/keys45-before.json
```

Expected: fixture copied; `/tmp/keys45-before.json` non-empty (snapshot of keys[4] and keys[5] for the untouched-neighbors assertion).

- [ ] **Step 3: Run the script against the fixture (interop, with retries)**

Define the retry wrapper once per shell session and use it for EVERY interop call in this task and Task 9:

```bash
retry_interop() {
  local i
  for i in 1 2 3; do
    "$@" && return 0
    echo "interop attempt $i failed; waiting 45s" >&2
    [ "$i" -lt 3 ] && sleep 45
  done
  echo "interop failed after 3 attempts: $*" >&2
  return 1
}
retry_interop powershell.exe -NoProfile -ExecutionPolicy Bypass -File '\\wsl.localhost\Ubuntu\home\dan\code\mutastic\.worktrees\opendeck-light-button\deploy\set-light-key.ps1' -ProfilePath 'C:\Users\dan\AppData\Local\Temp\lightkey-fixture.json'
```

Expected output: `keys[2] set to Mutastic Lights (backup: C:\Users\dan\AppData\Local\Temp\lightkey-fixture.json.bak-deckplugin-light-<timestamp>)` (timestamp = this run's `yyyyMMdd-HHmmss`)

- [ ] **Step 4: Assert the fixture result (filesystem evidence, no interop)**

```bash
python3 - <<'PYEOF'
import json
fx = "/mnt/c/Users/dan/AppData/Local/Temp/lightkey-fixture.json"
raw = open(fx, "rb").read()
assert raw[:1] == b"{", "BOM or junk at file start: %r" % raw[:3]
p = json.loads(raw)
k2 = p["keys"][2]
assert k2["action"]["uuid"] == "com.danshapiro.mutastic.light", k2["action"]["uuid"]
assert k2["action"]["plugin"] == "com.danshapiro.mutastic.sdPlugin"
assert k2["action"]["disable_automatic_states"] is True
assert k2["context"] == "Keypad.2.0", k2["context"]
assert k2["current_state"] == 0
imgs = [s["image"] for s in k2["states"]]
assert imgs == [
    "plugins/com.danshapiro.mutastic.sdPlugin/icons/mutastic-light-off.png",
    "plugins/com.danshapiro.mutastic.sdPlugin/icons/mutastic-light-on.png",
], imgs
before = json.load(open("/tmp/keys45-before.json"))
assert json.dumps([p["keys"][4], p["keys"][5]], sort_keys=True) == json.dumps(before, sort_keys=True), "keys[4]/keys[5] were disturbed"
print("fixture assertions ok")
PYEOF
```

Expected: `fixture assertions ok`

- [ ] **Step 5: Idempotency re-run + cleanup**

```bash
retry_interop powershell.exe -NoProfile -ExecutionPolicy Bypass -File '\\wsl.localhost\Ubuntu\home\dan\code\mutastic\.worktrees\opendeck-light-button\deploy\set-light-key.ps1' -ProfilePath 'C:\Users\dan\AppData\Local\Temp\lightkey-fixture.json'
rm -f /mnt/c/Users/dan/AppData/Local/Temp/lightkey-fixture.json /mnt/c/Users/dan/AppData/Local/Temp/lightkey-fixture.json.bak-deckplugin-light-* /tmp/keys45-before.json
```

Expected: second run prints `keys[2] already Mutastic Lights; no change`; fixtures removed.

- [ ] **Step 6: Wire deploy.cmd**

`deploy/deploy.cmd` is CRLF — make these edits with an editor that preserves line endings (verify afterwards). Three edits:

(a) In the `== Installing OpenDeck plugin ==` block, immediately after the existing line
`copy /Y "%SRC%\deck\icons\mutastic-mic-muted.png" "%ODPLUGDIR%\icons\mutastic-mic-muted.png" >nul || goto :fail`
add:

```bat
copy /Y "%SRC%\deck\icons\mutastic-light-on.png" "%ODPLUGDIR%\icons\mutastic-light-on.png" >nul || goto :fail
copy /Y "%SRC%\deck\icons\mutastic-light-off.png" "%ODPLUGDIR%\icons\mutastic-light-off.png" >nul || goto :fail
```

(b) Immediately after the existing line
`copy /Y "%SRC%\deploy\set-mute-key.ps1" "%DEST%\set-mute-key.ps1" >nul || goto :fail`
add:

```bat
copy /Y "%SRC%\deploy\set-light-key.ps1" "%DEST%\set-light-key.ps1" >nul || goto :fail
```

(c) Replace the two profile-edit lines

```bat
echo == Pointing profile keys[5] at the plugin ==
powershell -NoProfile -ExecutionPolicy Bypass -File "%DEST%\set-mute-key.ps1" || goto :fail
```

with

```bat
echo == Pointing profile keys[5]+keys[2] at the plugin ==
powershell -NoProfile -ExecutionPolicy Bypass -File "%DEST%\set-mute-key.ps1" || goto :fail
powershell -NoProfile -ExecutionPolicy Bypass -File "%DEST%\set-light-key.ps1" || goto :fail
```

- [ ] **Step 7: Verify line endings and commit**

```bash
file deploy/deploy.cmd deploy/set-light-key.ps1
```

Expected: `deploy.cmd` reports `CRLF line terminators`; `set-light-key.ps1` reports ASCII text with no CRLF mention (LF, matching `set-mute-key.ps1`).

```bash
git -C /home/dan/code/mutastic/.worktrees/opendeck-light-button add deploy/set-light-key.ps1 deploy/deploy.cmd
git -C /home/dan/code/mutastic/.worktrees/opendeck-light-button commit -m "feat(deploy): profile editor points keys[2] at Mutastic Lights; deploy ships light icons + runs it"
```

---

### Task 8: README — document the second action

**Files:**
- Modify: `README.md` (the `### Stream Deck (OpenDeck plugin)` section, lines 100-123)

**Interfaces:**
- Consumes: everything above (documents shipped behavior only)
- Produces: user-facing docs; nothing downstream

- [ ] **Step 1: Update the intro sentence**

Replace (the first sentence of the section's intro paragraph, lines 102-104):

```markdown
The deck's lower-right key is a native OpenDeck plugin action, **Mutastic
Mute** (`com.danshapiro.mutastic.mute`), served by the plugin mode built
into `mutastic.exe` itself.
```

with:

```markdown
Two deck keys are native OpenDeck plugin actions served by the plugin
mode built into `mutastic.exe` itself: the lower-right key is **Mutastic
Mute** (`com.danshapiro.mutastic.mute`) and the top-right key is
**Mutastic Lights** (`com.danshapiro.mutastic.light`).
```

(The rest of the intro paragraph — install path, launch args — stays.)

- [ ] **Step 2: Replace the bullet list**

Replace the current three bullets (lines 110-118) with:

```markdown
- **Mute press** = the full mute-everything flow, in-process: `toggle` to
  the daemon over UDP 42814 plus one SendInput F24 for the meeting-app
  sweep (no cmd/AHK hop; both halves run even if the other fails).
- **Mute icon** = the TRUE mic state. The plugin polls the daemon's
  `status` every 750ms and drives the icon via `setState`, so physical
  mic-button presses, the pedal, and the CLI all show up on the deck.
  `unknown` (fresh daemon) keeps the last icon.
- **Lights press** = `light toggle` to the daemon over UDP 42814: if ANY
  light is on, ALL turn off; otherwise ALL turn on, each restoring its
  own last look (the same collective semantics as the pedal). No F24.
- **Lights icon** = whether ANY connected light is on. Polled with
  `light status` on the same 750ms tick (one extra UDP round trip, not a
  second timer). All-unknown or an unreachable daemon keeps the last
  icon. Newly plugged-in lights (more PL81 PROs are on order) are picked
  up automatically by the daemon's hot-plug rescan, so the button
  controls the whole fleet with zero reconfiguration.
- **Log:** `%LOCALAPPDATA%\mutastic\deckplugin.log` (every `setState` is
  logged).
```

- [ ] **Step 3: Update the closing deploy paragraph**

Replace (lines 120-123):

```markdown
`deploy\deploy.cmd` installs the plugin directory, points the profile's
`keys[5]` at the action (backup kept at `Default.json.bak-deckplugin`),
and restarts OpenDeck. `deploy\mute-everything.cmd` remains as a CLI
entry point but the deck no longer uses it.
```

with:

```markdown
`deploy\deploy.cmd` installs the plugin directory, points the profile's
`keys[5]` at the mute action and `keys[2]` at the lights action (backups
kept at `Default.json.bak-deckplugin` and timestamped
`Default.json.bak-deckplugin-light-<timestamp>` files), and restarts
OpenDeck.
`deploy\mute-everything.cmd` remains as a CLI entry point but the deck no
longer uses it.
```

- [ ] **Step 4: Proofread + commit**

```bash
grep -n 'mutastic.light\|Lights' README.md
```

Expected: hits ONLY inside the `### Stream Deck (OpenDeck plugin)` section; all Windows paths single-backslash.

```bash
git -C /home/dan/code/mutastic/.worktrees/opendeck-light-button add README.md
git -C /home/dan/code/mutastic/.worktrees/opendeck-light-button commit -m "docs: README documents the Mutastic Lights deck key (collective toggle, hot-plug fleet)"
```

---

### Task 9: Full gate, build, deploy, live E2E, restore

**Files:**
- No source changes (verification only). Evidence files under `/tmp/`.

**Interfaces:**
- Consumes: everything above; the `retry_interop` wrapper defined in Task 7 Step 3 (redefine it in this task's shell)
- Produces: a deployed, verified live plugin; recorded pre/post light state; the final human-verification questions

WSL interop is flaky today (vsock errors recovering in ~30-60s): EVERY `*.exe` invocation below goes through `retry_interop` (3 attempts, 45s waits). Filesystem reads via `/mnt/c` are the preferred evidence channel and always work. Trust the deploy transcript, not cmd.exe's exit code.

- [ ] **Step 1: Full local gate**

```bash
cd /home/dan/code/mutastic/.worktrees/opendeck-light-button
go test -race ./... && go vet ./...
GOOS=windows GOARCH=amd64 CGO_ENABLED=1 CC=x86_64-w64-mingw32-gcc go vet .
./build.sh
```

Expected: all packages `ok`, vet silent, `built bin/mutastic.exe`.

- [ ] **Step 2: Record pre-test state (light + mic)**

```bash
retry_interop /mnt/c/Users/dan/code/mutastic-deploy/mutastic.exe light status | tee /tmp/light-pre.txt
retry_interop /mnt/c/Users/dan/code/mutastic-deploy/mutastic.exe status | tee /tmp/mic-pre.txt
```

Expected: `/tmp/light-pre.txt` holds one line per light, e.g. `COM4 desk-right: on 30% 2900K` (record whatever it actually says — Step 7 restores it); `/tmp/mic-pre.txt` holds `muted`/`unmuted`/`unknown`.

- [ ] **Step 3: Deploy (NEW log filename to dodge lingering-console file locks)**

```bash
DEPLOYLOG=/tmp/deploy-light-$(date +%s).log
timeout 120 cmd.exe /c '\\wsl.localhost\Ubuntu\home\dan\code\mutastic\.worktrees\opendeck-light-button\deploy\deploy.cmd' '\\wsl.localhost\Ubuntu\home\dan\code\mutastic\.worktrees\opendeck-light-button' > "$DEPLOYLOG" 2>&1 || true
cat "$DEPLOYLOG"
```

Success = the transcript contains ALL of: `== Installing OpenDeck plugin ==`; `keys[5] set to Mutastic Mute` OR `keys[5] already Mutastic Mute; no change`; `keys[2] set to Mutastic Lights` OR `keys[2] already Mutastic Lights; no change`; `Deploy complete.` (single-quoted UNC paths are mandatory; the exit code is unreliable because the started daemon inherits the console handle). If the transcript is empty or shows a vsock/exec-format interop failure, wait 45s and re-run with a fresh `$DEPLOYLOG` filename, up to 3 total attempts, before declaring a blocker.

- [ ] **Step 4: Verify registration + both willAppear events (log evidence)**

```bash
sleep 20
LOG=/mnt/c/Users/dan/AppData/Local/mutastic/deckplugin.log
grep -c 'Registered plugin com.danshapiro.mutastic.sdPlugin' /mnt/c/Users/dan/AppData/Local/OpenDeck/logs/OpenDeck.log || true
grep -n 'willAppear com.danshapiro.mutastic.mute sd-A00DA6141I07PW.Default.Keypad.5.0' "$LOG" | tail -3
grep -n 'willAppear com.danshapiro.mutastic.light sd-A00DA6141I07PW.Default.Keypad.2.0' "$LOG" | tail -3
```

Expected: registration count >= 1; at least one willAppear line for EACH context (deckplugin.log rotates at plugin start, so these lines are from this deploy — sanity-check the timestamps).

- [ ] **Step 5: Drive the lights via CLI and watch setState (delta-counted, mute untouched)**

```bash
CTX2='sd-A00DA6141I07PW.Default.Keypad.2.0'
CTX5='sd-A00DA6141I07PW.Default.Keypad.5.0'
retry_interop /mnt/c/Users/dan/code/mutastic-deploy/mutastic.exe light off && sleep 3   # normalize: fleet off
on_before=$(grep -c "setState $CTX2 -> 1" "$LOG" || true)
off_before=$(grep -c "setState $CTX2 -> 0" "$LOG" || true)
mute_before=$(grep -c "setState $CTX5" "$LOG" || true)

retry_interop /mnt/c/Users/dan/code/mutastic-deploy/mutastic.exe light on && sleep 3
on_after=$(grep -c "setState $CTX2 -> 1" "$LOG" || true)
echo "setState->1 delta: $((on_after - on_before))"   # MUST be >= 1

retry_interop /mnt/c/Users/dan/code/mutastic-deploy/mutastic.exe light off && sleep 3
off_after=$(grep -c "setState $CTX2 -> 0" "$LOG" || true)
echo "setState->0 delta: $((off_after - off_before))"  # MUST be >= 1

mute_after=$(grep -c "setState $CTX5" "$LOG" || true)
echo "mute setState delta: $((mute_after - mute_before))"  # MUST be 0
```

Expected: `->1` delta >= 1 after `light on`; `->0` delta >= 1 after `light off`; mute delta exactly 0 (the mute instance stays untouched while lights are driven).

- [ ] **Step 6: Verify the mic command path still works, and leave the mic unmuted**

```bash
retry_interop /mnt/c/Users/dan/code/mutastic-deploy/mutastic.exe status   # informational: normally `unknown` right after Step 3's restart
retry_interop /mnt/c/Users/dan/code/mutastic-deploy/mutastic.exe unmute
retry_interop /mnt/c/Users/dan/code/mutastic-deploy/mutastic.exe status   # the gate
```

Expected: the FINAL `status` prints `unmuted` — that line is the pass/fail gate. The first `status` is informational only: mic mute state lives solely in daemon memory (nothing persists it across restarts), and Step 3's deploy unconditionally killed and restarted `mutastic.exe`, so it normally prints `unknown` (`muted`/`unmuted` are also possible if the mic was physically toggled since the restart — record whatever it says). The unconditional `unmute` re-establishes a known state through the mic command path, doubling as a regression check that the lights work didn't break mic routing.

- [ ] **Step 7: Restore the light's pre-test state**

Read `/tmp/light-pre.txt`. For the `desk-right` line:
- If it was `on <N>% <K>K` (last known: `on 30% 2900K`):

```bash
retry_interop /mnt/c/Users/dan/code/mutastic-deploy/mutastic.exe light@desk-right brightness <N>
retry_interop /mnt/c/Users/dan/code/mutastic-deploy/mutastic.exe light@desk-right temp <K>
```

- If it was `off`: `retry_interop /mnt/c/Users/dan/code/mutastic-deploy/mutastic.exe light@desk-right off`
- If it was `unknown`: leave it as the final `light off` left it and note that in the task summary (an unknown pre-state cannot be reproduced — the hardware has no query command).

Then verify:

```bash
retry_interop /mnt/c/Users/dan/code/mutastic-deploy/mutastic.exe light status | tee /tmp/light-post.txt
diff /tmp/light-pre.txt /tmp/light-post.txt && echo "light state restored"
```

Expected: `light state restored` (byte-identical status; Kelvin values are quantized hardware steps, so setting the recorded `<K>` reproduces the recorded reading exactly).

- [ ] **Step 8: Record the human-only checks**

No commit in this task. Record these two questions as the final human-verification items (they cannot be automated):

1. On the physical deck, does the top-right key show the light-panel icons correctly (OFF = dim gray outline, ON = bright warm panel with rays), visually consistent with the mic icons next to it?
2. Does a physical press of the top-right key toggle all connected lights (any-on -> all-off, otherwise all-on) with the icon tracking, while the lower-right mute key keeps working exactly as before?

---

## Verification Summary

- `go test -race ./...` + `go vet ./...` + cross-compile vet: clean (Task 9 Step 1)
- Unit coverage: reply parsing -> state decision for on/off/mixed/disconnected/unknown/unreachable (Task 1); per-action routing mute-vs-light keyDown, poll gating, setState dedupe, cross-contamination, unknown action (Task 2); per-verb timeout (Task 3); daemon log latch (Task 4); manifest contract (Task 6)
- Fixture-tested profile edit; the live profile is only ever edited by deploy.cmd with OpenDeck stopped (Task 7)
- Live E2E: deploy transcript markers, both willAppear contexts, CLI-driven `setState -> 1` / `-> 0` deltas for `Keypad.2.0` with zero mute-key setState churn, light state restored, mic re-established `unmuted` via the mic command path after the deploy restart (Task 9)
- Human-only: physical icon appearance and a real deck keypress (Task 9 Step 8)
