#!/usr/bin/env bash
# mine.sh <teranode-index> [blocks] [address]   — generate blocks on a teranode (regtest).
# SV nodes are followers here (they sync from the teranodes); mine on a teranode.
set -euo pipefail
n=${1:?node index}; blocks=${2:-1}; addr=${3:-}
case "$n" in sv*) echo "mine on a teranode; SV nodes follow the teranodes over the legacy service" >&2; exit 2;; esac
dir="$(cd "$(dirname "$0")" && pwd)"
if [[ -n "$addr" ]]; then "$dir/rpc.sh" "$n" generatetoaddress "[$blocks,\"$addr\"]"; else "$dir/rpc.sh" "$n" generate "[$blocks]"; fi
