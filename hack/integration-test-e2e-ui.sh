#!/bin/bash
set -euo pipefail

# UI-only E2E: Kind cluster + CRDs + UI container + Playwright page navigation.
# No GCP/provisioning. Verifies each UI page loads without "Cluster not found" or API errors.
#
# Prerequisites: docker, kind, kubectl, node/npm (for Playwright)
# Usage: ./hack/integration-test-e2e-ui.sh

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
UI_REPO="${PROJECT_ROOT}/../cloudburst-autoscaler-ui"
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-cloudburst}"
CLUSTER_ID="kind-${KIND_CLUSTER_NAME}"

log_info()  { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }

[ -f "${SCRIPT_DIR}/e2e-common-ui.sh" ] && source "${SCRIPT_DIR}/e2e-common-ui.sh"

cleanup() {
  declare -f stop_ui &>/dev/null && stop_ui
  kind delete cluster --name "${KIND_CLUSTER_NAME}" 2>/dev/null || true
  log_info "Cleanup complete"
}
trap cleanup EXIT

main() {
  log_info "=== CloudBurst UI E2E (Kind + UI + Playwright) ==="
  log_info "Cluster: ${KIND_CLUSTER_NAME} (ID: ${CLUSTER_ID})"
  echo ""

  # Prerequisites
  for cmd in docker kind kubectl node; do
    if ! command -v $cmd &>/dev/null; then
      log_error "$cmd not found"
      exit 1
    fi
  done
  log_info "✓ Prerequisites OK"

  # 1. Kind cluster
  log_info "Creating Kind cluster..."
  export HOST_TS_IP="${HOST_TS_IP:-127.0.0.1}"
  "${SCRIPT_DIR}/create-kind.sh"
  kubectl config use-context "${CLUSTER_ID}" 2>/dev/null || true
  kubectl cluster-info --context "${CLUSTER_ID}" &>/dev/null || { log_error "Kind not reachable"; exit 1; }
  log_info "✓ Kind cluster ready"

  # 2. CRDs (needed for NodeClaims etc. in cluster registry)
  log_info "Installing CRDs..."
  if make -C "${PROJECT_ROOT}" install 2>/dev/null; then
    log_info "✓ CRDs installed"
  else
    for f in "${PROJECT_ROOT}"/config/crd/bases/cloudburst.io_*.yaml; do
      [ -f "$f" ] && kubectl apply -f "$f" 2>/dev/null && true
    done
  fi

  # 3. Deploy UI
  if [ ! -d "${UI_REPO}" ]; then
    log_error "cloudburst-autoscaler-ui not found at ${UI_REPO}"
    exit 1
  fi
  log_info "Deploying UI..."
  deploy_ui || exit 1
  echo ""
  verify_ui_ready || exit 1
  echo ""
  log_info "Waiting for dashboard API to be ready..."
  local cluster_id_ready
  cluster_id_ready=$(curl -s --connect-timeout 5 "http://127.0.0.1:8082/api/clusters" 2>/dev/null | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4) || true
  [ -z "${cluster_id_ready}" ] && cluster_id_ready="${CLUSTER_ID}"
  local i=0
  while [ $i -lt 12 ]; do
    if curl -s --connect-timeout 5 "http://127.0.0.1:8082/api/clusters/${cluster_id_ready}/dashboard" 2>/dev/null | grep -q '"totalPods"'; then
      log_info "✓ Dashboard API ready"
      break
    fi
    sleep 2
    i=$((i + 1))
  done
  if [ $i -ge 12 ]; then
    log_warn "Dashboard API may not be ready; Playwright will run anyway"
  fi
  echo ""

  # 4. Playwright - navigate each page
  if [ ! -d "${UI_REPO}/web" ]; then
    log_error "cloudburst-autoscaler-ui/web not found"
    exit 1
  fi
  log_info "Running Playwright UI E2E..."
  cd "${UI_REPO}/web"
  if ! npm ls @playwright/test &>/dev/null; then
    log_info "Installing npm dependencies (including Playwright)..."
    npm install
  fi
  log_info "Installing Playwright browsers (chromium)..."
  npx playwright install chromium
  # Use cluster ID from API so tests match what backend returns
  E2E_CLUSTER_ID=$(curl -s --connect-timeout 5 "http://127.0.0.1:8082/api/clusters" 2>/dev/null | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4) || true
  [ -z "${E2E_CLUSTER_ID}" ] && E2E_CLUSTER_ID="${CLUSTER_ID}"
  log_info "E2E cluster ID: ${E2E_CLUSTER_ID}"
  if UI_BASE_URL="http://127.0.0.1:8082" CLUSTER_ID="${E2E_CLUSTER_ID}" npm run e2e 2>&1; then
    log_info "✓ All UI pages passed"
  else
    log_error "Playwright E2E failed"
    exit 1
  fi

  echo ""
  log_info "=== UI E2E PASSED ==="
}

main "$@"
