; MuteAllMeetings.ahk  (AutoHotkey v1.1)
;
; Middle USB foot pedal (F14) toggles microphone mute in ALL running
; meeting apps at once: MS Teams, Zoom, Webex, and Google Meet tabs.
; Left pedal (F13) toggles the NEEWER PL81 PRO light via mutastic.
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

Menu, Tray, Icon, %A_ScriptDir%\mic_mute_light.ico
Menu, Tray, Tip, MuteAllMeetings - F14 mutes meetings+mic`, F13 toggles light

F14::
Run, "%A_ScriptDir%\mutastic.exe" toggle, %A_ScriptDir%, Hide UseErrorLevel
ToggleAllMeetings()
return

F13::
Run, "%A_ScriptDir%\mutastic.exe" light toggle, %A_ScriptDir%, Hide UseErrorLevel
return

ToggleAllMeetings() {
    prev := WinExist("A")
    report := ""

    ; --- Microsoft Teams (new + classic) : Ctrl+Shift+M ---
    SendToAll("ahk_exe ms-teams.exe", "^+m", "Teams", report)
    SendToAll("ahk_exe Teams.exe", "^+m", "Teams classic", report)

    ; --- Zoom : Alt+A (meeting window only, class ZPContentViewWndClass) ---
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
