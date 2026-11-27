# deploy/release - signing and publishing AirView releases

`deploy/release` is the release tool for the signed self-update of `airview`
and `airview-feeder` (`internal/selfupdate`). It creates the Ed25519 release
key, writes signed release directories, and verifies them. Build it once and
keep it on the release machine:

```
go build -o avrelease ./deploy/release        # avrelease.exe on Windows
```

(`go run ./deploy/release <command> ...` works too.)

## 1. Create the release key (once)

```
avrelease keygen -out ~/airview-keys            # writes airview-release.key (0600) and .pub
```

- `airview-release.key` is the private key (PKCS#8 PEM). Keep it offline
  (encrypted USB stick, password manager file attachment, HSM export) and
  back it up. Anyone with this file can push code to every feeder that
  trusts the key. It never goes onto the aggregator server.
- `airview-release.pub` holds the public key as one base64 line (44
  characters). That value goes into:
  - `update.keys` of airview / airview-feeder configurations,
  - the Pi installer (`--manifest-key`, or `PRESET_MANIFEST_KEY` filled in
    by the aggregator),
  - the Windows feeder installer.
- `avrelease pubkey -key airview-release.key` prints it again.
- keygen refuses to overwrite existing key files.

**Rotation:** create a new key, ship a release whose configurations/installers
list both public keys (`"keys": ["OLD", "NEW"]`), sign the following releases
with the new key, and drop the old key from the configurations later. A
compromised key is removed the same way; clients that only trust the
compromised key must be updated by hand (installer).

## 2. Publish a release

Build first (`deploy/build.ps1` names outputs `<product>-<target>[.exe]`),
then:

```
avrelease publish -key ~/airview-keys/airview-release.key \
    -product airview-feeder -version 1.4.0 -channel stable \
    -dist dist-build/ -out releases/feeder \
    [-notes "Fixes the Radarcape reconnect"] [-expires-days 90]
```

- `-dist DIR` picks up `airview-feeder-linux-arm64`, `-linux-armv7`,
  `-linux-amd64`, `-windows-amd64.exe`, ... (for `-product airview` also
  `airview.exe` as windows-amd64). `-file target=path` adds or overrides
  single artifacts.
- Every artifact must be an executable for its target OS (ELF / PE /
  Mach-O); size and SHA-256 are recorded in the manifest.
- Output layout (what `selfupdate.ReleaseHandler` and the Pi installer
  expect):

  ```
  releases/feeder/manifest.json
  releases/feeder/manifest.json.sig
  releases/feeder/dl/<target>
  releases/feeder/beta/...            (-channel beta)
  ```

- The tool verifies its own signature before writing, copies the artifacts
  first and writes the signature before the manifest, so a client never sees
  a manifest whose files are missing.
- `-expires-days N` makes clients refuse the manifest after N days (protects
  against a server that keeps serving an old release); re-publish (or
  re-sign) before it runs out. Without it the manifest never expires; clients
  still refuse a manifest older than one they have already accepted.
- `-url-prefix` changes the artifact URLs (default `dl/`, relative to the
  manifest; an absolute `https://cdn.example/airview/1.4.0/` works too).
- Versions are semantic (`MAJOR.MINOR.PATCH[-pre]`); clients install only a
  newer version unless the operator runs `update -force`.

Copy the directory to the aggregator's release directory (for example
`rsync -a releases/feeder/ server:/var/lib/airview/releases/feeder/`); the
aggregator serves it under `/feeder/`. Any static HTTPS server works as well:
point `update.manifestUrl` at its `manifest.json`.

Channels: beta lives in `<out>/beta/`; a client with `update.channel: beta`
fetches `.../feeder/beta/manifest.json` (or the URL with `{channel}`
replaced). A beta manifest is refused at the stable URL and vice versa.

## 3. Sign or verify by hand

```
avrelease sign   -key airview-release.key -in manifest.json          # writes manifest.json.sig
avrelease verify -pubfile airview-release.pub -in releases/feeder/manifest.json \
                 -dir releases/feeder                                 # also checks every artifact
```

`sign` refuses a manifest that does not validate. `verify` exits non-zero
on a bad signature, an invalid manifest, or an artifact whose size or
SHA-256 differs. OpenSSL 3 can check a signature independently:

```
base64 -d manifest.json.sig > sig.bin
# SubjectPublicKeyInfo DER = fixed 12-byte Ed25519 prefix + the raw 32-byte key
{ printf '\060\052\060\005\006\003\053\145\160\003\041\000'; base64 -d airview-release.pub; } > pub.der
openssl pkeyutl -verify -pubin -inkey pub.der -keyform DER -rawin -in manifest.json -sigfile sig.bin
```

### Check a published channel from outside

```
avrelease check -pubfile airview-release.pub -url https://aggregator.example/feeder/manifest.json [-channel beta] [-download]
avrelease serve -dir releases/feeder -addr 127.0.0.1:8080      # local test server, same routes as the aggregator
```

`check` does what a client does (signature, channel, expiry, with
`-download` every artifact's size and SHA-256) and warns when the manifest
expires within 7 days, so it fits a monitoring job.

## 4. On the clients

```
sudo airview-feeder update -check          # exit 10 when a newer version exists
sudo airview-feeder update                 # install, restart the service, 60 s health check, rollback
sudo airview-feeder update -status [-json]
airview update ...                         # same options for the main program
```

See `internal/selfupdate/WIRING.md` for the configuration block and how the
programs integrate it.
