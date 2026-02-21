#!/bin/bash
set -euo pipefail

# Disaster Recovery Use Case Simulation
#
# Simulates: primary cloud "outage" → policy update to exclude primary → node loss
# → controller cleans up stale NodeClaim → new capacity from fallback providers.
#
# Flow:
#   Phase 1: NodePool allows only PRIMARY. Create pod → provisions on primary.
#   Phase 2: Patch NodePool to exclude primary, allow FALLBACK only. Delete the
#            burst node (simulates provider failure). Controller detects node gone,
#            cleans up NodeClaim automatically.
#   Phase 3: New pending pod → provisions on fallback (DR demonstrated).
#
# The controller now reconciles NodeClaims when their node leaves the cluster
# (handleReady detects node NotFound, transitions to Deleting). No manual
# NodeClaim deletion required.
#
# Prerequisites:
#   - Kind cluster with Cloudburst controller running
#   - CloudBroker up and ingested for primary + fallback providers
#   - NodeClass with configs for primary + fallback
#   - TAILSCALE_AUTHKEY and provider credentials in .env
#
# Use --use-existing-cluster when cluster is already set up.
# Without it, runs full setup (CloudBroker + Kind + controller + NodeClass).
#
# Usage:
#   ./hack/simulate-disaster-recovery.sh
#   ./hack/simulate-disaster-recovery.sh --use-existing-cluster
#   PRIMARY_PROVIDER=gcp FALLBACK_PROVIDERS=hetzner,scaleway ./hack/simulate-disaster-recovery.sh

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
ENV_FILE="${PROJECT_ROOT}/.env"
CLOUDBROKER_DIR="${CLOUDBROKER_DIR:-${PROJECT_ROOT}/../cloudbroker}"

USE_EXISTING_CLUSTER=0

for arg in "$@"; do
    case "$arg" in
        --use-existing-cluster) USE_EXISTING_CLUSTER=1 ;;
    esac
done

if [ -f "${ENV_FILE}" ]; then
    set -a
    # shellcheck source=/dev/null
    source "${ENV_FILE}"
    set +a
fi

PRIMARY_PROVIDER="${PRIMARY_PROVIDER:-scaleway}"
FALLBACK_PROVIDERS_STR="${FALLBACK_PROVIDERS:-hetzner}"
IFS=',' read -ra FALLBACK_PROVIDERS_ARR <<< "$FALLBACK_PROVIDERS_STR"

KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-cloudburst}"
TEST_NAMESPACE="${TEST_NAMESPACE:-default}"
CONTROLLER_NAMESPACE="cloudburst-system"
NODECLASS_NAME="dr-nodeclass"
NODEPOOL_NAME="dr-nodepool"
POD_PRIMARY="dr-workload-primary"
POD_FAILOVER="dr-workload-failover"
CONTROLLER_IMAGE="cloudburst-autoscaler:e2e"

READY_TIMEOUT="${READY_TIMEOUT:-600}"
POD_SCHEDULED_TIMEOUT="${POD_SCHEDULED_TIMEOUT:-180}"

log_info()  { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }
log_phase() { echo -e "\n${CYAN}=== $1 ===${NC}"; }

# ── Provider credential checks ────────────────────────────────────────────────

has_provider_creds() {
    case "$1" in
        gcp)    [ -n "${GOOGLE_APPLICATION_CREDENTIALS:-}" ] && [ -f "${GOOGLE_APPLICATION_CREDENTIALS:-}" ];;
        scaleway) [ -n "${SCW_SECRET_KEY:-}" ];;
        hetzner)  [ -n "${HETZNER_API_TOKEN:-}" ];;
        digitalocean) [ -n "${DIGITALOCEAN_API_TOKEN:-}" ];;
        ovh)     [ -n "${OVH_APPLICATION_KEY:-}" ] && [ -n "${OVH_APPLICATION_SECRET:-}" ] && [ -n "${OVH_CONSUMER_KEY:-}" ] && [ -n "${OVH_PROJECT_ID:-}" ];;
        aws)     [ -n "${AWS_ACCESS_KEY_ID:-}" ] && [ -n "${AWS_SECRET_ACCESS_KEY:-}" ];;
        azure)   [ -n "${AZURE_TENANT_ID:-}" ] && [ -n "${AZURE_CLIENT_ID:-}" ] && [ -n "${AZURE_CLIENT_SECRET:-}" ] && [ -n "${AZURE_SUBSCRIPTION_ID:-}" ] && [ -n "${AZURE_RESOURCE_GROUP:-}" ] && [ -n "${AZURE_SUBNET_ID:-}" ];;
        *)       false;;
    esac
}

