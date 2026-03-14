#!/bin/bash
# Test UpCloud server creation via curl until it works.
# Usage: source .env && ./hack/upcloud-create-server-curl.sh

set -euo pipefail

: "${UPCLOUD_API_TOKEN:?UPCLOUD_API_TOKEN required}"
ZONE="${UPCLOUD_ZONE:-fi-hel1}"
SERVER_NAME="cb-curl-test-$(date +%s)"
# Ubuntu 22.04 template UUID (from provisioner.go)
TEMPLATE_UUID="01000000-0000-4000-8000-000030220200"
# Plan: 2xCPU-4GB (matches E2E pod 1500m/2Gi -> 2 vCPU, 2GB min, use 4GB plan)
PLAN="2xCPU-4GB"

echo "=== Step 1: List plans (GET /1.2/plan) ==="
curl -s -H "Authorization: Bearer ${UPCLOUD_API_TOKEN}" \
  "https://api.upcloud.com/1.2/plan" | python3 -m json.tool 2>/dev/null | head -80

echo ""
echo "=== Step 2: List templates (GET /1.2/storage/template) ==="
curl -s -H "Authorization: Bearer ${UPCLOUD_API_TOKEN}" \
  "https://api.upcloud.com/1.2/storage/template" | python3 -c "
import sys, json
d = json.load(sys.stdin)
storages = d.get('storage_templates', {}).get('storage_template', [])
if isinstance(storages, dict):
    storages = [storages]
for s in storages[:5]:
    print(s.get('uuid'), s.get('title', '')[:60])
" 2>/dev/null || true

echo ""
echo "=== Step 3: Create server (POST /1.3/server) ==="
# UpCloud API 1.3: metadata required for cloud-init templates (Ubuntu 22.04); 1.2 rejects metadata
# DEV plans use hdd tier; regular plans use maxiops
TIER="maxiops"
[[ "$PLAN" == DEV-* ]] && TIER="hdd"

BODY=$(cat <<EOF
{
  "server": {
    "zone": "${ZONE}",
    "title": "${SERVER_NAME}",
    "hostname": "${SERVER_NAME}",
    "plan": "${PLAN}",
    "metadata": "yes",
    "password_delivery": "none",
    "storage_devices": {
      "storage_device": [
        {
          "action": "clone",
          "storage": "${TEMPLATE_UUID}",
          "title": "${SERVER_NAME}-disk0",
          "size": 25,
          "tier": "${TIER}"
        }
      ]
    }
  }
}
EOF
)

echo "Request body:"
echo "$BODY" | python3 -m json.tool

RESP=$(curl -s -w "\n%{http_code}" -X POST "https://api.upcloud.com/1.3/server" \
  -H "Authorization: Bearer ${UPCLOUD_API_TOKEN}" \
  -H "Content-Type: application/json" \
  -d "$BODY")

BODY_RESP=$(echo "$RESP" | sed '$d')
CODE=$(echo "$RESP" | tail -1)

echo ""
echo "Response HTTP code: $CODE"
echo "Response body:"
echo "$BODY_RESP" | python3 -m json.tool 2>/dev/null || echo "$BODY_RESP"

if [ "$CODE" != "200" ] && [ "$CODE" != "201" ] && [ "$CODE" != "202" ]; then
  echo ""
  echo "ERROR: Create failed. Checking for error details..."
  echo "$BODY_RESP" | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
    err = d.get('error', {})
    if err:
        print('Error:', err.get('error_message', err))
        print('Error code:', err.get('error_code', ''))
except: pass
" 2>/dev/null || true
  exit 1
fi

UUID=$(echo "$BODY_RESP" | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
    s = d.get('server', {})
    print(s.get('uuid', ''))
except: print('')
" 2>/dev/null)

if [ -n "$UUID" ]; then
  echo ""
  echo "=== Server created: $UUID ==="
  echo "Polling state..."
  for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
    STATE=$(curl -s -H "Authorization: Bearer ${UPCLOUD_API_TOKEN}" \
      "https://api.upcloud.com/1.3/server/${UUID}" | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
    print(d.get('server', {}).get('state', 'unknown'))
except: print('unknown')
" 2>/dev/null)
    echo "  Attempt $i: state=$STATE"
    [ "$STATE" = "started" ] && { echo "Server is running!"; exit 0; }
    [ "$STATE" = "error" ] && { echo "Server in error state"; exit 1; }
    sleep 10
  done
  echo "Timeout waiting for started"
fi
