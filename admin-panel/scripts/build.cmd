@echo off
setlocal
cd /d "%~dp0.."
set "GUARD_GO=go"
if exist "..\.tools\go\bin\go.exe" set "GUARD_GO=..\.tools\go\bin\go.exe"
set "GOCACHE=%CD%\.cache\go-build"
set "GOMODCACHE=%CD%\.cache\go-mod"
set "GOTOOLCHAIN=local"
set "CGO_ENABLED=0"
if not exist dist mkdir dist
"%GUARD_GO%" build -trimpath -o dist\guard-admin.exe ./cmd/guard
if errorlevel 1 (
  echo Build failed. Install Go 1.26 or newer from https://go.dev/dl/
  pause
  exit /b 1
)
echo Build complete: dist\guard-admin.exe
"%GUARD_GO%" build -trimpath -o dist\streamcheck.exe ./cmd/streamcheck
if errorlevel 1 exit /b 1
echo RTSP diagnostics: dist\streamcheck.exe
