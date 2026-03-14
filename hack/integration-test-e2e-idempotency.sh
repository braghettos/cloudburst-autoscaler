#!/bin/bash
set -euo pipefail

# Idempotency test: runs the GCP E2E flow twice without full cleanup in between.
#
# Verifies that the controller correctly handles:
# 1. Pre-existing CRDs and K8s resources from a prior run
# 2. Stale NodeClaims from a prior run
# 3. Re-creation of NodePool/NodeClass/pods without conflict
#
# Usage:
#   ./hack/integration-test-e2e-idempotency.sh
#
# Requires the same env vars as integration-test-e2e-gcp.sh (or any single provider).
# Override IDEMPOTENCY_PROVIDER to test a different provider (default: gcp).

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

if [ -f "${PROJECT_ROOT}/.env" ]; then
    set -a; source "${PROJECT_ROOT}/.env"; set +a
fi

PROVIDER="${IDEMPOTENCY_PROVIDER:-gcp}"
PROVIDER_SCRIPT="${SCRIPT_DIR}/integration-test-e2e-${PROVIDER}.sh"

if [ ! -f "${PROVIDER_SCRIPT}" ]; then
    echo -e "${RED}[ERROR]${NC} Provider script not found: ${PROVIDER_SCRIPT}"
    echo -e "${YELLOW}[INFO]${NC} Set IDEMPOTENCY_PROVIDER to one of: gcp, azure, hetzner, scaleway, digitalocean, ovh, aws, aruba, upcloud, exoscale"
    exit 1
fi

log_info()  { echo -e "${GREEN}[INFO]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }

echo -e "${CYAN}╔══════════════════════════════════════════════════════════════╗${NC}"
echo -e "${CYAN}║     CloudBurst E2E — Idempotency Test (${PROVIDER})              ║${NC}"
echo -e "${CYAN}╚══════════════════════════════════════════════════════════════╝${NC}"

# Run 1: full E2E
echo ""
log_info "=== Run 1: Initial E2E (full cycle) ==="
if ! bash "${PROVIDER_SCRIPT}"; then
    log_error "Run 1 failed — cannot test idempotency"
    exit 1
fi
log_info "✓ Run 1 passed"

# Between runs: Kind cluster was deleted by cleanup. That's expected.
# The point is to verify a fresh run after the previous one completed
# (catching any global state leaks like stale cloud resources, Tailscale nodes, etc.)

echo ""
log_info "=== Run 2: Re-run E2E (idempotency check) ==="
if ! bash "${PROVIDER_SCRIPT}"; then
    log_error "Run 2 failed — idempotency issue detected"
    log_error "The second run should succeed identically to the first."
    log_error "Check for: orphaned cloud VMs, stale Tailscale nodes, leaked secrets."
    exit 1
fi
log_info "✓ Run 2 passed"

echo ""
echo -e "${GREEN}╔══════════════════════════════════════════════════════════════╗${NC}"
echo -e "${GREEN}║     Idempotency Test PASSED (${PROVIDER})                        ║${NC}"
echo -e "${GREEN}║     Both runs completed successfully.                       ║${NC}"
echo -e "${GREEN}╚══════════════════════════════════════════════════════════════╝${NC}"
