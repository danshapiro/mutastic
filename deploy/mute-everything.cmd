@echo off
REM Mute everything: Yeti X hardware mute + meeting-app sweep.
REM Lives at a no-spaces path so `cmd /C <path>` needs no quoting
REM (OpenDeck's Run Command is subject to cmd's quote-stripping rule).
"C:\Users\dan\code\mutastic-deploy\mutastic.exe" toggle
"C:\Program Files\AutoHotkey\AutoHotkeyU64.exe" "C:\Users\dan\code\mutastic-deploy\SendF24.ahk"
