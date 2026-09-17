#!/usr/bin/env bash
# mine.sh <node-index> [blocks] [address]   — generate blocks on a node (regtest).
set -euo pipefail
n=${1:?node index}; blocks=${2:-1}; addr=${3:-}
dir="$(cd "$(dirname "$0")" && pwd)"
if [[ -n "$addr" ]]; then "$dir/rpc.sh" "$n" generatetoaddress "[$blocks,\"$addr\"]"; else "$dir/rpc.sh" "$n" generate "[$blocks]"; fi
