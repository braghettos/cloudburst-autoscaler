#!/bin/bash
# Delete orphaned cb-* DigitalOcean droplets (account-wide).
# Use after E2E runs where scale-down may have left instances behind.
#
# Env: DIGITALOCEAN_API_TOKEN (required)
#      Or run from project root with .env loaded: source .env && ./hack/digitalocean-cleanup.sh

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
[ -f "${PROJECT_ROOT}/.env" ] && set -a && source "${PROJECT_ROOT}/.env" && set +a

DIGITALOCEAN_API_TOKEN="${DIGITALOCEAN_API_TOKEN:-}"

if [ -z "${DIGITALOCEAN_API_TOKEN}" ]; then
    echo "DIGITALOCEAN_API_TOKEN not set; skipping DigitalOcean cleanup"
    exit 0
fi

if ! command -v curl &>/dev/null || ! command -v python3 &>/dev/null; then
    echo "curl and python3 required for DigitalOcean cleanup"
    exit 1
fi

response=$(curl -s -H "Authorization: Bearer ${DIGITALOCEAN_API_TOKEN}" \
    "https://api.digitalocean.com/v2/droplets?per_page=200" 2>/dev/null || true)

total=0
while IFS= read -r line; do
    [ -z "$line" ] && continue
    id=$(echo "$line" | awk '{print $1}')
    name=$(echo "$line" | awk '{print $2}')
    echo "Deleting $name (ID: $id)..."
    curl -s -X DELETE -H "Authorization: Bearer ${DIGITALOCEAN_API_TOKEN}" \
        "https://api.digitalocean.com/v2/droplets/${id}" 2>/dev/null || true
    total=$((total + 1))
done < <(echo "$response" | python3 -c "
import sys, json
try:
    data = json.load(sys.stdin)
    for d in data.get('droplets', []):
        if d.get('name', '').startswith('cb-'):
            print(d['id'], d['name'])
except: pass
" 2>/dev/null || true)

if [ $total -gt 0 ]; then
    echo "Deleted $total cb-* DigitalOcean droplet(s)"
else
    echo "No cb-* DigitalOcean droplets found"
fi
