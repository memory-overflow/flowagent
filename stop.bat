@echo off
REM ============================================================================
REM flowagent stop script (Windows)
REM
REM Stops the services started by start.bat for THIS project only:
REM   - agent-server / agent-scheduler: matched by executable path under
REM     this project's bin\ directory, so instances in other directories
REM     are left untouched.
REM   - frontend: stopped by the window title opened by start.bat.
REM
REM Usage: stop.bat
REM ============================================================================
setlocal
cd /d "%~dp0"

echo === flowagent stop ===

REM Stop backend by executable path under this project's bin\ directory.
powershell -NoProfile -Command "$bin = Join-Path (Get-Location) 'bin'; Get-Process agent-server,agent-scheduler -ErrorAction SilentlyContinue | Where-Object { $_.Path -and $_.Path.StartsWith($bin) } | ForEach-Object { Write-Host ('  stop ' + $_.Name + ' (pid ' + $_.Id + ')'); Stop-Process -Id $_.Id -Force }"

REM Stop the frontend window opened by start.bat.
taskkill /FI "WINDOWTITLE eq flowagent-frontend*" /F >nul 2>nul

echo.
echo Done.
endlocal
