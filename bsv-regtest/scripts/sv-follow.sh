#!/usr/bin/env bash
# sv-follow.sh <teranodes> <svnodes>  — make the teranodes advertise NODE_NETWORK so the SV nodes
# follow the chain, then check that they do. Run by `make wait`; does nothing when it already holds.
#
# SV Node syncs only from peers advertising NODE_NETWORK. Teranode's legacy service advertises it
# when the block persister has stored a block within the retention window of the tip, and decides
# once, at start-up. A teranode started on an empty chain therefore advertises
# NODE_NETWORK_LIMITED until it restarts. For each teranode whose legacy service started limited:
# give the chain a block, wait for that node's persister to store it, restart the node.
set -euo pipefail
n=${1:?teranode count}; sv=${2:?SV node count}
[ "$sv" -gt 0 ] || exit 0
dir="$(cd "$(dirname "$0")" && pwd)"
RUNTIME=${RUNTIME:-$(command -v podman >/dev/null 2>&1 && echo podman || echo docker)}
PROJECT=${PROJECT:-bsv-regtest}

asset() { echo "http://localhost:$((20000 + ($1-1)*2000 + 90))/api/v1"; }
tip() { curl -sf --max-time 2 "$(asset 1)/bestblockheader/json" | jq -r '.height // empty'; }
# mode <i> [since]: the storage mode (full | pruned) the node's legacy service last started with.
mode() {
  $RUNTIME logs ${2:+--since "$2"} "$PROJECT-teranode$1" 2>&1 |
    sed -n 's/.*Legacy service determined storage mode: \([a-z]*\).*/\1/p' | tail -1
}
has_mode() { [ -n "$(mode "$1")" ]; }
full_since() { [ "$(mode "$1" "$2")" = full ]; }
persisted() { [ "$(curl -sf --max-time 2 "$(asset "$1")/service/heights" | jq -r '.block_persister_height // 0')" -gt 0 ] 2>/dev/null; }
healthy() { curl -sf --max-time 2 "http://localhost:$((20000 + ($1-1)*2000))/health" >/dev/null; }
reached() { [ "$("$dir/rpc.sh" "sv$1" getblockcount | jq -r '.result // -1')" -ge "$2" ] 2>/dev/null; }
# until_ok <tries> <cmd…>: retry every 2 s.
until_ok() { local tries=$1; shift; for _ in $(seq 1 "$tries"); do "$@" && return 0; sleep 2; done; return 1; }

limited=()
for i in $(seq 1 "$n"); do
  # The line is logged once per start and can rotate out of a long-running node's log; the SV
  # height check below is the verdict either way.
  until_ok 15 has_mode "$i" || { echo "teranode$i: no 'Legacy service determined storage mode' line in its log; not touching it" >&2; continue; }
  [ "$(mode "$i")" = full ] || limited+=("$i")
done

if [ ${#limited[@]} -gt 0 ]; then
  if [ "$(tip)" = 0 ]; then
    echo "mining block 1 on teranode1: the block persisters need a block before the legacy service can advertise NODE_NETWORK"
    "$dir/mine.sh" 1 1 >/dev/null
  fi
  for i in "${limited[@]}"; do
    until_ok 60 persisted "$i" || { echo "teranode$i: its block persister stored nothing in 2m" >&2; exit 1; }
  done
  echo "restarting teranode ${limited[*]}: the legacy service decides NODE_NETWORK at start-up"
  since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  for i in "${limited[@]}"; do $RUNTIME restart "$PROJECT-teranode$i" >/dev/null; done
  for i in "${limited[@]}"; do
    until_ok 150 healthy "$i" || { echo "teranode$i did not become healthy in 5m after the restart" >&2; exit 1; }
    until_ok 30 full_since "$i" "$since" || { echo "teranode$i still advertises NODE_NETWORK_LIMITED after the restart" >&2; exit 1; }
  done
fi

# The SV nodes must reach the teranode tip over the legacy service.
want=$(tip)
[ -n "$want" ] || { echo "teranode1 did not report its tip" >&2; exit 1; }
for j in $(seq 1 "$sv"); do
  until_ok 60 reached "$j" "$want" || {
    echo "svnode$j did not reach height $want in 2m; its peers' services (NODE_NETWORK = last hex digit odd):" >&2
    "$dir/rpc.sh" "sv$j" getpeerinfo | jq -r '.result[] | "  \(.addr) \(.services)"' >&2
    exit 1
  }
  echo "svnode$j following (height >= $want)"
done
