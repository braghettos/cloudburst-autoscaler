#!/bin/bash
set -euo pipefail

# End-to-end integration test for Clouding.io.
# See hack/e2e-common.sh for the shared test framework.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ── Provider Configuration ────────────────────────────────────────────────────

PROVIDER_NAME="clouding"
NODECLASS_NAME="e2e-clouding-nodeclass"
NODEPOOL_NAME="e2e-clouding-nodepool"
PENDING_POD_NAME="e2e-clouding-resource-hungry-pod"

CLOUDING_API_KEY="${CLOUDING_API_KEY:?CLOUDING_API_KEY must be set}"
CLOUDING_FIREWALL_ID="${CLOUDING_FIREWALL_ID:-}"

# Source shared library
source "${SCRIPT_DIR}/e2e-common.sh"

# ── Provider Callbacks ────────────────────────────────────────────────────────

provider_check_credentials() {
    [ -z "${CLOUDING_API_KEY}" ] && { log_error "CLOUDING_API_KEY not set"; missing=$((missing+1)); }
}

provider_create_controller_secrets() {
    kubectl create secret generic clouding-credentials \
        --from-literal=CLOUDING_API_KEY="${CLOUDING_API_KEY}" \
        --namespace="${CONTROLLER_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -
}

provider_create_test_secrets() {
    log_info "Creating Clouding.io credentials secret in test namespace..."
    kubectl create secret generic clouding-credentials \
        --from-literal=CLOUDING_API_KEY="${CLOUDING_API_KEY}" \
        --namespace="${TEST_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -
    log_info "✓ Clouding.io secret created"
}

provider_controller_env_block() {
    cat <<'EOF'
        - name: CLOUDING_API_KEY
          valueFrom:
            secretKeyRef:
              name: clouding-credentials
              key: CLOUDING_API_KEY
EOF
}

provider_nodeclass_spec() {
    local firewall_block=""
    if [ -n "${CLOUDING_FIREWALL_ID}" ]; then
        firewall_block="
    firewallId: \"${CLOUDING_FIREWALL_ID}\""
    fi
    cat <<EOF
  clouding:
    region: "es-bcn-1"${firewall_block}
    apiKeySecretRef:
      name: clouding-credentials
      key: CLOUDING_API_KEY
EOF
}

provider_cleanup_vms() {
    log_info "Cleaning up existing cb-* Clouding.io servers..."
    local servers
    servers=$(curl -sf \
        -H "X-API-KEY: ${CLOUDING_API_KEY}" \
        "https://api.clouding.io/v1/servers" 2>/dev/null | \
        python3 -c "
import sys, json
data = json.load(sys.stdin)
items = data.get('items', data) if isinstance(data, dict) else data
if not isinstance(items, list):
    items = []
for s in items:
    name = s.get('name', s.get('hostname', ''))
    if name and name.startswith('cb-'):
        print(s.get('id', ''))
" 2>/dev/null || true)
    for sid in $servers; do
        [ -z "$sid" ] && continue
        log_info "Deleting Clouding.io server: $sid"
        curl -sf -X DELETE \
            -H "X-API-KEY: ${CLOUDING_API_KEY}" \
            "https://api.clouding.io/v1/servers/${sid}" 2>/dev/null || true
    done
    log_info "✓ Clouding.io cleanup done"
}

provider_verify_vm_deleted() {
    local instance_id="$1"
    local timeout="${2:-${VM_DELETION_TIMEOUT}}"
    # instance_id format: clouding/<region>/<serverId>
    local server_id
    server_id=$(echo "$instance_id" | cut -d'/' -f3)
    [ -z "$server_id" ] && { log_warn "Could not parse Clouding instance ID"; return 0; }

    log_info "Verifying Clouding.io server ${server_id} is deleted (timeout: ${timeout}s)..."
    local elapsed=0
    while [ $elapsed -lt $timeout ]; do
        local status_code
        status_code=$(curl -sf -o /dev/null -w "%{http_code}" \
            -H "X-API-KEY: ${CLOUDING_API_KEY}" \
            "https://api.clouding.io/v1/servers/${server_id}" 2>/dev/null || echo "000")
        if [ "$status_code" = "404" ] || [ "$status_code" = "000" ]; then
            log_info "✓ Clouding.io server ${server_id} confirmed deleted"; return 0
        fi
        echo -n "."; sleep 15; elapsed=$((elapsed + 15))
    done
    log_error "Timeout: Clouding.io server ${server_id} still exists"; return 1
}

# ── Run ───────────────────────────────────────────────────────────────────────

main_e2e
