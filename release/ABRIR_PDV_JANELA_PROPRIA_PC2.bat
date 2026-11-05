@echo off
setlocal
cd /d "%~dp0"
title SIR BEEF - ABRIR PDV JANELA PROPRIA (PC 2 - CAIXA SECUNDARIO)
color 0A

echo ==============================================================================
echo   SIR BEEF - PDV EM JANELA PROPRIA (PC 2 / CAIXA ADICIONAL)
echo ==============================================================================
echo.
echo   [*] Este terminal conecta diretamente ao PC 1 (Servidor Principal).
echo   [*] Vendas, comandas e estoque sao sincronizados em tempo real.
echo.

set "CFG_FILE=pc2_connection.txt"
set "TARGET_URL="

if exist "%CFG_FILE%" (
    set /p TARGET_URL=<"%CFG_FILE%"
)

if "%TARGET_URL%"=="" (
    set "TARGET_URL=https://sirbeefnup.github.io/pdv"
    echo https://sirbeefnup.github.io/pdv> "%CFG_FILE%"
)

:: 1. Localiza executavel do Edge ou Chrome
set "BROWSER_EXE="
if exist "C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe" set "BROWSER_EXE=C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe"
if not defined BROWSER_EXE if exist "C:\Program Files\Microsoft\Edge\Application\msedge.exe" set "BROWSER_EXE=C:\Program Files\Microsoft\Edge\Application\msedge.exe"
if not defined BROWSER_EXE if exist "C:\Program Files\Google\Chrome\Application\chrome.exe" set "BROWSER_EXE=C:\Program Files\Google\Chrome\Application\chrome.exe"
if not defined BROWSER_EXE if exist "C:\Program Files (x86)\Google\Chrome\Application\chrome.exe" set "BROWSER_EXE=C:\Program Files (x86)\Google\Chrome\Application\chrome.exe"

:: 2. Cria atalho na Area de Trabalho do PC 2
if exist "backend\scripts\create_desktop_shortcut.ps1" (
    powershell -NoProfile -ExecutionPolicy Bypass -File "backend\scripts\create_desktop_shortcut.ps1" -TargetUrl "%TARGET_URL%" -ShortcutName "Sir Beef - PDV Caixa 2.lnk" >nul 2>&1
)

echo   [*] Abrindo janela do PDV Caixa 2 em tela cheia...
echo       Destino: %TARGET_URL%
echo.

if defined BROWSER_EXE (
    start "" "%BROWSER_EXE%" --app="%TARGET_URL%" --start-maximized --window-name="SirBeefPDV2"
) else (
    start "" "%TARGET_URL%"
)

echo   [OK] PDV Caixa 2 iniciado com sucesso!
echo        Atalho criado na Area de Trabalho deste computador!
echo.
ping 127.0.0.1 -n 2 >nul
exit
