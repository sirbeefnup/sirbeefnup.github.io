@echo off
chcp 65001 >nul
title SIR BEEF — Inicializador Master (Modo Administrador + Rede Local + Tunel)

:: ============================================================================
:: 1. AUTO-ELEVACAO AUTOMATICA PARA ADMINISTRADOR (UAC / PRIVILEGIO MAXIMO)
:: ============================================================================
net session >nul 2>&1
if %errorlevel% neq 0 (
    echo [SIR BEEF] Elevando automaticamente para privilegio de ADMINISTRADOR...
    powershell -NoProfile -ExecutionPolicy Bypass -Command "Start-Process -FilePath '%~f0' -WorkingDirectory '%~dp0' -Verb RunAs"
    exit /b
)

cd /d "%~dp0"
echo ======================================================================
echo   SIR BEEF - BOUTIQUE DAS CARNES E BURGERS (SERVIDOR MASTER ADM)
echo ======================================================================
echo.

:: ============================================================================
:: 2. VERIFICA SE ESTE PC E O DA LOJA (PRODUCAO) OU O SEU PC (TESTE_DEV)
:: ============================================================================
set "ENV_MODE=PRODUCAO_LOJA"
if exist "%~dp0backend\data\server_env_mode.txt" (
    set /p ENV_MODE=<"%~dp0backend\data\server_env_mode.txt"
)

set "IS_TEST_MODE=0"
if /I "%ENV_MODE%"=="TESTE_DEV" set "IS_TEST_MODE=1"
if /I "%ENV_MODE%"=="TESTE_LOCAL" set "IS_TEST_MODE=1"

if "%IS_TEST_MODE%"=="1" (
    echo [1/5] MODO TESTE / DESENVOLVIMENTO DETECTADO ^(SEU PC^):
    echo       -> Desativando inicio automatico com o Windows neste PC de teste...
    schtasks /Delete /TN "SirBeef_AutoStart_Admin" /F >nul 2>&1
    reg delete "HKCU\Software\Microsoft\Windows\CurrentVersion\Run" /v "SirBeef_Server_Admin" /f >nul 2>&1
) else (
    echo [1/5] MODO LOJA OFICIAL DETECTADO ^(PC DA HAMBURGUERIA^):
    echo       -> Ativando Inicio Automatico junto com o Windows ^(Privilegio ADM^)...
    schtasks /Create /TN "SirBeef_AutoStart_Admin" /TR "\"%~f0\" --autostart" /SC ONLOGON /RL HIGHEST /F >nul 2>&1
    reg add "HKCU\Software\Microsoft\Windows\CurrentVersion\Run" /v "SirBeef_Server_Admin" /t REG_SZ /d "schtasks /Run /TN \"SirBeef_AutoStart_Admin\"" /f >nul 2>&1
    echo       -> Inicio Automatico com o Windows ^(Modo ADM sem bloqueio UAC^) ATIVO!
)

:: ============================================================================
:: 3. CONFIGURACAO AUTOMATICA DE REDE PRIVADA E FIREWALL DO WINDOWS
:: ============================================================================
echo [2/5] Alterando perfil da rede atual (Wi-Fi/Ethernet) para REDE PRIVADA...
powershell -NoProfile -ExecutionPolicy Bypass -Command "Get-NetConnectionProfile | Where-Object { $_.NetworkCategory -ne 'Private' } | ForEach-Object { Set-NetConnectionProfile -InterfaceIndex $_.InterfaceIndex -NetworkCategory Private -ErrorAction SilentlyContinue }"

echo [3/5] Liberando portas 8080 e 80 (TCP/UDP) no Firewall como Administrador...
netsh advfirewall firewall delete rule name="SirBeef_Host_LAN_WiFi_8080" >nul 2>&1
netsh advfirewall firewall add rule name="SirBeef_Host_LAN_WiFi_8080" dir=in action=allow protocol=TCP localport=8080,80 profile=any >nul
netsh advfirewall firewall add rule name="SirBeef_Host_LAN_WiFi_8080_UDP" dir=in action=allow protocol=UDP localport=8080 profile=any >nul
netsh advfirewall firewall add rule name="SirBeef_Host_LAN_WiFi_8080_OUT" dir=out action=allow protocol=TCP localport=8080,80 profile=any >nul
netsh advfirewall firewall add rule name="SirBeef_Host_LAN_WiFi_8080_EXE" dir=in action=allow program="%~dp0backend\sirbeef-server.exe" enable=yes profile=any >nul
echo       -> Firewall e Rede Wi-Fi desbloqueados com privilegio de Administrador!

