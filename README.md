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
  Also owns the NEEWER PL81 PRO light (CH340 serial, VID 1A86 PID 7523,
  115200 8N1) with its own independent reconnect loop, tracking the light's
  true state from its echo/broadcast frames and persisting the last look to
  `%LOCALAPPDATA%\mutastic\light-state.json`.
- **One-shot client** — `mutastic toggle | mute | unmute | status` sends one
  command to the daemon and prints the reply (`muted`, `unmuted`, `unknown`,
  or `error: <reason>`). Exit codes: `0` = non-error reply, `1` = `error:`
  reply, `2` = no daemon reachable / bad usage.
- **Light commands** — `mutastic light toggle | on | off | status`,
  `mutastic light brightness <0-100>`, `mutastic light temp <2900-7000>`,
  `mutastic light preset <cold|sunlight|afternoon|sunset|candle>`.
  Replies: `on 64% 4950K`, `off`, `unknown`, or `error: <reason>` (same
  exit codes as the mic commands). Notes: OFF is brightness 0 (the panel
  has no working power command); `on`/`toggle` restore the last non-zero
  brightness and temperature (default 100% / 5000 K); setting `temp` while
  the light is off turns it on at the restored brightness; temperatures
  are quantized to the panel's 19 hardware steps (~228 K), so `temp 5000`
  reads back as `4950K`; `status` is `unknown` after a daemon restart
  until the light first echoes or its knob is touched (the hardware has no
  query command).
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
  `light: session ended` and reopens the port automatically.
- **`light ...` says `error: no light`:** the CH340 port wasn't found or
  couldn't be opened. The COM port is exclusive — close NEEWER Control
  Center (or anything else holding the port) and check the log's
  `light: serial port:` enumeration lines.
- **Light state file:** `%LOCALAPPDATA%\mutastic\light-state.json` holds
  the restore-on-`on` look; deleting it just resets the defaults
  (100% / 5000 K).

**Warning:** never `usbipd attach` the Yeti X to WSL — it steals the system
microphone from Windows. Always run the built exe on the Windows side.

See `docs/yeti-x-hid-protocol.md` for the mic's reverse-engineered HID
protocol, `docs/pl81-pro-serial-protocol.md` for the light's serial
protocol, and `docs/pedal-and-mute.md` for the machine setup (pedal
firmware mapping, deployment, install state).

Runs on Windows; developed from WSL2 (cross-compiled Go, deployed to the
Windows side). Private repo; this tooling is specific to Dan's desk setup.
