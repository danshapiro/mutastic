# mutastic

One pedal press mutes everything: meeting apps AND the microphone itself.

The middle pedal of an iKKEGOL USB triple foot pedal (firmware-programmed to
send `F14`) triggers:

1. **`ahk/MuteAllMeetings.ahk`** (AutoHotkey v1, Windows) — toggles the in-app
   mute of every running meeting app (Teams desktop + browser tabs/PWA, Zoom,
   Webex, focused Google Meet tabs) by activating each window, sending its
   mute hotkey, and restoring focus.
2. **`mutastic` daemon (Go)** — toggles the Blue Yeti X's *hardware* mute
   (mute LED included) over its vendor HID protocol, and tracks the true mute
   state by listening to the mic's events — including physical button presses
   on the mic itself.

The left pedal (`F13`) toggles a NEEWER PL81 PRO LED streaming light: the
AHK script runs `mutastic.exe light toggle`, and the same daemon drives the
light over its CH340 USB-serial port.

## Components

- **`mutastic daemon`** — owns the Yeti X HID connection (VID 046D, vendor
  collection with `Usage == 1`), performs the init handshake, tracks mute
  state from the mic's events, and serves plain-text commands on UDP
  `127.0.0.1:42814`. Reconnects automatically if the mic disappears.
  Also owns every attached NEEWER PL81 PRO light (CH340 serial, VID 1A86
  PID 7523, 115200 8N1): a rescan every 5 s discovers newly plugged-in
  lights and tears down removed ones (no restart needed), with one
  independent reconnect loop per light, tracking each light's true state
  from its echo/broadcast frames and persisting each last look to
  `%LOCALAPPDATA%\mutastic\light-state-<COMx>.json`.
- **One-shot client** — `mutastic toggle | mute | unmute | status` sends one
  command to the daemon and prints the reply (`muted`, `unmuted`, `unknown`,
  or `error: <reason>`). Exit codes: `0` = non-error reply, `1` = `error:`
  reply, `2` = no daemon reachable / bad usage.
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
- **`ahk/MuteAllMeetings.ahk`** — the F14 handler runs
  `mutastic.exe toggle` (hidden, non-blocking) and then toggles the meeting
  apps as before; the F13 handler runs `mutastic.exe light toggle` the same
  way.

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
- copies `mutastic.exe` and `MuteAllMeetings.ahk` to
  `C:\Users\dan\code\mutastic-deploy\` (plus the tray icon if it can find
  one),
- creates/updates two Startup shortcuts — `MuteAllMeetings.lnk` (AutoHotkey
  v1 running the deployed script) and `Mutastic Daemon.lnk`
  (`mutastic.exe daemon`) — removing the old shortcut that pointed at
  `mute-unmute-meetings`,
- relaunches both programs.

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

## Troubleshooting

- **Log:** `%LOCALAPPDATA%\mutastic\mutastic.log` — daemon startup, HID
  collection enumeration, every command and device event, reconnect activity.
- **`status` says `unknown`:** normal right after daemon start; the state is
  known after the first mute command or device event.
- **Second daemon exits immediately:** UDP port 42814 doubles as the
  single-instance lock — the running daemon owns it.
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
