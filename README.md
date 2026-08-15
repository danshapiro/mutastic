# mutastic

One press mutes everything — the mic's own mute button or the Stream Deck
mute key toggles the meeting apps AND the microphone itself.

The iKKEGOL USB triple foot pedal remains firmware-programmed to emit `F13`,
`F14`, and `F15`; its firmware is not being changed. The left and center
handlers in `ahk/MuteAllMeetings.ahk` are deliberately disabled and consumed
by wildcard no-ops:

- **Left pedal (`F13`)** — disabled 2026-08-12; the consumed no-op prevents
  it from falling through to the foreground application.
- **Center pedal (`F14`)** — disabled 2026-08-09 because of accidental
  presses; its consumed no-op also prevents fall-through.

The right pedal (`F15`) remains Winpepper's push-to-talk hold key. Light
control remains available through the browser UI, the Stream Deck Lights
action, and `mutastic light ...` commands. The active mute paths remain:

1. **Physical Yeti X mute button** — the `mutastic` daemon observes the
   hardware event and injects `F24`; `ahk/MuteAllMeetings.ahk` handles `F24`
   by sweeping every running meeting app.
2. **Stream Deck Mutastic Mute key** — the OpenDeck plugin toggles the Yeti X
   through the daemon and injects the same `F24` app sweep.

Pressing the **mute button on the Yeti X itself** keeps the meeting apps
in sync: the daemon sees the mic's `0x21` DeviceMute event (emitted only for
physical presses — host-initiated commands echo `0x20` instead) and injects a
synthetic `F24` keystroke. The AHK script's active `*F24::` hotkey (the `*`
lets it fire even while modifier keys are held) runs the meeting-app sweep but
does NOT run `mutastic toggle`, because the mic already changed its own
hardware state. The active paths are loop-free:

- **Mic button:** the firmware toggles the mic and emits `0x21` → the daemon
  injects `F24` (debounced, 400 ms) → AHK sweeps the apps only → no further
  device command is sent.
- **Stream Deck mute:** the plugin sends `toggle` to the daemon and injects one
  `F24` app sweep → the mic's host-command echo is `0x20` (ignored) and the
  AHK `F24` path does not call `mutastic toggle` → nothing re-triggers.

## Components

- **`mutastic daemon`** — owns the Yeti X HID connection (VID 046D, vendor
  collection with `Usage == 1`), performs the init handshake, tracks mute
  state from the mic's events, and serves plain-text commands on UDP
  `127.0.0.1:42814`. Reconnects automatically if the mic disappears.
  On a physical mute-button press (`0x21` DeviceMute event), it injects a
  synthetic `F24` keystroke via `SendInput` so the AHK script sweeps the
  meeting apps; injections are debounced (400 ms) and logged as
  `mic button -> F24 app sweep`.
  Also owns every attached NEEWER PL81 PRO light (CH340 serial, VID 1A86
  PID 7523, 115200 8N1): a rescan every 5 s discovers newly plugged-in
  lights and tears down removed ones (no restart needed), with one
  independent reconnect loop per light, tracking each light's true state
  from its echo/broadcast frames and persisting each last look to
  `%LOCALAPPDATA%\mutastic\light-state-<COMx>.json`.
