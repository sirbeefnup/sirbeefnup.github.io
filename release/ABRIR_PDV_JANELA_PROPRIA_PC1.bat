@echo off
setlocal
cd /d "%~dp0"
title SIR BEEF - ABRIR PDV JANELA PROPRIA (PC 1)
color 0B

echo ==============================================================================
echo   SIR BEEF - ABRINDO PDV EM JANELA PROPRIA DEDICADA (PC 1)
echo ==============================================================================
echo.
echo   [+] Sem barra de enderecos (ganha +120px de altura util)
echo   [+] Sem abas nem favoritos do navegador
echo   [+] Layout espacoso: produtos, fotos e carrinho grandes e nitidos
echo   [+] O atendente nao fecha a tela por engano
echo.

:: 1. Cria icone e atalho na Area de Trabalho
if not exist "SirBeef.ico" (
    powershell -NoProfile -ExecutionPolicy Bypass -File "backend\scripts\create_icon.ps1" >nul 2>&1
)
powershell -NoProfile -ExecutionPolicy Bypass -File "backend\scripts\create_desktop_shortcut.ps1" -TargetUrl "http://127.0.0.1:8080/pdv" -ShortcutName "Sir Beef - PDV Caixa 1.lnk" >nul 2>&1

:: 2. Localiza executavel do Edge ou Chrome
set "BROWSER_EXE="
if exist "C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe" set "BROWSER_EXE=C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe"
if not defined BROWSER_EXE if exist "C:\Program Files\Microsoft\Edge\Application\msedge.exe" set "BROWSER_EXE=C:\Program Files\Microsoft\Edge\Application\msedge.exe"
if not defined BROWSER_EXE if exist "C:\Program Files\Google\Chrome\Application\chrome.exe" set "BROWSER_EXE=C:\Program Files\Google\Chrome\Application\chrome.exe"
if not defined BROWSER_EXE if exist "C:\Program Files (x86)\Google\Chrome\Application\chrome.exe" set "BROWSER_EXE=C:\Program Files (x86)\Google\Chrome\Application\chrome.exe"

echo   [*] Abrindo janela do PDV em tela cheia...
if defined BROWSER_EXE (
    start "" "%BROWSER_EXE%" --app="http://127.0.0.1:8080/pdv" --start-maximized --window-name="SirBeefPDV1"
) else (
    start "" "http://127.0.0.1:8080/pdv"
)

echo.
echo   [OK] PDV aberto com sucesso!
echo        Atalho 'Sir Beef - PDV Caixa 1' criado na sua Area de Trabalho!
echo.
ping 127.0.0.1 -n 2 >nul
exit
