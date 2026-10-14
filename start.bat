@echo off
REM ============================================================================
REM flowagent one-command launcher (Windows)
REM
REM Launches two windows:
REM   agent-supervisor (hosts agent-server + agent-scheduler, auto-restart on
REM   exit, escape-restart endpoint :9090) + frontend (vite dev)
REM
REM Usage: start.bat
REM Prereq: run install.bat first; config\config.yaml is configured.
REM
REM The console window always waits for a key press before closing, whether the
REM launch succeeded or failed.
REM ============================================================================
setlocal
cd /d "%~dp0"

REM Use project-local portable runtimes installed by install.bat (if any).
if exist "%~dp0.tools\go\bin" set "PATH=%~dp0.tools\go\bin;%PATH%"
if exist "%~dp0.tools\node" set "PATH=%~dp0.tools\node;%PATH%"
if exist "%~dp0.tools\python" set "PATH=%~dp0.tools\python;%~dp0.tools\python\Scripts;%PATH%"

call :main %*
set "RC=%errorlevel%"

echo.
if "%RC%"=="0" (
    echo === Launch finished ===
    echo Close each of the opened windows to stop the corresponding process.
) else (
    echo === LAUNCH FAILED ^(exit code %RC%^) ===
    echo Fix the problem reported above, then run start.bat again.
)

echo.
echo Press any key to close this window ...
pause >nul
endlocal & exit /b %RC%

REM ---------------------------------------------------------------------------
REM  Main flow
REM ---------------------------------------------------------------------------
:main
echo === flowagent launcher ===

REM --- 1. Runtime check ---
set "GOVER="
for /f "tokens=3" %%v in ('go version 2^>nul') do if not defined GOVER set "GOVER=%%v"
if not defined GOVER (
    echo [FAIL] Go not found or not runnable. Run install.bat first.
    exit /b 1
)
set "NODEVER="
for /f "tokens=1" %%v in ('node --version 2^>nul') do if not defined NODEVER set "NODEVER=%%v"
if not defined NODEVER (
    echo [FAIL] Node.js not found or not runnable. Run install.bat first.
    exit /b 1
)
echo [OK] Go %GOVER% / Node %NODEVER%

REM --- 2. Config check ---
if not exist config\config.yaml (
    echo [FAIL] config\config.yaml not found.
    echo   Run: copy config\config.example.yaml config\config.yaml then edit it.
    exit /b 1
)

REM --- 3. Build if missing ---
if not exist bin\agent-supervisor.exe (
    echo Building backend...
    if not exist bin mkdir bin
    if not defined GOPROXY set "GOPROXY=https://goproxy.cn,https://proxy.golang.org,direct"
    go build -trimpath -o bin\agent-server.exe ./cmd/server
    go build -trimpath -o bin\agent-scheduler.exe ./cmd/scheduler
    go build -trimpath -o bin\agent-supervisor.exe ./cmd/supervisor
)

REM --- 4. Frontend deps ---
if not exist frontend\node_modules (
    echo Installing frontend dependencies...
    cd frontend
    call npm install --no-audit --no-fund
    cd ..
)

REM --- 5. Launch supervisor + frontend ---
echo Launching supervisor (server + scheduler) and frontend...
start "flowagent-supervisor" cmd /k "bin\agent-supervisor.exe -config config\config.yaml"
start "flowagent-frontend" cmd /k "cd frontend && npm run dev"

REM 前端起来后自动打开浏览器（vite 冷启动需数秒，后台延迟打开，失败不阻断）
start "" cmd /c "timeout /t 6 /nobreak >nul & start http://localhost:3000"

echo Done. The web UI will open at http://localhost:3000 automatically.
echo Close each window to stop the corresponding process.
exit /b 0