:: ============================================================================
:: 4. TUNEL CLOUDFLARE EM SEGUNDO PLANO (SE NAO ESTIVER RODANDO)
:: ============================================================================
echo [4/5] Verificando Tunel Seguro Cloudflare...
tasklist /FI "IMAGENAME eq cloudflared.exe" 2>NUL | find /I /N "cloudflared.exe">NUL
if "%ERRORLEVEL%"=="1" (
    if exist "%~dp0backend\cloudflared.exe" (
        echo       -> Iniciando Tunel Cloudflare em segundo plano...
        start "SirBeef Cloudflare Tunnel" /MIN "%~dp0backend\cloudflared.exe" tunnel --url http://localhost:8080 --metrics 127.0.0.1:20241 --no-autoupdate
        timeout /t 3 /nobreak >nul
    )
) else (
    echo       -> Tunel Cloudflare ja esta ativo!
)

:: ============================================================================
:: 5. INICIANDO SERVIDOR HOST GO
:: ============================================================================
echo [5/5] Iniciando Servidor Host Local Sir Beef (Porta 8080 - Modo ADM)...
echo.
cd /d "%~dp0backend"

if "%IS_TEST_MODE%"=="1" (
    start "" "http://localhost:8080/pdv"
    echo [MODO TESTE] Para desligar o servidor no seu PC, basta fechar esta janela preta ou clicar em PARAR_SERVIDOR_NO_MEU_PC.bat
    if exist "sirbeef-server-new.exe" (
    move /Y "sirbeef-server-new.exe" "sirbeef-server.exe" >nul 2>&1
)
.\sirbeef-server.exe
    exit /b
)

start "" "https://sirbeefnup.github.io/pdv"

:WATCHDOG_LOOP
powershell -NoProfile -ExecutionPolicy Bypass -Command "$ErrorActionPreference='SilentlyContinue'; $vJson = Get-Content 'data\apk_version.json' -Raw | ConvertFrom-Json; $targetVer = [string]$vJson.version_name; $curVer = if (Test-Path 'data\binary_version.txt') { (Get-Content 'data\binary_version.txt' -Raw).Trim() } else { '' }; if ($targetVer -and $targetVer -ne $curVer) { Write-Host ('[AUTO-UPDATE] Baixando novo motor Sir Beef v' + $targetVer + ' da nuvem...') -ForegroundColor Cyan; Invoke-WebRequest -Uri 'https://raw.githubusercontent.com/sirbeefnup/sirbeefnup.github.io/main/release/backend/sirbeef-server.zip' -OutFile 'sirbeef-server.zip' -TimeoutSec 45; if (Test-Path 'sirbeef-server.zip') { Expand-Archive -Path 'sirbeef-server.zip' -DestinationPath '.\temp_bin' -Force; if (Test-Path '.\temp_bin\sirbeef-server.exe') { Move-Item -Path '.\temp_bin\sirbeef-server.exe' -Destination '.\sirbeef-server-new.exe' -Force; Remove-Item '.\temp_bin' -Recurse -Force; Remove-Item 'sirbeef-server.zip' -Force; Set-Content -Path 'data\binary_version.txt' -Value $targetVer -NoNewline; Stop-Process -Name 'sirbeef-server' -Force -ErrorAction SilentlyContinue; Start-Sleep -Seconds 1; Move-Item -Path '.\sirbeef-server-new.exe' -Destination '.\sirbeef-server.exe' -Force; Write-Host ('[AUTO-UPDATE] Motor Sir Beef v' + $targetVer + ' atualizado com sucesso!') -ForegroundColor Green } } }"

tasklist /FI "IMAGENAME eq sirbeef-server.exe" 2>NUL | find /I /N "sirbeef-server.exe">NUL
if "%ERRORLEVEL%"=="0" (
    timeout /t 10 /nobreak >nul
    goto WATCHDOG_LOOP
)

tasklist /FI "IMAGENAME eq cloudflared.exe" 2>NUL | find /I /N "cloudflared.exe">NUL
if "%ERRORLEVEL%"=="1" (
    if exist "%~dp0backend\cloudflared.exe" (
        start "SirBeef Cloudflare Tunnel" /MIN "%~dp0backend\cloudflared.exe" tunnel --url http://localhost:8080 --metrics 127.0.0.1:20241 --no-autoupdate
    )
)

if exist "sirbeef-server-new.exe" (
    move /Y "sirbeef-server-new.exe" "sirbeef-server.exe" >nul 2>&1
)
.\sirbeef-server.exe
echo.
echo [GUARDIAO SIR BEEF - LOJA] O servidor foi encerrado. Reiniciando automaticamente em 3 segundos...
echo (Para fechar definitivamente, feche esta janela preta no X)
timeout /t 3 /nobreak >nul
goto WATCHDOG_LOOP
