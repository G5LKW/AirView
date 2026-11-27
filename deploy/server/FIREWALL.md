# AirView server: ports and firewall

A public AirView aggregator needs these ports reachable from the internet. Everything else stays closed.

| Port | Proto | Who connects | Setting (`/etc/airview/airview.jsonc`) | Open to |
|---|---|---|---|---|
| 22 | TCP | you (SSH) | – | your addresses if possible; **keep it open before enabling a firewall** |
| 80 | TCP | browsers; Let's Encrypt HTTP challenge | `http.port` (80 without a proxy) | everyone |
| 443 | TCP (+UDP for HTTP/3 with Caddy) | browsers | reverse proxy (`proxy/`) | everyone |
| 30160 | TCP | AirView feeders (AVF/1, TLS 1.3) | `aggregator.avfPort` | everyone (feeders authenticate with keys and enrol tokens) |
| 30004 | TCP | readsb / Ultrafeeder `beast_reduce_plus_out` (map only) | `aggregator.beastPort` | everyone, or known feeder addresses |
| 31090 | TCP | mlat-client (MLAT) | `aggregator.mlatPort` | everyone, or known feeder addresses |
| 31090 | UDP | mlat-client UDP transport, only with `aggregator.mlat.udp: true` | `aggregator.mlatPort` | as TCP 31090 |

Do **not** open:

| Port | Why |
|---|---|
| 8080 (with `install.sh --proxy`) | AirView's HTTP behind the proxy; bound to 127.0.0.1 anyway. |
| 30154 | Hub (AirView-to-AirView data link). In aggregator mode it accepts only the admin login; open it only to the addresses that need it. |
| `aggregator.beastOutPort`, `aggregator.sbsOutPort` | Merged outputs (0 = off). Protected by `aggregator.outputToken`; open only to specific consumers. |
| `aggregator.sbsInLocalPort` | Listens on 127.0.0.1 only. |

A feeder listener is disabled by setting its port to 0 (then also close it here). If you change a port, change the rules too.

## ufw (Ubuntu, Debian)

```sh
sudo ufw default deny incoming
sudo ufw default allow outgoing
sudo ufw allow OpenSSH                 # or: sudo ufw allow from <your-ip> to any port 22 proto tcp
sudo ufw allow 80/tcp
sudo ufw allow 443/tcp
sudo ufw allow 443/udp                 # HTTP/3 (Caddy); harmless otherwise
sudo ufw allow 30160/tcp comment 'AirView AVF/1 feeders'
sudo ufw allow 30004/tcp comment 'AirView Beast compat'
sudo ufw allow 31090/tcp comment 'AirView mlat-client'
# only with aggregator.mlat.udp = true:
# sudo ufw allow 31090/udp comment 'AirView mlat-client UDP'
# hub for one remote admin station only:
# sudo ufw allow from 203.0.113.7 to any port 30154 proto tcp
sudo ufw enable
sudo ufw status verbose
```

To accept compat feeds only from known feeders instead of everyone:

```sh
sudo ufw delete allow 30004/tcp
sudo ufw allow from 198.51.100.23 to any port 30004 proto tcp
```

## nftables (Debian 12+, RHEL 9+, any distribution)

`/etc/nftables.conf` (merge with existing rules if the host has any; `sudo nft -c -f /etc/nftables.conf` checks the syntax, `sudo systemctl enable --now nftables` loads it):

```nft
#!/usr/sbin/nft -f
flush ruleset

table inet filter {
    set airview_tcp {
        type inet_service
        elements = { 80, 443, 30160, 30004, 31090 }
    }

    chain input {
        type filter hook input priority filter; policy drop;
        ct state established,related accept
        ct state invalid drop
        iif lo accept
        meta l4proto { icmp, ipv6-icmp } accept
        tcp dport 22 accept                       # SSH: restrict with ip saddr if you can
        tcp dport @airview_tcp ct state new accept
        udp dport 443 accept                      # HTTP/3 (Caddy)
        # udp dport 31090 accept                  # only with aggregator.mlat.udp = true
        # ip saddr 203.0.113.7 tcp dport 30154 accept   # hub, one admin station
    }

    chain forward {
        type filter hook forward priority filter; policy drop;
    }

    chain output {
        type filter hook output priority filter; policy accept;
    }
}
```

`flush ruleset` also removes rules that Docker, libvirt or fail2ban added. On such hosts drop that line and add only the `table inet filter` part (under another table name, e.g. `table inet airview`).

### Rate limiting new feeder connections (optional)

AirView limits stations per IP itself (`aggregator.limits.perIpStations`). A cheap extra guard against connection floods:

```nft
tcp dport { 30160, 30004, 31090 } ct state new meter feeders { ip saddr limit rate over 30/minute } drop
```

## firewalld (RHEL, Fedora, openSUSE)

```sh
sudo firewall-cmd --permanent --add-service=http --add-service=https
sudo firewall-cmd --permanent --add-port=30160/tcp --add-port=30004/tcp --add-port=31090/tcp
sudo firewall-cmd --reload
```

## Cloud provider firewalls

Most VPS providers (Hetzner, AWS security groups, GCP, Oracle Cloud, DigitalOcean) filter in front of the machine as well. Open the same ports there; otherwise feeders time out even though the host firewall allows them. Oracle Cloud images also ship iptables rules that block everything but SSH. Remove those or add the ports there too.

## Docker

Docker publishes container ports through its own iptables chains, **before** ufw sees the packets. A `ports:` entry in docker-compose is reachable from the internet even when ufw denies the port. Publish only what should be public. Bind everything else to loopback, e.g. `"127.0.0.1:8080:8080"` when a reverse proxy on the host serves HTTPS (see `docker-compose.example.yml`).

## Check from outside

From another machine:

```sh
nc -vz feed.example.org 30160      # AVF/1
nc -vz feed.example.org 30004      # Beast
nc -vz feed.example.org 31090      # mlat-client
curl -sI https://feed.example.org/ | head -n 1
```

On the server, `sudo ss -ltnp | grep airview` lists what AirView is listening on.
