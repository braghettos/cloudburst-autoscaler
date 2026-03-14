#!/bin/bash
set -euo pipefail

# End-to-end integration test for IBM Cloud VPC.
# See hack/e2e-common.sh for the shared test framework.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ── Provider Configuration ────────────────────────────────────────────────────

PROVIDER_NAME="ibm_cloud"
NODECLASS_NAME="e2e-ibm-cloud-nodeclass"
NODEPOOL_NAME="e2e-ibm-cloud-nodepool"
PENDING_POD_NAME="e2e-ibm-cloud-resource-hungry-pod"

IBMCLOUD_API_KEY="${IBMCLOUD_API_KEY:?IBMCLOUD_API_KEY must be set}"
IBMCLOUD_RESOURCE_GROUP="${IBMCLOUD_RESOURCE_GROUP:?IBMCLOUD_RESOURCE_GROUP must be set}"
IBMCLOUD_SUBNET_ID="${IBMCLOUD_SUBNET_ID:?IBMCLOUD_SUBNET_ID must be set}"
IBMCLOUD_REGION="${IBMCLOUD_REGION:?IBMCLOUD_REGION must be set}"

# Source shared library
source "${SCRIPT_DIR}/e2e-common.sh"

# ── Provider Callbacks ────────────────────────────────────────────────────────

provider_check_credentials() {
    [ -z "${IBMCLOUD_API_KEY}" ] && { log_error "IBMCLOUD_API_KEY not set"; missing=$((missing+1)); }
    [ -z "${IBMCLOUD_RESOURCE_GROUP}" ] && { log_error "IBMCLOUD_RESOURCE_GROUP not set"; missing=$((missing+1)); }
    [ -z "${IBMCLOUD_SUBNET_ID}" ] && { log_error "IBMCLOUD_SUBNET_ID not set"; missing=$((missing+1)); }
    [ -z "${IBMCLOUD_REGION}" ] && { log_error "IBMCLOUD_REGION not set"; missing=$((missing+1)); }
}

_get_ibm_token() {
    curl -sf -X POST "https://iam.cloud.ibm.com/identity/token" \
        -H "Content-Type: application/x-www-form-urlencoded" \
        -d "grant_type=urn:ibm:params:oauth:grant-type:apikey" \
        -d "apikey=${IBMCLOUD_API_KEY}" 2>/dev/null | \
        python3 -c "import sys,json; print(json.load(sys.stdin).get('access_token',''))" 2>/dev/null || echo ""
}

provider_create_controller_secrets() {
    kubectl create secret generic ibmcloud-credentials \
        --from-literal=IBMCLOUD_API_KEY="${IBMCLOUD_API_KEY}" \
        --namespace="${CONTROLLER_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -
}

provider_create_test_secrets() {
    log_info "Creating IBM Cloud credentials secret in test namespace..."
    kubectl create secret generic ibmcloud-credentials \
        --from-literal=IBMCLOUD_API_KEY="${IBMCLOUD_API_KEY}" \
        --namespace="${TEST_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -
    log_info "✓ IBM Cloud secret created"
}

provider_controller_env_block() {
    cat <<'EOF'
        - name: IBMCLOUD_API_KEY
          valueFrom:
            secretKeyRef:
              name: ibmcloud-credentials
              key: IBMCLOUD_API_KEY
EOF
}

provider_nodeclass_spec() {
    cat <<EOF
  ibmCloud:
    resourceGroup: "${IBMCLOUD_RESOURCE_GROUP}"
    subnetID: "${IBMCLOUD_SUBNET_ID}"
    region: "${IBMCLOUD_REGION}"
    apiKeySecretRef:
      name: ibmcloud-credentials
      key: IBMCLOUD_API_KEY
EOF
}

provider_cleanup_vms() {
    log_info "Cleaning up existing cb-* IBM Cloud VPC instances..."
    local token
    token=$(_get_ibm_token)
    [ -z "$token" ] && { log_warn "Could not get IBM Cloud token; skipping cleanup"; return 0; }

    local base_url="https://${IBMCLOUD_REGION}.iaas.cloud.ibm.com/v1"
    local instances
    instances=$(curl -sf -H "Authorization: Bearer ${token}" \
        "${base_url}/instances?version=2025-06-01&generation=2" 2>/dev/null | \
        python3 -c "
import sys, json
data = json.load(sys.stdin)
items = data.get('instances', [])
for i in items:
    name = i.get('name', '')
    if name.startswith('cb-'):
        print(i.get('id', ''))
" 2>/dev/null || true)
    for iid in $instances; do
        [ -z "$iid" ] && continue
        log_info "Deleting IBM Cloud instance: $iid"
        curl -sf -X DELETE -H "Authorization: Bearer ${token}" \
            "${base_url}/instances/${iid}?version=2025-06-01&generation=2" 2>/dev/null || true
    done
    log_info "✓ IBM Cloud cleanup done"
}

provider_verify_vm_deleted() {
    local instance_id="$1"
    local timeout="${2:-${VM_DELETION_TIMEOUT}}"
    # instance_id format: ibm_cloud/<region>/<instance_id>
    local inst_id
    inst_id=$(echo "$instance_id" | cut -d'/' -f3)
    [ -z "$inst_id" ] && { log_warn "Could not parse IBM Cloud instance ID"; return 0; }

    log_info "Verifying IBM Cloud instance ${inst_id} is deleted (timeout: ${timeout}s)..."
    local base_url="https://${IBMCLOUD_REGION}.iaas.cloud.ibm.com/v1"
    local elapsed=0
    while [ $elapsed -lt $timeout ]; do
        local token
        token=$(_get_ibm_token)
        [ -z "$token" ] && { echo -n "."; sleep 15; elapsed=$((elapsed + 15)); continue; }
        local status_code
        status_code=$(curl -sf -o /dev/null -w "%{http_code}" \
            -H "Authorization: Bearer ${token}" \
            "${base_url}/instances/${inst_id}?version=2025-06-01&generation=2" 2>/dev/null || echo "000")
        if [ "$status_code" = "404" ] || [ "$status_code" = "000" ]; then
            log_info "✓ IBM Cloud instance ${inst_id} confirmed deleted"; return 0
        fi
        echo -n "."; sleep 15; elapsed=$((elapsed + 15))
    done
    log_error "Timeout: IBM Cloud instance ${inst_id} still exists"; return 1
}

# ── Run ───────────────────────────────────────────────────────────────────────

main_e2e
