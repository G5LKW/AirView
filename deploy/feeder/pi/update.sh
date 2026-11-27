#!/usr/bin/env bash
# AirView Network feeder: update (thin wrapper; the logic is in install.sh next to this file).
# Usage: sudo /opt/airview-feeder/update.sh [options]   - see: install.sh --help
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
exec bash "$here/install.sh" --update "$@"