- **One-shot client** — `mutastic toggle | mute | unmute | status | shutdown`
  sends one command to the daemon and prints the reply. Mic replies are
  `muted`, `unmuted`, or `unknown`; `shutdown` replies `shutting down` (then
  the daemon process stops — the tray icon's Quit uses it). Errors are
  `error: <reason>`. Exit codes: `0` = non-error reply, `1` = `error:`
  reply, `2` = no daemon reachable / bad usage.
- **`mutastic ui`** — serves the loopback-only light controller at
  `http://127.0.0.1:42815/`. Plain `mutastic ui` opens or focuses the panel in
  the browser, reusing an already-running server; `mutastic ui --no-open`
  starts or reuses the server without opening a browser and is the login mode.
  The panel server also answers `POST /api/shutdown` (loopback, same
  origin/CSRF posture as the panel's mutating endpoints): it replies, then
  gracefully stops — the tray's Quit uses it.
- **`mutastic tray`** — resident Windows notification-area icon, a pure UDP
  client of the daemon (like the deck plugin): it owns no hardware, so
  quitting or crashing never drops the mic. The icon mirrors the true mic
  state (white mic = live, red muted, polled every 2 s; it starts as a
  neutral gray unknown icon, and an unknown or unreachable daemon keeps the
  last definitive icon — it never repaints a muted mic as live, the same
  keep-last-icon convention as the Stream Deck plugin). Left-click
  opens/focuses the light panel; right-click shows the menu: a synced
  **Muted** check item (mute-everything — mic toggle plus the F24
  meeting-app sweep, the same in-process flow as the Stream Deck mute key;
  both halves are attempted on every click and any failure is logged),
  **Toggle lights**, **Brightness** (applied in click order),
  **Light preset**, **Light panel…**, and **Quit** — Quit stops everything
  mutastic runs in one click: it sends the daemon's `shutdown` command,
  posts the light-panel server's `/api/shutdown`, and exits the tray.
  If a live daemon or panel *refuses* to stop, Quit logs the failure and
  the tray stays up — click Quit again to retry. (A port that actively
  refuses connections counts as already stopped; a hang or silence keeps
  the tray up so nothing live gets silently left behind.) With the
  daemon unreachable the action items gray out. Only one tray instance runs
  (loopback TCP 42816 is the single-instance lock, the same trick as the
  daemon's UDP bind). The tray logs JSONL with levels to
  `%LOCALAPPDATA%\mutastic\tray.log`.
- **Light commands** — every attached PL81 PRO is discovered automatically.
  Bare `mutastic light <cmd>` acts on ALL lights, one reply line per light
  (`COM4 desk: on 30% 2900K`); `mutastic light@<name|COMx> <cmd>` targets
  one light and replies bare (`on 30% 2900K`).

  | Command | Effect |
  |---|---|
  | `mutastic light toggle` | if ANY light is on, ALL turn off; otherwise ALL turn on, each restoring its own last look (the same collective semantics used by the Stream Deck Lights action) |
  | `mutastic light on \| off \| status` | power / status, all lights |
  | `mutastic light brightness <0-100>` | set brightness, all lights |
  | `mutastic light temp <2900-7000>` | set color temperature, all lights |
  | `mutastic light brightness-delta <-20..20>` | adjust every connected, known, on light by a relative brightness delta atomically |
  | `mutastic light temp-step-delta <-3..3>` | adjust every connected, known, on light by relative hardware temperature steps atomically |
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
- **`ahk/MuteAllMeetings.ahk`** — actively consumes F13 and F14 with wildcard
  no-op handlers, disabled on 2026-08-12 and 2026-08-09 respectively, so
  neither pedal key falls through to the foreground application. F15 is not
  bound here; Winpepper owns it as push-to-talk. Light control remains in the
  browser UI, Stream Deck, and `mutastic light ...` commands. The active
  `*F24` handler — used by the physical Yeti button and the Stream Deck mute
  action — runs the meeting-app sweep alone, with no `mutastic.exe` call, so
  nothing loops back.

### Stream Deck (OpenDeck plugin)

Two deck keys are native OpenDeck plugin actions served by the plugin
mode built into `mutastic.exe` itself: the lower-right key is **Mutastic
Mute** (`com.danshapiro.mutastic.mute`) and the top-right key is
**Mutastic Lights** (`com.danshapiro.mutastic.light`). OpenDeck launches the copy installed at
`%APPDATA%\opendeck\plugins\com.danshapiro.mutastic.sdPlugin\mutastic.exe`
with Elgato-style args (`-port N -pluginUUID ... -registerEvent ... -info ...`);
the binary auto-detects the leading `-port` flag as plugin mode
(`mutastic deckplugin -port ...` works for manual launches).

- **Mute press** = the full mute-everything flow, in-process: `toggle` to
  the daemon over UDP 42814 plus one SendInput F24 for the meeting-app
  sweep (no cmd/AHK hop; both halves run even if the other fails).
- **Mute icon** = the TRUE mic state. The plugin polls the daemon's
  `status` every 750ms and drives the icon via `setState`, so physical
  mic-button presses, Stream Deck mute actions, and CLI state changes are
  reflected on the deck. `unknown` (fresh daemon) keeps the last icon.
- **Lights press** = `light toggle` to the daemon over UDP 42814: if ANY
  light is on, ALL turn off; otherwise ALL turn on, each restoring its
  own last look (the same collective semantics as the CLI command). No F24.
- **Lights icon** = whether ANY connected light is on. Polled with
  `light status` on the same 750ms tick (one extra UDP round trip, not a
  second timer). All-unknown or an unreachable daemon keeps the last
  icon. Newly plugged-in lights (more PL81 PROs are on order) are picked
  up automatically by the daemon's hot-plug rescan, so the button
  controls the whole fleet with zero reconfiguration.
- **Log:** `%LOCALAPPDATA%\mutastic\deckplugin.log` (every `setState` is
  logged).

`deploy\deploy.cmd` installs the plugin directory, points the profile's
`keys[5]` at the mute action and `keys[2]` at the lights action (backups
kept at `Default.json.bak-deckplugin` and timestamped
`Default.json.bak-deckplugin-light-<timestamp>` files), and restarts
OpenDeck.
`deploy\mute-everything.cmd` remains as a CLI entry point but the deck no
longer uses it.

## Startup (Windows login)

The user Startup shortcut `Mutastic Daemon.lnk` runs the deployed
`C:\Users\dan\code\mutastic-deploy\mutastic-daemon.vbs` through
`wscript.exe`. The launcher starts `mutastic.exe daemon`, then
`mutastic.exe ui --no-open`, then `mutastic.exe tray`, all hidden and
asynchronously. Login therefore starts the hardware daemon, the
light-controller server, and the tray icon but **does not open
Chrome or any other browser**. `MuteAllMeetings.lnk` separately starts the
AutoHotkey app-sweep script.

After login, plain `mutastic ui` opens or focuses the already-running panel at
`http://127.0.0.1:42815/`. Duplicate launches are harmless: a second daemon
cannot claim UDP port 42814, while a second UI command probes and reuses the
existing server.

To disable Mutastic startup intentionally, turn off **Mutastic Daemon** in
Windows Settings → Apps → Startup (or Task Manager's Startup apps), or delete
`Mutastic Daemon.lnk` from `shell:startup`. A later `deploy\deploy.cmd` run
intentionally recreates and re-enables this owned Startup entry, so disable it
again after deployment if that remains desired.

## Build (from WSL)

```bash
./build.sh
```

Cross-compiles `bin/mutastic.exe` for windows/amd64 (cgo via
`x86_64-w64-mingw32-gcc`). The binary is not committed — build before
deploying.

## Deploy (on Windows)

Build first, then run `deploy\deploy.cmd` (e.g. from Explorer or cmd.exe via
the checkout's `\\wsl.localhost\...` UNC path). The source defaults to the
checkout the script lives in; an optional first argument overrides it.

The script:

- stops any running `mutastic.exe` and the MuteAllMeetings AutoHotkey process
  (other AHK scripts are left alone),
- copies `mutastic.exe`, `MuteAllMeetings.ahk`, and the unified hidden VBS
  launcher to `C:\Users\dan\code\mutastic-deploy\` (plus the tray icon if
  it can find one),
- creates/updates two Startup shortcuts — `MuteAllMeetings.lnk` (AutoHotkey
  v1 running the deployed script) and `Mutastic Daemon.lnk` (`wscript.exe`
  running the deployed VBS, which starts the daemon, `ui --no-open`, and the
  tray icon),
- removes and then verifies the absence of the filename-keyed
  `StartupApproved\StartupFolder` value for `Mutastic Daemon.lnk`; deployment
  intentionally re-enables Mutastic autostart even if Windows previously
  disabled that entry,
- relaunches the daemon, UI server, tray icon, AHK script, and OpenDeck. The
  UI relaunch uses `--no-open`, so deployment does not force-open a browser.

> **Deploying from WSL:** run `deploy.cmd` via `cmd.exe` with output
> redirected to a file — the `start` of the hidden launcher inherits the interop
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

## Troubleshooting

- **Log:** `%LOCALAPPDATA%\mutastic\mutastic.log` — daemon startup, HID
  collection enumeration, every command and device event, reconnect activity.
- **Nothing starts after Windows login:** check `Mutastic Daemon.lnk` in
  `shell:startup` and the **Mutastic Daemon** entry in Windows Startup apps.
  Running `deploy\deploy.cmd` recreates the shortcut, clears its exact
  `StartupApproved` disable record, verifies that record is absent, and
  intentionally re-enables login startup.
- **No browser opened after login:** expected. Startup uses `ui --no-open` so
  only the loopback server starts. Run plain `mutastic ui` to open or focus the
  controller at `http://127.0.0.1:42815/`.
- **Need startup intentionally disabled:** disable **Mutastic Daemon** in
  Windows Startup apps (or remove its shortcut from `shell:startup`) after the
  last deployment; every later deploy intentionally re-enables it.
- **`status` says `unknown`:** normal right after daemon start; the state is
  known after the first mute command or device event.
- **Second daemon exits immediately:** UDP port 42814 doubles as the
  single-instance lock — the running daemon owns it.
- **Second tray exits immediately:** same idea — loopback TCP 42816 is the
  tray's single-instance lock. Remember the tray's **Quit also stops the
  daemon**; to bring everything back (daemon, panel server, icon), rerun
  the `Mutastic Daemon` startup shortcut.
- **Tray icon missing after login:** the tray logs JSONL to
  `%LOCALAPPDATA%\mutastic\tray.log`, including the systray library's own
  error lines (its default logger is redirected there). If Windows never
  installs the icon, a startup watchdog exits the tray after a startup
  window so the single-instance lock is released and rerunning the
  `Mutastic Daemon` startup shortcut actually retries. If the tray stays
  "ready" but iconless, look for repeated `systray error:` lines every
  ~10 minutes: icon updates keep retrying on that heartbeat, so a
  repeating error means a permanent refusal.
- **Tray icon only in the taskbar corner overflow:** Windows 11 parks new
  notification-area icons under the overflow chevron by design; drag the
  mutastic icon onto the corner once (or Settings → Personalization →
  Taskbar → Other system tray icons → mutastic → On).
- **Mic unplugged/replugged:** the daemon logs the session ending and
  reopens the device automatically.
- **Light unplugged/replugged:** same as the mic — the daemon logs
  `COM4 light: session ended` (prefixed per light) and reopens that port automatically.
- **`light ...` says `error: no light`:** the CH340 port wasn't found or
  couldn't be opened. The COM port is exclusive — close NEEWER Control
  Center (or anything else holding the port). New per-light diagnostics:
  `light: rescan: ports now [COM4]`, `light COM4: starting session`, and
  per-light session lines like `COM4 light: port opened`. Clean teardown on
  unplug is logged as `COM4 light: session ended`.
- **Light state file:** `%LOCALAPPDATA%\mutastic\light-state-<COMx>.json`
  holds each light's restore-on-`on` look per COM port; deleting it just
  resets the defaults (100% / 5000 K). The old single-light
  `light-state.json` is auto-migrated on first multi-light startup with
  exactly one light attached.
- **Mic button mutes the mic but the meeting apps don't follow:** check
  the log right after the press's `event op=0x21 ...` line. No line at
  all → the daemon didn't see the event. `mic button ignored (debounce)`
  → the 400 ms debounce suppressed a double-fire.
  `mic button -> F24 app sweep` present but the apps didn't toggle →
  either the AHK script isn't running (`SendInput` succeeds regardless;
  relaunch it via its Startup shortcut), or an **elevated (admin) window
  was focused** → UIPI
  silently discards injected keystrokes with no error anywhere (OS
  design); refocus a normal window and press again.
  `mutastic.exe daemon --test-inject` fires one synthetic F24 to exercise
  the injection path without touching the mic.
- The daemon auto-adopts EVERY VID 1A86 / PID 7523 (CH340) serial device
  as a light and writes control frames to it. Do not leave non-light
  CH340 devices (Arduino clones, USB-serial dongles) attached while the
  daemon runs.
- If a newly plugged panel NEVER appears in `light list`, check Device
  Manager: newer panels could carry a CH343 bridge (VID 1A86 PID 55D3),
  which the installed CH340 INF does not bind - no COM port appears at
  all. Supporting one would need a driver install plus a code change
  (new PID).
- PL81 PRO panels are USB BUS-POWERED - 5 V / 2 A input, 5 W, no
  battery; the single Type-C port carries BOTH power and PC control. An
  under-powered port makes the panel auto-limit its brightness range
  (documented device behavior), and a port power reset drops its COM
  port - which shows up as port-gone/rescan churn in the log. Three
  panels can draw up to ~3 A total at 5 V: prefer directly-attached or
  self-powered hub ports (the current light sits behind a two-tier hub
  chain).

**Warning:** never `usbipd attach` the Yeti X to WSL — it steals the system
microphone from Windows. Always run the built exe on the Windows side.

See `docs/yeti-x-hid-protocol.md` for the mic's reverse-engineered HID
protocol, `docs/pl81-pro-serial-protocol.md` for the light's serial
protocol, and `docs/pedal-and-mute.md` for the machine setup (pedal
firmware mapping, deployment, install state).

Runs on Windows; developed from WSL2 (cross-compiled Go, deployed to the
Windows side). Private repo; this tooling is specific to Dan's desk setup.
