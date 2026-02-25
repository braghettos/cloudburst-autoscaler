#!/bin/bash
set -uo pipefail

# Run all available E2E provider tests sequentially, collecting results.
# Each test creates/destroys its own Kind cluster.

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
LOG_DIR="${PROJECT_ROOT}/e2e-logs"
mkdir -p "${LOG_DIR}"

# Source .env file if it exists (same as individual provider scripts)
if [ -f "${PROJECT_ROOT}/.env" ]; then
    echo -e "${GREEN}[INFO]${NC} Loading environment variables from .env file"
    set -a
    # shellcheck source=/dev/null
    source "${PROJECT_ROOT}/.env"
    set +a
fi

TIMESTAMP=$(date +%Y%m%d-%H%M%S)

# Export GCP_PROJECT from SA key if not set
if [ -z "${GCP_PROJECT:-}" ]; then
    GCP_SA_FILE="${GOOGLE_APPLICATION_CREDENTIALS:-}"
    if [ -n "$GCP_SA_FILE" ] && [ -f "$GCP_SA_FILE" ]; then
        export GCP_PROJECT=$(python3 -c "import json; print(json.load(open('$GCP_SA_FILE'))['project_id'])" 2>/dev/null || true)
    fi
fi

TOTAL_START=$(date +%s)
PROVIDERS_RUN=0
PROVIDERS_PASS=0
PROVIDERS_FAIL=0
PROVIDERS_SKIP=0
SUMMARY=""

run_test() {
    local provider=$1
    local script=$2
    local log_file="${LOG_DIR}/${provider}-${TIMESTAMP}.log"
    
    echo ""
    echo -e "${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
    echo -e "${CYAN}  E2E Test: ${provider} ($(date +%H:%M:%S))${NC}"
    echo -e "${CYAN}  Log: ${log_file}${NC}"
    echo -e "${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
    
    local start_time=$(date +%s)
    
    if bash "${script}" 2>&1 | tee "${log_file}"; then
        local end_time=$(date +%s)
        local duration=$(( end_time - start_time ))
        SUMMARY="${SUMMARY}  PASS  ${provider}  (${duration}s)\n"
        PROVIDERS_PASS=$((PROVIDERS_PASS + 1))
        echo -e "${GREEN}✓ ${provider}: PASSED (${duration}s)${NC}"
    else
        local end_time=$(date +%s)
        local duration=$(( end_time - start_time ))
        SUMMARY="${SUMMARY}  FAIL  ${provider}  (${duration}s)\n"
        PROVIDERS_FAIL=$((PROVIDERS_FAIL + 1))
        echo -e "${RED}✗ ${provider}: FAILED (${duration}s)${NC}"
        echo -e "${RED}  See log: ${log_file}${NC}"
    fi
    PROVIDERS_RUN=$((PROVIDERS_RUN + 1))
}

echo -e "${GREEN}╔══════════════════════════════════════════════════════════════╗${NC}"
echo -e "${GREEN}║       CloudBurst E2E — All Providers Sequential Run         ║${NC}"
echo -e "${GREEN}║       Started: $(date '+%Y-%m-%d %H:%M:%S')                          ║${NC}"
echo -e "${GREEN}╚══════════════════════════════════════════════════════════════╝${NC}"

# 1. Hetzner
if [ -n "${HETZNER_API_TOKEN:-}" ]; then
    run_test "hetzner" "${SCRIPT_DIR}/integration-test-e2e-hetzner.sh"
else
    echo -e "${YELLOW}[SKIP] Hetzner: HETZNER_API_TOKEN not set${NC}"
    PROVIDERS_SKIP=$((PROVIDERS_SKIP + 1))
fi

# 2. Scaleway
if [ -n "${SCW_SECRET_KEY:-}" ]; then
    run_test "scaleway" "${SCRIPT_DIR}/integration-test-e2e-scaleway.sh"
else
    echo -e "${YELLOW}[SKIP] Scaleway: SCW_SECRET_KEY not set${NC}"
    PROVIDERS_SKIP=$((PROVIDERS_SKIP + 1))
fi

# 3. DigitalOcean
if [ -n "${DIGITALOCEAN_API_TOKEN:-}" ]; then
    run_test "digitalocean" "${SCRIPT_DIR}/integration-test-e2e-digitalocean.sh"
