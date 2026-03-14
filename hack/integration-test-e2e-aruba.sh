#!/bin/bash
set -euo pipefail

# End-to-end integration test for the Aruba provider.
# See hack/e2e-common.sh for the shared test framework.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ── Provider Configuration ────────────────────────────────────────────────────

PROVIDER_NAME="aruba"
NODECLASS_NAME="e2e-aruba-nodeclass"
NODEPOOL_NAME="e2e-aruba-nodepool"
PENDING_POD_NAME="e2e-aruba-resource-hungry-pod"
COMPUTE_DELETE_TIMEOUT="${COMPUTE_DELETE_TIMEOUT:-180}"

ARUBA_API_KEY="${ARUBA_API_KEY:?ARUBA_API_KEY must be set}"
ARUBA_API_SECRET="${ARUBA_API_SECRET:?ARUBA_API_SECRET must be set}"
ARUBA_PROJECT_ID="${ARUBA_PROJECT_ID:?ARUBA_PROJECT_ID must be set}"

# Source shared library
source "${SCRIPT_DIR}/e2e-common.sh"

# ── Provider Callbacks ────────────────────────────────────────────────────────

provider_check_credentials() {
    [ -z "${ARUBA_API_KEY}" ] && { log_error "ARUBA_API_KEY not set"; missing=$((missing+1)); }
    [ -z "${ARUBA_API_SECRET}" ] && { log_error "ARUBA_API_SECRET not set"; missing=$((missing+1)); }
    [ -z "${ARUBA_PROJECT_ID}" ] && { log_error "ARUBA_PROJECT_ID not set"; missing=$((missing+1)); }
    if ! command -v go &>/dev/null; then log_warn "go not found; Aruba cleanup may be limited"; fi
}

provider_create_controller_secrets() {
    : # Aruba reads credentials from NodeClass secretRefs at runtime
}

provider_create_test_secrets() {
    log_info "Creating Aruba API credentials secret in test namespace..."
    kubectl create secret generic aruba-api-credentials \
        --from-literal=ARUBA_API_KEY="${ARUBA_API_KEY}" \
        --from-literal=ARUBA_API_SECRET="${ARUBA_API_SECRET}" \
        --namespace="${TEST_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -
    log_info "✓ Aruba secret created"
}

provider_controller_env_block() {
    : # No Aruba-specific env vars for controller
}

provider_nodeclass_spec() {
    cat <<EOF
  aruba:
    projectID: "${ARUBA_PROJECT_ID}"
    applicationKeySecretRef:
      name: aruba-api-credentials
      key: ARUBA_API_KEY
    applicationSecretSecretRef:
      name: aruba-api-credentials
      key: ARUBA_API_SECRET
EOF
}

provider_cleanup_vms() {
    log_info "Running Aruba orphan cleanup..."
    if command -v go &>/dev/null && [ -d "${PROJECT_ROOT}/hack/aruba-cleanup" ]; then
        (cd "${PROJECT_ROOT}" && go run ./hack/aruba-cleanup/main.go 2>&1) || log_warn "Aruba cleanup had warnings"
    else
        log_warn "go or aruba-cleanup not available; skipping"
    fi
    log_info "✓ Aruba cleanup done"
}

provider_verify_vm_deleted() {
    local instance_id="$1"
    local timeout="${2:-${COMPUTE_DELETE_TIMEOUT}}"

    if ! command -v go &>/dev/null || [ ! -d "${PROJECT_ROOT}/hack/aruba-cleanup" ]; then
        log_warn "go or aruba-cleanup not available; skipping verification"
        return 0
    fi

    log_info "Verifying Aruba instance deleted (timeout: ${timeout}s)..."
    local elapsed=0
    while [ $elapsed -lt $timeout ]; do
        if (cd "${PROJECT_ROOT}" && go run ./hack/aruba-cleanup/main.go --verify="${instance_id}" 2>/dev/null); then
            log_info "✓ Aruba instance confirmed deleted"
            return 0
        fi
        echo -n "."
        sleep 10
        elapsed=$((elapsed + 10))
    done
    log_error "Timeout: Aruba instance may still exist"
    return 1
}

provider_post_scaledown_cleanup() {
    if ! command -v go &>/dev/null || [ ! -d "${PROJECT_ROOT}/hack/aruba-cleanup" ]; then
        return 0
    fi
    log_info "Running Aruba orphan cleanup with retry (eventual consistency)..."
    local attempt=1 max_attempts=3
    while [ $attempt -le $max_attempts ]; do
        if (cd "${PROJECT_ROOT}" && go run ./hack/aruba-cleanup/main.go --cleanup-orphans 2>&1); then
            log_info "Verifying no orphans remain..."
            local verify_elapsed=0 verify_timeout=120
            while [ $verify_elapsed -lt $verify_timeout ]; do
                if (cd "${PROJECT_ROOT}" && go run ./hack/aruba-cleanup/main.go --verify-no-orphans 2>/dev/null); then
                    log_info "✓ No Aruba orphans detected (attempt ${attempt})"
                    return 0
                fi
                sleep 10; verify_elapsed=$((verify_elapsed + 10))
            done
        fi
        log_warn "Aruba orphan cleanup attempt ${attempt}/${max_attempts} incomplete, retrying..."
        attempt=$((attempt + 1))
    done
    log_warn "Aruba orphan cleanup did not fully complete after ${max_attempts} attempts"
}

# ── Run ───────────────────────────────────────────────────────────────────────

main_e2e
