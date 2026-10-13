@echo off
REM ============================================================================
REM flowagent dependency installer (Windows)
REM
REM Steps:
REM   1) Go (auto install)   2) Node.js (auto install)   3) Python (auto install)
REM   4) generate default config
REM   5) build backend       6) frontend npm install
REM   7) project venv        8) skills runtime deps
REM
REM Go / Node.js / Python are downloaded automatically when missing or when the
REM installed version is too old. They are unpacked into ".tools\" inside this
REM project (portable, no admin rights, nothing written outside the project).
REM
REM Usage: install.bat
REM        install.bat --reinstall-tools   force a fresh download of Go/Node/Python
REM Idempotent: safe to re-run. Already-satisfied requirements are skipped.
REM
REM NOTE: this script deliberately avoids ">nul" / "2>nul" redirection. On some
REM       systems the NUL device is unavailable, which makes cmd print
REM       "The system cannot find the file specified." and SKIP the command
REM       entirely - that silently breaks version probes. Output is captured
REM       with "for /f" and validated token by token instead.
REM
REM The console window always waits for a key press before closing, whether the
REM installation succeeded or failed.
REM ============================================================================
setlocal EnableDelayedExpansion
cd /d "%~dp0"

set "ROOT=%~dp0"
if "!ROOT:~-1!"=="\" set "ROOT=!ROOT:~0,-1!"
set "TOOLS_DIR=!ROOT!\.tools"

set "RC=0"
call :main %*
set "RC=%errorlevel%"

echo.
if "%RC%"=="0" (
    echo === All steps completed ===
    echo Next step: run start.bat
) else (
    echo === INSTALL FAILED ^(exit code %RC%^) ===
    echo Fix the problem reported above, then run install.bat again.
)

echo.
echo Press any key to close this window ...
pause
endlocal & exit /b %RC%


REM ---------------------------------------------------------------------------
REM  Main flow
REM ---------------------------------------------------------------------------
:main
echo === flowagent dependency installer ===

REM --reinstall-tools : always (re)download Go / Node.js / Python into .tools
set "FORCE_TOOLS=0"
for %%a in (%*) do if /i "%%a"=="--reinstall-tools" set "FORCE_TOOLS=1"

call :pick_tar

REM Make project-local portable runtimes visible for this session.
call :use_local_tools

REM --- 1. Go ---
echo.
echo [1/8] Check Go
call :ensure_go
if errorlevel 1 (
    echo   [FAIL] Go is unavailable.
    echo          Install it manually from https://go.dev/dl/ then re-run.
    exit /b 1
)

REM --- 2. Node.js ---
echo.
echo [2/8] Check Node.js ^(18+ required^)
call :ensure_node
if errorlevel 1 (
    echo   [FAIL] Node.js is unavailable.
    echo          Install it manually from https://nodejs.org/ then re-run.
    exit /b 1
)

REM --- 3. Python ---
echo.
echo [3/8] Check Python ^(3.10+ required^)
call :ensure_python
if errorlevel 1 (
    echo   [WARN] Python 3.10+ unavailable.
    echo          pdfkit-py will try to download its own runtime in step 8.
)

REM --- 4. Generate default config ---
echo.
echo [4/8] Generate default config
if not exist config\config.yaml (
    copy /y config\config.example.yaml config\config.yaml
    echo   [OK] config\config.yaml created from example ^(default sqlite^)
) else (
    echo   [OK] config\config.yaml already exists, skipped
)

REM --- 5. Build backend ---
echo.
echo [5/8] Build backend
if not exist bin mkdir bin
call :go_build ./cmd/server bin\agent-server.exe
if errorlevel 1 (
    echo   [FAIL] build agent-server failed
    exit /b 1
)
call :go_build ./cmd/scheduler bin\agent-scheduler.exe
if errorlevel 1 (
    echo   [FAIL] build agent-scheduler failed
    exit /b 1
)
call :go_build ./cmd/supervisor bin\agent-supervisor.exe
if errorlevel 1 (
    echo   [FAIL] build agent-supervisor failed
    exit /b 1
)
echo   [OK] backend built

REM --- 6. Frontend deps ---
echo.
echo [6/8] Install frontend dependencies
cd frontend
call npm install --no-audit --no-fund
if !errorlevel! NEQ 0 (
    echo   [--] npm install failed, retrying with registry.npmmirror.com ...
    call npm install --no-audit --no-fund --registry=https://registry.npmmirror.com
)
cd ..
if not exist frontend\node_modules (
    echo   [WARN] frontend\node_modules not found, frontend may not work.
)

