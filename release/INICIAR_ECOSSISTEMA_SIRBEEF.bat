@echo off
chcp 65001 >nul

cd /d "%~dp0"

:: ============================================================================
:: 1. AUTO-ELEVACAO ADMINISTRADOR OBRIGATORIA (SILENCIOSA)
:: ============================================================================
net session >nul 2>&1
if %errorlevel% neq 0 (
    powershell -NoProfile -ExecutionPolicy Bypass -Command "Start-Process cmd -ArgumentList '/c `\"`\"%~f0`\"`\"' -WorkingDirectory '%~dp0' -Verb RunAs -WindowStyle Hidden"
    exit /b 0
)

:: ============================================================================
:: 2. DETECCAO DO AMBIENTE (LOJA PRODUCAO VS TESTE DEV)
:: ============================================================================
set "ENV_MODE=PRODUCAO_LOJA"
if exist "%~dp0backend\data\server_env_mode.txt" (
    set /p ENV_MODE=<"%~dp0backend\data\server_env_mode.txt"
)

set "IS_TEST_MODE=0"
if /I "%ENV_MODE%"=="TESTE_DEV" set "IS_TEST_MODE=1"
if /I "%ENV_MODE%"=="TESTE_LOCAL" set "IS_TEST_MODE=1"

if "%IS_TEST_MODE%"=="1" (
    schtasks /Delete /TN "SirBeef_AutoStart_Admin" /F >nul 2>&1
    reg delete "HKCU\Software\Microsoft\Windows\CurrentVersion\Run" /v "SirBeef_Server_Admin" /f >nul 2>&1
) else (
    schtasks /Create /TN "SirBeef_AutoStart_Admin" /TR "\"%~dp0backend\sirbeef-server.exe\"" /SC ONLOGON /RL HIGHEST /F >nul 2>&1
    reg add "HKCU\Software\Microsoft\Windows\CurrentVersion\Run" /v "SirBeef_Server_Admin" /t REG_SZ /d "schtasks /Run /TN \"SirBeef_AutoStart_Admin\"" /f >nul 2>&1
)

:: ============================================================================
:: 3. CONFIGURACAO SILENCIOSA DE REDE PRIVADA E FIREWALL
:: ============================================================================
powershell -NoProfile -ExecutionPolicy Bypass -Command "Get-NetConnectionProfile | Where-Object { $_.NetworkCategory -ne 'Private' } | ForEach-Object { Set-NetConnectionProfile -InterfaceIndex $_.InterfaceIndex -NetworkCategory Private -ErrorAction SilentlyContinue }" >nul 2>&1

netsh advfirewall firewall add rule name="SirBeef_Host_LAN_WiFi_8080" dir=in action=allow protocol=TCP localport=8080,80 profile=any >nul 2>&1
netsh advfirewall firewall add rule name="SirBeef_Host_LAN_WiFi_8080_UDP" dir=in action=allow protocol=UDP localport=8080 profile=any >nul 2>&1
netsh advfirewall firewall add rule name="SirBeef_Host_LAN_WiFi_8080_OUT" dir=out action=allow protocol=TCP localport=8080,80 profile=any >nul 2>&1
netsh advfirewall firewall add rule name="SirBeef_Host_LAN_WiFi_8080_EXE" dir=in action=allow program="%~dp0backend\sirbeef-server.exe" enable=yes profile=any >nul 2>&1

:: ============================================================================
:: 4. APLICA ATUALIZACAO PENDENTE SE HOUVER (sirbeef-server-new.exe)
:: ============================================================================
cd /d "%~dp0backend"
if exist "sirbeef-server-new.exe" (
    taskkill /F /IM sirbeef-server.exe >nul 2>&1
    ping 127.0.0.1 -n 2 >nul
    move /Y "sirbeef-server-new.exe" "sirbeef-server.exe" >nul 2>&1
)

:: ============================================================================
:: 5. VERIFICA SE O SERVIDOR SIR BEEF JA ESTA EM EXECUCAO
:: ============================================================================
tasklist /FI "IMAGENAME eq sirbeef-server.exe" 2>nul | find /I "sirbeef-server.exe">nul
if "%ERRORLEVEL%"=="0" (
    exit /b 0
)

:: ============================================================================
:: 6. DISPARA O SERVIDOR GO 100%% EM SEGUNDO PLANO (NATIVO WINDOWS GUI SUBSYSTEM)
:: ============================================================================
start "" "%~dp0backend\sirbeef-server.exe"

exit /b 0
