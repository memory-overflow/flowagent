@echo off
REM ============================================================================
REM flowagent dependency installer (Windows)
REM
REM Installs everything needed to run flowagent:
REM   1) Go (check, guide if missing)        5) build backend to bin\
REM   2) Node.js (check, guide if missing)   6) frontend npm install
REM   3) Python (check, guide if missing)    7) project venv + base libs
REM   4) generate default config             8) skills runtime deps
REM
REM Usage: install.bat
REM Idempotent: safe to re-run.
REM ============================================================================
setlocal
cd /d "%~dp0"

echo === flowagent dependency installer ===

REM --- 1. Go ---
echo.
echo [1/8] Check Go ^(1.25+ required^)
where go >nul 2>nul
if %errorlevel%==0 (
    echo   [OK] Go found
) else (
    echo   [FAIL] Go not found. Install from https://go.dev/dl/ then re-run.
    exit /b 1
)

REM --- 2. Node.js ---
echo.
echo [2/8] Check Node.js ^(18+ required^)
where node >nul 2>nul
if %errorlevel%==0 (
    echo   [OK] Node.js found
) else (
    echo   [FAIL] Node.js not found. Install from https://nodejs.org/ then re-run.
    exit /b 1
)

REM --- 3. Python ---
echo.
echo [3/8] Check Python ^(3.10+ required^)
where python >nul 2>nul
if %errorlevel%==0 (
    echo   [OK] Python found
) else (
    echo   [WARN] Python not found - pdfkit-py will download its own runtime, continue.
)

REM --- 4. Generate default config ---
echo.
echo [4/8] Generate default config
if not exist config\config.yaml (
    copy config\config.example.yaml config\config.yaml >nul
    echo   [OK] config\config.yaml created from example ^(default sqlite^)
) else (
    echo   [OK] config\config.yaml already exists, skipped
)

REM --- 5. Build backend ---
echo.
echo [5/8] Build backend
if not exist bin mkdir bin
go build -trimpath -o bin\agent-server.exe ./cmd/server
if %errorlevel% neq 0 (
    echo   [FAIL] build agent-server failed
    exit /b 1
)
go build -trimpath -o bin\agent-scheduler.exe ./cmd/scheduler
if %errorlevel% neq 0 (
    echo   [FAIL] build agent-scheduler failed
    exit /b 1
)
echo   [OK] backend built

REM --- 6. Frontend deps ---
echo.
echo [6/8] Install frontend dependencies
cd frontend
call npm install --no-audit --no-fund
cd ..

REM --- 7. Project venv ---
echo.
echo [7/8] Create project venv ^(.venv + base libs^)
where python >nul 2>nul
if %errorlevel%==0 (
    if not exist .venv\Scripts\python.exe (
        python -m venv .venv
        echo   [OK] .venv created
    ) else (
        echo   [OK] .venv already exists
    )
    if not exist .venv\Scripts\python3.exe (
        copy .venv\Scripts\python.exe .venv\Scripts\python3.exe >nul
        echo   [OK] python3.exe created
    )
    .venv\Scripts\pip.exe install -r requirements-base.txt
    echo   [OK] base libs installed
    .venv\Scripts\pip.exe list --format=freeze 2>nul | findstr /I /B "requests== httpx== beautifulsoup4== lxml== PyYAML== pandas== openpyxl== python-dotenv== python-dateutil=="
) else (
    echo   [WARN] Python not found, skip project venv
)

REM --- 8. Skills runtime deps ---
echo.
echo [8/8] Install skills runtime dependencies
if exist skills\pdfkit-py\scripts\setup.bat (
    call skills\pdfkit-py\scripts\setup.bat
) else (
    echo   [WARN] skills\pdfkit-py\scripts\setup.bat not found, skipped.
)

echo.
echo === Done ===
echo Start with: start.bat
endlocal
