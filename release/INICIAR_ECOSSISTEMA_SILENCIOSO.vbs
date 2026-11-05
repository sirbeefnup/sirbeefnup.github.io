Set WshShell = CreateObject("WScript.Shell")
Set FSO = CreateObject("Scripting.FileSystemObject")
strDir = FSO.GetParentFolderName(WScript.ScriptFullName)
WshShell.Run chr(34) & strDir & "\INICIAR_ECOSSISTEMA_SIRBEEF.bat" & chr(34), 0, False
