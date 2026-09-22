#!/usr/bin/env bash
# tips.sh — best block of every node (teranodes: asset API, uncached; SV nodes: RPC), plus alert
# sequences via the hub and the SV nodes' alert sidecars.
set -euo pipefail
inv="$(dirname "$0")/../config/inventory.json"
python3 - "$inv" <<'PY'
import base64,json,sys,urllib.request
inv=json.load(open(sys.argv[1]))
auth="Basic "+base64.b64encode(f'{inv["rpcUser"]}:{inv["rpcPass"]}'.encode()).decode()
def rpc(url,method,params=None):
    req=urllib.request.Request(url,data=json.dumps({"jsonrpc":"1.0","id":"tips","method":method,"params":params or []}).encode(),
        headers={"Content-Type":"application/json","Authorization":auth})
    return json.load(urllib.request.urlopen(req,timeout=3))["result"]
for n in inv["nodes"]:
    try:
        if n.get("kind")=="svnode":
            ci=rpc(n["hostRPCURL"],"getblockchaininfo"); peers=len(rpc(n["hostRPCURL"],"getpeerinfo"))
            side=""
            try:
                h=json.load(urllib.request.urlopen(n["sidecar"]["hostAPIURL"]+"/health",timeout=3))
                side=f'  alert#{h["sequence"]} unprocessed={h["unprocessed_alerts"]}'
            except Exception as e: side="  sidecar: n/a"
            print(f'{n["name"]:10} height={ci["blocks"]:>6}  tip={ci["bestblockhash"]}  peers={peers}{side}')
        else:
            h=json.load(urllib.request.urlopen(n["hostAssetURL"]+"/bestblockheader/json",timeout=3))
            print(f'{n["name"]:10} height={h.get("height"):>6}  tip={h.get("hash")}')
    except Exception as e:
        print(f'{n["name"]:10} unreachable: {e}')
try:
    h=json.load(urllib.request.urlopen(inv["hub"]["hostAPIURL"]+"/health",timeout=3))
    print(f'hub        sequence={h["sequence"]} active_peers={h["active_peers"]}')
except Exception as e:
    print("hub unreachable:",e)
PY
