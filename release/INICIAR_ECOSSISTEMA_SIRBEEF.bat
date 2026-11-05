@echo off
chcp 65001 >nul

:: ============================================================================
:: 1. EXECUCAO 100%% EM SEGUNDO PLANO (ZERO JANELAS NA TELA E ZERO NA BARRA DE TAREFAS)
:: ============================================================================
if "%~1" neq "--headless" (
    powershell -NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -Command "Start-Process cmd -ArgumentList '/c `\"`\"%~f0`\" --headless`\"' -WindowStyle Hidden"
    exit /b 0
)

:: ============================================================================
:: 2. AUTO-ELEVACAO ADMINISTRADOR OBRIGATORIA (UAC SILENCIOSO)
:: ============================================================================
net session >nul 2>&1
if %errorlevel% neq 0 (
    powershell -NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -Command "Start-Process cmd -ArgumentList '/c `\"`\"%~f0`\" --headless`\"' -WorkingDirectory '%~dp0' -Verb RunAs -WindowStyle Hidden"
    exit /b 0
)

cd /d "%~dp0"

:: ============================================================================
:: 3. DETECCAO DO AMBIENTE (LOJA PRODUCAO VS TESTE DEV)
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
    :: Na Hamburgueria: Inicializacao automatica com o Windows 100%% em segundo plano
    schtasks /Create /TN "SirBeef_AutoStart_Admin" /TR "powershell -NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -Command \"Start-Process cmd -ArgumentList '/c \\\"\\\"%~f0\\\" --headless\\\"' -WindowStyle Hidden\"" /SC ONLOGON /RL HIGHEST /F >nul 2>&1
    reg add "HKCU\Software\Microsoft\Windows\CurrentVersion\Run" /v "SirBeef_Server_Admin" /t REG_SZ /d "schtasks /Run /TN \"SirBeef_AutoStart_Admin\"" /f >nul 2>&1
)

:: ============================================================================
:: 4. CONFIGURACAO SILENCIOSA DE REDE PRIVADA E FIREWALL
:: ============================================================================
powershell -NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -Command "Get-NetConnectionProfile | Where-Object { $_.NetworkCategory -ne 'Private' } | ForEach-Object { Set-NetConnectionProfile -InterfaceIndex $_.InterfaceIndex -NetworkCategory Private -ErrorAction SilentlyContinue }" >nul 2>&1

netsh advfirewall firewall add rule name="SirBeef_Host_LAN_WiFi_8080" dir=in action=allow protocol=TCP localport=8080,80 profile=any >nul 2>&1
netsh advfirewall firewall add rule name="SirBeef_Host_LAN_WiFi_8080_UDP" dir=in action=allow protocol=UDP localport=8080 profile=any >nul 2>&1
netsh advfirewall firewall add rule name="SirBeef_Host_LAN_WiFi_8080_OUT" dir=out action=allow protocol=TCP localport=8080,80 profile=any >nul 2>&1
netsh advfirewall firewall add rule name="SirBeef_Host_LAN_WiFi_8080_EXE" dir=in action=allow program="%~dp0backend\sirbeef-server.exe" enable=yes profile=any >nul 2>&1

:: ============================================================================
:: 5. VERIFICA SE O SERVIDOR SIR BEEF JA ESTA EM EXECUCAO
:: ============================================================================
tasklist /FI "IMAGENAME eq sirbeef-server.exe" 2>nul | find /I /N "sirbeef-server.exe">nul
if "%ERRORLEVEL%"=="0" (
    exit /b 0
)

:: ============================================================================
:: 6. APLICA ATUALIZACAO PENDENTE SE HOUVER (sirbeef-server-new.exe)
:: ============================================================================
cd /d "%~dp0backend"
if exist "sirbeef-server-new.exe" (
    taskkill /F /IM sirbeef-server.exe >nul 2>&1
    ping 127.0.0.1 -n 2 >nul
    move /Y "sirbeef-server-new.exe" "sirbeef-server.exe" >nul 2>&1
)

:: ============================================================================
:: 7. DISPARA O SERVIDOR GO 100%% DESCONECTADO DO CONSOLE (WMI WIN32_PROCESS)
::    (Zero janelas, zero na barra de tarefas, o Go gerencia o tunel de forma oculta)
:: ============================================================================
powershell -NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -Command "$exe = '%~dp0backend\sirbeef-server.exe'; $dir = '%~dp0backend'; Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{CommandLine=$exe; CurrentDirectory=$dir}" >nul 2>&1

exit /b 0
