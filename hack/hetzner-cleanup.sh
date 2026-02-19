#!/bin/bash
# Delete orphaned cb-* Hetzner Cloud servers (account-wide).
# Use after E2E runs where scale-down may have left instances behind.
#
# Env: HETZNER_API_TOKEN (or from .env)
#      Or run from project root: source .env && ./hack/hetzner-cleanup.sh

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
[ -f "${PROJECT_ROOT}/.env" ] && set -a && source "${PROJECT_ROOT}/.env" && set +a

HETZNER_API_TOKEN="${HETZNER_API_TOKEN:-}"
if [ -z "${HETZNER_API_TOKEN}" ]; then
    echo "HETZNER_API_TOKEN not set; skipping Hetzner cleanup"
    exit 0
fi

total=0
if command -v hcloud &>/dev/null; then
    while IFS= read -r line; do
        [ -z "$line" ] && continue
        id=$(echo "$line" | awk '{print $1}')
        name=$(echo "$line" | awk '{print $2}')
        echo "Deleting $name (ID: $id)..."
        HCLOUD_TOKEN="${HETZNER_API_TOKEN}" hcloud server delete "$id" 2>/dev/null || true
        total=$((total + 1))
    done < <(HCLOUD_TOKEN="${HETZNER_API_TOKEN}" hcloud server list -o noheader -o columns=id,name 2>/dev/null | grep "cb-" || true)
elif command -v curl &>/dev/null && command -v python3 &>/dev/null; then
    response=$(curl -s -H "Authorization: Bearer ${HETZNER_API_TOKEN}" "https://api.hetzner.cloud/v1/servers?per_page=50" 2>/dev/null || true)
    while IFS= read -r line; do
        [ -z "$line" ] && continue
        id=$(echo "$line" | awk '{print $1}')
        name=$(echo "$line" | awk '{print $2}')
        echo "Deleting $name (ID: $id)..."
        curl -s -X DELETE -H "Authorization: Bearer ${HETZNER_API_TOKEN}" "https://api.hetzner.cloud/v1/servers/$id" 2>/dev/null || true
        total=$((total + 1))
    done < <(echo "$response" | python3 -c "
import sys, json
try:
    data = json.load(sys.stdin)
    for s in data.get('servers', []):
        if s.get('name', '').startswith('cb-'):
            print(s['id'], s['name'])
except: pass
" 2>/dev/null || true)
else
    echo "hcloud or (curl + python3) required"
    exit 1
fi

if [ "${total:-0}" -gt 0 ]; then
    echo "Deleted $total cb-* server(s)"
else
    echo "No cb-* Hetzner servers found"
fi
