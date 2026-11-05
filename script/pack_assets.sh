#!/usr/bin/env bash
# Keep one build entry point so guest isolation and metadata checks cannot drift.
set -euo pipefail
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
export PVM_PACK_QEMU=0
exec "$SCRIPT_DIR/build.sh" "$@"
