@echo off
setlocal
cd /d "%~dp0.."
set "GUARD_GO=go"
if exist "..\.tools\go\bin\go.exe" set "GUARD_GO=..\.tools\go\bin\go.exe"
set "GOCACHE=%CD%\.cache\go-build"
set "GOMODCACHE=%CD%\.cache\go-mod"
set "GOTOOLCHAIN=local"
"%GUARD_GO%" test ./...
if errorlevel 1 exit /b 1
"%GUARD_GO%" vet ./...