REM --- 7. Project venv ---
echo.
echo [7/8] Create project venv ^(.venv + base libs^)
set "PYOK="
for /f "tokens=1,2" %%a in ('python --version 2^>^&1') do if not defined PYOK if /i "%%a"=="Python" set "PYOK=%%b"
if defined PYOK (
    if not exist .venv\Scripts\python.exe (
        python -m venv .venv
        echo   [OK] .venv created
    ) else (
        echo   [OK] .venv already exists
    )
    if not exist .venv\Scripts\python3.exe (
        copy /y .venv\Scripts\python.exe .venv\Scripts\python3.exe
        echo   [OK] python3.exe created
    )
    .venv\Scripts\pip.exe install -r requirements-base.txt
    echo   [OK] base libs installed
    .venv\Scripts\pip.exe list --format=freeze | findstr /I /B "requests== httpx== beautifulsoup4== lxml== PyYAML== pandas== openpyxl== python-dotenv== python-dateutil=="
) else (
    echo   [WARN] Python not found, skip project venv
)

REM --- 8. Skills runtime deps ---
echo.
echo [8/8] Install skills runtime dependencies
if exist skills\pdfkit-py\scripts\setup.bat (
    call skills\pdfkit-py\scripts\setup.bat --no-pause
) else (
    echo   [WARN] skills\pdfkit-py\scripts\setup.bat not found, skipped.
)

echo.
echo === Done ===
echo Start with: start.bat
exit /b 0


REM ===========================================================================
REM  Helpers
REM ===========================================================================

REM ---------------------------------------------------------------------------
REM  Locate a tar that can read .zip (the Windows built-in bsdtar).
REM  A GNU tar shipped with Git/MSYS cannot, even though it is named tar.
REM ---------------------------------------------------------------------------
:pick_tar
set "BSDTAR="
if exist "%SystemRoot%\System32\tar.exe" set "BSDTAR=%SystemRoot%\System32\tar.exe"
if not defined BSDTAR if exist "C:\Windows\System32\tar.exe" set "BSDTAR=C:\Windows\System32\tar.exe"
exit /b 0

REM ---------------------------------------------------------------------------
REM  Put project-local portable runtimes first on PATH (this session only).
REM ---------------------------------------------------------------------------
:use_local_tools
if exist "!TOOLS_DIR!\go\bin" set "PATH=!TOOLS_DIR!\go\bin;!PATH!"
if exist "!TOOLS_DIR!\node"   set "PATH=!TOOLS_DIR!\node;!PATH!"
if exist "!TOOLS_DIR!\python" set "PATH=!TOOLS_DIR!\python;!TOOLS_DIR!\python\Scripts;!PATH!"
exit /b 0

REM ---------------------------------------------------------------------------
REM  Detect CPU architecture -> ARCH (node naming) / GOARCH / PBSARCH
REM ---------------------------------------------------------------------------
:detect_arch
set "ARCH=x64"
set "GOARCH=amd64"
set "PBSARCH=x86_64"
if /i "%PROCESSOR_ARCHITECTURE%"=="ARM64" (
    set "ARCH=arm64"
    set "GOARCH=arm64"
    set "PBSARCH=aarch64"
)
exit /b 0

REM ---------------------------------------------------------------------------
REM  ver_ge <actual> <required>  ->  errorlevel 0 when actual >= required
REM ---------------------------------------------------------------------------
:ver_ge
setlocal
for /f "tokens=1,2,3 delims=." %%a in ("%~1") do (
    set "A1=%%a" & set "A2=%%b" & set "A3=%%c"
)
for /f "tokens=1,2,3 delims=." %%a in ("%~2") do (
    set "B1=%%a" & set "B2=%%b" & set "B3=%%c"
)
for %%x in (A1 A2 A3 B1 B2 B3) do if not defined %%x set "%%x=0"
set "R=1"
if !A1! GTR !B1! set "R=0"
if !A1! EQU !B1! if !A2! GTR !B2! set "R=0"
if !A1! EQU !B1! if !A2! EQU !B2! if !A3! GEQ !B3! set "R=0"
endlocal & exit /b %R%

REM ---------------------------------------------------------------------------
REM  download <url> <dest>   - curl first, PowerShell as fallback
REM ---------------------------------------------------------------------------
:download
if not exist "!TOOLS_DIR!" mkdir "!TOOLS_DIR!"
echo   URL: %~1
echo   Downloading ...
curl -L --fail --retry 3 --connect-timeout 30 -o "%~2" "%~1"
call :is_real_file "%~2"
if !errorlevel! EQU 0 exit /b 0
echo   [--] curl failed, retrying with PowerShell ...
powershell -NoProfile -ExecutionPolicy Bypass -Command "$ProgressPreference='SilentlyContinue'; [Net.ServicePointManager]::SecurityProtocol=[Net.SecurityProtocolType]::Tls12; try { Invoke-WebRequest -Uri '%~1' -OutFile '%~2' -UseBasicParsing } catch { exit 1 }"
call :is_real_file "%~2"
if !errorlevel! EQU 0 exit /b 0
if exist "%~2" del "%~2"
echo   [FAIL] download failed.
exit /b 1

