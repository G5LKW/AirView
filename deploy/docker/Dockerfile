# syntax=docker/dockerfile:1.7
#
# AirView server image from the PREBUILT release binaries (airview-docker-<version>.tar.gz,
# made by deploy/build.ps1). No compiler and no source tree needed: the build only copies the
# static, CGO-free binaries for the target CPU onto distroless (no shell, no package manager,
# runs as uid 65532). To build from source instead, use the Dockerfile at the repository root.
#
#   tar xzf airview-docker-<version>.tar.gz && cd airview-docker-<version>
#   docker build -t airview:<version> --build-arg VERSION=<version> .
#   docker buildx build --platform linux/amd64,linux/arm64 -t <registry>/airview:<version> --push .
#   cp docker-compose.example.yml docker-compose.yml && docker compose up -d
#
# Base image: pin it by digest for reproducible images
# (docker buildx imagetools inspect gcr.io/distroless/static-debian12:nonroot).
# Configuration, ports, volumes and health check are the same as the source-built image:
# AIRVIEW_* variables (docs/CONFIG.md), AIRVIEW_<NAME>_FILE secrets, optional operator file
# mounted at /etc/airview/airview.jsonc with AIRVIEW_CONFIG.

FROM gcr.io/distroless/static-debian12:nonroot
# Set by BuildKit / buildx from --platform (amd64 or arm64).
ARG TARGETARCH
ARG VERSION=""
LABEL org.opencontainers.image.title="AirView" \
      org.opencontainers.image.description="ADS-B statistics, mapping and network aggregation server (headless)" \
      org.opencontainers.image.version="${VERSION}"

COPY bin/airview-linux-${TARGETARCH} /usr/local/bin/airview
COPY bin/airview-healthcheck-linux-${TARGETARCH} /usr/local/bin/airview-healthcheck
# Empty directories shipped in the bundle: the data volume (owned by the runtime user) and the
# mount point of the optional operator file.
COPY --chown=65532:65532 data/ /data/
COPY etc-airview/ /etc/airview/

# GOMEMLIMIT=off: no soft heap target by default (AirView would otherwise apply its Raspberry Pi
# target of 384 MiB on arm64). With a container memory limit set GOMEMLIMIT to ~85 % of it.
# AIRVIEW_GENERATE_ADMIN_PASSWORD=1 (SEC-R2): a standalone server without an admin password
# generates one on its first start and writes it to /data/admin-password.txt (mode 0600); the
# log names the file. Behind Docker's port publishing every client can look like a LAN peer.
ENV AIRVIEW_DATA=/data \
    AIRVIEW_HTTP_PORT=8080 \
    AIRVIEW_GENERATE_ADMIN_PASSWORD=1 \
    GOMEMLIMIT=off

# 8080 HTTP (put a TLS reverse proxy in front), 30160 AVF/1 feeders (own TLS),
# 30004 Beast compat ingest, 31090 mlat-client (deploy/server/FIREWALL.md).
EXPOSE 8080/tcp 30160/tcp 30004/tcp 31090/tcp

VOLUME ["/data"]
WORKDIR /data
USER 65532:65532

HEALTHCHECK --interval=30s --timeout=6s --start-period=120s --retries=3 \
    CMD ["/usr/local/bin/airview-healthcheck"]

STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/airview"]
CMD ["-headless"]
