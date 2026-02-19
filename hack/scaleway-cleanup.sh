#!/bin/bash
# Delete orphaned cb-* Scaleway instances across all zones.
# Use after E2E runs where scale-down may have left instances behind.
#
# Env: SCW_SECRET_KEY (required), SCW_ACCESS_KEY (optional)
#      SCW_ZONES: space-separated zones to check (default: fr-par-1 nl-ams-1 pl-waw-1)
#      Or run from project root with .env loaded: source .env && ./hack/scaleway-cleanup.sh

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
[ -f "${PROJECT_ROOT}/.env" ] && set -a && source "${PROJECT_ROOT}/.env" && set +a

SCW_SECRET_KEY="${SCW_SECRET_KEY:-}"
# When SCW_ZONE is set (e.g. from e2e), only that zone is checked. Otherwise use SCW_ZONES.
SCW_ZONES="${SCW_ZONES:-${SCW_ZONE:-fr-par-1 nl-ams-1 pl-waw-1}}"

if [ -z "${SCW_SECRET_KEY}" ]; then
    echo "SCW_SECRET_KEY not set; skipping Scaleway cleanup"
    exit 0
fi

if ! command -v curl &>/dev/null || ! command -v python3 &>/dev/null; then
    echo "curl and python3 required for Scaleway cleanup"
    exit 1
fi

total=0
for zone in $SCW_ZONES; do
    url="https://api.scaleway.com/instance/v1/zones/${zone}/servers"
    response=$(curl -s -H "X-Auth-Token: ${SCW_SECRET_KEY}" "${url}" 2>/dev/null || true)
    while IFS= read -r line; do
        [ -z "$line" ] && continue
        id=$(echo "$line" | awk '{print $1}')
        name=$(echo "$line" | awk '{print $2}')
        echo "Deleting $name (zone: $zone, ID: $id)..."
        curl -s -X DELETE -H "X-Auth-Token: ${SCW_SECRET_KEY}" \
            "https://api.scaleway.com/instance/v1/zones/${zone}/servers/${id}" 2>/dev/null || true
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
done

if [ $total -gt 0 ]; then
    echo "Deleted $total cb-* Scaleway instance(s)"
else
    echo "No cb-* Scaleway instances found in zones: $SCW_ZONES"
fi
