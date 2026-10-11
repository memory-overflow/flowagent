@echo off
REM ============================================================================
REM flowagent one-command launcher (Windows)
REM
REM Launches three windows:
REM   agent-server (HTTP API) + agent-scheduler + frontend (vite dev)
REM
REM Usage: start.bat
REM Prereq: run install.bat first; config\config.yaml is configured.
REM ============================================================================
setlocal
cd /d "%~dp0"

echo === flowagent launcher ===

REM --- 1. Runtime check ---
where go >nul 2>nul
if %errorlevel% neq 0 (
    echo [FAIL] Go not found. Run install.bat first.
    exit /b 1
)
where node >nul 2>nul
if %errorlevel% neq 0 (
    echo [FAIL] Node.js not found. Run install.bat first.
    exit /b 1
)

REM --- 2. Config check ---
if not exist config\config.yaml (
    echo [FAIL] config\config.yaml not found.
    echo   Run: copy config\config.example.yaml config\config.yaml then edit it.
    exit /b 1
)

REM --- 3. Build if missing ---
if not exist bin\agent-server.exe (
    echo Building backend...
    if not exist bin mkdir bin
    go build -trimpath -o bin\agent-server.exe ./cmd/server
    go build -trimpath -o bin\agent-scheduler.exe ./cmd/scheduler
)

REM --- 4. Frontend deps ---
if not exist frontend\node_modules (
    echo Installing frontend dependencies...
    cd frontend
    call npm install --no-audit --no-fund
    cd ..
)

REM --- 5. Launch three windows ---
echo Launching agent-server, agent-scheduler and frontend...
start "flowagent-server" cmd /k "bin\agent-server.exe -config config\config.yaml"
start "flowagent-scheduler" cmd /k "bin\agent-scheduler.exe -config config\config.yaml"
start "flowagent-frontend" cmd /k "cd frontend && npm run dev"

echo Done. Close each window to stop the corresponding process.
endlocal
