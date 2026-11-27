AirView {{.Title}} {{.Version}} - Raspberry Pi image
============================================================

This folder (airview) and the file vendor-data on this partition set the Pi
up on its first boot. Nothing here needs editing.

First boot (takes a few minutes on the first start):
  1. Raspberry Pi OS applies the settings you made in Raspberry Pi Imager
     (user, password or SSH key, Wi-Fi, time zone, host name).
  2. cloud-init runs vendor-data, which runs airview/firstboot.sh: it checks
     the files (SHA256SUMS), installs AirView to /opt and starts it.
  3. Then open:  {{.URL}}
     ({{.Hostname}} is the default host name; if you chose another one in
     Raspberry Pi Imager, use that name, or the Pi's IP address.)
{{if eq .Kind "feeder"}}
The feeder needs an aggregator address (and maybe an enrol token) from the
network you feed. Enter them on the setup page, or before the first boot in
airview/feeder-setup.txt (see the comments in that file).
{{else}}
Settings: the AirView web page (Settings), or the operator file
/etc/airview/airview.jsonc. To start with your own operator file, copy it
here as airview/airview.jsonc before the first boot.
{{end}}
After a successful installation the program files and any setup answers are
removed from this partition (they contain secrets such as enrol tokens).

Problems: log in (SSH or keyboard) and read
  /var/log/airview-firstboot.log
  journalctl -u {{.Unit}}
Run the setup again by hand:  sudo sh /boot/firmware/airview/firstboot.sh

Raspberry Pi OS: {{.Base}}
AirView is not affiliated with Raspberry Pi Ltd.
