#!/bin/bash
set -euo pipefail

# End-to-end integration test for the GCP provider.
# See hack/e2e-common.sh for the shared test framework.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ── Provider Configuration ────────────────────────────────────────────────────

PROVIDER_NAME="gcp"
NODECLASS_NAME="e2e-nodeclass"
NODEPOOL_NAME="e2e-gcp-nodepool"
PENDING_POD_NAME="e2e-resource-hungry-pod"

GCP_PROJECT="${GCP_PROJECT:?GCP_PROJECT must be set (e.g. in .env or environment)}"
GCP_ZONE="${GCP_ZONE:-us-central1-a}"

# Source shared library (loads .env, sets common vars, defines helpers)
source "${SCRIPT_DIR}/e2e-common.sh"

# ── Provider Callbacks ────────────────────────────────────────────────────────

provider_check_credentials() {
    if [ -z "${GOOGLE_APPLICATION_CREDENTIALS:-}" ]; then
        log_error "GOOGLE_APPLICATION_CREDENTIALS not set"; missing=$((missing+1))
    elif [ ! -f "${GOOGLE_APPLICATION_CREDENTIALS}" ]; then
        log_error "GCP key not found: $GOOGLE_APPLICATION_CREDENTIALS"; missing=$((missing+1))
    fi
    if ! command -v gcloud &>/dev/null; then log_error "gcloud not found"; missing=$((missing+1)); else log_info "✓ gcloud"; fi
}

provider_create_controller_secrets() {
    kubectl create secret generic gcp-credentials \
        --from-file=credentials.json="${GOOGLE_APPLICATION_CREDENTIALS}" \
        --namespace="${CONTROLLER_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -
}

provider_controller_env_block() {
    cat <<'EOF'
        - name: GOOGLE_APPLICATION_CREDENTIALS
          value: /var/run/secrets/gcp/credentials.json
EOF
}

provider_controller_volume_mounts() {
    cat <<'EOF'
        volumeMounts:
        - name: gcp-credentials
          mountPath: /var/run/secrets/gcp
          readOnly: true
EOF
}

provider_controller_volumes() {
    cat <<'EOF'
      volumes:
      - name: gcp-credentials
        secret:
          secretName: gcp-credentials
EOF
}

provider_nodeclass_spec() {
    cat <<EOF
  gcp:
    project: "${GCP_PROJECT}"
    zone: "${GCP_ZONE}"
    imageFamily: "ubuntu-2204-lts"
    imageProject: "ubuntu-os-cloud"
EOF
}

provider_cloudbroker_ingest() {
    log_info "Ingesting GCP (make ingest-gcp)..."
    (cd "${CLOUDBROKER_DIR}" && make ingest-gcp 2>&1) | tail -5
}

provider_cleanup_vms() {
    log_info "Cleaning up existing cb-* GCP VMs${GCP_ZONE:+ (zone: ${GCP_ZONE})}..."
    if ! command -v gcloud &>/dev/null; then
        log_warn "gcloud not found; skipping GCP VM cleanup"
        return 0
    fi
    local total=0 name zone zones_arg=""
    [ -n "${GCP_ZONE}" ] && zones_arg="--zones=${GCP_ZONE}"
    while IFS=',' read -r name zone; do
        [ -z "$name" ] && continue
        log_info "Deleting VM: $name (zone: $zone)"
        gcloud compute instances delete "$name" --zone="${zone}" --project="${GCP_PROJECT}" --quiet 2>/dev/null || true
        total=$((total + 1))
    done < <(gcloud compute instances list --project="${GCP_PROJECT}" --filter="name~^cb-" ${zones_arg} --format="csv[no-heading](name,zone.basename())" 2>/dev/null || true)
    [ $total -gt 0 ] && log_info "✓ Deleted $total cb-* VM(s)" || log_info "No cb-* VMs found"
}

provider_verify_vm_deleted() {
    local instance_id="$1"
    local timeout="${2:-${VM_DELETION_TIMEOUT}}"
    local instance_name instance_zone="${GCP_ZONE}"

    instance_name=$(echo "$instance_id" | awk -F'/' '{print $NF}')
    if [[ "$instance_id" == *"/zones/"*"/instances/"* ]]; then
        instance_zone=$(echo "$instance_id" | sed -E 's|.*/zones/([^/]+)/instances/.*|\1|')
        [[ "$instance_zone" == "$instance_id" ]] && instance_zone="${GCP_ZONE}"
    fi

    log_info "Verifying GCP instance ${instance_name} is deleted (timeout: ${timeout}s)..."
    local elapsed=0
    while [ $elapsed -lt $timeout ]; do
        if ! gcloud compute instances describe "$instance_name" --zone="${instance_zone}" --project="${GCP_PROJECT}" &>/dev/null; then
            log_info "✓ GCP instance ${instance_name} confirmed deleted"
            return 0
        fi
        echo -n "."
        sleep 15
        elapsed=$((elapsed + 15))
    done
    log_error "Timeout: GCP instance ${instance_name} still exists"
    return 1
}

# ── Run ───────────────────────────────────────────────────────────────────────

main_e2e
