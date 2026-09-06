@echo off
REM build.bat - web-rdp one-click build (Windows entry)
REM   views pnpm build -> backend/static -> go build -s -w -trimpath -> dist/web-rdp.exe
REM Usage:
REM   scripts\build.bat            # manual run (pauses at the end)
REM   scripts\build.bat --no-pause # scripted run (pnpm build / CI)
setlocal
set "ROOT=%~dp0.."
set "PAUSE_ON_END=1"
if /i "%~1"=="--no-pause" set "PAUSE_ON_END=0"

echo ========================================
echo   web-rdp one-click build
echo ========================================
echo.

echo === [1/2] build frontend (views -^> backend/static) ===
pushd "%ROOT%\views"
call pnpm install
if errorlevel 1 goto :fail
call pnpm run build
if errorlevel 1 goto :fail
popd

echo.
echo === [2/2] compile backend -^> dist\web-rdp.exe ===
if not exist "%ROOT%\dist" mkdir "%ROOT%\dist"
pushd "%ROOT%\backend"
go build -trimpath -ldflags "-s -w" -o "%ROOT%\dist\web-rdp.exe" .
set ec=%errorlevel%
popd
if not "%ec%"=="0" goto :fail

echo.
echo Done: %ROOT%\dist\web-rdp.exe
if "%PAUSE_ON_END%"=="0" goto :done
pause
:done
exit /b 0

:fail
echo.
echo BUILD FAILED. See messages above.
if "%PAUSE_ON_END%"=="0" goto :fail_done
pause
:fail_done
exit /b 1