:is_real_file
for %%s in ("%~1") do if %%~zs GTR 200000 exit /b 0
exit /b 1

REM ---------------------------------------------------------------------------
REM  Go
REM ---------------------------------------------------------------------------
:ensure_go
REM Read the "go <version>" line straight from go.mod (no command, no quotes).
set "REQ="
if exist go.mod for /f "tokens=1,2" %%a in (go.mod) do if /i "%%a"=="go" if not defined REQ set "REQ=%%b"
if not defined REQ set "REQ=1.25.0"
if "!FORCE_TOOLS!"=="1" goto :go_install

set "GV="
for /f "tokens=1,2,3" %%a in ('go version 2^>^&1') do if not defined GV if /i "%%a"=="go" if /i "%%b"=="version" set "GV=%%c"
if defined GV (
    set "GN=!GV:go=!"
    call :ver_ge "!GN!" "!REQ!"
    if !errorlevel! EQU 0 (
        echo   [OK] Go !GN! found, meets requirement !REQ!
        exit /b 0
    )
    echo   [--] Go !GN! found but older than !REQ! required by go.mod.
) else (
    echo   [--] Go not found.
)

:go_install
echo   Installing portable Go !REQ! into .tools\go ...
call :install_go "!REQ!"
if errorlevel 1 exit /b 1
set "PATH=!TOOLS_DIR!\go\bin;!PATH!"
set "GV="
for /f "tokens=1,2,3" %%a in ('go version 2^>^&1') do if not defined GV if /i "%%a"=="go" if /i "%%b"=="version" set "GV=%%c"
if defined GV (echo   [OK] Go !GV! ready) else (echo   [FAIL] Go installed but not runnable & exit /b 1)
exit /b 0

:install_go
call :detect_arch
call :download "https://go.dev/dl/go%~1.windows-!GOARCH!.zip" "!TOOLS_DIR!\go.zip"
if errorlevel 1 exit /b 1
if exist "!TOOLS_DIR!\go" rmdir /s /q "!TOOLS_DIR!\go"
echo   Extracting ...
if defined BSDTAR "!BSDTAR!" -xf "!TOOLS_DIR!\go.zip" -C "!TOOLS_DIR!"
if not exist "!TOOLS_DIR!\go\bin\go.exe" (
    echo   [--] tar failed, retrying with PowerShell ...
    powershell -NoProfile -ExecutionPolicy Bypass -Command "Expand-Archive -LiteralPath '!TOOLS_DIR!\go.zip' -DestinationPath '!TOOLS_DIR!' -Force"
)
if exist "!TOOLS_DIR!\go.zip" del "!TOOLS_DIR!\go.zip"
if not exist "!TOOLS_DIR!\go\bin\go.exe" (
    echo   [FAIL] extracting Go failed.
    exit /b 1
)
exit /b 0

REM ---------------------------------------------------------------------------
REM  Node.js
REM ---------------------------------------------------------------------------
:ensure_node
if "!FORCE_TOOLS!"=="1" goto :node_install

set "NV="
for /f "tokens=1" %%a in ('node --version 2^>^&1') do if not defined NV set "NV=%%a"
if defined NV if not "!NV:~0,1!"=="v" set "NV="
if defined NV (
    set "NN=!NV:v=!"
    call :ver_ge "!NN!" "18.0.0"
    if !errorlevel! EQU 0 (
        echo   [OK] Node.js !NV! found, meets requirement 18+
        exit /b 0
    )
    echo   [--] Node.js !NV! found but older than 18 required.
) else (
    echo   [--] Node.js not found.
)

:node_install
echo   Installing portable Node.js 22 LTS into .tools\node ...
call :install_node
if errorlevel 1 exit /b 1
set "PATH=!TOOLS_DIR!\node;!PATH!"
set "NV="
for /f "tokens=1" %%a in ('node --version 2^>^&1') do if not defined NV set "NV=%%a"
if defined NV if not "!NV:~0,1!"=="v" set "NV="
if defined NV (echo   [OK] Node.js !NV! ready) else (echo   [FAIL] Node.js installed but not runnable & exit /b 1)
exit /b 0

