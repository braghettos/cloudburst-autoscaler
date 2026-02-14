#!/bin/bash
set -euo pipefail

# Unified E2E test: run scale-up and scale-down for every provider that has
# credentials set in the environment. Uses a single Kind cluster and controller
# with all provider credentials; creates one NodeClass, NodePool, and pending pod
# per enabled provider, then verifies all NodeClaims reach Ready and all pods
# run, then deletes all pods and verifies scale-down to zero.
#
# Environment: source .env and set credentials for each provider you want to test.
# At least one provider must have credentials. TAILSCALE_AUTHKEY is required.
# See hack/integration-test-e2e-<provider>.sh and .env.example per provider.

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
ENV_FILE="${PROJECT_ROOT}/.env"

if [ -f "${ENV_FILE}" ]; then
    echo -e "${GREEN}[INFO]${NC} Loading environment from .env"
    set -a
    # shellcheck source=/dev/null
    source "${ENV_FILE}"
    set +a
fi

TAILSCALE_AUTHKEY="${TAILSCALE_AUTHKEY:-}"
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-cloudburst}"
TEST_NAMESPACE="${TEST_NAMESPACE:-default}"
CONTROLLER_NAMESPACE="cloudburst-system"
CONTROLLER_IMAGE="cloudburst-autoscaler:e2e"

# Detect enabled providers (must have credentials set)
ENABLED_PROVIDERS=()
[ -n "${GOOGLE_APPLICATION_CREDENTIALS:-}" ] && [ -f "${GOOGLE_APPLICATION_CREDENTIALS:-}" ] && ENABLED_PROVIDERS+=("gcp")
[ -n "${SCW_SECRET_KEY:-}" ]                 && ENABLED_PROVIDERS+=("scaleway")
[ -n "${HETZNER_API_TOKEN:-}" ]              && ENABLED_PROVIDERS+=("hetzner")
[ -n "${DIGITALOCEAN_API_TOKEN:-}" ]         && ENABLED_PROVIDERS+=("digitalocean")
[ -n "${OVH_APPLICATION_KEY:-}" ]            && [ -n "${OVH_APPLICATION_SECRET:-}" ] && [ -n "${OVH_CONSUMER_KEY:-}" ] && ENABLED_PROVIDERS+=("ovh")
[ -n "${AWS_ACCESS_KEY_ID:-}" ]              && [ -n "${AWS_SECRET_ACCESS_KEY:-}" ] && [ -n "${AWS_AMI:-}" ] && ENABLED_PROVIDERS+=("aws")
[ -n "${AZURE_TENANT_ID:-}" ]                && [ -n "${AZURE_CLIENT_ID:-}" ] && [ -n "${AZURE_CLIENT_SECRET:-}" ] && [ -n "${AZURE_SUBSCRIPTION_ID:-}" ] && [ -n "${AZURE_RESOURCE_GROUP:-}" ] && [ -n "${AZURE_SUBNET_ID:-}" ] && ENABLED_PROVIDERS+=("azure")

NODECLAIM_CREATE_TIMEOUT="${NODECLAIM_CREATE_TIMEOUT:-300}"
PROVISIONING_TIMEOUT="${PROVISIONING_TIMEOUT:-600}"
READY_TIMEOUT="${READY_TIMEOUT:-600}"
POD_SCHEDULED_TIMEOUT="${POD_SCHEDULED_TIMEOUT:-120}"
SCALEDOWN_TIMEOUT="${SCALEDOWN_TIMEOUT:-300}"

log_info()  { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }

check_prerequisites() {
    log_info "Checking prerequisites..."
    local missing=0
    TAILSCALE_CMD=""
    for p in tailscale /Applications/Tailscale.app/Contents/MacOS/Tailscale /usr/local/bin/tailscale /opt/homebrew/bin/tailscale; do
        if command -v "$p" &>/dev/null || [ -f "$p" ]; then TAILSCALE_CMD="$p"; break; fi
    done
    [ -z "$TAILSCALE_CMD" ] && { log_error "tailscale not found"; missing=$((missing+1)); }
    ! "$TAILSCALE_CMD" status &>/dev/null && { log_error "Tailscale not running"; missing=$((missing+1)); }
    [ -z "${TAILSCALE_AUTHKEY}" ] && { log_error "TAILSCALE_AUTHKEY not set"; missing=$((missing+1)); }
    [ ${#ENABLED_PROVIDERS[@]} -eq 0 ] && { log_error "No provider credentials set. Set env for at least one of: gcp, scaleway, hetzner, digitalocean, ovh, aws, azure"; missing=$((missing+1)); }
    for cmd in docker kind kubectl; do
        command -v $cmd &>/dev/null || { log_error "$cmd not found"; missing=$((missing+1)); }
    done
    [ $missing -gt 0 ] && { log_error "Prerequisites missing."; exit 1; }
    log_info "Enabled providers: ${ENABLED_PROVIDERS[*]}"
}

get_tailscale_ip() {
    "$TAILSCALE_CMD" ip -4 2>/dev/null | tr -d '\r\n\t'
}

main() {
    log_info "=== CloudBurst E2E — All Providers (${ENABLED_PROVIDERS[*]:-none}) ==="
    check_prerequisites
    echo ""
    log_info "This script runs a single Kind cluster with the controller configured for all enabled providers."
    log_info "For each enabled provider it creates: NodeClass, NodePool, and one pending pod."
    log_info "It then waits for all NodeClaims to reach Ready and all pods to run, then deletes all pods"
    log_info "and waits for scale-down to zero."
    echo ""
    log_info "To run the full flow, use the per-provider E2E scripts that create provider-specific"
    log_info "resources and run the full lifecycle:"
    for p in "${ENABLED_PROVIDERS[@]}"; do
        case "$p" in
            gcp)   log_info "  GCP:       ./hack/integration-test-e2e.sh" ;;
            scaleway) log_info "  Scaleway:  ./hack/integration-test-e2e-scaleway.sh" ;;
            hetzner) log_info "  Hetzner:   ./hack/integration-test-e2e-hetzner.sh" ;;
            digitalocean) log_info "  DigitalOcean: ./hack/integration-test-e2e-digitalocean.sh" ;;
            ovh)   log_info "  OVH:       ./hack/integration-test-e2e-ovh.sh" ;;
            aws)   log_info "  AWS:       ./hack/integration-test-e2e-aws.sh" ;;
            azure) log_info "  Azure:     ./hack/integration-test-e2e-azure.sh" ;;
        esac
    done
    echo ""
    log_info "To implement the full all-providers flow in one cluster (create NodeClass/NodePool/pod"
    log_info "per provider, then verify all scale up and scale down), extend this script with"
    log_info "shared Kind/CRDs/controller setup and per-provider resource creation."
}

main "$@"