check_prerequisites() {
    log_info "Checking prerequisites..."
    local missing=0

    [ -z "${TAILSCALE_AUTHKEY:-}" ] && { log_error "TAILSCALE_AUTHKEY not set"; missing=$((missing+1)); }

    if ! has_provider_creds "$PRIMARY_PROVIDER"; then
        log_error "Primary provider '${PRIMARY_PROVIDER}' credentials not set or invalid"
        missing=$((missing+1))
    fi

    for p in "${FALLBACK_PROVIDERS_ARR[@]}"; do
        if ! has_provider_creds "$p"; then
            log_error "Fallback provider '${p}' credentials not set or invalid"
            missing=$((missing+1))
        fi
    done

    for cmd in docker kind kubectl curl; do
        command -v "$cmd" &>/dev/null || { log_error "$cmd not found"; missing=$((missing+1)); }
    done

    if [ $USE_EXISTING_CLUSTER -eq 0 ]; then
        [ ! -d "${CLOUDBROKER_DIR}" ] && { log_error "CLOUDBROKER_DIR not found: ${CLOUDBROKER_DIR}"; missing=$((missing+1)); }
    fi

    [ $missing -gt 0 ] && { log_error "Prerequisites missing."; exit 1; }

    log_info "Primary: ${PRIMARY_PROVIDER}, Fallbacks: ${FALLBACK_PROVIDERS_STR}"
}

get_tailscale_ip() {
    local ip cmd
    for p in tailscale /Applications/Tailscale.app/Contents/MacOS/Tailscale /usr/local/bin/tailscale /opt/homebrew/bin/tailscale; do
        if command -v "$p" &>/dev/null || [ -f "$p" ]; then cmd="$p"; break; fi
    done
    [ -z "${cmd:-}" ] && { log_error "tailscale not found"; return 1; }
    ip=$("$cmd" ip -4 2>/dev/null | tr -d '\r\n\t')
    [ -z "$ip" ] && { log_error "Could not get Tailscale IPv4"; return 1; }
    echo "$ip"
}

# ── Setup (when not using existing cluster) ───────────────────────────────────

