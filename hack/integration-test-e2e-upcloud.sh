#!/bin/bash
set -euo pipefail

# End-to-end integration test for the UpCloud provider.
# See hack/e2e-common.sh for the shared test framework.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ── Provider Configuration ────────────────────────────────────────────────────

PROVIDER_NAME="upcloud"
NODECLASS_NAME="e2e-upcloud-nodeclass"
NODEPOOL_NAME="e2e-upcloud-nodepool"
PENDING_POD_NAME="e2e-upcloud-resource-hungry-pod"
PROVIDER_MAX_PRICE="${UPCLOUD_MAX_PRICE_EUR_PER_HOUR:-0.01}"
PROVIDER_POD_CPU="1"
PROVIDER_POD_MEMORY="512Mi"

UPCLOUD_API_TOKEN="${UPCLOUD_API_TOKEN:?UPCLOUD_API_TOKEN must be set}"
UPCLOUD_ZONE="${UPCLOUD_ZONE:-fi-hel1}"

# Source shared library
source "${SCRIPT_DIR}/e2e-common.sh"

# ── Provider Callbacks ────────────────────────────────────────────────────────

provider_check_credentials() {
    [ -z "${UPCLOUD_API_TOKEN}" ] && { log_error "UPCLOUD_API_TOKEN not set"; missing=$((missing+1)); }
    if ! command -v curl &>/dev/null; then log_error "curl not found"; missing=$((missing+1)); fi
}

provider_pre_check() {
    log_info "Checking CloudBroker has UpCloud recommendations..."
    local url="${CLOUDBROKER_PRECHECK_URL:-http://127.0.0.1:8000}"
    local resp
    resp=$(curl -s -X POST "${url}/api/recommendations" \
        -H "Content-Type: application/json" \
        -d "{\"min_vcpu\":1,\"min_ram_gb\":1,\"arch\":\"x86_64\",\"region_constraint\":\"ANY\",\"max_price_eur_per_hour\":${PROVIDER_MAX_PRICE},\"limit\":5,\"allowed_providers\":[\"upcloud\"]}" \
        2>/dev/null || echo '[]')
    local count
    count=$(echo "$resp" | python3 -c "import sys,json; print(len(json.load(sys.stdin)))" 2>/dev/null || echo "0")
    if [ "$count" -gt 0 ]; then
        log_info "✓ CloudBroker returned ${count} UpCloud recommendation(s)"
    else
        log_error "CloudBroker returned 0 UpCloud recommendations. Ensure CloudBroker has UpCloud data ingested."
        log_error "Run: cd ../cloudbroker && make ingest-upcloud"
        exit 1
    fi
}

provider_create_controller_secrets() {
    kubectl create secret generic upcloud-credentials \
        --from-literal=UPCLOUD_API_TOKEN="${UPCLOUD_API_TOKEN}" \
        --namespace="${CONTROLLER_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -
}

provider_create_test_secrets() {
    log_info "Creating UpCloud credentials secret in test namespace..."
    kubectl create secret generic upcloud-credentials \
        --from-literal=UPCLOUD_API_TOKEN="${UPCLOUD_API_TOKEN}" \
        --namespace="${TEST_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -
    log_info "✓ UpCloud secret created"
}

provider_controller_env_block() {
    cat <<'EOF'
        - name: UPCLOUD_API_TOKEN
          valueFrom:
            secretKeyRef:
              name: upcloud-credentials
              key: UPCLOUD_API_TOKEN
EOF
}

provider_nodeclass_spec() {
    cat <<EOF
  upcloud:
    zone: "${UPCLOUD_ZONE}"
    apiTokenSecretRef:
      name: upcloud-credentials
      key: UPCLOUD_API_TOKEN
EOF
}

provider_cleanup_vms() {
    log_info "Cleaning up existing cb-* UpCloud servers..."
    local resp ids
    resp=$(curl -s -H "Authorization: Bearer ${UPCLOUD_API_TOKEN}" \
        "https://api.upcloud.com/1.3/server" 2>/dev/null || echo '{"servers":{"server":[]}}')
    ids=$(echo "$resp" | python3 -c "
import sys, json
data = json.load(sys.stdin)
servers = data.get('servers', {}).get('server', [])
for s in servers:
    title = s.get('title', '')
    hostname = s.get('hostname', '')
    if title.startswith('cb-') or hostname.startswith('cb-'):
        print(s['uuid'])
" 2>/dev/null || true)
    for uuid in $ids; do
        log_info "Stopping UpCloud server: $uuid"
        curl -s -X POST -H "Authorization: Bearer ${UPCLOUD_API_TOKEN}" \
            -H "Content-Type: application/json" \
            "https://api.upcloud.com/1.3/server/${uuid}/stop" \
            -d '{"stop_server":{"stop_type":"hard"}}' >/dev/null 2>&1 || true
        local wait=0
        while [ $wait -lt 30 ]; do
            local state
            state=$(curl -s -H "Authorization: Bearer ${UPCLOUD_API_TOKEN}" \
                "https://api.upcloud.com/1.3/server/${uuid}" 2>/dev/null | \
                python3 -c "import sys,json; print(json.load(sys.stdin).get('server',{}).get('state',''))" 2>/dev/null || echo "")
            [ "$state" = "stopped" ] && break
            sleep 2; wait=$((wait + 2))
        done
        log_info "Deleting UpCloud server: $uuid"
        curl -s -X DELETE -H "Authorization: Bearer ${UPCLOUD_API_TOKEN}" \
            "https://api.upcloud.com/1.3/server/${uuid}?storages=1" >/dev/null 2>&1 || true
    done
    log_info "✓ UpCloud cleanup done"
}

provider_verify_vm_deleted() {
    local instance_id="$1"
    local timeout="${2:-${VM_DELETION_TIMEOUT}}"
    # instance_id format: upcloud/<zone>/<server-uuid>
    local server_uuid
    server_uuid=$(echo "$instance_id" | awk -F'/' '{print $NF}')
    [ -z "$server_uuid" ] && { log_warn "Could not parse server UUID"; return 0; }

    log_info "Verifying UpCloud server ${server_uuid} is deleted (timeout: ${timeout}s)..."
    local elapsed=0
    while [ $elapsed -lt $timeout ]; do
        local code
        code=$(curl -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer ${UPCLOUD_API_TOKEN}" \
            "https://api.upcloud.com/1.3/server/${server_uuid}" 2>/dev/null || echo "000")
        if [ "$code" = "404" ]; then
            log_info "✓ UpCloud server ${server_uuid} confirmed deleted"
            return 0
        fi
        echo -n "."
        sleep 15
        elapsed=$((elapsed + 15))
    done
    log_error "Timeout: UpCloud server ${server_uuid} still exists"
    return 1
}

# ── Run ───────────────────────────────────────────────────────────────────────

main_e2e
