#!/bin/bash
set -euo pipefail

# End-to-end integration test for the OVH Cloud provider.
# Controller runs as a Deployment inside a Kind cluster.
# A pending pod triggers the NodePool controller to create a NodeClaim,
# which provisions an OVH Cloud instance, joins it to the cluster,
# and the pod gets scheduled onto the new node.
#
# After the pod is scheduled, it is deleted. The controller removes the NodeClaim
# and deletes the OVH instance, verifying scale-down to zero.
#
# Environment Variables:
#   Source .env if present. Required: OVH_APPLICATION_KEY, OVH_APPLICATION_SECRET,
#   OVH_CONSUMER_KEY, OVH_PROJECT_ID, TAILSCALE_AUTHKEY
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

OVH_APPLICATION_KEY="${OVH_APPLICATION_KEY:?OVH_APPLICATION_KEY required}"
OVH_APPLICATION_SECRET="${OVH_APPLICATION_SECRET:?OVH_APPLICATION_SECRET required}"
OVH_CONSUMER_KEY="${OVH_CONSUMER_KEY:?OVH_CONSUMER_KEY required}"
OVH_PROJECT_ID="${OVH_PROJECT_ID:?OVH_PROJECT_ID required}"
OVH_REGION="${OVH_REGION:-}"
TAILSCALE_AUTHKEY="${TAILSCALE_AUTHKEY:-}"
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-cloudburst}"
TEST_NAMESPACE="${TEST_NAMESPACE:-default}"
CONTROLLER_NAMESPACE="cloudburst-system"
NODECLASS_NAME="e2e-ovh-nodeclass"
NODEPOOL_NAME="e2e-ovh-nodepool"
CONTROLLER_IMAGE="cloudburst-autoscaler:e2e"
PENDING_POD_NAME="e2e-ovh-resource-hungry-pod"

NODECLAIM_CREATE_TIMEOUT="${NODECLAIM_CREATE_TIMEOUT:-300}"
PROVISIONING_TIMEOUT="${PROVISIONING_TIMEOUT:-600}"
READY_TIMEOUT="${READY_TIMEOUT:-600}"
POD_SCHEDULED_TIMEOUT="${POD_SCHEDULED_TIMEOUT:-120}"
SCALEDOWN_TIMEOUT="${SCALEDOWN_TIMEOUT:-300}"
VM_DELETION_TIMEOUT="${VM_DELETION_TIMEOUT:-300}"

log_info()  { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }

# Source common UI deploy/verify (optional)
[ -f "${SCRIPT_DIR}/e2e-common-ui.sh" ] && source "${SCRIPT_DIR}/e2e-common-ui.sh"

# OVH cleanup is project-scoped via OVH_PROJECT_ID (from NodeClass).
cleanup_ovh_instances() {
    log_info "Cleaning up existing cb-* OVH instances..."
    if ! command -v go &>/dev/null; then
        log_warn "go not found; skipping OVH pre-cleanup"
        return 0
    fi
    cd "${PROJECT_ROOT}" || return 0
    if go run ./hack/ovh-cleanup/main.go 2>/dev/null; then
        log_info "OVH cleanup done (or no cb-* instances)"
    else
        log_warn "OVH cleanup failed or no instances found"
    fi
    cd - >/dev/null || true
}

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
    [ -z "${OVH_APPLICATION_KEY}" ] && { log_error "OVH_APPLICATION_KEY not set"; missing=$((missing+1)); }
    [ -z "${OVH_APPLICATION_SECRET}" ] && { log_error "OVH_APPLICATION_SECRET not set"; missing=$((missing+1)); }
    [ -z "${OVH_CONSUMER_KEY}" ] && { log_error "OVH_CONSUMER_KEY not set"; missing=$((missing+1)); }
    [ -z "${OVH_PROJECT_ID}" ] && { log_error "OVH_PROJECT_ID not set"; missing=$((missing+1)); }
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
    kubectl create secret generic ovh-credentials \
        --from-literal=OVH_APPLICATION_KEY="${OVH_APPLICATION_KEY}" \
        --from-literal=OVH_APPLICATION_SECRET="${OVH_APPLICATION_SECRET}" \
        --from-literal=OVH_CONSUMER_KEY="${OVH_CONSUMER_KEY}" \
        --namespace="${CONTROLLER_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -

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
        - name: OVH_APPLICATION_KEY
          valueFrom:
            secretKeyRef:
              name: ovh-credentials
              key: OVH_APPLICATION_KEY
        - name: OVH_APPLICATION_SECRET
          valueFrom:
            secretKeyRef:
              name: ovh-credentials
              key: OVH_APPLICATION_SECRET
        - name: OVH_CONSUMER_KEY
          valueFrom:
            secretKeyRef:
              name: ovh-credentials
              key: OVH_CONSUMER_KEY
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

