#!/usr/bin/env bash
# AirView Network feeder: uninstall (thin wrapper; the logic is in install.sh next to this file).
# Usage: sudo /opt/airview-feeder/uninstall.sh [options]   - see: install.sh --help
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
exec bash "$here/install.sh" --uninstall "$@"
