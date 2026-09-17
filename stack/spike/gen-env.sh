#!/usr/bin/env bash
# Generates keys (once), the compose .env and the hub config for the spike.
set -euo pipefail
cd "$(dirname "$0")"
mkdir -p generated data/teranode1 data/alert-system
ALERTCTL=${ALERTCTL:-"go run ../../cmd/alertctl"}

if [[ ! -f generated/keys.json ]]; then
  echo "generating keys..."
  python3 - "$($ALERTCTL keygen secp256k1 -n 5)" "$($ALERTCTL keygen ed25519 -n 4)" > generated/keys.json <<'PY'
import json,sys
gen=json.loads(sys.argv[1]); ids=json.loads(sys.argv[2])
print(json.dumps({"genesis":gen,"publisher":ids[0],"hub":ids[1],"nodes":{"teranode1":{"p2p":ids[2],"alert":ids[3]}}},indent=2))
PY
fi

python3 - <<'PY'
import json
k=json.load(open("generated/keys.json"))
pubs=" | ".join(g["public_key_hex"] for g in k["genesis"])
n1=k["nodes"]["teranode1"]
env=f"""GENESIS_PUBKEYS={pubs}
HUB_PEER_ID={k['hub']['peer_id']}
TERANODE1_P2P_KEY={n1['p2p']['private_key_hex']}
TERANODE1_P2P_PEER_ID={n1['p2p']['peer_id']}
TERANODE1_ALERT_KEY={n1['alert']['private_key_hex']}
TERANODE1_ALERT_PEER_ID={n1['alert']['peer_id']}
INTERNAL=false
"""
open(".env","w").write(env)
cfg={
 "alert_processing_interval":"5m","alert_webhook_url":"","bitcoin_config_path":"",
 "datastore":{"auto_migrate":True,"debug":False,"engine":"sqlite","password":"",
   "sqlite":{"database_path":"/data/alert.db","shared":False},"table_prefix":"alert_system"},
 "disable_rpc_verification":True,"environment":"local",
 "genesis_keys":[g["public_key_hex"] for g in k["genesis"]],
 "log_output_file":"","log_level":"debug",
 "p2p":{"alert_system_protocol_id":"/bitcoin/alert-system/1.0.0",
   "bootstrap_peer":f"/ip4/192.0.0.141/tcp/9908/p2p/{n1['alert']['peer_id']}",
   "ip":"0.0.0.0","port":"9906","peer_discovery_interval":"15s",
   "allow_private_ip_addresses":True,"private_key":k["hub"]["private_key_hex"],
   "topic_name":"bitcoin_alert_system_regtest"},
 "request_logging":True,
 "rpc_connections":[{"host":"http://10.191.0.11:9292","user":"bitcoin","password":"bitcoin"}],
 "web_server":{"idle_timeout":"60s","port":"3000","read_timeout":"15s","write_timeout":"15s"}}
json.dump(cfg,open("generated/alert-system.json","w"),indent=2)
print("wrote .env and generated/alert-system.json")
print("teranode1 alert addr: /ip4/192.0.0.141/tcp/9908/p2p/"+n1['alert']['peer_id'])
print("hub addr:             /ip4/192.0.0.130/tcp/9906/p2p/"+k['hub']['peer_id'])
PY
