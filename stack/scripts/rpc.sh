#!/usr/bin/env bash
# rpc.sh <node-index> <method> [json-params]   — JSON-RPC against a node's host-published port.
set -euo pipefail
n=${1:?node index}; method=${2:?method}; params=${3:-[]}
port=$((20000 + (n-1)*2000 + 1292))
curl -s --max-time 300 -u "${RPC_USER:-bitcoin}:${RPC_PASS:-bitcoin}" -H 'Content-Type: application/json' \
  -d "{\"jsonrpc\":\"1.0\",\"id\":\"chaos\",\"method\":\"$method\",\"params\":$params}" "http://localhost:$port"
echo
