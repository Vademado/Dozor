@echo off
setlocal
cd /d "%~dp0.."
if not exist "dist\go2rtc\go2rtc.exe" (
  echo go2rtc.exe not found in dist\go2rtc.
  pause
  exit /b 1
)
echo Camera: http://127.0.0.1:1984/stream.html?src=luckfox
echo Standalone go2rtc only. Press Ctrl+C to stop.
"dist\go2rtc\go2rtc.exe" -config "go2rtc-standalone.yaml" %*
if errorlevel 1 pause
