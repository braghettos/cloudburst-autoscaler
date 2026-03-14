#!/bin/bash
set -euo pipefail

# End-to-end integration test for the DigitalOcean provider.
# See hack/e2e-common.sh for the shared test framework.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ── Provider Configuration ────────────────────────────────────────────────────

PROVIDER_NAME="digitalocean"
NODECLASS_NAME="e2e-digitalocean-nodeclass"
NODEPOOL_NAME="e2e-digitalocean-nodepool"
PENDING_POD_NAME="e2e-digitalocean-resource-hungry-pod"

DIGITALOCEAN_API_TOKEN="${DIGITALOCEAN_API_TOKEN:?DIGITALOCEAN_API_TOKEN must be set}"
DIGITALOCEAN_REGION="${DIGITALOCEAN_REGION:-nyc1}"

# Source shared library
source "${SCRIPT_DIR}/e2e-common.sh"

# ── Provider Callbacks ────────────────────────────────────────────────────────

provider_check_credentials() {
    [ -z "${DIGITALOCEAN_API_TOKEN}" ] && { log_error "DIGITALOCEAN_API_TOKEN not set"; missing=$((missing+1)); }
}

provider_create_controller_secrets() {
    kubectl create secret generic digitalocean-credentials \
        --from-literal=DIGITALOCEAN_API_TOKEN="${DIGITALOCEAN_API_TOKEN}" \
        --namespace="${CONTROLLER_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -
}

provider_create_test_secrets() {
    log_info "Creating DigitalOcean API token secret in test namespace..."
    kubectl create secret generic digitalocean-api-token \
        --from-literal=DIGITALOCEAN_API_TOKEN="${DIGITALOCEAN_API_TOKEN}" \
        --namespace="${TEST_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -
    log_info "✓ DigitalOcean secret created"
}

provider_controller_env_block() {
    cat <<'EOF'
        - name: DIGITALOCEAN_API_TOKEN
          valueFrom:
            secretKeyRef:
              name: digitalocean-credentials
              key: DIGITALOCEAN_API_TOKEN
EOF
}

provider_nodeclass_spec() {
    cat <<EOF
  digitalocean:
    region: "${DIGITALOCEAN_REGION}"
    apiTokenSecretRef:
      name: digitalocean-api-token
      key: DIGITALOCEAN_API_TOKEN
EOF
}

provider_cleanup_vms() {
    log_info "Cleaning up existing cb-* DigitalOcean droplets..."
    local resp ids
    resp=$(curl -s -H "Authorization: Bearer ${DIGITALOCEAN_API_TOKEN}" \
        "https://api.digitalocean.com/v2/droplets?per_page=200" 2>/dev/null || echo '{"droplets":[]}')
    ids=$(echo "$resp" | python3 -c "
import sys, json
data = json.load(sys.stdin)
for d in data.get('droplets', []):
    if d.get('name','').startswith('cb-'):
        print(d['id'])
" 2>/dev/null || true)
    for did in $ids; do
        log_info "Deleting DigitalOcean droplet: $did"
        curl -s -X DELETE -H "Authorization: Bearer ${DIGITALOCEAN_API_TOKEN}" \
            "https://api.digitalocean.com/v2/droplets/${did}" >/dev/null 2>&1 || true
    done
    log_info "✓ DigitalOcean cleanup done"
}

provider_verify_vm_deleted() {
    local instance_id="$1"
    local timeout="${2:-${VM_DELETION_TIMEOUT}}"
    local droplet_id
    droplet_id=$(echo "$instance_id" | awk -F'/' '{print $NF}')
    [ -z "$droplet_id" ] && { log_warn "Could not parse droplet ID"; return 0; }

    log_info "Verifying DigitalOcean droplet ${droplet_id} is deleted (timeout: ${timeout}s)..."
    local elapsed=0
    while [ $elapsed -lt $timeout ]; do
        local code
        code=$(curl -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer ${DIGITALOCEAN_API_TOKEN}" \
            "https://api.digitalocean.com/v2/droplets/${droplet_id}" 2>/dev/null || echo "000")
        if [ "$code" = "404" ]; then
            log_info "✓ DigitalOcean droplet ${droplet_id} confirmed deleted"
            return 0
        fi
        echo -n "."
        sleep 15
        elapsed=$((elapsed + 15))
    done
    log_error "Timeout: DigitalOcean droplet ${droplet_id} still exists"
    return 1
}

# ── Run ───────────────────────────────────────────────────────────────────────

main_e2e
