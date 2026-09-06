@echo off
REM dev-backend.bat - run backend via go run for quick debugging
REM   If backend/static is missing (fresh clone / cleared), build the frontend once first,
REM   otherwise go:embed compilation will fail.
setlocal
set "ROOT=%~dp0.."

if not exist "%ROOT%\backend\static\index.html" (
    echo [dev-backend] backend/static missing, building frontend first...
    pushd "%ROOT%\views"
    call pnpm install
    if errorlevel 1 goto :fail
    call pnpm run build
    if errorlevel 1 goto :fail
    popd
)

echo [dev-backend] go run backend (Ctrl+C to stop)
pushd "%ROOT%\backend"
go run .
set ec=%errorlevel%
popd
exit /b %ec%

:fail
echo [dev-backend] frontend build failed
popd
exit /b 1
