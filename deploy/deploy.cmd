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
set "ODPLUGDIR=%APPDATA%\opendeck\plugins\com.danshapiro.mutastic.sdPlugin"
set "OPENDECK_EXE=C:\Users\dan\AppData\Local\OpenDeck\opendeck.exe"

echo == Stopping running instances ==
taskkill /F /IM opendeck.exe >nul 2>&1
taskkill /F /IM mutastic.exe >nul 2>&1
ping -n 3 127.0.0.1 >nul
powershell -NoProfile -Command "Get-CimInstance Win32_Process -Filter \"Name='AutoHotkeyU64.exe'\" | Where-Object { $_.CommandLine -like '*MuteAllMeetings*' } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force }" >nul 2>&1

echo == Copying files from %SRC% ==
if not exist "%DEST%" mkdir "%DEST%"
copy /Y "%SRC%\bin\mutastic.exe" "%DEST%\mutastic.exe" >nul || goto :fail
copy /Y "%SRC%\ahk\MuteAllMeetings.ahk" "%DEST%\MuteAllMeetings.ahk" >nul || goto :fail
copy /Y "%SRC%\ahk\SendF24.ahk" "%DEST%\SendF24.ahk" >nul || goto :fail
copy /Y "%SRC%\deploy\mutastic-daemon.vbs" "%DEST%\mutastic-daemon.vbs" >nul || goto :fail
copy /Y "%SRC%\deploy\mute-everything.cmd" "%DEST%\mute-everything.cmd" >nul || goto :fail
if exist "%SRC%\ahk\mic_mute_light.ico" copy /Y "%SRC%\ahk\mic_mute_light.ico" "%DEST%\" >nul
if not exist "%DEST%\mic_mute_light.ico" if exist "%OLD_DEPLOY%\mic_mute_light.ico" copy /Y "%OLD_DEPLOY%\mic_mute_light.ico" "%DEST%\" >nul
if not exist "%DEST%\mic_mute_light.ico" echo WARNING: mic_mute_light.ico not found - tray icon will be missing

echo == Installing OpenDeck plugin ==
if not exist "%ODPLUGDIR%\icons" mkdir "%ODPLUGDIR%\icons"
copy /Y "%SRC%\deck\com.danshapiro.mutastic.sdPlugin\manifest.json" "%ODPLUGDIR%\manifest.json" >nul || goto :fail
copy /Y "%SRC%\deck\icons\mutastic-mic.png" "%ODPLUGDIR%\icons\mutastic-mic.png" >nul || goto :fail
copy /Y "%SRC%\deck\icons\mutastic-mic-muted.png" "%ODPLUGDIR%\icons\mutastic-mic-muted.png" >nul || goto :fail
set /a PLUGCOPYTRIES=0
:copyplugexe
copy /Y "%SRC%\bin\mutastic.exe" "%ODPLUGDIR%\mutastic.exe" >nul && goto :plugexeok
set /a PLUGCOPYTRIES+=1
if %PLUGCOPYTRIES% geq 5 goto :fail
echo plugin exe still locked, retrying %PLUGCOPYTRIES%/5 ...
ping -n 3 127.0.0.1 >nul
goto :copyplugexe
:plugexeok
copy /Y "%SRC%\deploy\set-mute-key.ps1" "%DEST%\set-mute-key.ps1" >nul || goto :fail

echo == Pointing profile keys[5] at the plugin ==
powershell -NoProfile -ExecutionPolicy Bypass -File "%DEST%\set-mute-key.ps1" || goto :fail

echo == Replacing startup shortcuts ==
if exist "%STARTUP%\MuteAllMeetings.lnk" del /F "%STARTUP%\MuteAllMeetings.lnk"
powershell -NoProfile -Command "$ws = New-Object -ComObject WScript.Shell; $s = $ws.CreateShortcut('%STARTUP%\MuteAllMeetings.lnk'); $s.TargetPath = '%AHK_EXE%'; $s.Arguments = '%DEST%\MuteAllMeetings.ahk'; $s.WorkingDirectory = '%DEST%'; $s.Save()" || goto :fail
powershell -NoProfile -Command "$ws = New-Object -ComObject WScript.Shell; $s = $ws.CreateShortcut('%STARTUP%\Mutastic Daemon.lnk'); $s.TargetPath = 'C:\Windows\System32\wscript.exe'; $s.Arguments = '"%DEST%\mutastic-daemon.vbs"'; $s.WorkingDirectory = '%DEST%'; $s.Save()" || goto :fail

echo == Relaunching ==
start "" wscript.exe "%DEST%\mutastic-daemon.vbs"
start "" "%AHK_EXE%" "%DEST%\MuteAllMeetings.ahk"
start "" "%OPENDECK_EXE%"

echo Deploy complete.
exit /b 0

:fail
echo DEPLOY FAILED
exit /b 1
