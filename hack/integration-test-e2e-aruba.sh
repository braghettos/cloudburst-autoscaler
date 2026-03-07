#!/bin/bash
set -euo pipefail

# End-to-end integration test for the Aruba Cloud provider.
# Controller runs as a Deployment inside a Kind cluster.
# A pending pod triggers the NodePool controller to create a NodeClaim,
# which provisions an Aruba Cloud instance, joins it to the cluster,
# and the pod gets scheduled onto the new node.
#
# After the pod is scheduled, it is deleted. The controller removes the NodeClaim
# and deletes the Aruba instance, verifying scale-down to zero.
#
# Environment Variables:
#   Source .env if present. Required: ARUBA_API_KEY, ARUBA_API_SECRET,
#   ARUBA_PROJECT_ID, TAILSCALE_AUTHKEY
#   See .env.example for all options.

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
ENV_FILE="${PROJECT_ROOT}/.env"

if [ -f "${ENV_FILE}" ]; then
    echo -e "${GREEN}[INFO]${NC} Loading environment variables from .env file"
    set -a
    # shellcheck source=/dev/null
    source "${ENV_FILE}"
    set +a
fi

ARUBA_API_KEY="${ARUBA_API_KEY:?ARUBA_API_KEY required}"
ARUBA_API_SECRET="${ARUBA_API_SECRET:?ARUBA_API_SECRET required}"
ARUBA_PROJECT_ID="${ARUBA_PROJECT_ID:?ARUBA_PROJECT_ID required}"
TAILSCALE_AUTHKEY="${TAILSCALE_AUTHKEY:-}"
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-cloudburst}"
TEST_NAMESPACE="${TEST_NAMESPACE:-default}"
CONTROLLER_NAMESPACE="cloudburst-system"
NODECLASS_NAME="e2e-aruba-nodeclass"
NODEPOOL_NAME="e2e-aruba-nodepool"
CONTROLLER_IMAGE="cloudburst-autoscaler:e2e"
PENDING_POD_NAME="e2e-aruba-resource-hungry-pod"

NODECLAIM_CREATE_TIMEOUT="${NODECLAIM_CREATE_TIMEOUT:-300}"
PROVISIONING_TIMEOUT="${PROVISIONING_TIMEOUT:-600}"
READY_TIMEOUT="${READY_TIMEOUT:-600}"
POD_SCHEDULED_TIMEOUT="${POD_SCHEDULED_TIMEOUT:-120}"
SCALEDOWN_TIMEOUT="${SCALEDOWN_TIMEOUT:-300}"
COMPUTE_DELETE_TIMEOUT="${COMPUTE_DELETE_TIMEOUT:-180}"

log_info()  { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }

# Source common UI deploy/verify (optional)
[ -f "${SCRIPT_DIR}/e2e-common-ui.sh" ] && source "${SCRIPT_DIR}/e2e-common-ui.sh"

check_prerequisites() {
    log_info "Checking prerequisites..."
    local missing=0
    TAILSCALE_CMD=""
    for p in tailscale /Applications/Tailscale.app/Contents/MacOS/Tailscale /usr/local/bin/tailscale /opt/homebrew/bin/tailscale; do
        if command -v "$p" &>/dev/null || [ -f "$p" ]; then TAILSCALE_CMD="$p"; break; fi
    done
    if [ -z "$TAILSCALE_CMD" ]; then
        log_error "tailscale not found"; missing=$((missing+1))
    elif ! "$TAILSCALE_CMD" status &>/dev/null; then
        log_error "Tailscale not running"; missing=$((missing+1))
    else
        export TAILSCALE_CMD; log_info "✓ Tailscale"
    fi
    for cmd in docker kind kubectl; do
        if ! command -v $cmd &>/dev/null; then log_error "$cmd not found"; missing=$((missing+1)); else log_info "✓ $cmd"; fi
    done
    [ -z "${ARUBA_API_KEY}" ] && { log_error "ARUBA_API_KEY not set"; missing=$((missing+1)); }
    [ -z "${ARUBA_API_SECRET}" ] && { log_error "ARUBA_API_SECRET not set"; missing=$((missing+1)); }
    [ -z "${ARUBA_PROJECT_ID}" ] && { log_error "ARUBA_PROJECT_ID not set"; missing=$((missing+1)); }
    [ -z "${TAILSCALE_AUTHKEY}" ] && { log_error "TAILSCALE_AUTHKEY not set"; missing=$((missing+1)); }
    [ $missing -gt 0 ] && { log_error "Prerequisites missing, aborting."; exit 1; }
    log_info "All prerequisites met!"
}

