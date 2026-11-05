Set WshShell = CreateObject("WScript.Shell")
strDir = WshShell.CurrentDirectory
WshShell.Run chr(34) & strDir & "\INICIAR_ECOSSISTEMA_SIRBEEF.bat" & chr(34) & " --headless", 0, False
