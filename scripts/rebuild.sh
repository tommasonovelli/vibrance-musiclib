#!/usr/bin/env bash
set -euo pipefail
[[ $# == 1 ]] || { printf '%s\n' 'Usage: scripts/rebuild.sh STORE_ID' >&2; exit 2; }
exec "$(dirname -- "${BASH_SOURCE[0]}")/../musiclib" rebuild --store-id "$1"
