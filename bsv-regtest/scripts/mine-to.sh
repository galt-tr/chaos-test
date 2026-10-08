#!/usr/bin/env bash
# mine-to.sh TARGET_HEIGHT [NODE] [ORCH]
# Mine on a teranode until its height reaches TARGET_HEIGHT. teranode caps each generate RPC at
# 30s (rpc_timeout), so this mines in small chunks and loops on the live height — a chunk that
# times out still advances the tip, so it just keeps going. Useful to drive the fleet to a target
# height (e.g. past a consensus-rule activation height) before running a scenario.
set -euo pipefail
target="${1:?usage: mine-to.sh TARGET_HEIGHT [node] [orch_url]}"
node="${2:-teranode1}"
orch="${3:-http://localhost:8600}"
asset="http://localhost:20090/api/v1"   # teranode1 host asset API
height() { curl -s --max-time 5 "$asset/bestblockheader/json" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("height",0))'; }
h=$(height)
echo "start height=$h target=$target"
while [ "$h" -lt "$target" ]; do
  curl -s --max-time 50 -X POST -H 'Content-Type: application/json' \
    -d "{\"node\":\"$node\",\"blocks\":400}" "$orch/api/mine" >/dev/null 2>&1 || true
  nh=$(height)
  [ "$nh" -le "$h" ] && sleep 1   # no progress; brief pause then retry
  h=$nh
  echo "height=$h"
done
echo "reached height=$h (>= $target)"
