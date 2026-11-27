@echo off
rem AirView Feeder: stop and remove the Windows service (keeps the configuration,
rem the identity and the logs; "airview-feeder uninstall-helper" lists them).
setlocal
cd /d "%~dp0"
net session >nul 2>&1
if errorlevel 1 (
  echo Asking Windows for administrator rights...
  powershell -NoProfile -ExecutionPolicy Bypass -Command "Start-Process -FilePath '%~f0' -Verb RunAs"
  exit /b
)
"%~dp0airview-feeder.exe" service remove
pause