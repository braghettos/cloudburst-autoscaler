#!/bin/bash
# Run cleanup for all cloud providers. Each provider is attempted; missing credentials
# cause that provider to be skipped. Use after E2E runs where scale-down may have left
# instances behind.
#
# Usage: make cleanup-all
#        Or: source .env && ./hack/cleanup-all-providers.sh

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
[ -f "${PROJECT_ROOT}/.env" ] && set -a && source "${PROJECT_ROOT}/.env" && set +a

echo "=== CloudBurst orphaned instance cleanup (all providers) ==="

run_cleanup() {
    local name=$1
    shift
    echo ""
    echo "--- $name ---"
    if "$@" 2>&1; then
        : # success
    else
        echo "(skipped or no instances)"
    fi
}

# Order: GCP, AWS, Azure, Hetzner, Scaleway, DigitalOcean, OVH
run_cleanup "GCP"            bash "${SCRIPT_DIR}/gcp-cleanup.sh"
run_cleanup "AWS"            bash "${SCRIPT_DIR}/aws-cleanup.sh"
run_cleanup "Azure"          bash "${SCRIPT_DIR}/azure-cleanup.sh"
run_cleanup "Hetzner"        bash "${SCRIPT_DIR}/hetzner-cleanup.sh"
run_cleanup "Scaleway"       bash "${SCRIPT_DIR}/scaleway-cleanup.sh"
run_cleanup "DigitalOcean"   bash "${SCRIPT_DIR}/digitalocean-cleanup.sh"
if [ -n "${OVH_APPLICATION_KEY:-}" ] && [ -n "${OVH_APPLICATION_SECRET:-}" ] && [ -n "${OVH_CONSUMER_KEY:-}" ] && [ -n "${OVH_PROJECT_ID:-}" ]; then
    echo ""
    echo "--- OVH ---"
    (cd "${PROJECT_ROOT}" && go run ./hack/ovh-cleanup/main.go) 2>&1 || echo "(skipped or no instances)"
else
    echo ""; echo "--- OVH ---"; echo "OVH credentials not set; skipping"
fi

echo ""
echo "=== Cleanup complete ==="
