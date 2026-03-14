#!/bin/bash
set -uo pipefail

# Run all available E2E provider tests sequentially, collecting results.
# Each test creates/destroys its own Kind cluster.
#
# Produces:
#   - Per-provider logs in e2e-logs/
#   - JUnit XML report in e2e-logs/junit-results-<timestamp>.xml

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

# Skip UI/screenshots to focus on core VM lifecycle (faster, avoids flaky Playwright timeouts)
export E2E_SKIP_UI="${E2E_SKIP_UI:-1}"

TIMESTAMP=$(date +%Y%m%d-%H%M%S)
JUNIT_FILE="${LOG_DIR}/junit-results-${TIMESTAMP}.xml"

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

# JUnit XML accumulator
JUNIT_TESTCASES=""

_xml_escape() {
    local s="$1"
    s="${s//&/&amp;}"
    s="${s//</&lt;}"
    s="${s//>/&gt;}"
    s="${s//\"/&quot;}"
    echo "$s"
}

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
        JUNIT_TESTCASES="${JUNIT_TESTCASES}    <testcase classname=\"e2e\" name=\"${provider}\" time=\"${duration}\"/>\n"
    else
        local end_time=$(date +%s)
        local duration=$(( end_time - start_time ))
        SUMMARY="${SUMMARY}  FAIL  ${provider}  (${duration}s)\n"
        PROVIDERS_FAIL=$((PROVIDERS_FAIL + 1))
        echo -e "${RED}✗ ${provider}: FAILED (${duration}s)${NC}"
        echo -e "${RED}  See log: ${log_file}${NC}"
        local tail_output
        tail_output=$(_xml_escape "$(tail -20 "${log_file}" 2>/dev/null || echo 'no log')")
        JUNIT_TESTCASES="${JUNIT_TESTCASES}    <testcase classname=\"e2e\" name=\"${provider}\" time=\"${duration}\">\n      <failure message=\"E2E test failed\"><![CDATA[${tail_output}]]></failure>\n    </testcase>\n"
    fi
    PROVIDERS_RUN=$((PROVIDERS_RUN + 1))
}

skip_test() {
    local provider=$1
    local reason=$2
    echo -e "${YELLOW}[SKIP] ${provider}: ${reason}${NC}"
    PROVIDERS_SKIP=$((PROVIDERS_SKIP + 1))
    JUNIT_TESTCASES="${JUNIT_TESTCASES}    <testcase classname=\"e2e\" name=\"${provider}\">\n      <skipped message=\"$(_xml_escape "${reason}")\"/>\n    </testcase>\n"
}

echo -e "${GREEN}╔══════════════════════════════════════════════════════════════╗${NC}"
echo -e "${GREEN}║       CloudBurst E2E — All Providers Sequential Run         ║${NC}"
echo -e "${GREEN}║       Started: $(date '+%Y-%m-%d %H:%M:%S')                          ║${NC}"
echo -e "${GREEN}╚══════════════════════════════════════════════════════════════╝${NC}"

# 1. Hetzner
if [ -n "${HETZNER_API_TOKEN:-}" ]; then
    run_test "hetzner" "${SCRIPT_DIR}/integration-test-e2e-hetzner.sh"
else
    skip_test "hetzner" "HETZNER_API_TOKEN not set"
fi

# 2. Scaleway
if [ -n "${SCW_SECRET_KEY:-}" ]; then
    run_test "scaleway" "${SCRIPT_DIR}/integration-test-e2e-scaleway.sh"
else
    skip_test "scaleway" "SCW_SECRET_KEY not set"
fi

# 3. DigitalOcean
if [ -n "${DIGITALOCEAN_API_TOKEN:-}" ]; then
    run_test "digitalocean" "${SCRIPT_DIR}/integration-test-e2e-digitalocean.sh"
else
    skip_test "digitalocean" "DIGITALOCEAN_API_TOKEN not set"
fi

# 4. Aruba
if [ -n "${ARUBA_API_KEY:-}" ] && [ -n "${ARUBA_API_SECRET:-}" ] && [ -n "${ARUBA_PROJECT_ID:-}" ]; then
    run_test "aruba" "${SCRIPT_DIR}/integration-test-e2e-aruba.sh"
else
    skip_test "aruba" "missing one or more ARUBA_API_KEY, ARUBA_API_SECRET, ARUBA_PROJECT_ID"
fi

# 5. OVH
if [ -n "${OVH_APPLICATION_KEY:-}" ] && [ -n "${OVH_APPLICATION_SECRET:-}" ] && [ -n "${OVH_CONSUMER_KEY:-}" ] && [ -n "${OVH_PROJECT_ID:-}" ]; then
    run_test "ovh" "${SCRIPT_DIR}/integration-test-e2e-ovh.sh"
else
    skip_test "ovh" "missing one or more OVH_* credentials"
fi

# 6. Azure (AZURE_SUBNET_ID optional; provisioner auto-creates VNet when empty)
if [ -n "${AZURE_SUBSCRIPTION_ID:-}" ] && [ -n "${AZURE_RESOURCE_GROUP:-}" ] && [ -n "${AZURE_TENANT_ID:-}" ] && [ -n "${AZURE_CLIENT_ID:-}" ] && [ -n "${AZURE_CLIENT_SECRET:-}" ]; then
    run_test "azure" "${SCRIPT_DIR}/integration-test-e2e-azure.sh"
else
    skip_test "azure" "missing one or more AZURE_SUBSCRIPTION_ID, AZURE_RESOURCE_GROUP, AZURE_TENANT_ID, AZURE_CLIENT_ID, AZURE_CLIENT_SECRET"
fi