create_ovh_secret() {
    log_info "Creating OVH credentials secret in test namespace..."
    kubectl create secret generic ovh-api-credentials \
        --from-literal=OVH_APPLICATION_KEY="${OVH_APPLICATION_KEY}" \
        --from-literal=OVH_APPLICATION_SECRET="${OVH_APPLICATION_SECRET}" \
        --from-literal=OVH_CONSUMER_KEY="${OVH_CONSUMER_KEY}" \
        --namespace="${TEST_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -
}

create_nodeclass() {
    log_info "Creating OVH NodeClass..."
    HOST_TS_IP=$(get_tailscale_ip)

    # Build optional region field: CloudBroker now returns proper OVH region slugs
    # (GRA7, DE1, etc.), so pinning is not required. Only include if explicitly set.
    local region_field=""
    if [ -n "${OVH_REGION:-}" ]; then
        region_field="    region: \"${OVH_REGION}\""
        log_info "Pinning to OVH region: ${OVH_REGION}"
    else
        log_info "Using CloudBroker-recommended region (no pin)"
    fi

    cat <<EOF | kubectl apply -f -
apiVersion: cloudburst.io/v1alpha1
kind: NodeClass
metadata:
  name: ${NODECLASS_NAME}
  namespace: ${TEST_NAMESPACE}
spec:
  ovh:
    projectID: "${OVH_PROJECT_ID}"
${region_field:+${region_field}}
    applicationKeySecretRef:
      name: ovh-api-credentials
      key: OVH_APPLICATION_KEY
    applicationSecretSecretRef:
      name: ovh-api-credentials
      key: OVH_APPLICATION_SECRET
    consumerKeySecretRef:
      name: ovh-api-credentials
      key: OVH_CONSUMER_KEY
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
    log_info "Creating OVH NodePool..."
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
    allowedProviders: ["ovh"]
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

# Do NOT delete Kind until pod, NodeClaim, and OVH instance are gone. On timeout: force teardown (cleanup_ovh_instances + kind).

CLEANUP_DONE=""
cleanup() {
    [ -n "${CLEANUP_DONE:-}" ] && return
    CLEANUP_DONE=1
    declare -f stop_ui &>/dev/null && stop_ui
    log_info "Cleaning up (will not delete Kind until pod, NodeClaim, and OVH instance are gone)..."
    local instance_id force_teardown=0
    local kind_ctx="kind-${KIND_CLUSTER_NAME:-cloudburst}"
    if ! kubectl cluster-info --context "${kind_ctx}" &>/dev/null; then
        log_warn "Kind cluster unreachable - will force teardown (OVH instances only)"
        force_teardown=1
    fi
    instance_id=$(kubectl get nodeclaim "${NODECLAIM_NAME:-none}" -n "${TEST_NAMESPACE}" -o jsonpath='{.status.instanceID}' 2>/dev/null || echo "")

    if [ "$force_teardown" -eq 0 ]; then
    log_info "Ensuring pod ${PENDING_POD_NAME} is deleted..."
    kubectl delete pod "${PENDING_POD_NAME}" -n "${TEST_NAMESPACE}" --ignore-not-found=true --grace-period=0 --force 2>/dev/null || true
    local waited=0 pod_timeout=60
    while [ $waited -lt $pod_timeout ]; do
        if ! kubectl get pod "${PENDING_POD_NAME}" -n "${TEST_NAMESPACE}" &>/dev/null; then log_info "✓ Pod deleted"; break; fi
        sleep 5; waited=$((waited + 5))
    done
    kubectl get pod "${PENDING_POD_NAME}" -n "${TEST_NAMESPACE}" &>/dev/null && { log_warn "Pod still exists after ${pod_timeout}s"; force_teardown=1; }

    [ -n "${NODECLAIM_NAME:-}" ] && [ "$NODECLAIM_NAME" != "none" ] && kubectl delete nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}" --ignore-not-found=true 2>/dev/null || true
    kubectl delete nodeclaims --all -n "${TEST_NAMESPACE}" --ignore-not-found=true 2>/dev/null || true

    local nc_timeout=${SCALEDOWN_TIMEOUT:-300}
    waited=0
    log_info "Waiting for NodeClaim(s) to be deleted (timeout: ${nc_timeout}s)..."
    while [ $waited -lt $nc_timeout ]; do
        local claims
        claims=$(kubectl get nodeclaims -n "${TEST_NAMESPACE}" --no-headers 2>/dev/null | wc -l | tr -d ' ')
        [ "${claims:-0}" -eq 0 ] && { log_info "✓ NodeClaim(s) deleted"; break; }
        sleep 10; waited=$((waited + 10))
    done
    claims=$(kubectl get nodeclaims -n "${TEST_NAMESPACE}" --no-headers 2>/dev/null | wc -l | tr -d ' ')
    [ "${claims:-0}" -gt 0 ] && { log_warn "NodeClaim(s) still exist after ${nc_timeout}s"; force_teardown=1; }

    if [ "$force_teardown" -eq 0 ] && [ -n "$instance_id" ] && command -v go &>/dev/null; then
        local term_timeout=${VM_DELETION_TIMEOUT:-180}
        waited=0
        log_info "Waiting for OVH instance $instance_id to be deleted (timeout: ${term_timeout}s)..."
        cd "${PROJECT_ROOT}" || true
        while [ $waited -lt $term_timeout ]; do
            if go run ./hack/ovh-cleanup/main.go --verify="${instance_id}" 2>/dev/null; then
                log_info "✓ OVH instance deleted"
                break
            fi
            sleep 10; waited=$((waited + 10))
        done
        go run ./hack/ovh-cleanup/main.go --verify="${instance_id}" 2>/dev/null || { log_warn "OVH instance still exists after ${term_timeout}s"; force_teardown=1; }
        cd - >/dev/null || true
    fi
    fi

    [ "$force_teardown" -eq 1 ] && { log_warn "=== Teardown timeout - forcing manual cleanup (Kind + cb-* OVH instances) ==="; cleanup_ovh_instances 2>/dev/null || true; }
    # Final sweep: orphaned cb-* in this test's project (OVH_PROJECT_ID) before teardown
    log_info "Sweeping orphaned cb-* OVH instances in project ${OVH_PROJECT_ID}..."
    cleanup_ovh_instances 2>/dev/null || true
    kubectl delete nodepool "${NODEPOOL_NAME}" -n "${TEST_NAMESPACE}" --ignore-not-found=true 2>/dev/null || true
    kubectl delete nodeclass "${NODECLASS_NAME}" -n "${TEST_NAMESPACE}" --ignore-not-found=true 2>/dev/null || true
    kubectl delete secret tailscale-auth ovh-api-credentials -n "${TEST_NAMESPACE}" --ignore-not-found=true 2>/dev/null || true
    kubectl delete namespace "${CONTROLLER_NAMESPACE}" --ignore-not-found=true 2>/dev/null || true
    kind delete cluster --name "${KIND_CLUSTER_NAME}" 2>/dev/null || true
    log_info "✓ Cleanup complete"
}