get_tailscale_ip() {
    local ip
    ip=$("$TAILSCALE_CMD" ip -4 2>/dev/null | tr -d '\r\n\t')
    [ -z "$ip" ] && { log_error "Could not get Tailscale IPv4" >&2; return 1; }
    echo "$ip"
}

setup_kind_cluster() {
    log_info "Setting up kind cluster..."
    HOST_TS_IP=$(get_tailscale_ip)
    export HOST_TS_IP
    log_info "Host Tailscale IP: $HOST_TS_IP"
    ./hack/create-kind.sh
    KIND_CONTEXT="kind-${KIND_CLUSTER_NAME}"
    kubectl config set-cluster "${KIND_CONTEXT}" --server="https://127.0.0.1:6443" 2>/dev/null || true
    kubectl cluster-info --context "${KIND_CONTEXT}" &>/dev/null || { log_error "Kind cluster not accessible"; exit 1; }
    log_info "✓ Kind cluster ready"
}

install_crds() {
    log_info "Installing CRDs..."
    if make install 2>/dev/null; then log_info "✓ CRDs installed"; return; fi
    local applied=0
    for f in config/crd/bases/cloudburst.io_*.yaml; do
        [ -f "$f" ] && kubectl apply -f "$f" 2>/dev/null && applied=$((applied+1))
    done
    [ $applied -gt 0 ] && { log_info "✓ CRDs installed ($applied)"; return; }
    log_error "Could not install CRDs"; exit 1
}

build_and_load_controller_image() {
    log_info "Building controller Docker image: ${CONTROLLER_IMAGE}..."
    docker build -t "${CONTROLLER_IMAGE}" . 2>&1 | tail -5
    log_info "Loading image into kind cluster..."
    local nodes=("${KIND_CLUSTER_NAME}-control-plane" "${KIND_CLUSTER_NAME}-worker")
    for node in "${nodes[@]}"; do
        if docker ps --format '{{.Names}}' | grep -q "^${node}$"; then
            docker save "${CONTROLLER_IMAGE}" | docker exec -i "${node}" ctr --namespace=k8s.io images import - 2>&1 | grep -v "^$" || true
            log_info "✓ Image imported into ${node}"
        fi
    done
}

deploy_controller() {
    log_info "Deploying controller..."
    HOST_TS_IP=$(get_tailscale_ip)
    local cloudbroker_url="${CLOUDBROKER_BASEURL:-http://${HOST_TS_IP}:8000}"
    kubectl create namespace "${CONTROLLER_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

    # RBAC (abbreviated; same as other E2E scripts)
    cat <<EOF | kubectl apply -f -
---
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

    cat <<EOF | kubectl apply -f -
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
      nodeSelector:
        cloudburst.io/node: controller-worker
      containers:
      - name: manager
        image: ${CONTROLLER_IMAGE}
        imagePullPolicy: Never
        command: ["/manager"]
        args: ["--leader-elect=false"]
        env:
        - name: CLOUDBROKER_BASEURL
          value: "${cloudbroker_url}"
        - name: TAILSCALE_AUTHKEY
          value: "${TAILSCALE_AUTHKEY}"
        livenessProbe:
          httpGet:
            path: /healthz
            port: 8081
          initialDelaySeconds: 10
          periodSeconds: 20
        readinessProbe:
          httpGet:
            path: /readyz
            port: 8081
          initialDelaySeconds: 5
          periodSeconds: 10
        resources:
          requests:
            cpu: 50m
            memory: 128Mi
          limits:
            cpu: 500m
            memory: 256Mi
EOF

    log_info "Waiting for controller deployment..."
    kubectl rollout status deployment/cloudburst-controller -n "${CONTROLLER_NAMESPACE}" --timeout=120s || { log_error "Controller not ready"; kubectl logs -n "${CONTROLLER_NAMESPACE}" -l app=cloudburst-controller --tail=50; exit 1; }
    log_info "✓ Controller ready"
}

create_tailscale_secret() {
    kubectl create secret generic tailscale-auth \
        --from-literal=authkey="${TAILSCALE_AUTHKEY}" \
        --namespace="${TEST_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -
}

