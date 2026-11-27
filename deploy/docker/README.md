# AirView Docker image from the release binaries

`airview-docker-<version>.tar.gz` (made by `deploy/build.ps1`) is a ready Docker build context:

```
airview-docker-<version>/
  Dockerfile                    distroless image from the prebuilt binaries (this directory's Dockerfile)
  docker-compose.example.yml    aggregator + optional Caddy HTTPS front end
  bin/airview-linux-{amd64,arm64}               static server binaries (same as the server bundles)
  bin/airview-healthcheck-linux-{amd64,arm64}   HEALTHCHECK helper (deploy/server/healthcheck)
  data/  etc-airview/           empty directories copied into the image (/data, /etc/airview)
  VERSION  THIRD-PARTY-NOTICES.txt  README.md
```

## Build and run

```sh
tar xzf airview-docker-1.2.3.tar.gz && cd airview-docker-1.2.3
docker build -t airview:1.2.3 --build-arg VERSION=1.2.3 .
# both CPUs at once, pushed to your registry:
docker buildx build --platform linux/amd64,linux/arm64 -t registry.example.org/airview:1.2.3 --push .

cp docker-compose.example.yml docker-compose.yml   # edit the marked values
docker compose up -d
docker compose logs -f airview                     # the first start prints a one-time setup code
docker compose run --rm airview config check       # validate AIRVIEW_* and the operator file
```

The build needs BuildKit (default since Docker 23) because the Dockerfile picks the binary with
`TARGETARCH`. Only the base image `gcr.io/distroless/static-debian12:nonroot` is pulled; pin it by
digest for reproducible images.

Configuration, ports, volumes, backup and upgrade are described in the comments of
`docker-compose.example.yml` and in `docs/CONFIG.md` (every setting has an `AIRVIEW_*` variable;
secrets can come from files with `AIRVIEW_<NAME>_FILE`). The image runs `airview -headless` as uid 65532
with data in the `/data` volume and answers `GET /healthz` on port 8080.

To build from source instead (no release bundle), use the `Dockerfile` at the repository root.

## Admin password (standalone mode)

The image sets `AIRVIEW_GENERATE_ADMIN_PASSWORD=1`. When a standalone server
starts without an admin password (`http.adminPasswordHash`), it generates one,
stores its hash in `/data/config.json` and writes the password itself to
`/data/admin-password.txt` (mode 0600); the log names the file. Read it with
`docker cp <container>:/data/admin-password.txt -` (the image has no shell), change it in Settings
and the file is removed at the next start. Docker's port publishing can make every client appear to
come from the bridge gateway (a private address), so the image does not rely on
the "this machine and the LAN are admin" default of a bare installation.

To choose the password yourself instead, pass
`-e AIRVIEW_HTTP_ADMIN_PASSWORD_HASH=plain:<password>` (hashed when loaded) or a
`_FILE` variant. Aggregator mode uses its own accounts and is not affected.
