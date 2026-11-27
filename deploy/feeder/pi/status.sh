#!/usr/bin/env bash
# AirView Network feeder: status (thin wrapper; the logic is in install.sh next to this file).
# Usage: sudo /opt/airview-feeder/status.sh [options]   - see: install.sh --help
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
exec bash "$here/install.sh" --status "$@"
