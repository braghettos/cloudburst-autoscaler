#!/bin/bash
# Delete orphaned cb-* UpCloud servers (account-wide).
# Use after E2E runs where scale-down may have left instances behind.
#
# Env: UPCLOUD_API_TOKEN (or from .env)
#      Or run from project root: source .env && ./hack/upcloud-cleanup.sh

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
[ -f "${PROJECT_ROOT}/.env" ] && set -a && source "${PROJECT_ROOT}/.env" && set +a

UPCLOUD_API_TOKEN="${UPCLOUD_API_TOKEN:-}"
if [ -z "${UPCLOUD_API_TOKEN}" ]; then
    echo "UPCLOUD_API_TOKEN not set; skipping UpCloud cleanup"
    exit 0
fi

total=0
response=$(curl -s -H "Authorization: Bearer ${UPCLOUD_API_TOKEN}" \
    "https://api.upcloud.com/1.3/server" 2>/dev/null || true)

while IFS= read -r line; do
    [ -z "$line" ] && continue
    uuid=$(echo "$line" | awk '{print $1}')
    title=$(echo "$line" | cut -d' ' -f2-)
    [ -z "$uuid" ] && continue
    echo "Deleting $title (UUID: $uuid)..."
    # UpCloud requires server to be stopped before delete
    curl -s -X POST -H "Authorization: Bearer ${UPCLOUD_API_TOKEN}" \
        -H "Content-Type: application/json" -d '{"stop_server":{"stop_type":"hard"}}' \
        "https://api.upcloud.com/1.3/server/${uuid}/stop" 2>/dev/null || true
    for _ in 1 2 3 4 5 6 7 8 9 10 11 12; do
        state=$(curl -s -H "Authorization: Bearer ${UPCLOUD_API_TOKEN}" \
            "https://api.upcloud.com/1.3/server/${uuid}" 2>/dev/null | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
    print(d.get('server', {}).get('state', 'unknown'))
except: print('unknown')
" 2>/dev/null || true)
        [ "$state" = "stopped" ] && break
        sleep 5
    done
    curl -s -X DELETE -H "Authorization: Bearer ${UPCLOUD_API_TOKEN}" \
        "https://api.upcloud.com/1.3/server/${uuid}?storages=1" 2>/dev/null || true
    total=$((total + 1))
done < <(echo "$response" | python3 -c "
import sys, json
try:
    data = json.load(sys.stdin)
    servers = data.get('servers', {}).get('server', [])
    if isinstance(servers, dict):
        servers = [servers]
    for s in servers:
        title = s.get('title', '') or s.get('hostname', '')
        if str(title).startswith('cb-'):
            print(s.get('uuid', ''), title)
except: pass
" 2>/dev/null || true)

if [ "${total:-0}" -gt 0 ]; then
    echo "Deleted $total cb-* UpCloud server(s)"
else
    echo "No cb-* UpCloud servers found"
fi
