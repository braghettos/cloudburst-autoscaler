#!/bin/bash
set -euo pipefail

# End-to-end integration test for Contabo.
# See hack/e2e-common.sh for the shared test framework.
# Note: Contabo cancel schedules deletion; instance may remain until next billing cycle.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ── Provider Configuration ────────────────────────────────────────────────────

PROVIDER_NAME="contabo"
NODECLASS_NAME="e2e-contabo-nodeclass"
NODEPOOL_NAME="e2e-contabo-nodepool"
PENDING_POD_NAME="e2e-contabo-resource-hungry-pod"

CONTABO_CLIENT_ID="${CONTABO_CLIENT_ID:?CONTABO_CLIENT_ID must be set}"
CONTABO_CLIENT_SECRET="${CONTABO_CLIENT_SECRET:?CONTABO_CLIENT_SECRET must be set}"
CONTABO_API_USER="${CONTABO_API_USER:?CONTABO_API_USER must be set}"
CONTABO_API_PASSWORD="${CONTABO_API_PASSWORD:?CONTABO_API_PASSWORD must be set}"
CONTABO_REGION="${CONTABO_REGION:-eu-de-1}"

# Source shared library
source "${SCRIPT_DIR}/e2e-common.sh"

# ── Provider Callbacks ────────────────────────────────────────────────────────

provider_check_credentials() {
    [ -z "${CONTABO_CLIENT_ID}" ] && { log_error "CONTABO_CLIENT_ID not set"; missing=$((missing+1)); }
    [ -z "${CONTABO_CLIENT_SECRET}" ] && { log_error "CONTABO_CLIENT_SECRET not set"; missing=$((missing+1)); }
    [ -z "${CONTABO_API_USER}" ] && { log_error "CONTABO_API_USER not set"; missing=$((missing+1)); }
    [ -z "${CONTABO_API_PASSWORD}" ] && { log_error "CONTABO_API_PASSWORD not set"; missing=$((missing+1)); }
}

_get_contabo_token() {
    curl -sf -X POST "https://auth.contabo.com/auth/realms/contabo/protocol/openid-connect/token" \
        -H "Content-Type: application/x-www-form-urlencoded" \
        -d "client_id=${CONTABO_CLIENT_ID}" \
        -d "client_secret=${CONTABO_CLIENT_SECRET}" \
        -d "username=${CONTABO_API_USER}" \
        -d "password=${CONTABO_API_PASSWORD}" \
        -d "grant_type=password" 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin).get('access_token',''))" 2>/dev/null || echo ""
}

provider_create_controller_secrets() {
    kubectl create secret generic contabo-credentials \
        --from-literal=CONTABO_CLIENT_ID="${CONTABO_CLIENT_ID}" \
        --from-literal=CONTABO_CLIENT_SECRET="${CONTABO_CLIENT_SECRET}" \
        --from-literal=CONTABO_API_USER="${CONTABO_API_USER}" \
        --from-literal=CONTABO_API_PASSWORD="${CONTABO_API_PASSWORD}" \
        --namespace="${CONTROLLER_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -
}

provider_create_test_secrets() {
    log_info "Creating Contabo credentials secret in test namespace..."
    kubectl create secret generic contabo-credentials \
        --from-literal=CONTABO_CLIENT_ID="${CONTABO_CLIENT_ID}" \
        --from-literal=CONTABO_CLIENT_SECRET="${CONTABO_CLIENT_SECRET}" \
        --from-literal=CONTABO_API_USER="${CONTABO_API_USER}" \
        --from-literal=CONTABO_API_PASSWORD="${CONTABO_API_PASSWORD}" \
        --namespace="${TEST_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -
    log_info "✓ Contabo secret created"
}

provider_controller_env_block() {
    cat <<'EOF'
        - name: CONTABO_CLIENT_ID
          valueFrom:
            secretKeyRef:
              name: contabo-credentials
              key: CONTABO_CLIENT_ID
        - name: CONTABO_CLIENT_SECRET
          valueFrom:
            secretKeyRef:
              name: contabo-credentials
              key: CONTABO_CLIENT_SECRET
        - name: CONTABO_API_USER
          valueFrom:
            secretKeyRef:
              name: contabo-credentials
              key: CONTABO_API_USER
        - name: CONTABO_API_PASSWORD
          valueFrom:
            secretKeyRef:
              name: contabo-credentials
              key: CONTABO_API_PASSWORD
EOF
}

