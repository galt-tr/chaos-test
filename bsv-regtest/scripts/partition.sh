#!/usr/bin/env bash
# partition.sh <node> <p2p|alert> <on|off>
#   <node> = teranode index (1..N) or any node name from inventory.json (teranodeN, svnodeN)
#   on  = disconnect from that network (chaosnet = node p2p / datahub / legacy, alertnet = alert p2p)
#   off = reconnect with the pinned IP. Control-plane access (ctlnet) is never touched.
#   For an SV node the alert plane is its go-alert-system sidecar container.
set -euo pipefail
n=${1:?node}; plane=${2:?p2p|alert}; action=${3:?on|off}
inv="$(dirname "$0")/../config/inventory.json"
read -r ctr ip net < <(python3 - "$inv" "$n" "$plane" <<'PY'
import json,sys
inv,n,plane=json.load(open(sys.argv[1])),sys.argv[2],sys.argv[3]
node=next((x for x in inv["nodes"] if x["name"]==n or (x.get("kind")!="svnode" and str(x["index"])==n)),None)
if node is None: sys.exit(f"no node {n} in inventory")
if plane=="p2p": print(node["container"], node["chaosIP"], "chaos_chaosnet")
elif plane=="alert":
    ctr=node["sidecar"]["container"] if node.get("kind")=="svnode" else node["container"]
    print(ctr, node["alertIP"], "chaos_alertnet")
else: sys.exit("plane must be p2p or alert")
PY
)
case "$action" in
  on)  podman network disconnect "$net" "$ctr" && echo "$ctr partitioned from $net";;
  off) podman network connect --ip "$ip" "$net" "$ctr" && echo "$ctr reconnected to $net as $ip";;
  *) echo "action must be on or off" >&2; exit 2;;
esac
