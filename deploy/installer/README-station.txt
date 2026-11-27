AirView for Windows
===================

ADS-B / Mode S statistics and mapping station. Installed by AirView-Setup.

Starting
--------
  Start menu > AirView > AirView (or the desktop shortcut). AirView opens in
  its own window (Microsoft Edge WebView2, part of Windows 11). Without
  WebView2 it opens in your default browser instead.

Where things are
----------------
  Programs       this folder
  Settings, history, accounts
                 %APPDATA%\AirView (your user profile; created at the first
                 start). AirView uses a "data" folder next to airview.exe
                 instead when that folder is writable, which it is not in
                 Program Files for normal users.
  Documentation  the "docs" folder here (Start menu > AirView > AirView
                 documentation): operator guide, feeder guide, privacy notice.

Other computers on the network
------------------------------
  The web interface listens on the HTTP port of the settings (default 80).
  Setup can add a Windows Firewall rule for it (private and domain networks
  only): "Allow other computers on the local network to connect".

Command line
------------
  airview -help                 all options
  airview -data <folder>        use another data folder
  airview backup -o file.zip    consistent backup of settings and databases
  airview restore file.zip      restore one

Uninstalling
------------
Settings > Apps > AirView > Uninstall. Your settings and history are kept
unless you tick "Also delete the configuration and data"; a later reinstall
continues where you left off.

Silent install (administrators)
-------------------------------
  AirView-Setup-<version>.exe /S [/NODESKTOP] [/FIREWALL [/PORT=8080]] [/D=C:\Program Files\AirView]
  "C:\Program Files\AirView\uninstall.exe" /S [/REMOVEDATA]
  AirView-Setup-<version>.exe /? lists every switch and the exit codes.
