; Sends F24 so the running MuteAllMeetings.ahk fires the meeting-app
; sweep (*F24 hotkey). Used by the Stream Deck mute button, chained
; after `mutastic.exe toggle`. SendInput-injected input triggers hook
; hotkeys in other AHK processes (same mechanism as the daemon's
; mic-button injection, proven live).
#NoEnv
SendMode Input
Send {F24}
ExitApp
