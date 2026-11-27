@echo off
rem AirView Feeder: install and start the Windows service (portable ZIP).
rem The service runs airview-feeder.exe from this folder as NT SERVICE\AirViewFeeder;
rem the configuration and identity stay in %ProgramData%\AirView Feeder.
setlocal
cd /d "%~dp0"
net session >nul 2>&1
if errorlevel 1 (
  echo Asking Windows for administrator rights...
  powershell -NoProfile -ExecutionPolicy Bypass -Command "Start-Process -FilePath '%~f0' -Verb RunAs"
  exit /b
)
echo Closing the tray icon (it hands the feeder over to the service)...
"%~dp0airview-feeder.exe" tray -signal quit
"%~dp0airview-feeder.exe" service install -now
if errorlevel 1 (
  echo.
  echo The service was not installed. See the messages above.
  pause
  exit /b 1
)
echo.
echo Done. Start airview-feeder-tray.exe again for the tray icon.
pause