create_aruba_secret() {
    log_info "Creating Aruba credentials secret in test namespace..."
    kubectl create secret generic aruba-api-credentials \
        --from-literal=ARUBA_API_KEY="${ARUBA_API_KEY}" \
        --from-literal=ARUBA_API_SECRET="${ARUBA_API_SECRET}" \
        --namespace="${TEST_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -
}

create_nodeclass() {
    log_info "Creating Aruba NodeClass..."
    HOST_TS_IP=$(get_tailscale_ip)

    cat <<EOF | kubectl apply -f -
apiVersion: cloudburst.io/v1alpha1
kind: NodeClass
metadata:
  name: ${NODECLASS_NAME}
  namespace: ${TEST_NAMESPACE}
spec:
  aruba:
    projectID: "${ARUBA_PROJECT_ID}"
    applicationKeySecretRef:
      name: aruba-api-credentials
      key: ARUBA_API_KEY
    applicationSecretSecretRef:
      name: aruba-api-credentials
      key: ARUBA_API_SECRET
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
EOF
}

create_nodepool() {
    log_info "Creating Aruba NodePool..."
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
    maxPriceEurPerHour: 0.15
    allowedProviders: ["aruba"]
  limits:
    maxNodes: 3
    minNodes: 0
  template:
    labels:
      cloudburst.io/nodepool: "${NODEPOOL_NAME}"
  disruption:
    ttlSecondsAfterEmpty: 30
    ttlSecondsUntilExpired: 3600
  weight: 1
EOF
}

create_pending_pod() {
    log_info "Creating resource-hungry pod..."
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: ${PENDING_POD_NAME}
  namespace: ${TEST_NAMESPACE}
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
    log_info "Waiting for pod to become Unschedulable..."
    local elapsed=0
    while [ $elapsed -lt 60 ]; do
        local reason
        reason=$(kubectl get pod "${PENDING_POD_NAME}" -n "${TEST_NAMESPACE}" -o jsonpath='{.status.conditions[?(@.reason=="Unschedulable")].reason}' 2>/dev/null || true)
        if [ "$reason" = "Unschedulable" ]; then
            log_info "✓ Pod is Unschedulable"
            return 0
        fi
        sleep 5
        elapsed=$((elapsed + 5))
    done
    log_warn "Pod may not be Unschedulable yet, continuing..."
}

wait_for_nodeclaim_creation() {
    local timeout=${1:-300}
    local elapsed=0
    log_info "Waiting for NodeClaim (timeout: ${timeout}s)..."
    while [ $elapsed -lt $timeout ]; do
        local claims
        claims=$(kubectl get nodeclaims -n "${TEST_NAMESPACE}" --no-headers 2>/dev/null | wc -l | tr -d ' ')
        if [ "$claims" -gt 0 ]; then
            NODECLAIM_NAME=$(kubectl get nodeclaims -n "${TEST_NAMESPACE}" --no-headers -o custom-columns=NAME:.metadata.name 2>/dev/null | head -1)
            log_info "✓ NodeClaim created: ${NODECLAIM_NAME}"
            return 0
        fi
        echo -n "."
        sleep 10
        elapsed=$((elapsed + 10))
    done
    log_error "Timeout: no NodeClaim created"
    kubectl logs -n "${CONTROLLER_NAMESPACE}" -l app=cloudburst-controller --tail=60 2>/dev/null || true
    return 1
}

wait_for_phase() {
    local phase=$1
    local timeout=${2:-600}
    local elapsed=0
    log_info "Waiting for NodeClaim to reach phase: $phase (timeout: ${timeout}s)"
    while [ $elapsed -lt $timeout ]; do
        local current
        current=$(kubectl get nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
        case "$phase" in
            Provisioning) [[ "$current" =~ ^(Provisioning|Joining|Ready)$ ]] && { log_info "✓ Phase: $current"; return 0; } ;;
            Joining)      [[ "$current" =~ ^(Joining|Ready)$ ]]              && { log_info "✓ Phase: $current"; return 0; } ;;
            Ready)        [ "$current" = "Ready" ]                            && { log_info "✓ Phase: Ready"; return 0; } ;;
            *)            [ "$current" = "$phase" ]                           && { log_info "✓ Phase: $phase"; return 0; } ;;
        esac
        [ "$current" = "Failed" ] && { log_error "NodeClaim Failed"; kubectl describe nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}"; return 1; }
        echo -n "."
        sleep 10
        elapsed=$((elapsed + 10))
    done
    log_error "Timeout for phase $phase"
    return 1
}

