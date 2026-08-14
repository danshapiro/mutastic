Set shell = CreateObject("WScript.Shell")
shell.Run """C:\Users\dan\code\mutastic-deploy\mutastic.exe"" daemon", 0, False
shell.Run """C:\Users\dan\code\mutastic-deploy\mutastic.exe"" ui --no-open", 0, False
shell.Run """C:\Users\dan\code\mutastic-deploy\mutastic.exe"" tray", 0, False
