; Sends F24 so the running MuteAllMeetings.ahk fires the meeting-app
; sweep (*F24 hotkey). Used by the Stream Deck mute button, chained
; after `mutastic.exe toggle`. SendInput-injected input triggers hook
; hotkeys in other AHK processes (same mechanism as the daemon's
; mic-button injection, proven live).
#NoEnv
SendMode Input
; SendLevel 1 is REQUIRED: AHK-injected input is tagged and ignored by
; other AHK scripts' hotkeys at the default level 0. Without this, the
; *F24 hotkey in MuteAllMeetings.ahk never fires (observed live).
SendLevel, 1
Send {F24}
ExitApp