else
    echo -e "${YELLOW}[SKIP] DigitalOcean: DIGITALOCEAN_API_TOKEN not set${NC}"
    PROVIDERS_SKIP=$((PROVIDERS_SKIP + 1))
fi

# 4. OVH
if [ -n "${OVH_APPLICATION_KEY:-}" ] && [ -n "${OVH_APPLICATION_SECRET:-}" ] && [ -n "${OVH_CONSUMER_KEY:-}" ] && [ -n "${OVH_PROJECT_ID:-}" ]; then
    run_test "ovh" "${SCRIPT_DIR}/integration-test-e2e-ovh.sh"
else
    echo -e "${YELLOW}[SKIP] OVH: missing one or more OVH_* credentials${NC}"
    PROVIDERS_SKIP=$((PROVIDERS_SKIP + 1))
fi

# 5. Azure
if [ -n "${AZURE_SUBSCRIPTION_ID:-}" ] && [ -n "${AZURE_RESOURCE_GROUP:-}" ] && [ -n "${AZURE_SUBNET_ID:-}" ] && [ -n "${AZURE_TENANT_ID:-}" ]; then
    run_test "azure" "${SCRIPT_DIR}/integration-test-e2e-azure.sh"
else
    echo -e "${YELLOW}[SKIP] Azure: missing one or more AZURE_SUBSCRIPTION_ID, AZURE_RESOURCE_GROUP, AZURE_SUBNET_ID, AZURE_TENANT_ID${NC}"
    PROVIDERS_SKIP=$((PROVIDERS_SKIP + 1))
fi

# 6. GCP
if [ -n "${GOOGLE_APPLICATION_CREDENTIALS:-}" ] && [ -f "${GOOGLE_APPLICATION_CREDENTIALS:-/nonexistent}" ] && [ -n "${GCP_PROJECT:-}" ]; then
    run_test "gcp" "${SCRIPT_DIR}/integration-test-e2e-gcp.sh"
else
    echo -e "${YELLOW}[SKIP] GCP: GOOGLE_APPLICATION_CREDENTIALS or GCP_PROJECT not set${NC}"
    PROVIDERS_SKIP=$((PROVIDERS_SKIP + 1))
fi

# 7. AWS
if [ -n "${AWS_ACCESS_KEY_ID:-}" ] && [ -n "${AWS_SECRET_ACCESS_KEY:-}" ]; then
    run_test "aws" "${SCRIPT_DIR}/integration-test-e2e-aws.sh"
else
    echo -e "${YELLOW}[SKIP] AWS: missing AWS_ACCESS_KEY_ID or AWS_SECRET_ACCESS_KEY${NC}"
    PROVIDERS_SKIP=$((PROVIDERS_SKIP + 1))
fi

# Summary
TOTAL_END=$(date +%s)
TOTAL_DURATION=$(( TOTAL_END - TOTAL_START ))

echo ""
echo -e "${CYAN}╔══════════════════════════════════════════════════════════════╗${NC}"
echo -e "${CYAN}║                    E2E Test Summary                         ║${NC}"
echo -e "${CYAN}╠══════════════════════════════════════════════════════════════╣${NC}"
echo -e "${CYAN}║  Total time: ${TOTAL_DURATION}s                                          ${NC}"
echo -e "${CYAN}║  Run: ${PROVIDERS_RUN}  Pass: ${PROVIDERS_PASS}  Fail: ${PROVIDERS_FAIL}  Skip: ${PROVIDERS_SKIP}              ${NC}"
echo -e "${CYAN}╠══════════════════════════════════════════════════════════════╣${NC}"

echo -e "${SUMMARY}"

echo -e "${CYAN}╠══════════════════════════════════════════════════════════════╣${NC}"
echo -e "${CYAN}║  Logs: ${LOG_DIR}/${NC}"
echo -e "${CYAN}╚══════════════════════════════════════════════════════════════╝${NC}"

# Run cleanup-all to remove any orphaned cb-* instances (from failed tests or async provider deletion)
echo ""
echo -e "${GREEN}[INFO]${NC} Running cleanup-all to remove any orphaned instances..."
if make -C "${PROJECT_ROOT}" cleanup-all 2>/dev/null; then
    echo -e "${GREEN}[INFO]${NC} Cleanup complete"
else
    echo -e "${YELLOW}[WARN]${NC} Cleanup-all had warnings or partial failures (see above)"
fi

if [ $PROVIDERS_FAIL -gt 0 ]; then
    exit 1
fi
exit 0