# 7. GCP
if [ -n "${GOOGLE_APPLICATION_CREDENTIALS:-}" ] && [ -f "${GOOGLE_APPLICATION_CREDENTIALS:-/nonexistent}" ] && [ -n "${GCP_PROJECT:-}" ]; then
    run_test "gcp" "${SCRIPT_DIR}/integration-test-e2e-gcp.sh"
else
    skip_test "gcp" "GOOGLE_APPLICATION_CREDENTIALS or GCP_PROJECT not set"
fi

# 8. AWS
if [ -n "${AWS_ACCESS_KEY_ID:-}" ] && [ -n "${AWS_SECRET_ACCESS_KEY:-}" ]; then
    run_test "aws" "${SCRIPT_DIR}/integration-test-e2e-aws.sh"
else
    skip_test "aws" "missing AWS_ACCESS_KEY_ID or AWS_SECRET_ACCESS_KEY"
fi

# 9. UpCloud
if [ -n "${UPCLOUD_API_TOKEN:-}" ]; then
    run_test "upcloud" "${SCRIPT_DIR}/integration-test-e2e-upcloud.sh"
else
    skip_test "upcloud" "UPCLOUD_API_TOKEN not set"
fi

# 10. Exoscale
if [ -n "${EXOSCALE_API_KEY:-}" ] && [ -n "${EXOSCALE_API_SECRET:-}" ]; then
    run_test "exoscale" "${SCRIPT_DIR}/integration-test-e2e-exoscale.sh"
else
    skip_test "exoscale" "missing EXOSCALE_API_KEY or EXOSCALE_API_SECRET"
fi

# 11. Seeweb
if [ -n "${SEEWEB_API_TOKEN:-}" ]; then
    run_test "seeweb" "${SCRIPT_DIR}/integration-test-e2e-seeweb.sh"
else
    skip_test "seeweb" "SEEWEB_API_TOKEN not set"
fi

# 12. Cherry Servers
if [ -n "${CHERRY_SERVERS_API_TOKEN:-}" ] && [ -n "${CHERRY_SERVERS_PROJECT_ID:-}" ]; then
    run_test "cherry_servers" "${SCRIPT_DIR}/integration-test-e2e-cherry-servers.sh"
else
    skip_test "cherry_servers" "CHERRY_SERVERS_API_TOKEN or CHERRY_SERVERS_PROJECT_ID not set"
fi

# 13. Clouding
if [ -n "${CLOUDING_API_KEY:-}" ]; then
    run_test "clouding" "${SCRIPT_DIR}/integration-test-e2e-clouding.sh"
else
    skip_test "clouding" "CLOUDING_API_KEY not set"
fi

# 14. Contabo
if [ -n "${CONTABO_CLIENT_ID:-}" ] && [ -n "${CONTABO_CLIENT_SECRET:-}" ] && [ -n "${CONTABO_API_USER:-}" ] && [ -n "${CONTABO_API_PASSWORD:-}" ]; then
    run_test "contabo" "${SCRIPT_DIR}/integration-test-e2e-contabo.sh"
else
    skip_test "contabo" "missing one or more CONTABO_CLIENT_ID, CONTABO_CLIENT_SECRET, CONTABO_API_USER, CONTABO_API_PASSWORD"
fi

# 15. Gcore
if [ -n "${GCORE_API_KEY:-}" ] && [ -n "${GCORE_PROJECT_ID:-}" ]; then
    run_test "gcore" "${SCRIPT_DIR}/integration-test-e2e-gcore.sh"
else
    skip_test "gcore" "GCORE_API_KEY or GCORE_PROJECT_ID not set"
fi

# 16. IBM Cloud
if [ -n "${IBMCLOUD_API_KEY:-}" ] && [ -n "${IBMCLOUD_RESOURCE_GROUP:-}" ] && [ -n "${IBMCLOUD_SUBNET_ID:-}" ] && [ -n "${IBMCLOUD_REGION:-}" ]; then
    run_test "ibm_cloud" "${SCRIPT_DIR}/integration-test-e2e-ibm-cloud.sh"
else
    skip_test "ibm_cloud" "missing one or more IBMCLOUD_API_KEY, IBMCLOUD_RESOURCE_GROUP, IBMCLOUD_SUBNET_ID, IBMCLOUD_REGION"
fi

# 18. Disaster Recovery (requires Hetzner + Scaleway — kept last so cleanups run before DR)
if [ -n "${HETZNER_API_TOKEN:-}" ] && [ -n "${SCW_SECRET_KEY:-}" ]; then
    if [ -f "${SCRIPT_DIR}/simulate-disaster-recovery.sh" ]; then
        run_test "disaster-recovery" "${SCRIPT_DIR}/simulate-disaster-recovery.sh --with-api-mock"
    else
        skip_test "disaster-recovery" "simulate-disaster-recovery.sh not found"
    fi
else
    skip_test "disaster-recovery" "requires both HETZNER_API_TOKEN and SCW_SECRET_KEY"
fi

# Write JUnit XML report
TOTAL_END=$(date +%s)
TOTAL_DURATION=$(( TOTAL_END - TOTAL_START ))

cat > "${JUNIT_FILE}" <<JUNITEOF
<?xml version="1.0" encoding="UTF-8"?>
<testsuites>
  <testsuite name="cloudburst-e2e" tests="${PROVIDERS_RUN}" failures="${PROVIDERS_FAIL}" skipped="${PROVIDERS_SKIP}" time="${TOTAL_DURATION}">
$(echo -e "${JUNIT_TESTCASES}")
  </testsuite>
</testsuites>
JUNITEOF
echo -e "${GREEN}[INFO]${NC} JUnit XML report: ${JUNIT_FILE}"

# Summary
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
echo -e "${CYAN}║  JUnit: ${JUNIT_FILE}${NC}"
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
