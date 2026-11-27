AirView Feeder for Windows
==========================

Feeds your ADS-B / Mode S receiver to an AirView aggregator. Installed by
AirView-Feeder-Setup: the feeder runs as the Windows service "AirView
Feeder" (account NT SERVICE\AirViewFeeder) and the tray icon starts when
you log on.

Tray icon (right-click)
-----------------------
  Open status page      http://127.0.0.1:8099/ (also a double-click)
  Settings...           aggregator, position, privacy; save, then the feeder
                        applies them
  Get claim code...     link the station to your account on the aggregator
  Pause / Resume        stop feeding until you resume (or Windows restarts)
  Restart feeder
  Open logs folder
  Start with Windows    the tray icon (the service always starts)
  Quit tray             the service keeps feeding

Where things are
----------------
  Programs       this folder
  Configuration  %ProgramData%\AirView Feeder\feeder.json
  Identity       %ProgramData%\AirView Feeder\identity.key (private)
  Logs           %ProgramData%\AirView Feeder\logs\feeder.log
                 (start, stop and failures also go to the Windows Event Log,
                 source "AirViewFeeder")
The data folder is readable only by SYSTEM, Administrators and the service.

Command line (administrator console, in this folder)
----------------------------------------------------
  airview-feeder setup                  change the configuration interactively
  airview-feeder config check           validate feeder.json
  airview-feeder service restart        apply a hand-edited feeder.json
  airview-feeder service status
  airview-feeder status                 is it connected?
  airview-feeder claim                  claim code
  airview-feeder help                   everything else

If the setup did not finish
---------------------------
Open the status page and use Settings, or run "airview-feeder setup" in an
administrator console, then "airview-feeder service restart".

Uninstalling
------------
Settings > Apps > AirView Feeder > Uninstall. The configuration and identity
are kept unless you choose to delete them; reinstalling then continues as the
same station. The feeder never changes readsb, dump1090 or other programs.
