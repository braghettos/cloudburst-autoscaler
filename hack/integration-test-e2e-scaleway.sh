#!/bin/bash
set -euo pipefail

# End-to-end integration test for the Scaleway provider.
# See hack/e2e-common.sh for the shared test framework.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ── Provider Configuration ────────────────────────────────────────────────────

PROVIDER_NAME="scaleway"
NODECLASS_NAME="e2e-scaleway-nodeclass"
NODEPOOL_NAME="e2e-scaleway-nodepool"
PENDING_POD_NAME="e2e-scaleway-resource-hungry-pod"

SCW_SECRET_KEY="${SCW_SECRET_KEY:?SCW_SECRET_KEY must be set}"
SCW_ACCESS_KEY="${SCW_ACCESS_KEY:-}"
SCW_ZONE="${SCW_ZONE:-fr-par-1}"
SCW_PROJECT_ID="${SCW_PROJECT_ID:-}"
SCW_ORGANIZATION_ID="${SCW_ORGANIZATION_ID:-${SCW_DEFAULT_ORGANIZATION_ID:-}}"

# Source shared library
source "${SCRIPT_DIR}/e2e-common.sh"

# ── Provider Callbacks ────────────────────────────────────────────────────────

provider_check_credentials() {
    [ -z "${SCW_SECRET_KEY}" ] && { log_error "SCW_SECRET_KEY not set"; missing=$((missing+1)); }
}

provider_create_controller_secrets() {
    local args=(--from-literal=SCW_SECRET_KEY="${SCW_SECRET_KEY}")
    [ -n "${SCW_ACCESS_KEY}" ] && args+=(--from-literal=SCW_ACCESS_KEY="${SCW_ACCESS_KEY}")
    kubectl create secret generic scaleway-credentials \
        "${args[@]}" \
        --namespace="${CONTROLLER_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -
}

provider_create_test_secrets() {
    log_info "Creating Scaleway API key secret in test namespace..."
    local args=(--from-literal=SCW_SECRET_KEY="${SCW_SECRET_KEY}")
    [ -n "${SCW_ACCESS_KEY}" ] && args+=(--from-literal=SCW_ACCESS_KEY="${SCW_ACCESS_KEY}")
    kubectl create secret generic scaleway-api-keys \
        "${args[@]}" \
        --namespace="${TEST_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -
    log_info "✓ Scaleway secret created"
}

provider_controller_env_block() {
    cat <<EOF
        - name: SCW_SECRET_KEY
          valueFrom:
            secretKeyRef:
              name: scaleway-credentials
              key: SCW_SECRET_KEY
        - name: SCW_DEFAULT_PROJECT_ID
          value: "${SCW_PROJECT_ID:-}"
        - name: SCW_DEFAULT_ORGANIZATION_ID
          value: "${SCW_ORGANIZATION_ID:-}"
EOF
}

provider_nodeclass_spec() {
    {
        echo "  scaleway:"
        echo "    zone: \"${SCW_ZONE}\""
        [ -n "${SCW_PROJECT_ID}" ] && echo "    projectID: \"${SCW_PROJECT_ID}\""
        [ -n "${SCW_ORGANIZATION_ID}" ] && echo "    organizationID: \"${SCW_ORGANIZATION_ID}\""
        echo "    apiKeySecretRef:"
        echo "      name: scaleway-api-keys"
        echo "      key: SCW_SECRET_KEY"
    }
}

provider_cleanup_vms() {
    log_info "Cleaning up existing cb-* Scaleway instances (zone: ${SCW_ZONE})..."
    local zone="${SCW_ZONE}"
    local resp ids
    resp=$(curl -s -H "X-Auth-Token: ${SCW_SECRET_KEY}" \
        "https://api.scaleway.com/instance/v1/zones/${zone}/servers" 2>/dev/null || echo '{"servers":[]}')
    ids=$(echo "$resp" | python3 -c "
import sys, json
data = json.load(sys.stdin)
for s in data.get('servers', []):
    if s.get('name','').startswith('cb-'):
        print(s['id'])
" 2>/dev/null || true)
    for sid in $ids; do
        log_info "Deleting Scaleway instance: $sid"
        curl -s -X DELETE -H "X-Auth-Token: ${SCW_SECRET_KEY}" \
            "https://api.scaleway.com/instance/v1/zones/${zone}/servers/${sid}" >/dev/null 2>&1 || true
    done
    log_info "✓ Scaleway cleanup done"
}

provider_verify_vm_deleted() {
    local instance_id="$1"
    local timeout="${2:-${VM_DELETION_TIMEOUT}}"
    # instance_id format: scaleway/<zone>/<server-id>
    local zone server_id
    zone=$(echo "$instance_id" | cut -d'/' -f2)
    server_id=$(echo "$instance_id" | cut -d'/' -f3)
    [ -z "$server_id" ] && { log_warn "Could not parse server ID"; return 0; }

    log_info "Verifying Scaleway instance ${server_id} is deleted (timeout: ${timeout}s)..."
    local elapsed=0
    while [ $elapsed -lt $timeout ]; do
        local code
        code=$(curl -s -o /dev/null -w "%{http_code}" -H "X-Auth-Token: ${SCW_SECRET_KEY}" \
            "https://api.scaleway.com/instance/v1/zones/${zone}/servers/${server_id}" 2>/dev/null || echo "000")
        if [ "$code" = "404" ]; then
            log_info "✓ Scaleway instance ${server_id} confirmed deleted"
            return 0
        fi
        echo -n "."
        sleep 15
        elapsed=$((elapsed + 15))
    done
    log_error "Timeout: Scaleway instance ${server_id} still exists"
    return 1
}

# ── Run ───────────────────────────────────────────────────────────────────────

main_e2e
