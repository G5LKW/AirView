# syntax=docker/dockerfile:1.7
#
# AirView server image: a static, CGO-free binary on distroless (no shell, no
# package manager, runs as uid 65532). Headless; no GUI or WebView parts.
#
#   docker build -t airview:latest .
#   docker build -t airview:1.2.3 --build-arg VERSION=1.2.3 .
#   docker buildx build --platform linux/amd64,linux/arm64 -t <registry>/airview:1.2.3 --push .
#
# Run (see docker-compose.example.yml for the full example with volumes):
#   docker run -d --name airview -p 8080:8080 -p 30160:30160 -p 30004:30004 -p 31090:31090 \
#     -v airview-data:/data -e AIRVIEW_MODE=aggregator airview:latest
#
# Configuration: AIRVIEW_* environment variables (one per setting, see
# docs/CONFIG.md; secrets also as AIRVIEW_<NAME>_FILE=/run/secrets/...), and/or
# an operator file mounted read-only with AIRVIEW_CONFIG=/etc/airview/airview.jsonc.
# Commands run against the same image:
#   docker compose run --rm airview config check
#   docker compose exec airview airview admin init
#
# The image listens for HTTP on 8080 (AIRVIEW_HTTP_PORT), not 80, so it needs
# no capabilities. Data lives in the /data volume (AIRVIEW_DATA).
#
# Base images: pin them by digest for reproducible builds
# (docker buildx imagetools inspect golang:1.27.3-alpine).
# To use "scratch" instead of distroless, copy /etc/ssl/certs/ca-certificates.crt
# from the build stage and create /data owned by 65532; the binary already
# embeds the time-zone database (-tags timetzdata).

ARG GO_VERSION=1.27.3

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
ARG VERSION=""
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-mod=readonly

# Modules first (cached as long as go.mod/go.sum do not change).
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

# Only the sources the binaries need: never the data or dist directories.
COPY cmd ./cmd
COPY internal ./internal
COPY web ./web
COPY deploy/server/healthcheck ./deploy/server/healthcheck
COPY deploy/feeder/pi ./deploy/feeder/pi

RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    set -eu; \
    export GOOS="$TARGETOS" GOARCH="$TARGETARCH"; \
    if [ "$TARGETARCH" = arm ]; then export GOARM="${TARGETVARIANT#v}"; fi; \
    ldflags="-s -w"; \
    if [ -n "$VERSION" ]; then ldflags="$ldflags -X main.version=$VERSION"; fi; \
    go build -trimpath -tags timetzdata -ldflags "$ldflags" -o /out/airview ./cmd/airview; \
    go build -trimpath -ldflags "-s -w" -o /out/airview-healthcheck ./deploy/server/healthcheck; \
    mkdir -p /out/data /out/etc-airview

FROM gcr.io/distroless/static-debian12:nonroot
ARG VERSION=""
LABEL org.opencontainers.image.title="AirView" \
      org.opencontainers.image.description="ADS-B statistics, mapping and network aggregation server (headless)" \
      org.opencontainers.image.version="${VERSION}"

COPY --from=build /out/airview /usr/local/bin/airview
COPY --from=build /out/airview-healthcheck /usr/local/bin/airview-healthcheck
COPY --from=build --chown=65532:65532 /out/data /data
# Mount point for an optional operator file (AIRVIEW_CONFIG=/etc/airview/airview.jsonc).
COPY --from=build /out/etc-airview /etc/airview

# GOMEMLIMIT=off: no soft heap target by default (AirView would otherwise
# apply its Raspberry Pi target of 384 MiB on arm64). With a container
# memory limit, set GOMEMLIMIT to ~85 % of it, e.g. -e GOMEMLIMIT=3400MiB.
# AIRVIEW_GENERATE_ADMIN_PASSWORD=1 (SEC-R2): see deploy/docker/README.md.
ENV AIRVIEW_DATA=/data \
    AIRVIEW_HTTP_PORT=8080 \
    AIRVIEW_GENERATE_ADMIN_PASSWORD=1 \
    GOMEMLIMIT=off

# 8080 HTTP (put a TLS reverse proxy in front), 30160 AVF/1 feeders (own TLS),
# 30004 Beast compat ingest, 31090 mlat-client. The hub (30154) and the merged
# outputs stay unpublished unless you need them (FIREWALL.md).
EXPOSE 8080/tcp 30160/tcp 30004/tcp 31090/tcp

VOLUME ["/data"]
WORKDIR /data
USER 65532:65532

# /healthz on AIRVIEW_HTTP_PORT; the first start (database creation,
# reference data import) can take a while on small hosts.
HEALTHCHECK --interval=30s --timeout=6s --start-period=120s --retries=3 \
    CMD ["/usr/local/bin/airview-healthcheck"]

# SIGTERM stops AirView cleanly (database writes flushed); give it time:
# docker stop -t 60 / compose stop_grace_period.
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/airview"]
CMD ["-headless"]
