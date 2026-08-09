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

## Components

- **`mutastic daemon`** — owns the Yeti X HID connection (VID 046D, vendor
  collection with `Usage == 1`), performs the init handshake, tracks mute
  state from the mic's events, and serves plain-text commands on UDP
  `127.0.0.1:42814`. Reconnects automatically if the mic disappears.
- **One-shot client** — `mutastic toggle | mute | unmute | status` sends one
  command to the daemon and prints the reply (`muted`, `unmuted`, `unknown`,
  or `error: <reason>`). Exit codes: `0` = non-error reply, `1` = `error:`
  reply, `2` = no daemon reachable / bad usage.
- **`ahk/MuteAllMeetings.ahk`** — the F14 handler runs
  `mutastic.exe toggle` (hidden, non-blocking) and then toggles the meeting
  apps as before.

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

**Warning:** never `usbipd attach` the Yeti X to WSL — it steals the system
microphone from Windows. Always run the built exe on the Windows side.

See `docs/yeti-x-hid-protocol.md` for the reverse-engineered HID protocol and
`docs/pedal-and-mute.md` for the machine setup (pedal firmware mapping,
deployment, install state).

Runs on Windows; developed from WSL2 (cross-compiled Go, deployed to the
Windows side). Private repo; this tooling is specific to Dan's desk setup.
