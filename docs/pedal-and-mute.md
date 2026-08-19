# USB Foot Pedal (iKKEGOL triple, USB ID 3553:b001)

## Firmware key mapping (persists in the pedal itself)

| Pedal | Key | Used by |
|-------|-----|---------|
| Left | `F13` | Disabled 2026-08-12; consumed no-op (light control remains in browser UI, Stream Deck, and `mutastic light ...`) |
| Center | `F14` | Disabled 2026-08-09; consumed no-op because of accidental presses |
| Right | `F15` | Winpepper push-to-talk hold hotkey |

Evidence: `footswitch -r` readback in Amplifier session `e1940e46` (2026-08-03):
`[switch 1]: f13 / [switch 2]: f14 / [switch 3]: f15`.
Winpepper hotkeys confirmed in `C:\Users\dan\AppData\Local\Winpepper\settings.json`
(`holdHotkey: F15`, `toggleHotkey: RightCtrl+RightShift+Space`).

To reprogram the pedal firmware, use the `reprogram-foot-pedal` skill in
`~/code/pedal` (uses rgerganov/footswitch via usbipd attach to WSL).

## Active controls and mute flow

The pedal firmware is unchanged and still emits `F13`, `F14`, and `F15`. The
left and center AHK bindings are active wildcard consumed no-ops, so neither
key can fall through to the foreground application. The active controls are:

- **Left pedal (`F13`)** — consumed no-op; it does not toggle lights. Light
  control remains available through the browser UI, the Stream Deck Lights
  action, and `mutastic light ...` commands.
- **Center pedal (`F14`)** — consumed no-op to prevent accidental presses.
- **Right pedal (`F15`)** — remains Winpepper's push-to-talk hold key.
- **Physical Yeti X mute button** — remains active. The mic changes its own
  hardware mute, the daemon observes the `0x21` event, and injects `F24`.
  AutoHotkey's active `*F24::` handler sweeps the meeting apps without issuing
  another hardware toggle.
- **Stream Deck Mutastic Mute** — remains active. The OpenDeck plugin sends
  `toggle` to the daemon for the Yeti and injects one `F24` meeting-app sweep.

The physical-button and Stream Deck paths are loop-free: host mute commands
echo `0x20` rather than the physical-button `0x21` event, while the `F24`
handler changes meeting apps only and never calls `mutastic.exe toggle`.

### Meeting-app sweep

`ToggleAllMeetings()` activates each matching window briefly, sends that
application's own mute shortcut, restores focus, and shows a summary tooltip.
It is currently reached through the active `*F24::` binding, not either
disabled pedal binding.

| App | Window match | Hotkey sent |
|-----|--------------|-------------|
| MS Teams (new + classic) | `ms-teams.exe` / `Teams.exe` | `Ctrl+Shift+M` |
| Zoom | *(no keystroke)* `zoom-mute.exe` invokes the Meeting-tools mute button via MSAA; built at deploy time from `deploy/zoom-mute.cs` | — |
| Zoom (fallback) | class `ConfMultiTabContentWndClass` (6.x) / `ZPContentViewWndClass` (pre-6.x) meeting window | `Alt+A` |
| Webex | `CiscoCollabHost.exe` | `Ctrl+M` |
| Google Meet | browser window titled `Meet - …` (chrome/edge/firefox/brave/opera) | `Ctrl+D` |
| Teams in browser tab / PWA | browser window whose title contains `Microsoft Teams` | `Ctrl+Shift+M` |

Known limits: per-app *toggles* can end up opposite if apps start in different
mute states (sync once manually); a Meet or Teams tab hidden behind another tab
in the same browser window cannot be reached because its title is not visible.
Upstream `MuteMeetings.ahk` (the single-app GUI version) remains pristine as a
fallback and does not bind `F14`.

## Deployment and automatic startup

The canonical source is `/home/dan/code/mutastic`. Build from WSL with
`./build.sh`, then run the checkout's `deploy\deploy.cmd` on Windows. That
script is the supported rebuild/redeploy path; the retired
`mute-unmute-meetings` setup script is not used.

Runtime files are deployed to:

```text
C:\Users\dan\code\mutastic-deploy\
```

This includes `mutastic.exe`, `MuteAllMeetings.ahk`, and
`mutastic-daemon.vbs`. AutoHotkey v1.1 remains installed at
`C:\Program Files\AutoHotkey`; these scripts use v1 syntax and do not run under
AutoHotkey v2.

The deploy script creates two user Startup shortcuts:

- **`MuteAllMeetings.lnk`** starts the deployed AHK script.
- **`Mutastic Daemon.lnk`** runs the deployed `mutastic-daemon.vbs` through
  `wscript.exe`. That one hidden asynchronous launcher starts
  `mutastic.exe daemon` first, then `mutastic.exe ui --no-open`.

Thus both resident Mutastic components restart after Windows login, hidden,
without opening Chrome or another browser. Later, plain `mutastic ui` (or
`C:\Users\dan\code\mutastic-deploy\mutastic.exe ui`) opens or focuses the
already-running light controller.

Windows remembers disabled Startup items by shortcut filename. After updating
`Mutastic Daemon.lnk`, `deploy\deploy.cmd` removes its exact value from:

```text
HKCU\Software\Microsoft\Windows\CurrentVersion\Explorer\StartupApproved\StartupFolder
```

It then verifies that `Mutastic Daemon.lnk` is absent from that key and fails
deployment if it remains. Running deployment therefore intentionally
re-enables Mutastic login startup.

To disable Mutastic startup intentionally, turn off **Mutastic Daemon** in
Windows Settings → Apps → Startup (or Task Manager's Startup apps), or delete
that shortcut from `shell:startup`. A later `deploy\deploy.cmd` run recreates
and re-enables the entry, so disable it again after deployment if desired.