provider_nodeclass_spec() {
    cat <<EOF
  contabo:
    region: "${CONTABO_REGION}"
    clientIDSecretRef:
      name: contabo-credentials
      key: CONTABO_CLIENT_ID
    clientSecretSecretRef:
      name: contabo-credentials
      key: CONTABO_CLIENT_SECRET
    apiUserSecretRef:
      name: contabo-credentials
      key: CONTABO_API_USER
    apiPasswordSecretRef:
      name: contabo-credentials
      key: CONTABO_API_PASSWORD
EOF
}

provider_cleanup_vms() {
    log_info "Cleaning up existing cb-* Contabo instances..."
    local token
    token=$(_get_contabo_token)
    [ -z "$token" ] && { log_warn "Could not get Contabo token; skipping cleanup"; return 0; }

    local instances
    instances=$(curl -sf \
        -H "Authorization: Bearer ${token}" \
        -H "x-request-id: $(uuidgen 2>/dev/null || echo "cleanup-$(date +%s)")" \
        "https://api.contabo.com/v1/compute/instances" 2>/dev/null | \
        python3 -c "
import sys, json
data = json.load(sys.stdin)
items = data.get('data', [])
if isinstance(items, dict):
    items = items.get('instances', []) if 'instances' in items else []
for i in items:
    name = i.get('displayName', i.get('name', ''))
    if name and name.startswith('cb-'):
        print(i.get('instanceId', i.get('id', '')))
" 2>/dev/null || true)
    for iid in $instances; do
        [ -z "$iid" ] && continue
        log_info "Cancelling Contabo instance: $iid"
        token=$(_get_contabo_token)
        [ -n "$token" ] && curl -sf -X POST \
            -H "Authorization: Bearer ${token}" \
            -H "x-request-id: $(uuidgen 2>/dev/null || echo "cancel-$(date +%s)")" \
            -H "Content-Type: application/json" \
            "https://api.contabo.com/v1/compute/instances/${iid}/cancel" 2>/dev/null || true
    done
    log_info "✓ Contabo cleanup done"
}

provider_verify_vm_deleted() {
    local instance_id="$1"
    local timeout="${2:-${VM_DELETION_TIMEOUT}}"
    # instance_id format: contabo/<region>/<instanceId>
    local inst_id
    inst_id=$(echo "$instance_id" | cut -d'/' -f3)
    [ -z "$inst_id" ] && { log_warn "Could not parse Contabo instance ID"; return 0; }

    log_info "Verifying Contabo instance ${inst_id} is deleted (timeout: ${timeout}s)..."
    log_info "(Note: Contabo cancel is async; instance may remain until next billing cycle)"
    local elapsed=0
    while [ $elapsed -lt $timeout ]; do
        local token
        token=$(_get_contabo_token)
        if [ -z "$token" ]; then
            echo -n "."; sleep 15; elapsed=$((elapsed + 15)); continue
        fi
        local status_code status
        status_code=$(curl -sf -o /tmp/contabo_check.json -w "%{http_code}" \
            -H "Authorization: Bearer ${token}" \
            -H "x-request-id: $(uuidgen 2>/dev/null || echo "check-$(date +%s)")" \
            "https://api.contabo.com/v1/compute/instances/${inst_id}" 2>/dev/null || echo "000")
        if [ "$status_code" = "404" ] || [ "$status_code" = "000" ]; then
            log_info "✓ Contabo instance ${inst_id} confirmed deleted"; return 0
        fi
        status=$(python3 -c "import json; d=json.load(open('/tmp/contabo_check.json')); print(d.get('data',d).get('status','') if isinstance(d.get('data'), dict) else '')" 2>/dev/null || echo "")
        if [ "$status" = "cancelled" ] || [ "$status" = "deleted" ]; then
            log_info "✓ Contabo instance ${inst_id} cancelled/deleted"; return 0
        fi
        echo -n "."; sleep 15; elapsed=$((elapsed + 15))
    done
    log_error "Timeout: Contabo instance ${inst_id} still exists"; return 1
}

# ── Run ───────────────────────────────────────────────────────────────────────

main_e2e
