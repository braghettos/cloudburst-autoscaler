#!/bin/bash
set -euo pipefail

# End-to-end integration test for Seeweb ECS.
# Startup scripts are attached as Cloud Scripts after server creation.
# See hack/e2e-common.sh for the shared test framework.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ── Provider Configuration ────────────────────────────────────────────────────

PROVIDER_NAME="seeweb"
NODECLASS_NAME="e2e-seeweb-nodeclass"
NODEPOOL_NAME="e2e-seeweb-nodepool"
PENDING_POD_NAME="e2e-seeweb-resource-hungry-pod"

SEEWEB_API_TOKEN="${SEEWEB_API_TOKEN:?SEEWEB_API_TOKEN must be set}"
SEEWEB_DATACENTER="${SEEWEB_DATACENTER:-it-mi2}"
SEEWEB_INSTANCE_TYPE="${SEEWEB_INSTANCE_TYPE:-ECS.C2.S}"

# Source shared library
source "${SCRIPT_DIR}/e2e-common.sh"

# ── Provider Callbacks ────────────────────────────────────────────────────────

provider_check_credentials() {
    [ -z "${SEEWEB_API_TOKEN}" ] && { log_error "SEEWEB_API_TOKEN not set"; missing=$((missing+1)); }
}

provider_create_controller_secrets() {
    kubectl create secret generic seeweb-credentials \
        --from-literal=SEEWEB_API_TOKEN="${SEEWEB_API_TOKEN}" \
        --namespace="${CONTROLLER_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -
}

provider_create_test_secrets() {
    log_info "Creating Seeweb credentials secret in test namespace..."
    kubectl create secret generic seeweb-credentials \
        --from-literal=SEEWEB_API_TOKEN="${SEEWEB_API_TOKEN}" \
        --namespace="${TEST_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -
    log_info "✓ Seeweb secret created"
}

provider_controller_env_block() {
    cat <<'EOF'
        - name: SEEWEB_API_TOKEN
          valueFrom:
            secretKeyRef:
              name: seeweb-credentials
              key: SEEWEB_API_TOKEN
EOF
}

provider_nodeclass_spec() {
    cat <<EOF
  seeweb:
    datacenter: "${SEEWEB_DATACENTER}"
    apiTokenSecretRef:
      name: seeweb-credentials
      key: SEEWEB_API_TOKEN
EOF
}

provider_cleanup_vms() {
    log_info "Cleaning up existing cb-* Seeweb servers (datacenter: ${SEEWEB_DATACENTER})..."
    local servers
    servers=$(curl -sf \
        -H "X-APITOKEN: ${SEEWEB_API_TOKEN}" \
        "https://api.seeweb.it/ecs/v2/servers" 2>/dev/null | \
        python3 -c "
import sys, json
data = json.load(sys.stdin)
items = data.get('servers', data if isinstance(data, list) else [])
for s in items:
    name = s.get('name', '')
    if name.startswith('cb-'):
        print(s.get('id', ''))
" 2>/dev/null || true)
    for sid in $servers; do
        [ -z "$sid" ] && continue
        log_info "Deleting Seeweb server: $sid"
        curl -sf -X DELETE \
            -H "X-APITOKEN: ${SEEWEB_API_TOKEN}" \
            "https://api.seeweb.it/ecs/v2/servers/${sid}" 2>/dev/null || true
    done
    log_info "✓ Seeweb cleanup done"
}

provider_verify_vm_deleted() {
    local instance_id="$1"
    local timeout="${2:-${VM_DELETION_TIMEOUT}}"
    local server_id
    server_id=$(echo "$instance_id" | cut -d'/' -f3)
    [ -z "$server_id" ] && { log_warn "Could not parse Seeweb instance ID"; return 0; }

    log_info "Verifying Seeweb server ${server_id} is deleted (timeout: ${timeout}s)..."
    local elapsed=0
    while [ $elapsed -lt $timeout ]; do
        local status_code
        status_code=$(curl -sf -o /dev/null -w "%{http_code}" \
            -H "X-APITOKEN: ${SEEWEB_API_TOKEN}" \
            "https://api.seeweb.it/ecs/v2/servers/${server_id}" 2>/dev/null || echo "000")
        if [ "$status_code" = "404" ] || [ "$status_code" = "000" ]; then
            log_info "✓ Seeweb server ${server_id} confirmed deleted"; return 0
        fi
        echo -n "."; sleep 15; elapsed=$((elapsed + 15))
    done
    log_error "Timeout: Seeweb server ${server_id} still exists"; return 1
}

# ── Run ───────────────────────────────────────────────────────────────────────

main_e2e