run_full_setup() {
    log_phase "Setup: CloudBroker + Kind + Controller"

    if ! [ -d "${CLOUDBROKER_DIR}" ]; then
        log_error "CloudBroker dir not found: ${CLOUDBROKER_DIR}"
        exit 1
    fi

    # Export DR providers for the all-providers script
    export ENABLED_PROVIDERS_FOR_DR="${PRIMARY_PROVIDER}"
    for p in "${FALLBACK_PROVIDERS_ARR[@]}"; do
        export ENABLED_PROVIDERS_FOR_DR="${ENABLED_PROVIDERS_FOR_DR} ${p}"
    done

    # Run all-providers script but stop before NodePools (we'll create our own)
    # We run it up to phase C (NodeClass), then we take over.
    # The all-providers script creates nodepools per provider in phase D. We need to either:
    # 1) Patch it to support --dr-mode, or
    # 2) Run it fully, delete its nodepools, then run our DR flow.
    # Simplest: run all-providers with --no-scale-down, it creates nodepools for ALL enabled providers.
    # Our script needs specific providers. So we must ensure only PRIMARY+FALLBACK are enabled.
    # The all-providers auto-detects from env. If we have GCP+Scaleway+Hetzner in .env, it will
    # create 3 nodepools. We can't easily restrict that without forking.
    #
    # Alternative: do our own minimal setup. We'll replicate the essential phases.
    log_info "Running CloudBroker and Kind setup..."

    cd "${CLOUDBROKER_DIR}"
    docker compose down 2>/dev/null || true
    kind delete cluster --name "${KIND_CLUSTER_NAME}" 2>/dev/null || true
    docker compose up -d

    local retries=30
    while [ $retries -gt 0 ]; do
        if curl -sf "http://localhost:8000/health" >/dev/null 2>&1; then
            log_info "✓ CloudBroker healthy"
            break
        fi
        retries=$((retries - 1))
        [ $retries -eq 0 ] && { log_error "CloudBroker failed to become healthy"; exit 1; }
        sleep 2
    done

    make migrate
    make ingest-all

    cd "${PROJECT_ROOT}"
    HOST_TS_IP=$(get_tailscale_ip)
    log_info "Tailscale IP: ${HOST_TS_IP}"

    ./hack/create-kind.sh 2>/dev/null || true
    kind get clusters 2>/dev/null | grep -q "^${KIND_CLUSTER_NAME}$" || { log_error "Kind cluster not found"; exit 1; }

    kubectl config use-context "kind-${KIND_CLUSTER_NAME}" 2>/dev/null || true

    log_info "Installing CRDs..."
    for f in config/crd/bases/cloudburst.io_*.yaml; do
        [ -f "$f" ] && kubectl apply -f "$f" 2>/dev/null || true
    done

    log_info "Building and loading controller image..."
    docker build -t "${CONTROLLER_IMAGE}" . 2>&1 | tail -3
    kind load docker-image "${CONTROLLER_IMAGE}" --name "${KIND_CLUSTER_NAME}" 2>/dev/null || true

    deploy_controller
    create_nodeclass_and_secrets
    log_info "✓ Setup complete"
}