:install_node
call :detect_arch
echo   Resolving latest Node.js 22 LTS package ...
set "NF="
for /f "tokens=2" %%f in ('curl -s -L https://nodejs.org/dist/latest-v22.x/SHASUMS256.txt ^| findstr /i win-!ARCH!.zip') do if not defined NF set "NF=%%f"
if not defined NF (
    echo   [FAIL] cannot resolve the Node.js package name.
    exit /b 1
)
echo   Package: !NF!
call :download "https://nodejs.org/dist/latest-v22.x/!NF!" "!TOOLS_DIR!\node.zip"
if errorlevel 1 exit /b 1
if exist "!TOOLS_DIR!\node" rmdir /s /q "!TOOLS_DIR!\node"
mkdir "!TOOLS_DIR!\node"
echo   Extracting ...
if defined BSDTAR "!BSDTAR!" -xf "!TOOLS_DIR!\node.zip" -C "!TOOLS_DIR!\node" --strip-components=1
if not exist "!TOOLS_DIR!\node\node.exe" (
    echo   [--] tar failed, retrying with PowerShell ...
    if exist "!TOOLS_DIR!\node-tmp" rmdir /s /q "!TOOLS_DIR!\node-tmp"
    powershell -NoProfile -ExecutionPolicy Bypass -Command "Expand-Archive -LiteralPath '!TOOLS_DIR!\node.zip' -DestinationPath '!TOOLS_DIR!\node-tmp' -Force"
    for /d %%d in ("!TOOLS_DIR!\node-tmp\node-*") do xcopy /e /i /q /y "%%d\*" "!TOOLS_DIR!\node\"
    if exist "!TOOLS_DIR!\node-tmp" rmdir /s /q "!TOOLS_DIR!\node-tmp"
)
if exist "!TOOLS_DIR!\node.zip" del "!TOOLS_DIR!\node.zip"
if not exist "!TOOLS_DIR!\node\node.exe" (
    echo   [FAIL] extracting Node.js failed.
    exit /b 1
)
exit /b 0

REM ---------------------------------------------------------------------------
REM  Python
REM ---------------------------------------------------------------------------
:ensure_python
if "!FORCE_TOOLS!"=="1" goto :python_install

set "PV="
for /f "tokens=1,2" %%a in ('python --version 2^>^&1') do if not defined PV if /i "%%a"=="Python" set "PV=%%b"
if defined PV (
    call :ver_ge "!PV!" "3.10.0"
    if !errorlevel! EQU 0 (
        echo   [OK] Python !PV! found, meets requirement 3.10+
        exit /b 0
    )
    echo   [--] Python !PV! found but older than 3.10 required.
) else (
    echo   [--] Python not found.
)

:python_install
echo   Installing portable Python into .tools\python ...
call :install_python
if errorlevel 1 exit /b 1
set "PATH=!TOOLS_DIR!\python;!TOOLS_DIR!\python\Scripts;!PATH!"
set "PV="
for /f "tokens=1,2" %%a in ('python --version 2^>^&1') do if not defined PV if /i "%%a"=="Python" set "PV=%%b"
if defined PV (echo   [OK] Python !PV! ready) else (echo   [FAIL] Python installed but not runnable & exit /b 1)
exit /b 0

:install_python
call :detect_arch
set "PVER=3.13.13"
set "PTAG=20260414"
call :download "https://github.com/astral-sh/python-build-standalone/releases/download/!PTAG!/cpython-!PVER!+!PTAG!-!PBSARCH!-pc-windows-msvc-install_only.tar.gz" "!TOOLS_DIR!\python.tar.gz"
if errorlevel 1 exit /b 1
if exist "!TOOLS_DIR!\python" rmdir /s /q "!TOOLS_DIR!\python"
mkdir "!TOOLS_DIR!\python"
echo   Extracting ...
if defined BSDTAR (
    "!BSDTAR!" -xzf "!TOOLS_DIR!\python.tar.gz" -C "!TOOLS_DIR!\python" --strip-components=1
) else (
    tar -xzf "!TOOLS_DIR!\python.tar.gz" -C "!TOOLS_DIR!\python" --strip-components=1
)
if exist "!TOOLS_DIR!\python.tar.gz" del "!TOOLS_DIR!\python.tar.gz"
if not exist "!TOOLS_DIR!\python\python.exe" (
    echo   [FAIL] extracting Python failed.
    exit /b 1
)
exit /b 0

REM ---------------------------------------------------------------------------
REM  go build with a China proxy fallback
REM ---------------------------------------------------------------------------
:go_build
go build -trimpath -o "%~2" %~1
if !errorlevel! EQU 0 exit /b 0
if not defined GOPROXY (
    echo   [--] go build failed, retrying with GOPROXY=https://goproxy.cn,direct
    set "GOPROXY=https://goproxy.cn,direct"
    go build -trimpath -o "%~2" %~1
    if !errorlevel! EQU 0 exit /b 0
)
exit /b 1
