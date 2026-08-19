; MuteAllMeetings.ahk  (AutoHotkey v1.1)
;
; Left USB foot pedal (F13) is disabled 2026-08-12 by request.
; Center USB foot pedal (F14) is disabled 2026-08-09 because of accidental
; presses. Both keys are consumed by active wildcard no-op hotkeys.
; Right USB foot pedal (F15) remains Winpepper's push-to-talk hold hotkey.
; Light control remains available through the browser UI, Stream Deck, and
; mutastic light commands.
; The physical Yeti X mute button and Stream Deck mute inject F24 for the
; meeting-app sweep; F24 remains the active meeting control.
;
; How it works: for every matching window, briefly activate it, send that
; app's own in-app mute-toggle hotkey, then return focus to where you were.
;
; Caveat: these are per-app TOGGLES. If two apps start out with opposite
; mute states, toggling keeps them opposite. Sync them once (mute/unmute
; the odd one manually) and they stay in sync afterwards.
;
; Local tool for this machine. Documented in the mutastic repo:
; README.md and docs/pedal-and-mute.md

#SingleInstance Force
#NoEnv
SendMode Input
SetWorkingDir %A_ScriptDir%
DetectHiddenWindows, Off

; No tray icon: mic state and quit live on the Mutastic tray icon now; this
; script is hotkeys only (F13/F14 no-ops, F15 PTT hold, F24 meeting sweep).
#NoTrayIcon

; F13 (left pedal) DISABLED 2026-08-12 by request.
; Consume it so it cannot fall through to the foreground application.
*F13::return

; F14 (center pedal) DISABLED 2026-08-09 because of accidental presses.
; Consume it so it cannot fall through to the foreground application.
*F14::return

; F24 is injected by the mutastic daemon when the mic's own mute
; button is pressed (0x21 DeviceMute event). Sweep the meeting apps
; only - the mic has already toggled its own hardware mute, so
; running mutastic.exe toggle here would undo it. The * prefix
; fires the hotkey even while Ctrl/Shift/Alt/Win are held - an
; injected press must never be swallowed mid-chord.
*F24::
ToggleAllMeetings()
return

ToggleAllMeetings() {
    prev := WinExist("A")
    report := ""

    ; --- Microsoft Teams (new + classic) : Ctrl+Shift+M ---
    SendToAll("ahk_exe ms-teams.exe", "^+m", "Teams", report)
    SendToAll("ahk_exe Teams.exe", "^+m", "Teams classic", report)

    ; --- Zoom : Alt+A (meeting window only). Zoom Workplace 6.x renamed the
    ;     meeting window class to ConfMultiTabContentWndClass; pre-6.x Zoom
    ;     used ZPContentViewWndClass. Try the new class first and only fall
    ;     back when it matched nothing - if BOTH classes ever answered one
    ;     meeting, two Alt+A sends would toggle it twice and net no change. ---
    if (SendToAll("ahk_class ConfMultiTabContentWndClass", "!a", "Zoom", report) = 0)
        SendToAll("ahk_class ZPContentViewWndClass", "!a", "Zoom", report)

    ; --- Webex : Ctrl+M ---
    SendToAll("ahk_exe CiscoCollabHost.exe", "^m", "Webex", report)

    ; --- Google Meet : Ctrl+D, browser windows whose ACTIVE tab is a Meet
    ;     call (window title starts "Meet - ..."). A Meet tab hidden behind
    ;     another tab in the same browser window cannot be reached. ---
    SetTitleMatchMode, RegEx
    SendToAll("^Meet\s+[-\x{2013}\x{2014}] ahk_exe i)(chrome|msedge|firefox|brave|opera)\.exe", "^d", "Meet", report)

    ; --- Teams in a browser tab or PWA : Ctrl+Shift+M, browser windows whose
    ;     ACTIVE tab title contains "Microsoft Teams" (teams.microsoft.com /
    ;     teams.live.com set that title; desktop Teams is matched by exe above,
    ;     so there is no double-toggle). Same active-tab limit as Meet. ---
    SendToAll("Microsoft Teams ahk_exe i)(chrome|msedge|firefox|brave|opera)\.exe", "^+m", "Teams (tab)", report)
    SetTitleMatchMode, 2

    ; back to where we were
    if (prev)
        WinActivate, ahk_id %prev%

    if (report = "")
        report := "No meeting windows found"
    ToolTip, %report%
    SetTimer, ClearToolTip, -1500
}

SendToAll(criteria, keys, name, ByRef report) {
    WinGet, ids, List, %criteria%
    n := 0
    Loop, %ids% {
        id := ids%A_Index%
        WinGetTitle, title, ahk_id %id%
        if (title = "")
            continue
        if InStr(title, "Notification")
            continue
        WinActivate, ahk_id %id%
        WinWaitActive, ahk_id %id%,, 1
        if (ErrorLevel)
            continue
        Sleep, 50
        Send, %keys%
        Sleep, 100
        n += 1
    }
    if (n > 0)
        report .= name . ": " . n . "   "
    return n
}

ClearToolTip:
    ToolTip
return
