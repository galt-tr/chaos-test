#!/usr/bin/env bash
# rpc.sh <node> <method> [json-params]   — JSON-RPC against a node's host-published port.
#   <node> = teranode index (1..N), "teranodeN", or an SV node: "svN" / "svnodeN".
set -euo pipefail
n=${1:?node}; method=${2:?method}; params=${3:-[]}
case "$n" in
  sv*)   j=${n#svnode}; j=${j#sv}; port=$((40000 + (j-1)*1000 + 332));;
  teranode*) i=${n#teranode}; port=$((20000 + (i-1)*2000 + 1292));;
  *)     port=$((20000 + (n-1)*2000 + 1292));;
esac
curl -s --max-time 300 -u "${RPC_USER:-bitcoin}:${RPC_PASS:-bitcoin}" -H 'Content-Type: application/json' \
  -d "{\"jsonrpc\":\"1.0\",\"id\":\"bsv-regtest\",\"method\":\"$method\",\"params\":$params}" "http://localhost:$port"
echo
