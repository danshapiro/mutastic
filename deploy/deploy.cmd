@echo off
setlocal
REM mutastic deployment (supersedes finish-setup.cmd.legacy).
REM Usage: deploy.cmd [source-repo-root]
REM Default source = this script's parent dir (the repo checkout).

set "SRC=%~dp0.."
if not "%~1"=="" set "SRC=%~1"
set "DEST=C:\Users\dan\code\mutastic-deploy"
set "STARTUP=%APPDATA%\Microsoft\Windows\Start Menu\Programs\Startup"
set "AHK_EXE=C:\Program Files\AutoHotkey\AutoHotkeyU64.exe"
set "OLD_DEPLOY=C:\Users\dan\code\mute-unmute-meetings"

echo == Stopping running instances ==
taskkill /F /IM mutastic.exe >nul 2>&1
powershell -NoProfile -Command "Get-CimInstance Win32_Process -Filter \"Name='AutoHotkeyU64.exe'\" | Where-Object { $_.CommandLine -like '*MuteAllMeetings*' } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force }" >nul 2>&1

echo == Copying files from %SRC% ==
if not exist "%DEST%" mkdir "%DEST%"
copy /Y "%SRC%\bin\mutastic.exe" "%DEST%\mutastic.exe" >nul || goto :fail
copy /Y "%SRC%\ahk\MuteAllMeetings.ahk" "%DEST%\MuteAllMeetings.ahk" >nul || goto :fail
if exist "%SRC%\ahk\mic_mute_light.ico" copy /Y "%SRC%\ahk\mic_mute_light.ico" "%DEST%\" >nul
if not exist "%DEST%\mic_mute_light.ico" if exist "%OLD_DEPLOY%\mic_mute_light.ico" copy /Y "%OLD_DEPLOY%\mic_mute_light.ico" "%DEST%\" >nul
if not exist "%DEST%\mic_mute_light.ico" echo WARNING: mic_mute_light.ico not found - tray icon will be missing

echo == Replacing startup shortcuts ==
if exist "%STARTUP%\MuteAllMeetings.lnk" del /F "%STARTUP%\MuteAllMeetings.lnk"
powershell -NoProfile -Command "$ws = New-Object -ComObject WScript.Shell; $s = $ws.CreateShortcut('%STARTUP%\MuteAllMeetings.lnk'); $s.TargetPath = '%AHK_EXE%'; $s.Arguments = '%DEST%\MuteAllMeetings.ahk'; $s.WorkingDirectory = '%DEST%'; $s.Save()" || goto :fail
powershell -NoProfile -Command "$ws = New-Object -ComObject WScript.Shell; $s = $ws.CreateShortcut('%STARTUP%\Mutastic Daemon.lnk'); $s.TargetPath = '%DEST%\mutastic.exe'; $s.Arguments = 'daemon'; $s.WorkingDirectory = '%DEST%'; $s.Save()" || goto :fail

echo == Relaunching ==
start "" "%DEST%\mutastic.exe" daemon
start "" "%AHK_EXE%" "%DEST%\MuteAllMeetings.ahk"

echo Deploy complete.
exit /b 0

:fail
echo DEPLOY FAILED
exit /b 1
