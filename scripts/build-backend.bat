@echo off
REM build-backend.bat - compile backend only (assumes backend/static was built by frontend)
REM Usage: scripts\build-backend.bat [--no-pause]
setlocal
set "ROOT=%~dp0.."
set "PAUSE_ON_END=1"
if /i "%~1"=="--no-pause" set "PAUSE_ON_END=0"

if not exist "%ROOT%\backend\static\index.html" (
    echo [WARN] backend/static is empty. Run "pnpm run build:fe" or scripts\build.bat first.
    echo        (go:embed needs static files to compile)
)

if not exist "%ROOT%\dist" mkdir "%ROOT%\dist"
pushd "%ROOT%\backend"
go build -trimpath -ldflags "-s -w" -o "%ROOT%\dist\web-rdp.exe" .
set ec=%errorlevel%
popd
if not "%ec%"=="0" goto :fail

echo Backend build done: %ROOT%\dist\web-rdp.exe
if "%PAUSE_ON_END%"=="0" goto :done
pause
:done
exit /b 0

:fail
echo.
echo BACKEND BUILD FAILED.
if "%PAUSE_ON_END%"=="0" goto :fail_done
pause
:fail_done
exit /b 1
