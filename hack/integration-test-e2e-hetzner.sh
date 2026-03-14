#!/bin/bash
set -euo pipefail

# End-to-end integration test for the Hetzner provider.
# See hack/e2e-common.sh for the shared test framework.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ── Provider Configuration ────────────────────────────────────────────────────

PROVIDER_NAME="hetzner"
NODECLASS_NAME="e2e-hetzner-nodeclass"
NODEPOOL_NAME="e2e-hetzner-nodepool"
PENDING_POD_NAME="e2e-hetzner-resource-hungry-pod"

HETZNER_API_TOKEN="${HETZNER_API_TOKEN:?HETZNER_API_TOKEN must be set}"
HETZNER_LOCATION="${HETZNER_LOCATION:-fsn1}"

# Source shared library
source "${SCRIPT_DIR}/e2e-common.sh"

# ── Provider Callbacks ────────────────────────────────────────────────────────

provider_check_credentials() {
    [ -z "${HETZNER_API_TOKEN}" ] && { log_error "HETZNER_API_TOKEN not set"; missing=$((missing+1)); }
}

provider_create_controller_secrets() {
    kubectl create secret generic hetzner-credentials \
        --from-literal=HETZNER_API_TOKEN="${HETZNER_API_TOKEN}" \
        --namespace="${CONTROLLER_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -
}

provider_create_test_secrets() {
    log_info "Creating Hetzner API token secret in test namespace..."
    kubectl create secret generic hetzner-api-token \
        --from-literal=HETZNER_API_TOKEN="${HETZNER_API_TOKEN}" \
        --namespace="${TEST_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -
    log_info "✓ Hetzner secret created"
}

provider_controller_env_block() {
    cat <<'EOF'
        - name: HETZNER_API_TOKEN
          valueFrom:
            secretKeyRef:
              name: hetzner-credentials
              key: HETZNER_API_TOKEN
EOF
}

provider_nodeclass_spec() {
    cat <<EOF
  hetzner:
    location: "${HETZNER_LOCATION}"
    apiTokenSecretRef:
      name: hetzner-api-token
      key: HETZNER_API_TOKEN
EOF
}

provider_cleanup_vms() {
    log_info "Cleaning up existing cb-* Hetzner servers..."
    if command -v hcloud &>/dev/null; then
        while IFS=$'\t' read -r id name; do
            [ -z "$id" ] && continue
            log_info "Deleting Hetzner server: $name ($id)"
            hcloud server delete "$id" 2>/dev/null || true
        done < <(hcloud server list -o noheader -o columns=id,name 2>/dev/null | grep "cb-" || true)
    else
        local resp names
        resp=$(curl -s -H "Authorization: Bearer ${HETZNER_API_TOKEN}" "https://api.hetzner.cloud/v1/servers?per_page=50" 2>/dev/null || echo '{"servers":[]}')
        names=$(echo "$resp" | python3 -c "
import sys, json
data = json.load(sys.stdin)
for s in data.get('servers', []):
    if s.get('name','').startswith('cb-'):
        print(s['id'])
" 2>/dev/null || true)
        for sid in $names; do
            log_info "Deleting Hetzner server: $sid"
            curl -s -X DELETE -H "Authorization: Bearer ${HETZNER_API_TOKEN}" "https://api.hetzner.cloud/v1/servers/${sid}" >/dev/null 2>&1 || true
        done
    fi
    log_info "✓ Hetzner cleanup done"
}

provider_verify_vm_deleted() {
    local instance_id="$1"
    local timeout="${2:-${VM_DELETION_TIMEOUT}}"
    # instance_id format: hetzner/<location>/<server-id>
    local server_id
    server_id=$(echo "$instance_id" | awk -F'/' '{print $NF}')
    [ -z "$server_id" ] && { log_warn "Could not parse server ID"; return 0; }

    log_info "Verifying Hetzner server ${server_id} is deleted (timeout: ${timeout}s)..."
    local elapsed=0
    while [ $elapsed -lt $timeout ]; do
        local code
        code=$(curl -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer ${HETZNER_API_TOKEN}" "https://api.hetzner.cloud/v1/servers/${server_id}" 2>/dev/null || echo "000")
        if [ "$code" = "404" ]; then
            log_info "✓ Hetzner server ${server_id} confirmed deleted"
            return 0
        fi
        echo -n "."
        sleep 15
        elapsed=$((elapsed + 15))
    done
    log_error "Timeout: Hetzner server ${server_id} still exists"
    return 1
}

# ── Run ───────────────────────────────────────────────────────────────────────

main_e2e