verify_ovh_instance_deleted() {
    local instance_id=$1
    local timeout=${2:-180}
    local elapsed=0
    log_info "Verifying OVH instance ${instance_id} is deleted (timeout: ${timeout}s)..."
    cd "${PROJECT_ROOT}" || return 1
    while [ $elapsed -lt $timeout ]; do
        if go run ./hack/ovh-cleanup/main.go --verify="${instance_id}" 2>/dev/null; then
            log_info "✓ OVH instance ${instance_id} confirmed deleted"
            cd - >/dev/null || true
            return 0
        fi
        echo -n "."
        sleep 10
        elapsed=$((elapsed + 10))
    done
    cd - >/dev/null || true
    log_error "Timeout: OVH instance ${instance_id} still exists"
    return 1
}

main() {
    log_info "=== CloudBurst E2E — OVH Cloud ==="
    log_info "OVH Region: ${OVH_REGION:-auto (CloudBroker)}, Project: ${OVH_PROJECT_ID}"
    echo ""
    check_prerequisites
    echo ""
    cleanup_ovh_instances
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
    create_ovh_secret
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
    if ! wait_for_phase "Joining" 600; then
        log_error "Joining phase failed"
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
    OVH_INSTANCE_ID=$(kubectl get nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}" -o jsonpath='{.status.instanceID}' 2>/dev/null || echo "")
    log_info "=== Scale-down ==="
    delete_pending_pod
    echo ""
    if ! wait_for_nodeclaim_deletion "${SCALEDOWN_TIMEOUT}"; then
        log_error "Scale-down failed"
        exit 1
    fi

    if [ -n "${OVH_INSTANCE_ID}" ]; then
        echo ""
        if ! verify_ovh_instance_deleted "${OVH_INSTANCE_ID}" "${VM_DELETION_TIMEOUT}"; then
            log_error "OVH instance was not deleted"
            exit 1
        fi
    else
        log_warn "Could not capture instanceID for verification"
    fi

    echo ""
    log_info "=== E2E PASSED (OVH Cloud) ==="
}

trap 'cleanup' EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM
main "$@"
