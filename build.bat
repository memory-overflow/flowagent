@echo off
REM ============================================================================
REM flowagent build script (Windows)
REM
REM Compiles the backend binaries into bin\ and optionally builds the frontend.
REM
REM Usage:
REM   build.bat              build backend only (agent-server / agent-scheduler /
REM                          agent-supervisor) into bin\
REM   build.bat -f           also build the frontend (frontend\dist)
REM   build.bat --frontend   same as -f
REM   build.bat --all        same as -f
REM   build.bat -h           show help
REM
REM Prereq: run install.bat first so Go / Node.js are available.
REM
REM The console window always waits for a key press before closing, whether the
REM build succeeded or failed.
REM ============================================================================
setlocal EnableDelayedExpansion
cd /d "%~dp0"

REM Use project-local portable runtimes installed by install.bat (if any).
if exist "%~dp0.tools\go\bin" set "PATH=%~dp0.tools\go\bin;%PATH%"
if exist "%~dp0.tools\node" set "PATH=%~dp0.tools\node;%PATH%"
if exist "%~dp0.tools\python" set "PATH=%~dp0.tools\python;%~dp0.tools\python\Scripts;%PATH%"

set "RC=0"
call :main %*
set "RC=%errorlevel%"

echo.
if "%RC%"=="0" (
    if defined HELP_SHOWN (
        echo === Help shown ===
    ) else (
        echo === Build finished ===
        echo Binaries are in bin\.
    )
) else (
    echo === BUILD FAILED ^(exit code %RC%^) ===
    echo Fix the problem reported above, then run build.bat again.
)

echo.
echo Press any key to close this window ...
pause
endlocal & exit /b %RC%


REM ---------------------------------------------------------------------------
REM  Main flow
REM ---------------------------------------------------------------------------
:main
echo === flowagent build ===

REM --- 1. Parse arguments ---
set "BUILD_FRONTEND=0"
set "SHOW_USAGE=0"
set "HELP_SHOWN="
for %%a in (%*) do (
    set "ARG=%%a"
    set "KNOWN=0"
    if /i "%%a"=="-f"         set "KNOWN=1"
    if /i "%%a"=="--frontend" set "KNOWN=1"
    if /i "%%a"=="--all"      set "KNOWN=1"
    if /i "%%a"=="all"        set "KNOWN=1"
    if /i "%%a"=="-h"         set "KNOWN=1"
    if /i "%%a"=="--help"     set "KNOWN=1"
    if "!KNOWN!"=="0" (
        echo Unknown argument: !ARG!  ^(use -h for help^)
        exit /b 1
    )
    if /i "!ARG!"=="-f"         set "BUILD_FRONTEND=1"
    if /i "!ARG!"=="--frontend" set "BUILD_FRONTEND=1"
    if /i "!ARG!"=="--all"      set "BUILD_FRONTEND=1"
    if /i "!ARG!"=="all"        set "BUILD_FRONTEND=1"
    if /i "!ARG!"=="-h"         set "SHOW_USAGE=1"
    if /i "!ARG!"=="--help"     set "SHOW_USAGE=1"
)
if "!SHOW_USAGE!"=="1" (
    set "HELP_SHOWN=1"
    call :usage
    exit /b 0
)

REM --- 2. Runtime check ---
set "GOVER="
for /f "tokens=1,2,3" %%a in ('go version 2^>^&1') do if not defined GOVER if /i "%%a"=="go" if /i "%%b"=="version" set "GOVER=%%c"
if not defined GOVER (
    echo [FAIL] Go not found or not runnable. Run install.bat first.
    exit /b 1
)
echo [OK] Go %GOVER%

REM --- 3. Output directory ---
echo.
echo ==^> Creating output directory bin
if not exist bin mkdir bin

echo ==^> Building backend binaries ...
REM Keep the default GOPROXY chain China-friendly. Only set it when the user
REM has not already exported GOPROXY.
if not defined GOPROXY set "GOPROXY=https://goproxy.cn,https://proxy.golang.org,direct"

echo   -^> bin\agent-server.exe
call :go_build ./cmd/server bin\agent-server.exe
if errorlevel 1 (
    echo [FAIL] build agent-server failed
    exit /b 1
)

echo   -^> bin\agent-scheduler.exe
call :go_build ./cmd/scheduler bin\agent-scheduler.exe
if errorlevel 1 (
    echo [FAIL] build agent-scheduler failed
    exit /b 1
)

echo   -^> bin\agent-supervisor.exe
call :go_build ./cmd/supervisor bin\agent-supervisor.exe
if errorlevel 1 (
    echo [FAIL] build agent-supervisor failed
    exit /b 1
)

REM --- 4. Frontend (optional) ---
if "!BUILD_FRONTEND!"=="1" (
    echo.
    echo ==^> Building frontend ...
    if not exist frontend\node_modules (
        echo   [WARN] frontend\node_modules missing, running npm install first ...
        pushd frontend
        call npm install --no-audit --no-fund
        popd
    )
    pushd frontend
    call npm run build
    if errorlevel 1 (
        popd
        echo [FAIL] frontend build failed
        exit /b 1
    )
    popd
    echo   [OK] frontend built to frontend\dist
)

echo.
echo ==^> Build complete
echo.
echo Contents of bin\:
dir /b bin
exit /b 0


REM ---------------------------------------------------------------------------
REM  go build with a default GOPROXY chain (China-friendly)
REM  %1 = package path, %2 = output path
REM ---------------------------------------------------------------------------
:go_build
REM GOTOOLCHAIN=auto: auto-download the toolchain pinned by go.mod when the
REM local Go is older than required (matches build.sh).
set "GOTOOLCHAIN=auto"
go build -trimpath -o "%~2" %~1
exit /b %errorlevel%


REM ---------------------------------------------------------------------------
REM  Help
REM ---------------------------------------------------------------------------
:usage
echo flowagent build script: backend binaries into bin\, optional frontend build.
echo.
echo Usage:
echo   build.bat              build backend only into bin\
echo   build.bat -f           also build the frontend ^(frontend\dist^)
echo   build.bat --frontend   same as -f
echo   build.bat --all        same as -f
exit /b 0
