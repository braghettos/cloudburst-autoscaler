#!/bin/bash
set -euo pipefail

# End-to-end integration test for the Exoscale provider.
# See hack/e2e-common.sh for the shared test framework.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ── Provider Configuration ────────────────────────────────────────────────────

PROVIDER_NAME="exoscale"
NODECLASS_NAME="e2e-exoscale-nodeclass"
NODEPOOL_NAME="e2e-exoscale-nodepool"
PENDING_POD_NAME="e2e-exoscale-resource-hungry-pod"

EXOSCALE_API_KEY="${EXOSCALE_API_KEY:?EXOSCALE_API_KEY must be set}"
EXOSCALE_API_SECRET="${EXOSCALE_API_SECRET:?EXOSCALE_API_SECRET must be set}"
EXOSCALE_ZONE="${EXOSCALE_ZONE:-ch-gva-2}"

# Source shared library
source "${SCRIPT_DIR}/e2e-common.sh"

# ── Provider Callbacks ────────────────────────────────────────────────────────

provider_check_credentials() {
    [ -z "${EXOSCALE_API_KEY}" ] && { log_error "EXOSCALE_API_KEY not set"; missing=$((missing+1)); }
    [ -z "${EXOSCALE_API_SECRET}" ] && { log_error "EXOSCALE_API_SECRET not set"; missing=$((missing+1)); }
    if command -v exo &>/dev/null; then log_info "✓ exo CLI"; fi
}

provider_create_controller_secrets() {
    kubectl create secret generic exoscale-credentials \
        --from-literal=EXOSCALE_API_KEY="${EXOSCALE_API_KEY}" \
        --from-literal=EXOSCALE_API_SECRET="${EXOSCALE_API_SECRET}" \
        --namespace="${CONTROLLER_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -
}

provider_create_test_secrets() {
    log_info "Creating Exoscale credentials secret in test namespace..."
    kubectl create secret generic exoscale-credentials \
        --from-literal=EXOSCALE_API_KEY="${EXOSCALE_API_KEY}" \
        --from-literal=EXOSCALE_API_SECRET="${EXOSCALE_API_SECRET}" \
        --namespace="${TEST_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -
    log_info "✓ Exoscale secret created"
}

provider_controller_env_block() {
    cat <<'EOF'
        - name: EXOSCALE_API_KEY
          valueFrom:
            secretKeyRef:
              name: exoscale-credentials
              key: EXOSCALE_API_KEY
        - name: EXOSCALE_API_SECRET
          valueFrom:
            secretKeyRef:
              name: exoscale-credentials
              key: EXOSCALE_API_SECRET
EOF
}

provider_nodeclass_spec() {
    cat <<EOF
  exoscale:
    zone: "${EXOSCALE_ZONE}"
    apiKeySecretRef:
      name: exoscale-credentials
      key: EXOSCALE_API_KEY
    apiSecretSecretRef:
      name: exoscale-credentials
      key: EXOSCALE_API_SECRET
EOF
}

provider_cleanup_vms() {
    log_info "Cleaning up existing cb-* Exoscale instances (zone: ${EXOSCALE_ZONE})..."
    if command -v exo &>/dev/null; then
        local instances
        instances=$(exo compute instance list --zone "${EXOSCALE_ZONE}" -O json 2>/dev/null | \
            python3 -c "
import sys, json
data = json.load(sys.stdin)
for i in data:
    if i.get('name','').startswith('cb-'):
        print(i['id'])
" 2>/dev/null || true)
        for iid in $instances; do
            log_info "Deleting Exoscale instance: $iid"
            exo compute instance delete "$iid" --zone "${EXOSCALE_ZONE}" --force 2>/dev/null || true
        done
    else
        log_warn "exo CLI not found; skipping Exoscale VM cleanup"
    fi
    log_info "✓ Exoscale cleanup done"
}

provider_verify_vm_deleted() {
    local instance_id="$1"
    local timeout="${2:-${VM_DELETION_TIMEOUT}}"
    # instance_id format: exoscale/<zone>/<instance-id>
    local zone iid
    zone=$(echo "$instance_id" | cut -d'/' -f2)
    iid=$(echo "$instance_id" | cut -d'/' -f3)
    [ -z "$iid" ] && { log_warn "Could not parse Exoscale instance ID"; return 0; }
    [ -z "$zone" ] && zone="${EXOSCALE_ZONE}"

    if ! command -v exo &>/dev/null; then
        log_warn "exo CLI not found; skipping verification"
        return 0
    fi

    log_info "Verifying Exoscale instance ${iid} is deleted (timeout: ${timeout}s)..."
    local elapsed=0
    while [ $elapsed -lt $timeout ]; do
        if ! exo compute instance show "$iid" --zone "$zone" &>/dev/null 2>&1; then
            log_info "✓ Exoscale instance ${iid} confirmed deleted"
            return 0
        fi
        echo -n "."
        sleep 15
        elapsed=$((elapsed + 15))
    done
    log_error "Timeout: Exoscale instance ${iid} still exists"
    return 1
}

# ── Run ───────────────────────────────────────────────────────────────────────

main_e2e
