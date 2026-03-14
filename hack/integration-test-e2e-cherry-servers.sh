#!/bin/bash
set -euo pipefail

# End-to-end integration test for Cherry Servers.
# See hack/e2e-common.sh for the shared test framework.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ── Provider Configuration ────────────────────────────────────────────────────

PROVIDER_NAME="cherry_servers"
NODECLASS_NAME="e2e-cherry-servers-nodeclass"
NODEPOOL_NAME="e2e-cherry-servers-nodepool"
PENDING_POD_NAME="e2e-cherry-servers-resource-hungry-pod"

CHERRY_SERVERS_API_TOKEN="${CHERRY_SERVERS_API_TOKEN:?CHERRY_SERVERS_API_TOKEN must be set}"
CHERRY_SERVERS_PROJECT_ID="${CHERRY_SERVERS_PROJECT_ID:?CHERRY_SERVERS_PROJECT_ID must be set}"
CHERRY_SERVERS_REGION="${CHERRY_SERVERS_REGION:-eu-lt-1}"

# Source shared library
source "${SCRIPT_DIR}/e2e-common.sh"

# ── Provider Callbacks ────────────────────────────────────────────────────────

provider_check_credentials() {
    [ -z "${CHERRY_SERVERS_API_TOKEN}" ] && { log_error "CHERRY_SERVERS_API_TOKEN not set"; missing=$((missing+1)); }
    [ -z "${CHERRY_SERVERS_PROJECT_ID}" ] && { log_error "CHERRY_SERVERS_PROJECT_ID not set"; missing=$((missing+1)); }
}

provider_create_controller_secrets() {
    kubectl create secret generic cherry-servers-credentials \
        --from-literal=CHERRY_SERVERS_API_TOKEN="${CHERRY_SERVERS_API_TOKEN}" \
        --namespace="${CONTROLLER_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -
}

provider_create_test_secrets() {
    log_info "Creating Cherry Servers credentials secret in test namespace..."
    kubectl create secret generic cherry-servers-credentials \
        --from-literal=CHERRY_SERVERS_API_TOKEN="${CHERRY_SERVERS_API_TOKEN}" \
        --namespace="${TEST_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -
    log_info "✓ Cherry Servers secret created"
}

provider_controller_env_block() {
    cat <<'EOF'
        - name: CHERRY_SERVERS_API_TOKEN
          valueFrom:
            secretKeyRef:
              name: cherry-servers-credentials
              key: CHERRY_SERVERS_API_TOKEN
EOF
}

provider_nodeclass_spec() {
    cat <<EOF
  cherryServers:
    projectID: ${CHERRY_SERVERS_PROJECT_ID}
    region: "${CHERRY_SERVERS_REGION}"
    apiTokenSecretRef:
      name: cherry-servers-credentials
      key: CHERRY_SERVERS_API_TOKEN
EOF
}

provider_cleanup_vms() {
    log_info "Cleaning up existing cb-* Cherry Servers..."
    local servers
    servers=$(curl -sf \
        -H "Authorization: Bearer ${CHERRY_SERVERS_API_TOKEN}" \
        "https://api.cherryservers.com/v1/projects/${CHERRY_SERVERS_PROJECT_ID}/servers" 2>/dev/null | \
        python3 -c "
import sys, json
data = json.load(sys.stdin)
items = data if isinstance(data, list) else data.get('servers', data.get('results', []))
for s in items:
    name = s.get('name', s.get('hostname', ''))
    if name and name.startswith('cb-'):
        print(s.get('id', ''))
" 2>/dev/null || true)
    for sid in $servers; do
        [ -z "$sid" ] && continue
        log_info "Deleting Cherry Servers server: $sid"
        curl -sf -X DELETE \
            -H "Authorization: Bearer ${CHERRY_SERVERS_API_TOKEN}" \
            "https://api.cherryservers.com/v1/servers/${sid}" 2>/dev/null || true
    done
    log_info "✓ Cherry Servers cleanup done"
}

provider_verify_vm_deleted() {
    local instance_id="$1"
    local timeout="${2:-${VM_DELETION_TIMEOUT}}"
    # instance_id format: cherry_servers/<region>/<serverId>
    local server_id
    server_id=$(echo "$instance_id" | cut -d'/' -f3)
    [ -z "$server_id" ] && { log_warn "Could not parse Cherry Servers instance ID"; return 0; }

    log_info "Verifying Cherry Servers server ${server_id} is deleted (timeout: ${timeout}s)..."
    local elapsed=0
    while [ $elapsed -lt $timeout ]; do
        local status_code
        status_code=$(curl -sf -o /dev/null -w "%{http_code}" \
            -H "Authorization: Bearer ${CHERRY_SERVERS_API_TOKEN}" \
            "https://api.cherryservers.com/v1/servers/${server_id}" 2>/dev/null || echo "000")
        if [ "$status_code" = "404" ] || [ "$status_code" = "000" ]; then
            log_info "✓ Cherry Servers server ${server_id} confirmed deleted"; return 0
        fi
        echo -n "."; sleep 15; elapsed=$((elapsed + 15))
    done
    log_error "Timeout: Cherry Servers server ${server_id} still exists"; return 1
}

# ── Run ───────────────────────────────────────────────────────────────────────

main_e2e
