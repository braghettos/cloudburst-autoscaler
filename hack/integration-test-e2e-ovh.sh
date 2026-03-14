#!/bin/bash
set -euo pipefail

# End-to-end integration test for the OVH provider.
# See hack/e2e-common.sh for the shared test framework.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ── Provider Configuration ────────────────────────────────────────────────────

PROVIDER_NAME="ovh"
NODECLASS_NAME="e2e-ovh-nodeclass"
NODEPOOL_NAME="e2e-ovh-nodepool"
PENDING_POD_NAME="e2e-ovh-resource-hungry-pod"
VM_DELETION_TIMEOUT="${VM_DELETION_TIMEOUT:-600}"

OVH_APPLICATION_KEY="${OVH_APPLICATION_KEY:?OVH_APPLICATION_KEY must be set}"
OVH_APPLICATION_SECRET="${OVH_APPLICATION_SECRET:?OVH_APPLICATION_SECRET must be set}"
OVH_CONSUMER_KEY="${OVH_CONSUMER_KEY:?OVH_CONSUMER_KEY must be set}"
OVH_PROJECT_ID="${OVH_PROJECT_ID:?OVH_PROJECT_ID must be set}"
OVH_REGION="${OVH_REGION:-}"

# Source shared library
source "${SCRIPT_DIR}/e2e-common.sh"

# ── Provider Callbacks ────────────────────────────────────────────────────────

provider_check_credentials() {
    [ -z "${OVH_APPLICATION_KEY}" ] && { log_error "OVH_APPLICATION_KEY not set"; missing=$((missing+1)); }
    [ -z "${OVH_APPLICATION_SECRET}" ] && { log_error "OVH_APPLICATION_SECRET not set"; missing=$((missing+1)); }
    [ -z "${OVH_CONSUMER_KEY}" ] && { log_error "OVH_CONSUMER_KEY not set"; missing=$((missing+1)); }
    [ -z "${OVH_PROJECT_ID}" ] && { log_error "OVH_PROJECT_ID not set"; missing=$((missing+1)); }
    if ! command -v go &>/dev/null; then log_warn "go not found; OVH cleanup/verify may be limited"; fi
}

provider_create_controller_secrets() {
    kubectl create secret generic ovh-credentials \
        --from-literal=OVH_APPLICATION_KEY="${OVH_APPLICATION_KEY}" \
        --from-literal=OVH_APPLICATION_SECRET="${OVH_APPLICATION_SECRET}" \
        --from-literal=OVH_CONSUMER_KEY="${OVH_CONSUMER_KEY}" \
        --namespace="${CONTROLLER_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -
}

provider_create_test_secrets() {
    log_info "Creating OVH API credentials secret in test namespace..."
    kubectl create secret generic ovh-api-credentials \
        --from-literal=OVH_APPLICATION_KEY="${OVH_APPLICATION_KEY}" \
        --from-literal=OVH_APPLICATION_SECRET="${OVH_APPLICATION_SECRET}" \
        --from-literal=OVH_CONSUMER_KEY="${OVH_CONSUMER_KEY}" \
        --namespace="${TEST_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -
    log_info "✓ OVH secret created"
}

provider_controller_env_block() {
    cat <<'EOF'
        - name: OVH_APPLICATION_KEY
          valueFrom:
            secretKeyRef:
              name: ovh-credentials
              key: OVH_APPLICATION_KEY
        - name: OVH_APPLICATION_SECRET
          valueFrom:
            secretKeyRef:
              name: ovh-credentials
              key: OVH_APPLICATION_SECRET
        - name: OVH_CONSUMER_KEY
          valueFrom:
            secretKeyRef:
              name: ovh-credentials
              key: OVH_CONSUMER_KEY
EOF
}

provider_nodeclass_spec() {
    {
        echo "  ovh:"
        echo "    projectID: \"${OVH_PROJECT_ID}\""
        [ -n "${OVH_REGION}" ] && echo "    region: \"${OVH_REGION}\""
        echo "    applicationKeySecretRef:"
        echo "      name: ovh-api-credentials"
        echo "      key: OVH_APPLICATION_KEY"
        echo "    applicationSecretSecretRef:"
        echo "      name: ovh-api-credentials"
        echo "      key: OVH_APPLICATION_SECRET"
        echo "    consumerKeySecretRef:"
        echo "      name: ovh-api-credentials"
        echo "      key: OVH_CONSUMER_KEY"
    }
}

provider_cleanup_vms() {
    log_info "Cleaning up existing cb-* OVH instances..."
    if command -v go &>/dev/null && [ -d "${PROJECT_ROOT}/hack/ovh-cleanup" ]; then
        (cd "${PROJECT_ROOT}" && go run ./hack/ovh-cleanup/main.go 2>&1) || log_warn "OVH cleanup had warnings"
    else
        log_warn "go or ovh-cleanup not available; skipping OVH VM cleanup"
    fi
    log_info "✓ OVH cleanup done"
}

provider_verify_vm_deleted() {
    local instance_id="$1"
    local timeout="${2:-${VM_DELETION_TIMEOUT}}"

    if ! command -v go &>/dev/null || [ ! -d "${PROJECT_ROOT}/hack/ovh-cleanup" ]; then
        log_warn "go or ovh-cleanup not available; skipping OVH VM deletion verification"
        return 0
    fi

    log_info "Verifying OVH instance deleted (timeout: ${timeout}s)..."
    local elapsed=0
    while [ $elapsed -lt $timeout ]; do
        if (cd "${PROJECT_ROOT}" && go run ./hack/ovh-cleanup/main.go --verify="${instance_id}" 2>/dev/null); then
            log_info "✓ OVH instance confirmed deleted"
            return 0
        fi
        echo -n "."
        sleep 10
        elapsed=$((elapsed + 10))
    done
    log_error "Timeout: OVH instance may still exist"
    return 1
}

# ── Run ───────────────────────────────────────────────────────────────────────

main_e2e
