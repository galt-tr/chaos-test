#!/usr/bin/env bash
# tips.sh — best block of every node (asset API, uncached), plus alert sequence via the hub.
set -euo pipefail
inv="$(dirname "$0")/../config/inventory.json"
python3 - "$inv" <<'PY'
import json,sys,urllib.request
inv=json.load(open(sys.argv[1]))
for n in inv["nodes"]:
    try:
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
