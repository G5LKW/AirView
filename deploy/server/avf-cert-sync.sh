#!/bin/sh
# Copies a CA-issued certificate (Let's Encrypt via certbot or Caddy) to
# /etc/airview/tls/ for the AVF/1 feeder listener (TCP 30160) and restarts
# AirView when it changed. AVF/1 terminates TLS itself (it is not HTTP and
# cannot go through the reverse proxy), so it needs its own copy of the
# certificate; the service account cannot read /etc/letsencrypt or Caddy's
# storage directly.
#
# certbot (runs after every renewal; RENEWED_LINEAGE is set by certbot):
#   sudo certbot certonly ... --deploy-hook /opt/airview/avf-cert-sync.sh
# Caddy (daily, e.g. /etc/cron.daily/airview-cert):
#   /opt/airview/avf-cert-sync.sh \
#     --cert /var/lib/caddy/.local/share/caddy/certificates/acme-v02.api.letsencrypt.org-directory/feed.example.org/feed.example.org.crt \
#     --key  /var/lib/caddy/.local/share/caddy/certificates/acme-v02.api.letsencrypt.org-directory/feed.example.org/feed.example.org.key
#
# Then point AirView at the copies once, in /etc/airview/airview.jsonc:
#   "aggregator": { "tlsCert": "/etc/airview/tls/avf.crt", "tlsKey": "/etc/airview/tls/avf.key" }
#
# Feeders configured with a ServerPin of the old self-signed certificate must
# switch to system roots (empty pin) or the new pin when you change over;
# routine renewals with the same key type need no feeder change.
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib.sh
. "$HERE/lib.sh"

CERT=""
KEY=""
NORESTART=0
while [ $# -gt 0 ]; do
    case "$1" in
        --cert) CERT="${2:?--cert needs a file}"; shift 2 ;;
        --key) KEY="${2:?--key needs a file}"; shift 2 ;;
        --no-restart) NORESTART=1; shift ;;
        -h | --help) sed -n '2,24p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
        *) echo "unknown option: $1" >&2; exit 2 ;;
    esac
done
if [ -z "$CERT" ] && [ -n "${RENEWED_LINEAGE:-}" ]; then
    CERT="$RENEWED_LINEAGE/fullchain.pem"
    KEY="$RENEWED_LINEAGE/privkey.pem"
fi
[ -n "$CERT" ] && [ -n "$KEY" ] || die "give --cert FILE --key FILE (or run as a certbot deploy hook)"
[ -r "$CERT" ] || die "cannot read $CERT"
[ -r "$KEY" ] || die "cannot read $KEY"
grep -q 'BEGIN CERTIFICATE' "$CERT" || die "$CERT is not a PEM certificate"
grep -q 'PRIVATE KEY' "$KEY" || die "$KEY is not a PEM private key"

need_root
dir="$AV_ETC/tls"
mkdir -p "$dir"
chown "root:$AV_GROUP" "$dir"
chmod 0750 "$dir"

changed=0
copy() {
    if ! cmp -s "$1" "$2" 2>/dev/null; then
        install -m 0640 "$1" "$2.tmp"
        chown "root:$AV_GROUP" "$2.tmp"
        mv -f "$2.tmp" "$2"
        changed=1
    fi
}
copy "$CERT" "$dir/avf.crt"
copy "$KEY" "$dir/avf.key"

if [ "$changed" -eq 0 ]; then
    exit 0
fi
info "AVF/1 certificate updated in $dir"
if [ "$NORESTART" -eq 0 ] && svc_active; then
    # Feeders reconnect within seconds and backfill from their buffers.
    systemctl try-restart "$AV_SERVICE"
    note "restarted $AV_SERVICE"
fi
