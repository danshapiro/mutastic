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

See `docs/yeti-x-hid-protocol.md` for the reverse-engineered HID protocol and
`docs/pedal-and-mute.md` for the machine setup (pedal firmware mapping,
deployment, install state).

Runs on Windows; developed from WSL2 (cross-compiled Go, deployed to the
Windows side). Private repo; this tooling is specific to Dan's desk setup.