wait_for_pod_running() {
    local timeout=${1:-120}
    local elapsed=0
    log_info "Waiting for pod to be Running (timeout: ${timeout}s)..."
    while [ $elapsed -lt $timeout ]; do
        local phase
        phase=$(kubectl get pod "${PENDING_POD_NAME}" -n "${TEST_NAMESPACE}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
        if [ "$phase" = "Running" ]; then
            log_info "✓ Pod is Running"
            return 0
        fi
        echo -n "."
        sleep 10
        elapsed=$((elapsed + 10))
    done
    log_error "Pod never reached Running"
    return 1
}

delete_pending_pod() {
    log_info "Deleting pod to trigger scale-down..."
    kubectl delete pod "${PENDING_POD_NAME}" -n "${TEST_NAMESPACE}" --grace-period=0 --force 2>/dev/null || true
    local elapsed=0
    while [ $elapsed -lt 60 ]; do
        if ! kubectl get pod "${PENDING_POD_NAME}" -n "${TEST_NAMESPACE}" &>/dev/null; then
            log_info "✓ Pod deleted"
            return 0
        fi
        sleep 5
        elapsed=$((elapsed + 5))
    done
    log_warn "Pod may still be terminating..."
}

wait_for_nodeclaim_deletion() {
    local timeout=${1:-300}
    local elapsed=0
    log_info "Waiting for NodeClaim to be deleted (timeout: ${timeout}s)..."
    while [ $elapsed -lt $timeout ]; do
        if ! kubectl get nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}" &>/dev/null; then
            log_info "✓ NodeClaim deleted"
            return 0
        fi
        echo -n "."
        sleep 10
        elapsed=$((elapsed + 10))
    done
    log_error "Timeout: NodeClaim not deleted"
    return 1
}

get_instance_id_from_nodeclaim() {
    local id
    id=$(kubectl get nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}" -o jsonpath='{.status.instanceID}' 2>/dev/null || true)
    if [ -z "$id" ]; then
        log_error "Could not get instance ID from NodeClaim ${NODECLAIM_NAME}"
        return 1
    fi
    echo "$id"
}

wait_for_compute_deleted() {
    local instance_id=$1
    local timeout=${2:-180}
    local elapsed=0
    log_info "Waiting for Aruba compute instance to be deleted (timeout: ${timeout}s)..."
    while [ $elapsed -lt $timeout ]; do
        if (cd "${PROJECT_ROOT}" && go run ./hack/aruba-cleanup/main.go --verify="${instance_id}") 2>/dev/null; then
            log_info "✓ Compute instance deleted"
            return 0
        fi
        echo -n "."
        sleep 10
        elapsed=$((elapsed + 10))
    done
    log_error "Timeout: compute instance still exists"
    return 1
}

CLEANUP_DONE=""
cleanup() {
    [ -n "${CLEANUP_DONE:-}" ] && return
    CLEANUP_DONE=1
    declare -f stop_ui &>/dev/null && stop_ui
    log_info "Cleaning up..."
    kubectl delete pod "${PENDING_POD_NAME}" -n "${TEST_NAMESPACE}" --ignore-not-found=true --grace-period=0 --force 2>/dev/null || true
    [ -n "${NODECLAIM_NAME:-}" ] && [ "$NODECLAIM_NAME" != "none" ] && kubectl delete nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}" --ignore-not-found=true 2>/dev/null || true
    kubectl delete nodeclaims --all -n "${TEST_NAMESPACE}" --ignore-not-found=true 2>/dev/null || true
    kubectl delete nodepool "${NODEPOOL_NAME}" -n "${TEST_NAMESPACE}" --ignore-not-found=true 2>/dev/null || true
    kubectl delete nodeclass "${NODECLASS_NAME}" -n "${TEST_NAMESPACE}" --ignore-not-found=true 2>/dev/null || true
    kubectl delete secret tailscale-auth aruba-api-credentials -n "${TEST_NAMESPACE}" --ignore-not-found=true 2>/dev/null || true
    kubectl delete namespace "${CONTROLLER_NAMESPACE}" --ignore-not-found=true 2>/dev/null || true
    kind delete cluster --name "${KIND_CLUSTER_NAME}" 2>/dev/null || true
    log_info "✓ Cleanup complete"
}

