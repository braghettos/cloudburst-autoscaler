#!/bin/bash
set -euo pipefail

# End-to-end integration test for Gcore Cloud.
# See hack/e2e-common.sh for the shared test framework.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ── Provider Configuration ────────────────────────────────────────────────────

PROVIDER_NAME="gcore"
NODECLASS_NAME="e2e-gcore-nodeclass"
NODEPOOL_NAME="e2e-gcore-nodepool"
PENDING_POD_NAME="e2e-gcore-resource-hungry-pod"

GCORE_API_KEY="${GCORE_API_KEY:?GCORE_API_KEY must be set}"
GCORE_PROJECT_ID="${GCORE_PROJECT_ID:?GCORE_PROJECT_ID must be set}"
GCORE_REGION="${GCORE_REGION:-lux-1}"

# Source shared library
source "${SCRIPT_DIR}/e2e-common.sh"

# ── Provider Callbacks ────────────────────────────────────────────────────────

provider_check_credentials() {
    [ -z "${GCORE_API_KEY}" ] && { log_error "GCORE_API_KEY not set"; missing=$((missing+1)); }
    [ -z "${GCORE_PROJECT_ID}" ] && { log_error "GCORE_PROJECT_ID not set"; missing=$((missing+1)); }
}

provider_create_controller_secrets() {
    kubectl create secret generic gcore-credentials \
        --from-literal=GCORE_API_KEY="${GCORE_API_KEY}" \
        --namespace="${CONTROLLER_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -
}

provider_create_test_secrets() {
    log_info "Creating Gcore credentials secret in test namespace..."
    kubectl create secret generic gcore-credentials \
        --from-literal=GCORE_API_KEY="${GCORE_API_KEY}" \
        --namespace="${TEST_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -
    log_info "✓ Gcore secret created"
}

provider_controller_env_block() {
    cat <<'EOF'
        - name: GCORE_API_KEY
          valueFrom:
            secretKeyRef:
              name: gcore-credentials
              key: GCORE_API_KEY
EOF
}

provider_nodeclass_spec() {
    cat <<EOF
  gcore:
    projectID: ${GCORE_PROJECT_ID}
    region: "${GCORE_REGION}"
    apiKeySecretRef:
      name: gcore-credentials
      key: GCORE_API_KEY
EOF
}

_get_gcore_region_id() {
    curl -sf -H "Authorization: apikey ${GCORE_API_KEY}" \
        "https://api.gcore.com/cloud/v1/regions" 2>/dev/null | \
        python3 -c "
import sys, json
data = json.load(sys.stdin)
slug = '${GCORE_REGION}'.lower()
for r in data.get('results', []):
    dn = (r.get('display_name') or '').lower()
    kn = (r.get('keystone_name') or '').lower()
    if slug in dn or slug in kn or 'luxembourg' in dn and 'lux' in slug or 'frankfurt' in dn and 'fra' in slug:
        print(r.get('id', ''))
        break
" 2>/dev/null || echo ""
}

provider_cleanup_vms() {
    log_info "Cleaning up existing cb-* Gcore instances..."
    local region_id
    region_id=$(_get_gcore_region_id)
    [ -z "$region_id" ] && { log_warn "Could not get Gcore region ID; skipping cleanup"; return 0; }

    local instances
    instances=$(curl -sf -H "Authorization: apikey ${GCORE_API_KEY}" \
        "https://api.gcore.com/cloud/v1/instances/${GCORE_PROJECT_ID}/${region_id}" 2>/dev/null | \
        python3 -c "
import sys, json
data = json.load(sys.stdin)
items = data.get('results', data) if isinstance(data, dict) else data
if not isinstance(items, list):
    items = []
for i in items:
    name = i.get('name', i.get('instance', {}).get('name', ''))
    if name and name.startswith('cb-'):
        print(i.get('id', i.get('instance_id', i.get('instance', {}).get('id', ''))))
" 2>/dev/null || true)
    for iid in $instances; do
        [ -z "$iid" ] && continue
        log_info "Deleting Gcore instance: $iid"
        curl -sf -X DELETE \
            -H "Authorization: apikey ${GCORE_API_KEY}" \
            "https://api.gcore.com/cloud/v1/instances/${GCORE_PROJECT_ID}/${region_id}/${iid}" 2>/dev/null || true
    done
    log_info "✓ Gcore cleanup done"
}

provider_verify_vm_deleted() {
    local instance_id="$1"
    local timeout="${2:-${VM_DELETION_TIMEOUT}}"
    # instance_id format: gcore/<region_id>/<instance_id>
    local region_id inst_id
    region_id=$(echo "$instance_id" | cut -d'/' -f2)
    inst_id=$(echo "$instance_id" | cut -d'/' -f3)
    [ -z "$inst_id" ] && { log_warn "Could not parse Gcore instance ID"; return 0; }

    log_info "Verifying Gcore instance ${inst_id} is deleted (timeout: ${timeout}s)..."
    local elapsed=0
    while [ $elapsed -lt $timeout ]; do
        local status_code
        status_code=$(curl -sf -o /dev/null -w "%{http_code}" \
            -H "Authorization: apikey ${GCORE_API_KEY}" \
            "https://api.gcore.com/cloud/v1/instances/${GCORE_PROJECT_ID}/${region_id}/${inst_id}" 2>/dev/null || echo "000")
        if [ "$status_code" = "404" ] || [ "$status_code" = "000" ]; then
            log_info "✓ Gcore instance ${inst_id} confirmed deleted"; return 0
        fi
        echo -n "."; sleep 15; elapsed=$((elapsed + 15))
    done
    log_error "Timeout: Gcore instance ${inst_id} still exists"; return 1
}

# ── Run ───────────────────────────────────────────────────────────────────────

main_e2e
