# USB Foot Pedal (iKKEGOL triple, USB ID 3553:b001)

## Firmware key mapping (persists in the pedal itself)

| Pedal | Key | Used by |
|-------|-----|---------|
| Left | `F13` | mutastic light toggle (NEEWER PL81 PRO streaming light) |
| Center | `F14` | MuteMeetings AHK script (mic mute/unmute toggle) |
| Right | `F15` | Winpepper push-to-talk hold hotkey |

Evidence: `footswitch -r` readback in Amplifier session `e1940e46` (2026-08-03):
`[switch 1]: f13 / [switch 2]: f14 / [switch 3]: f15`.
Winpepper hotkeys confirmed in `C:\Users\dan\AppData\Local\Winpepper\settings.json`
(`holdHotkey: F15`, `toggleHotkey: RightCtrl+RightShift+Space`).

To reprogram the pedal firmware, use the `reprogram-foot-pedal` skill in
`~/code/pedal` (uses rgerganov/footswitch via usbipd attach to WSL).

## Middle pedal: mute/unmute meetings (set up 2026-08-08)

[bralexc/mute-unmute-meetings](https://github.com/bralexc/mute-unmute-meetings)
(AutoHotkey **v1** scripts) cloned to `C:\Users\dan\code\mute-unmute-meetings`.

The active script is a **local addition**, `MuteAllMeetings.ahk` (not upstream):
on `F14` it toggles mute in **all running meeting apps at once** — for every
matching window it briefly activates it, sends that app's in-app mute hotkey,
then restores focus and shows a tooltip summary of what it toggled.

| App | Window match | Hotkey sent |
|-----|--------------|-------------|
| MS Teams (new + classic) | `ms-teams.exe` / `Teams.exe` | `Ctrl+Shift+M` |
| Zoom | class `ZPContentViewWndClass` (meeting window) | `Alt+A` |
| Webex | `CiscoCollabHost.exe` | `Ctrl+M` |
| Google Meet | browser window titled `Meet - …` (chrome/edge/firefox/brave/opera) | `Ctrl+D` |
| Teams in browser tab / PWA | browser window whose title contains `Microsoft Teams` | `Ctrl+Shift+M` |

Known limits: per-app *toggles* can end up opposite if apps start in different
mute states (sync once manually); a Meet or Teams tab hidden behind another tab
in the same browser window can't be reached (its title isn't visible).

Upstream `MuteMeetings.ahk` (single-app GUI version) is kept pristine as a
fallback — it no longer binds F14.

Install state (completed 2026-08-08): AutoHotkey v1.1 installed to
`C:\Program Files\AutoHotkey` (scripts are v1 syntax; AHK v2 will NOT run
them); `MuteAllMeetings.lnk` Startup shortcut created; script running under
`AutoHotkeyU64.exe`.

Rebuild pieces:

- `C:\Users\dan\Downloads\AutoHotkey_1.1_setup.exe` — AHK v1.1 installer
- `C:\Users\dan\code\mute-unmute-meetings\finish-setup.cmd` — one-click
  re-setup: silent-installs AHK, recreates the Startup shortcut, launches
  the script
- To undo autostart: delete `MuteAllMeetings.lnk` from `shell:startup`