main() {
    log_info "=== CloudBurst E2E — Aruba Cloud ==="
    log_info "Aruba Project: ${ARUBA_PROJECT_ID}"
    echo ""
    check_prerequisites
    echo ""
    setup_kind_cluster
    echo ""
    install_crds
    echo ""
    build_and_load_controller_image
    echo ""
    deploy_controller
    echo ""
    if declare -f deploy_ui &>/dev/null; then deploy_ui || exit 1; echo ""; verify_ui_ready || exit 1; echo ""; fi
    create_tailscale_secret
    create_aruba_secret
    create_nodeclass
    create_nodepool
    echo ""
    create_pending_pod
    echo ""

    if ! wait_for_nodeclaim_creation "${NODECLAIM_CREATE_TIMEOUT}"; then
        log_error "No NodeClaim created"
        exit 1
    fi
    if ! wait_for_phase "Provisioning" "${PROVISIONING_TIMEOUT}"; then
        log_error "Provisioning phase failed"
        exit 1
    fi
    if ! wait_for_phase "Joining" "${READY_TIMEOUT:-600}"; then
        log_error "Joining phase failed"
        log_info "Dumping NodeClaim status and controller logs for debugging..."
        kubectl get nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}" -o yaml 2>/dev/null || true
        kubectl logs -n "${CONTROLLER_NAMESPACE}" -l app=cloudburst-controller --tail=100 2>/dev/null || true
        exit 1
    fi
    if ! wait_for_phase "Ready" "${READY_TIMEOUT}"; then
        log_error "Ready phase failed"
        exit 1
    fi
    if ! wait_for_pod_running "${POD_SCHEDULED_TIMEOUT}"; then
        log_error "Pod not scheduled"
        exit 1
    fi

    echo ""
    if declare -f run_playwright_ui_assertions &>/dev/null; then
        run_playwright_ui_assertions "aruba" || exit 1
        echo ""
    fi
    log_info "=== Scale-down ==="
    ARUBA_INSTANCE_ID=$(get_instance_id_from_nodeclaim) || { log_error "Could not get instance ID"; exit 1; }
    log_info "Instance ID to verify: ${ARUBA_INSTANCE_ID}"
    delete_pending_pod
    echo ""
    if ! wait_for_nodeclaim_deletion "${SCALEDOWN_TIMEOUT}"; then
        log_error "Scale-down failed"
        exit 1
    fi
    echo ""
    if ! wait_for_compute_deleted "${ARUBA_INSTANCE_ID}" "${COMPUTE_DELETE_TIMEOUT}"; then
        log_error "Compute instance was not deleted"
        exit 1
    fi

    echo ""
    log_info "Cleaning up any orphaned resources (volumes)..."
    ORPHAN_VERIFIED=""
    for attempt in 1 2 3; do
        (cd "${PROJECT_ROOT}" && go run ./hack/aruba-cleanup/main.go --cleanup-orphans) 2>&1 || true
        log_info "Waiting for Aruba API eventual consistency (instances/volumes may appear in list briefly after delete)..."
        verify_elapsed=0
        verify_timeout=120
        while [ $verify_elapsed -lt $verify_timeout ]; do
            sleep 15
            verify_elapsed=$((verify_elapsed + 15))
            log_info "Verifying no orphaned instances or volumes (attempt $attempt/3, ${verify_elapsed}s)..."
            if (cd "${PROJECT_ROOT}" && go run ./hack/aruba-cleanup/main.go --verify-no-orphans) 2>&1; then
                log_info "✓ No orphaned instances or boot volumes"
                ORPHAN_VERIFIED=1
                break
            fi
        done
        [ -n "${ORPHAN_VERIFIED}" ] && break
        if [ "$attempt" -eq 3 ]; then
            log_error "Orphaned cb-* instances or cb-*-boot volumes still present after 3 attempts"
            exit 1
        fi
        log_warn "Orphans still present, retrying cleanup..."
    done

    echo ""
    log_info "=== E2E PASSED (Aruba Cloud) ==="
}

trap 'cleanup' EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM
main "$@"
