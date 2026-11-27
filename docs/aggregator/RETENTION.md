# Retention schedule

This is the retention schedule of an AirView aggregator (DESIGN §8.5, §20.2). It is published as part of the privacy notice ([PRIVACY-NOTICE.md](PRIVACY-NOTICE.md) §6). The defaults below are what AirView does when it is not configured otherwise. If you change a setting, change this schedule (and your published notice) in the same go.

The "Setting = default" column is checked by a test against the built-in defaults (`model.DefaultAggregator()`), so the numbers here cannot silently drift from the code.

**Legend.** *Automatic*: AirView deletes the data itself. *Operator*: you must configure something outside AirView. *Memory*: never written to disk.

---

## 1. Data held by AirView

| Data | Where | Kept for | Setting = default | How it is removed |
|---|---|---|---|---|
| Raw IP address of a feeder or visitor | process memory | while connected / for rate limiting | – | Memory: never written by AirView (§8.4) |
| Connection log: station, connect and disconnect time, protocol, bytes, HMAC of the IP with a daily salt | `stations.db` `conn_log` | 30 days | `aggregator.retention.connLogDays` = `30` | Automatic. The salt of each day is deleted when the day ends, so the hashes cannot be linked across days or recomputed afterwards. |
| Per-station minute statistics (rates, counters, clock health) | `stations.db` | 30 days | `aggregator.retention.minuteDays` = `30` | Automatic. 7 days when the aggregator runs from an SD card. |
| Per-station hourly statistics, range, coverage, signal, heat | `stations.db` | 13 months | `aggregator.retention.hourMonths` = `13` | Automatic |
| Location estimates (survey, Locate Me) | `stations.db` `location_estimates` | the latest 5 per method per station | `aggregator.retention.estimatesPerMethod` = `5` | Automatic, on each new estimate |
| Station record of an **unclaimed** station that is offline: identity key, UUID, name, declared and effective position, statistics | `stations.db` | 90 days after it was last seen | `aggregator.retention.offlinePurgeDays` = `90` | Automatic: the station is queued for deletion and every row about it is removed |
| Station record of a **claimed** station | `stations.db` | while the owner keeps it | – | Owner deletes it (immediate), or the admin does |
| Audit log (admin views of exact data, privacy changes, admissions, deletions) | `stations.db` `audit` | 24 months | `aggregator.retention.auditMonths` = `24` | Automatic. When a station or owner is deleted, their entries are pseudonymised at once. |
| Owner account: user name, password hash, TOTP secret, hashed recovery codes, creation and last-login time | `stations.db` `accounts` | until the owner deletes it | – | Owner **Delete account** (immediate) |
| Sessions (no IP stored) | `stations.db` `sessions` | owner: 30 days absolute / 14 days idle; admin: 12 hours / 120 minutes idle | `aggregator.auth.ownerSessionDays` = `30`, `aggregator.auth.ownerIdleDays` = `14`, `aggregator.auth.adminSessionHours` = `12`, `aggregator.auth.adminIdleMinutes` = `120` | Automatic expiry |
| Claim codes (hashed) | `stations.db` `claims` | 24 hours, single use | – | Expire; removed with the station |
| Enrol tokens (hashed, with the admin's note) | `stations.db` `enrol_tokens` | usable until expiry (default 7 days) or revocation; the record stays until the admin deletes it | – | Admin. A token record holds no feeder data, only the admin's note, use count and dates; keep notes free of personal details. |
| Merged network picture: aircraft positions and trails | `airview.db` | 30 days | `storage.trailRetentionDays` = `30` | Automatic |
| Flights, including which stations heard each flight (`flight_station`) | `airview.db` | 730 days | `storage.flightRetentionDays` = `730` | Automatic; `flight_station` rows of a deleted station are removed with it |
| Network-wide minute statistics (no station dimension) | `airview.db` | 90 days | `storage.minuteRetentionDays` = `90` | Automatic |
| MLAT quality statistics (1° cells, no station dimension) | `airview.db` | as hourly statistics | – | Automatic |
| MLAT timing data (per-station times of arrival, clock state) | process memory | seconds to minutes | – | Memory: never written to disk |
| MLAT console history: per station (by station ID) σθ, drift, clock outlier share, residual bias, contributions per hour; network-wide MLAT rates and hold-out error (no positions, no times of arrival) | process memory and mlat-history.gob.gz in the data directory | 2 hours per station, 24 hours network-wide | – | Automatic: the file is rewritten every 10 minutes and at shutdown. A deleted station's entries leave memory at the next one-minute sample and the file at the next save (at most 10 minutes). |
| Application log `airview.log` (no raw IPs, positions or per-station timing in aggregator mode) | data directory | 2 files × 4 MB, rotated | – | Automatic size rotation |

## 2. Data held outside AirView (operator duties)

| Data | Where | Recommended retention | What to configure |
|---|---|---|---|
| Backups of the data directory (contain everything above, encrypted) | your backup location | **30 days** | Rotate backups at 30 days ([OPERATOR-GUIDE.md](OPERATOR-GUIDE.md) §13.3). This is what makes "deleted within 30 days from backups" true. Owners are told this number in the deletion notice (consent text C8): set ggregator.retention.backupDays to your real rotation if it differs. |
| Reverse-proxy access logs (contain visitor IPs, URLs, user agents) | `/var/log/caddy`, `/var/log/nginx` | 14 days, or off | Caddy `roll_keep_for 336h`; nginx: logrotate `daily` + `rotate 14`, or `access_log off` ([OPERATOR-GUIDE.md](OPERATOR-GUIDE.md) §5.4) |
| systemd journal (start-up messages; the one-time admin setup token if used) | `/var/log/journal` | 30 days | `MaxRetentionSec=30day` in `journald.conf` |
| Firewall / fail2ban logs, if any | host | 30 days | your log rotation |
| Hosting provider's own logs | provider | per the provider's terms | name the provider in the notice |

## 3. Data held on the feeder

| Data | Where | Kept for |
|---|---|---|
| Identity key and UUID | feeder data directory | until uninstalled with `--purge` or replaced with `new-identity` |
| Unsent data while the network is unreachable | RAM ring (10 minutes, 12 MB) and, except on SD cards, a disk spool | until delivered, or overwritten oldest first; disk spool default 256 MB on Linux, 1 GB on Windows (`uplinks[].spoolMb` = `-1`, automatic) |
| Feeder logs | journal / Windows event log | the host's settings |

## 4. Deletion on request

- **Owner deletes account:** the account and sessions go at once; if the owner chose to delete their stations too, every row about them (positions, estimates, statistics, connection log, keys, public point) is removed by the deletion job, which survives restarts, and audit entries are pseudonymised. Otherwise the stations are released (unclaimed) and then follow the offline-purge rule.
- **Backups** keep a copy until they rotate out (30 days). If you restore a backup, re-apply the deletions made since it was taken.
- **Aircraft data** already merged into the network picture is not about the feeder owner and is not separated out on deletion; only the link between the station and the flights (`flight_station`) is removed.

## 5. Review

Review this schedule once a year and whenever you change a retention setting, the hosting, or the backup rotation. Record the date of the review here:

| Date | Reviewed by | Changes |
|---|---|---|
| [[date]] | [[name]] | first version |