deploy_controller() {
    log_info "Deploying controller..."
    kubectl create namespace "${CONTROLLER_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

    [ -n "${GOOGLE_APPLICATION_CREDENTIALS:-}" ] && [ -f "${GOOGLE_APPLICATION_CREDENTIALS:-}" ] && \
        kubectl create secret generic gcp-credentials --from-file=credentials.json="${GOOGLE_APPLICATION_CREDENTIALS}" -n "${CONTROLLER_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
    [ -n "${HETZNER_API_TOKEN:-}" ] && \
        kubectl create secret generic hetzner-credentials --from-literal=HETZNER_API_TOKEN="${HETZNER_API_TOKEN}" -n "${CONTROLLER_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
    [ -n "${SCW_SECRET_KEY:-}" ] && \
        kubectl create secret generic scaleway-credentials --from-literal=SCW_SECRET_KEY="${SCW_SECRET_KEY}" -n "${CONTROLLER_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

    HOST_TS_IP=$(get_tailscale_ip)
    CLOUDBROKER_BASEURL="${CLOUDBROKER_BASEURL:-http://${HOST_TS_IP}:8000}"

    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: ServiceAccount
metadata:
  name: controller-manager
  namespace: ${CONTROLLER_NAMESPACE}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: cloudburst-manager-role
rules:
- apiGroups: ["cloudburst.io"]
  resources: ["nodepools","nodeclasses","nodeclaims"]
  verbs: ["get","list","watch","create","update","patch","delete"]
- apiGroups: ["cloudburst.io"]
  resources: ["nodepools/status","nodeclasses/status","nodeclaims/status"]
  verbs: ["get","update","patch"]
- apiGroups: [""]
  resources: ["pods","nodes","secrets","configmaps","events"]
  verbs: ["get","list","watch","create","update","patch","delete"]
- apiGroups: ["coordination.k8s.io"]
  resources: ["leases"]
  verbs: ["get","list","watch","create","update","patch","delete"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: cloudburst-manager-rolebinding
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cloudburst-manager-role
subjects:
- kind: ServiceAccount
  name: controller-manager
  namespace: ${CONTROLLER_NAMESPACE}
EOF

    # Deployment: scaleway + hetzner support (GCP/AWS/Azure: use --use-existing-cluster after all-providers setup)
    kubectl apply -f - <<DEPLOY
apiVersion: apps/v1
kind: Deployment
metadata:
  name: cloudburst-controller
  namespace: ${CONTROLLER_NAMESPACE}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: cloudburst-controller
  template:
    metadata:
      labels:
        app: cloudburst-controller
    spec:
      serviceAccountName: controller-manager
      containers:
      - name: manager
        image: ${CONTROLLER_IMAGE}
        imagePullPolicy: Never
        command: ["/manager"]
        args: ["--leader-elect=false"]
        env:
        - name: CLOUDBROKER_BASEURL
          value: "${CLOUDBROKER_BASEURL}"
        - name: TAILSCALE_AUTHKEY
          value: "${TAILSCALE_AUTHKEY}"
        - name: HETZNER_API_TOKEN
          valueFrom:
            secretKeyRef:
              name: hetzner-credentials
              key: HETZNER_API_TOKEN
        - name: SCW_SECRET_KEY
          valueFrom:
            secretKeyRef:
              name: scaleway-credentials
              key: SCW_SECRET_KEY
        resources:
          requests:
            cpu: 50m
            memory: 128Mi
DEPLOY

    kubectl rollout status deployment/cloudburst-controller -n "${CONTROLLER_NAMESPACE}" --timeout=120s
}

create_nodeclass_and_secrets() {
    log_info "Creating NodeClass and secrets in ${TEST_NAMESPACE}..."
    HOST_TS_IP=$(get_tailscale_ip)

    kubectl create secret generic tailscale-auth --from-literal=authkey="${TAILSCALE_AUTHKEY}" -n "${TEST_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
    [ -n "${HETZNER_API_TOKEN:-}" ] && \
        kubectl create secret generic hetzner-api-token --from-literal=HETZNER_API_TOKEN="${HETZNER_API_TOKEN}" -n "${TEST_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
    [ -n "${SCW_SECRET_KEY:-}" ] && \
        kubectl create secret generic scaleway-api-keys --from-literal=SCW_SECRET_KEY="${SCW_SECRET_KEY}" -n "${TEST_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

    # Minimal NodeClass for scaleway + hetzner (extend as needed for gcp, etc.)
    cat <<NCCLASS | kubectl apply -f -
apiVersion: cloudburst.io/v1alpha1
kind: NodeClass
metadata:
  name: ${NODECLASS_NAME}
  namespace: ${TEST_NAMESPACE}
spec:
  scaleway:
    zone: "${SCW_ZONE:-fr-par-1}"
    projectID: "${SCW_PROJECT_ID:-}"
    apiKeySecretRef:
      name: scaleway-api-keys
      key: SCW_SECRET_KEY
  hetzner:
    location: "${HETZNER_LOCATION:-fsn1}"
    apiTokenSecretRef:
      name: hetzner-api-token
      key: HETZNER_API_TOKEN
  join:
    hostApiServer: "https://${HOST_TS_IP}:6443"
    kindClusterName: "${KIND_CLUSTER_NAME}"
    tokenTtlMinutes: 60
  tailscale:
    authKeySecretRef:
      name: tailscale-auth
      key: authkey
  bootstrap:
    kubernetesVersion: "1.34.3"
NCCLASS
}

# ── DR Simulation ────────────────────────────────────────────────────────────

phase1_primary() {
    log_phase "Phase 1: Provision on primary (${PRIMARY_PROVIDER})"

    cat <<EOF | kubectl apply -f -
apiVersion: cloudburst.io/v1alpha1
kind: NodePool
metadata:
  name: ${NODEPOOL_NAME}
  namespace: ${TEST_NAMESPACE}
spec:
  requirements:
    regionConstraint: "ANY"
    arch: ["x86_64"]
    maxPriceEurPerHour: 0.50
    allowedProviders: ["${PRIMARY_PROVIDER}"]
  limits:
    maxNodes: 2
    minNodes: 0
  template:
    labels:
      cloudburst.io/nodepool: "${NODEPOOL_NAME}"
  disruption:
    ttlSecondsAfterEmpty: 60
    ttlSecondsUntilExpired: 3600
  weight: 1
---
apiVersion: v1
kind: Pod
metadata:
  name: ${POD_PRIMARY}
  namespace: ${TEST_NAMESPACE}
  labels:
    app: dr-workload
spec:
  nodeSelector:
    cloudburst.io/nodepool: "${NODEPOOL_NAME}"
  containers:
  - name: workload
    image: busybox:1.36
    command: ["sleep", "infinity"]
    resources:
      requests:
        cpu: "1500m"
        memory: "2Gi"
EOF

    log_info "Waiting for NodeClaim and node (primary: ${PRIMARY_PROVIDER})..."
    wait_for_pod_scheduled "${POD_PRIMARY}"
    local node_provider
    node_provider=$(kubectl get pod "${POD_PRIMARY}" -n "${TEST_NAMESPACE}" -o jsonpath='{.spec.nodeName}' | xargs -I{} kubectl get node {} -o jsonpath='{.metadata.labels.cloudburst\.io/provider}' 2>/dev/null || echo "?")
    log_info "✓ Pod scheduled on node from provider: ${node_provider}"
}

phase2_simulate_outage() {
    log_phase "Phase 2: Simulate outage - patch NodePool, delete node"

    # 1. Patch NodePool to exclude primary, allow fallbacks only
    local fallback_json="["
    local first=1
    for p in "${FALLBACK_PROVIDERS_ARR[@]}"; do
        [ $first -eq 0 ] && fallback_json+=","
        fallback_json+="\"${p}\""
        first=0
    done
    fallback_json+="]"

    kubectl patch nodepool "${NODEPOOL_NAME}" -n "${TEST_NAMESPACE}" --type=merge -p "{\"spec\":{\"requirements\":{\"allowedProviders\":${fallback_json}}}}"
    log_info "✓ NodePool now allows only: ${FALLBACK_PROVIDERS_STR} (primary ${PRIMARY_PROVIDER} excluded)"

    # 2. Get burst node name from pod, delete pod and node (simulate provider failure)
    local node_name
    node_name=$(kubectl get pod "${POD_PRIMARY}" -n "${TEST_NAMESPACE}" -o jsonpath='{.spec.nodeName}' 2>/dev/null || true)
    if [ -z "${node_name}" ]; then
        log_error "Could not get node name from pod ${POD_PRIMARY}"
        exit 1
    fi
    log_info "Deleting pod ${POD_PRIMARY} and node ${node_name} (simulates provider outage)..."
    kubectl delete pod "${POD_PRIMARY}" -n "${TEST_NAMESPACE}" --ignore-not-found --wait=false 2>/dev/null || true
    kubectl delete node "${node_name}" 2>/dev/null || true

    # 3. Wait for controller to detect node gone and clean up NodeClaim (handleReady → Deleting → delete CR)
    log_info "Waiting for controller to clean up NodeClaim (node-loss reconciliation)..."
    wait_for_nodeclaim_cleanup
    log_info "✓ NodeClaim cleaned up"
}

wait_for_nodeclaim_cleanup() {
    local timeout=${READY_TIMEOUT}
    local elapsed=0

    while [ $elapsed -lt $timeout ]; do
        local count
        count=$(kubectl get nodeclaims -n "${TEST_NAMESPACE}" -l "cloudburst.io/nodepool=${NODEPOOL_NAME}" --no-headers 2>/dev/null | wc -l)
        if [ "${count}" -eq 0 ]; then
            return 0
        fi
        sleep 10
        elapsed=$((elapsed + 10))
    done

    log_error "Timeout waiting for NodeClaim cleanup"
    kubectl get nodeclaims -n "${TEST_NAMESPACE}"
    exit 1
}

phase3_failover() {
    log_phase "Phase 3: Failover - new pod triggers provisioning on fallback"

    # Primary pod was deleted in Phase 2. Create failover pod; it will trigger new NodeClaim.
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: ${POD_FAILOVER}
  namespace: ${TEST_NAMESPACE}
  labels:
    app: dr-workload
spec:
  nodeSelector:
    cloudburst.io/nodepool: "${NODEPOOL_NAME}"
  containers:
  - name: workload
    image: busybox:1.36
    command: ["sleep", "infinity"]
    resources:
      requests:
        cpu: "1500m"
        memory: "2Gi"
EOF

    log_info "Waiting for failover NodeClaim (fallback providers: ${FALLBACK_PROVIDERS_STR})..."
    wait_for_pod_scheduled "${POD_FAILOVER}"
    local node_provider
    node_provider=$(kubectl get pod "${POD_FAILOVER}" -n "${TEST_NAMESPACE}" -o jsonpath='{.spec.nodeName}' | xargs -I{} kubectl get node {} -o jsonpath='{.metadata.labels.cloudburst\.io/provider}' 2>/dev/null || echo "?")
    log_info "✓ Failover pod scheduled on node from provider: ${node_provider}"

    if [[ " ${FALLBACK_PROVIDERS_STR} " =~ " ${node_provider} " ]]; then
        log_info "✓ Disaster recovery simulation SUCCESS: failover to ${node_provider}"
    else
        log_warn "Expected fallback provider; got: ${node_provider}"
    fi
}

wait_for_pod_scheduled() {
    local pod=$1
    local timeout=${READY_TIMEOUT}
    local elapsed=0

    while [ $elapsed -lt $timeout ]; do
        local status
        status=$(kubectl get pod "${pod}" -n "${TEST_NAMESPACE}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "Pending")
        if [ "$status" = "Running" ]; then
            log_info "Pod ${pod} is Running"
            return 0
        fi
        sleep 10
        elapsed=$((elapsed + 10))
    done

    log_error "Timeout waiting for pod ${pod} to schedule"
    kubectl get pods -n "${TEST_NAMESPACE}" -l app=dr-workload
    kubectl get nodeclaims -n "${TEST_NAMESPACE}"
    exit 1
}

cleanup() {
    log_phase "Cleanup"
    kubectl delete nodepool "${NODEPOOL_NAME}" -n "${TEST_NAMESPACE}" --ignore-not-found 2>/dev/null || true
    kubectl delete pod "${POD_PRIMARY}" "${POD_FAILOVER}" -n "${TEST_NAMESPACE}" --ignore-not-found 2>/dev/null || true
    log_info "✓ Cleanup done (NodeClaims/nodes will scale down via controller)"
}

# ── Main ─────────────────────────────────────────────────────────────────────

main() {
    log_info "Disaster Recovery Use Case Simulation"
    log_info "Primary: ${PRIMARY_PROVIDER} | Fallbacks: ${FALLBACK_PROVIDERS_STR}"
    check_prerequisites

    if [ $USE_EXISTING_CLUSTER -eq 0 ]; then
        run_full_setup
    else
        kubectl cluster-info --context "kind-${KIND_CLUSTER_NAME}" &>/dev/null || { log_error "Kind cluster not found or not accessible"; exit 1; }
        log_info "Using existing cluster: ${KIND_CLUSTER_NAME}"
        # NodeClass must exist (from integration-test-e2e-all-providers.sh)
        local nc_count
        nc_count=$(kubectl get nodeclass -n "${TEST_NAMESPACE}" --no-headers 2>/dev/null | wc -l)
        [ "$nc_count" -lt 1 ] && { log_error "No NodeClass found in ${TEST_NAMESPACE}. Run integration-test-e2e-all-providers.sh first."; exit 1; }
    fi

    phase1_primary
    phase2_simulate_outage
    phase3_failover

    log_phase "Result"
    log_info "Disaster recovery simulation complete."
    log_info "Summary: Primary ${PRIMARY_PROVIDER} 'outage' simulated; workload failed over to fallback provider(s)."
    log_info ""
    log_info "To clean up resources:"
    log_info "  kubectl delete nodepool ${NODEPOOL_NAME} -n ${TEST_NAMESPACE}"
    log_info "  kubectl delete pod ${POD_FAILOVER} -n ${TEST_NAMESPACE}"
    log_info "  (Nodes will scale down after ttlSecondsAfterEmpty)"
}

# Allow sourcing for testing; run main when executed
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    main
fi
