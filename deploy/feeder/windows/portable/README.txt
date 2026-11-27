AirView Feeder for Windows - portable ZIP
==========================================

Feeds your ADS-B / Mode S receiver (Radarcape, a Beast, readsb or dump1090
on the network, airspy_adsb, dump1090-win, ...) to an AirView aggregator.

Files
-----
  airview-feeder.exe        the feeder (command line; also the Windows service)
  airview-feeder-tray.exe   the same program without a console window: the
                            notification-area (tray) icon
  airview-feeder.ico        icon for shortcuts
  Install service.cmd       install and start the Windows service (asks for
                            administrator rights)
  Remove service.cmd        stop and remove the service (keeps the configuration)

Quick start (no administrator rights needed)
--------------------------------------------
1. Unzip the folder anywhere, for example to Documents\AirView Feeder.
2. Double-click airview-feeder-tray.exe. The icon appears in the notification
   area (click the ^ arrow next to the clock if it is hidden). Without the
   service, the tray runs the feeder itself while you are logged on.
3. The status page opens in your browser the first time
   (http://127.0.0.1:8099/). Under Settings, enter the aggregator from your
   network's "Feed this network" page, your antenna position and the privacy
   choices, then save.
4. Right-click the icon > "Get claim code..." and enter the code on the
   aggregator's claim page to link the station to your account.
5. Right-click > "Start with Windows" to start the tray when you log on.

To feed when nobody is logged on, install the Windows service:
right-click the icon > "Install as Windows service...", or run
"Install service.cmd". The service uses the same configuration and identity.

Where things are
----------------
  Configuration  %ProgramData%\AirView Feeder\feeder.json
  Identity       %ProgramData%\AirView Feeder\identity.key (private; keep it)
  Logs           %ProgramData%\AirView Feeder\logs\feeder.log
Other locations: airview-feeder-tray.exe -config <file> -data <dir>.

Command line (in a console in this folder)
------------------------------------------
  airview-feeder setup                 interactive setup in the console
  airview-feeder status                is it connected?
  airview-feeder claim                 claim code
  airview-feeder config check          validate feeder.json
  airview-feeder service install -now  install and start the service (administrator)
  airview-feeder service status|pause|continue|restart|remove
  airview-feeder uninstall-helper      list the feeder's own files
  airview-feeder help                  everything else

Windows SmartScreen
-------------------
If the programs are not signed, Windows may say "Windows protected your PC"
the first time. Check that the ZIP came from your network's own page, then
click "More info" > "Run anyway". Right-click the ZIP > Properties >
"Unblock" before unzipping avoids the question for every file.

Removing
--------
Quit the tray (right-click > Quit), run "Remove service.cmd" if you installed
the service, run "airview-feeder uninstall-helper -purge -yes" if you also
want the configuration and identity gone, then delete this folder.
The feeder never changes readsb, dump1090 or any other program.
