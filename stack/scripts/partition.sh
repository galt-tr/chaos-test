#!/usr/bin/env bash
# partition.sh <node-index> <p2p|alert> <on|off>
#   on  = disconnect the node from that network (chaosnet = node p2p / datahub, alertnet = alert p2p)
#   off = reconnect with its pinned IP. Control-plane access (ctlnet) is never touched.
set -euo pipefail
n=${1:?node index}; plane=${2:?p2p|alert}; action=${3:?on|off}
inv="$(dirname "$0")/../config/inventory.json"
case "$plane" in
  p2p)   net=chaos_chaosnet; ip=$(python3 -c "import json;print(json.load(open('$inv'))['nodes'][$n-1]['chaosIP'])");;
  alert) net=chaos_alertnet; ip=$(python3 -c "import json;print(json.load(open('$inv'))['nodes'][$n-1]['alertIP'])");;
  *) echo "plane must be p2p or alert" >&2; exit 2;;
esac
ctr="chaos-teranode$n"
case "$action" in
  on)  podman network disconnect "$net" "$ctr" && echo "$ctr partitioned from $net";;
  off) podman network connect --ip "$ip" "$net" "$ctr" && echo "$ctr reconnected to $net as $ip";;
  *) echo "action must be on or off" >&2; exit 2;;
esac
