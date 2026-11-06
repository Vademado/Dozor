@echo off
setlocal
if not defined CAMERA_RTSP_URL set "CAMERA_RTSP_URL=rtsp://95.163.220.75:8554/mystream"
cd /d "%~dp0.."
if not exist "dist\guard-admin.exe" (
  echo guard-admin.exe not found. Run scripts\build.cmd first.
  pause
  exit /b 1
)
echo Admin panel: http://127.0.0.1:8080
echo Keep your existing go2rtc window running on port 1984.
echo This server does not start or configure go2rtc. Press Ctrl+C to stop.
"dist\guard-admin.exe" -listen 127.0.0.1:8080 -data data %*
if errorlevel 1 pause
