#!/bin/bash
set -euo pipefail

# End-to-end integration test: controller runs as a Deployment inside the kind cluster.
# A pending pod (unschedulable due to resource requests) triggers the NodePool controller
# to auto-create a NodeClaim, which provisions a GCP VM, joins it to the cluster, and the
# pod gets scheduled onto the new node.
#
# After the pod is scheduled, it is deleted. The controller detects the node is empty
# and removes the NodeClaim (which also deletes the GCP VM), verifying scale-down to zero.
#
# Environment Variables:
#   This script will automatically source .env file if it exists in the project root.
#   See .env.example for all available configuration options.

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

# Source .env file if it exists (from project root)
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
ENV_FILE="${PROJECT_ROOT}/.env"

if [ -f "${ENV_FILE}" ]; then
    echo -e "${GREEN}[INFO]${NC} Loading environment variables from .env file"
    # shellcheck source=/dev/null
    set -a
    source "${ENV_FILE}"
    set +a
fi

# Configuration
CLOUDBROKER_DIR="${CLOUDBROKER_DIR:-${PROJECT_ROOT}/../cloudbroker}"
GCP_PROJECT="${GCP_PROJECT:?GCP_PROJECT must be set (e.g. in .env or environment)}"
GCP_ZONE="${GCP_ZONE:-us-central1-a}"
TAILSCALE_AUTHKEY="${TAILSCALE_AUTHKEY:-}"
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-cloudburst}"
TEST_NAMESPACE="${TEST_NAMESPACE:-default}"
CONTROLLER_NAMESPACE="cloudburst-system"
NODECLASS_NAME="e2e-nodeclass"
NODEPOOL_NAME="e2e-gcp-nodepool"
CONTROLLER_IMAGE="cloudburst-autoscaler:e2e"
PENDING_POD_NAME="e2e-resource-hungry-pod"

# Timeouts
NODECLAIM_CREATE_TIMEOUT="${NODECLAIM_CREATE_TIMEOUT:-300}"
PROVISIONING_TIMEOUT="${PROVISIONING_TIMEOUT:-600}"
READY_TIMEOUT="${READY_TIMEOUT:-600}"
POD_SCHEDULED_TIMEOUT="${POD_SCHEDULED_TIMEOUT:-120}"
SCALEDOWN_TIMEOUT="${SCALEDOWN_TIMEOUT:-300}"
VM_DELETION_TIMEOUT="${VM_DELETION_TIMEOUT:-180}"

log_info()  { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }

# Source common UI deploy/verify (optional)
[ -f "${SCRIPT_DIR}/e2e-common-ui.sh" ] && source "${SCRIPT_DIR}/e2e-common-ui.sh"

# ── Prerequisites ──────────────────────────────────────────────────────────────

check_prerequisites() {
    log_info "Checking prerequisites..."
    local missing=0

    # Tailscale CLI
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

    for cmd in docker kind kubectl gcloud; do
        if ! command -v $cmd &>/dev/null; then log_error "$cmd not found"; missing=$((missing+1)); else log_info "✓ $cmd"; fi
    done

    [ -z "${GOOGLE_APPLICATION_CREDENTIALS:-}" ] && { log_error "GOOGLE_APPLICATION_CREDENTIALS not set"; missing=$((missing+1)); }
    [ -n "${GOOGLE_APPLICATION_CREDENTIALS:-}" ] && [ ! -f "${GOOGLE_APPLICATION_CREDENTIALS}" ] && { log_error "GCP key not found: $GOOGLE_APPLICATION_CREDENTIALS"; missing=$((missing+1)); }
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

# ── CloudBroker (required for recommendations) ───────────────────────────────────

ensure_cloudbroker() {
    if curl -sf "http://127.0.0.1:8000/health" >/dev/null 2>&1; then
        log_info "CloudBroker already running at :8000"
        return 0
    fi
    if [ ! -d "${CLOUDBROKER_DIR}" ]; then
        log_error "CloudBroker not running and CLOUDBROKER_DIR not found: ${CLOUDBROKER_DIR}"
        log_error "Start CloudBroker manually: cd ../cloudbroker && make up && make migrate && make ingest-gcp"
        exit 1
    fi
    log_info "Starting CloudBroker from ${CLOUDBROKER_DIR}..."
    (cd "${CLOUDBROKER_DIR}" && docker compose up -d 2>&1) | tail -5
    log_info "Waiting for CloudBroker API health..."
    local retries=30
    while [ $retries -gt 0 ]; do
        if curl -sf "http://127.0.0.1:8000/health" >/dev/null 2>&1; then
            log_info "✓ CloudBroker API is healthy"
            break
        fi
        retries=$((retries - 1))
        [ $retries -eq 0 ] && { log_error "CloudBroker API failed to become healthy"; exit 1; }
        sleep 2
    done
    log_info "Running migrations..."
    (cd "${CLOUDBROKER_DIR}" && make migrate 2>&1) | tail -3
    log_info "Ingesting GCP (make ingest-gcp)..."
    (cd "${CLOUDBROKER_DIR}" && make ingest-gcp 2>&1) | tail -5
    log_info "✓ CloudBroker ready"
}

# ── GCP VM Cleanup ─────────────────────────────────────────────────────────────

# Uses GCP_ZONE when set (from NodeClass) to scope cleanup to this test's zone.
delete_idle_gcp_vms() {
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

# ── Kind Cluster ───────────────────────────────────────────────────────────────

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

# ── CRDs ───────────────────────────────────────────────────────────────────────

install_crds() {
    log_info "Installing CRDs..."
    if make install 2>/dev/null; then log_info "✓ CRDs installed (make install)"; return; fi
    log_warn "make install failed, trying kubectl apply..."
    local applied=0
    for f in config/crd/bases/cloudburst.io_*.yaml; do
        [ -f "$f" ] && kubectl apply -f "$f" 2>/dev/null && applied=$((applied+1))
    done
    [ $applied -gt 0 ] && { log_info "✓ CRDs installed ($applied)"; return; }
    log_error "Could not install CRDs"; exit 1
}

# ── Build & Load Controller Image ─────────────────────────────────────────────

build_and_load_controller_image() {
    log_info "Building controller Docker image: ${CONTROLLER_IMAGE}..."
    docker build -t "${CONTROLLER_IMAGE}" . 2>&1 | tail -5

    log_info "Loading image into kind cluster..."
    
    # Load image into all nodes (control-plane and worker nodes)
    local nodes=("${KIND_CLUSTER_NAME}-control-plane" "${KIND_CLUSTER_NAME}-worker")
    
    for node in "${nodes[@]}"; do
        # Check if node exists (worker node might not exist in older clusters)
        if docker ps --format '{{.Names}}' | grep -q "^${node}$"; then
            log_info "Importing image into ${node} via docker save | ctr import..."
            docker save "${CONTROLLER_IMAGE}" | docker exec -i "${node}" ctr --namespace=k8s.io images import - 2>&1 | grep -v "^$" || true
            log_info "✓ Image imported into ${node}"
            
            # Verify image is available
            if docker exec "${node}" crictl images 2>/dev/null | grep -q "cloudburst-autoscaler"; then
                log_info "✓ Image verified in ${node}"
            else
                log_warn "Image not found via crictl in ${node}; listing images:"
                docker exec "${node}" crictl images 2>/dev/null | head -15 || true
            fi
        fi
    done
}

# ── Deploy Controller ──────────────────────────────────────────────────────────

deploy_controller() {
    log_info "Deploying controller into cluster..."

    HOST_TS_IP=$(get_tailscale_ip)

    # Resolve CloudBroker URL: prefer env, else use host Tailscale IP (CloudBroker runs on host:8000)
    local cloudbroker_url="${CLOUDBROKER_BASEURL:-http://${HOST_TS_IP}:8000}"
    log_info "CloudBroker URL: ${cloudbroker_url}"

    # Create namespace
    kubectl create namespace "${CONTROLLER_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

    # Create GCP credentials secret from the SA key file
    kubectl create secret generic gcp-credentials \
        --from-file=credentials.json="${GOOGLE_APPLICATION_CREDENTIALS}" \
        --namespace="${CONTROLLER_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -

    # Apply RBAC
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
- apiGroups: ["cloudburst.io"]
  resources: ["nodepools/finalizers","nodeclasses/finalizers","nodeclaims/finalizers"]
  verbs: ["update"]
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["get","list","watch"]
- apiGroups: [""]
  resources: ["pods/exec"]
  verbs: ["create"]
- apiGroups: [""]
  resources: ["nodes"]
  verbs: ["get","list","watch","create","update","patch","delete"]
- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["get","list","watch","create","update","patch","delete"]
- apiGroups: [""]
  resources: ["configmaps"]
  verbs: ["get","list","watch"]
- apiGroups: [""]
  resources: ["events"]
  verbs: ["create","patch"]
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

    # Deploy controller
    cat <<EOF | kubectl apply -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: cloudburst-controller
  namespace: ${CONTROLLER_NAMESPACE}
  labels:
    app: cloudburst-controller
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
      # Ensure controller runs on the dedicated worker node
      nodeSelector:
        cloudburst.io/node: controller-worker
      containers:
      - name: manager
        image: ${CONTROLLER_IMAGE}
        imagePullPolicy: Never
        command: ["/manager"]
        args: ["--leader-elect=false"]
        env:
        - name: GOOGLE_APPLICATION_CREDENTIALS
          value: /var/run/secrets/gcp/credentials.json
        - name: CLOUDBROKER_BASEURL
          value: "${cloudbroker_url}"
        - name: TAILSCALE_AUTHKEY
          value: "${TAILSCALE_AUTHKEY}"
        volumeMounts:
        - name: gcp-credentials
          mountPath: /var/run/secrets/gcp
          readOnly: true
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
      volumes:
      - name: gcp-credentials
        secret:
          secretName: gcp-credentials
EOF

    # Wait for deployment to be ready
    log_info "Waiting for controller deployment to be ready..."
    if kubectl rollout status deployment/cloudburst-controller -n "${CONTROLLER_NAMESPACE}" --timeout=120s; then
        log_info "✓ Controller deployment is ready"
    else
        log_error "Controller deployment failed to become ready"
        kubectl get pods -n "${CONTROLLER_NAMESPACE}" -o wide
        kubectl logs -n "${CONTROLLER_NAMESPACE}" -l app=cloudburst-controller --tail=50 2>/dev/null || true
        exit 1
    fi
}

# ── Create NodeClass & NodePool ────────────────────────────────────────────────

create_nodeclass() {
    log_info "Creating NodeClass..."
    HOST_TS_IP=$(get_tailscale_ip)
    cat <<EOF | kubectl apply -f -
apiVersion: cloudburst.io/v1alpha1
kind: NodeClass
metadata:
  name: ${NODECLASS_NAME}
  namespace: ${TEST_NAMESPACE}
spec:
  gcp:
    project: "${GCP_PROJECT}"
    zone: "${GCP_ZONE}"
    imageFamily: "ubuntu-2204-lts"
    imageProject: "ubuntu-os-cloud"
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
    log_info "✓ NodeClass created"
}

create_tailscale_secret() {
    log_info "Creating Tailscale auth key secret..."
    kubectl create secret generic tailscale-auth \
        --from-literal=authkey="${TAILSCALE_AUTHKEY}" \
        --namespace="${TEST_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -
    log_info "✓ Tailscale secret created"
}

create_nodepool() {
    log_info "Creating NodePool..."
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
    allowedProviders: ["gcp"]
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
    log_info "✓ NodePool created"
}

# ── Create Pending Pod (trigger) ───────────────────────────────────────────────

create_pending_pod() {
    log_info "Creating resource-hungry pod to trigger autoscaling..."

    # Use a nodeSelector label that only exists on nodes created by the NodePool,
    # so this pod cannot be scheduled on existing Kind nodes (control-plane or worker).
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: ${PENDING_POD_NAME}
  namespace: ${TEST_NAMESPACE}
  labels:
    app: e2e-workload
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
    log_info "✓ Pending pod created"

    # Verify it becomes Pending/Unschedulable
    log_info "Waiting for pod to become Unschedulable..."
    local elapsed=0
    while [ $elapsed -lt 60 ]; do
        local reason
        reason=$(kubectl get pod "${PENDING_POD_NAME}" -n "${TEST_NAMESPACE}" -o jsonpath='{.status.conditions[?(@.reason=="Unschedulable")].reason}' 2>/dev/null || true)
        if [ "$reason" = "Unschedulable" ]; then
            log_info "✓ Pod is Unschedulable (will trigger autoscaling)"
            return 0
        fi
        sleep 5
        elapsed=$((elapsed + 5))
    done
    log_warn "Pod may not be marked Unschedulable yet, continuing anyway..."
}

# ── Wait Helpers ───────────────────────────────────────────────────────────────

# Wait for a NodeClaim to be auto-created by the controller
wait_for_nodeclaim_creation() {
    local timeout=${1:-300}
    local elapsed=0
    log_info "Waiting for controller to auto-create a NodeClaim (timeout: ${timeout}s)..."
    while [ $elapsed -lt $timeout ]; do
        local claims
        claims=$(kubectl get nodeclaims -n "${TEST_NAMESPACE}" --no-headers 2>/dev/null | wc -l | tr -d ' ')
        if [ "$claims" -gt 0 ]; then
            NODECLAIM_NAME=$(kubectl get nodeclaims -n "${TEST_NAMESPACE}" --no-headers -o custom-columns=NAME:.metadata.name 2>/dev/null | head -1)
            log_info "✓ NodeClaim auto-created: ${NODECLAIM_NAME}"
            return 0
        fi
        echo -n "."
        sleep 10
        elapsed=$((elapsed + 10))
    done
    log_error "Timeout: no NodeClaim was created"
    log_info "Controller logs (last 60 lines):"
    kubectl logs -n "${CONTROLLER_NAMESPACE}" -l app=cloudburst-controller --tail=60 2>/dev/null || true
    return 1
}

# Wait for NodeClaim to be exactly in a given phase (for screenshot capture)
# Use when we need to capture a specific transient phase (e.g. Provisioning)
wait_for_phase_exact() {
    local phase=$1
    local timeout=${2:-120}
    local elapsed=0
    local current=""

    log_info "Waiting for NodeClaim ${NODECLAIM_NAME} to be in phase: $phase (timeout: ${timeout}s)"
    while [ $elapsed -lt $timeout ]; do
        current=$(kubectl get nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")

        [ "$current" = "Failed" ] && { log_error "NodeClaim entered Failed phase"; return 1; }
        if [ "$current" = "$phase" ]; then
            log_info "✓ NodeClaim is in phase: $phase"
            return 0
        fi

        echo -n "."
        sleep 2
        elapsed=$((elapsed + 2))
    done
    log_warn "Timeout: NodeClaim never in exact phase $phase (last: ${current:-unknown})"
    return 1
}

# Wait for NodeClaim to reach a given phase (or later)
wait_for_phase() {
    local phase=$1
    local timeout=${2:-600}
    local elapsed=0

    log_info "Waiting for NodeClaim ${NODECLAIM_NAME} to reach phase: $phase (timeout: ${timeout}s)"
    while [ $elapsed -lt $timeout ]; do
        local current
        current=$(kubectl get nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")

        case "$phase" in
            Provisioning) [[ "$current" =~ ^(Provisioning|Joining|Ready)$ ]] && { log_info "✓ Phase reached: $phase (current: $current)"; return 0; } ;;
            Joining)      [[ "$current" =~ ^(Joining|Ready)$ ]]              && { log_info "✓ Phase reached: $phase (current: $current)"; return 0; } ;;
            Ready)        [ "$current" = "Ready" ]                            && { log_info "✓ Phase reached: $phase (current: $current)"; return 0; } ;;
            *)            [ "$current" = "$phase" ]                           && { log_info "✓ Phase reached: $phase (current: $current)"; return 0; } ;;
        esac

        [ "$current" = "Failed" ] && { log_error "NodeClaim entered Failed phase"; kubectl describe nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}"; return 1; }

        echo -n "."
        sleep 10
        elapsed=$((elapsed + 10))
    done
    log_error "Timeout waiting for phase $phase (last: $(kubectl get nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}" -o jsonpath='{.status.phase}' 2>/dev/null || echo 'unknown'))"
    kubectl describe nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}" 2>/dev/null || true
    return 1
}

# Wait for the pending pod to be scheduled and running
wait_for_pod_running() {
    local timeout=${1:-120}
    local elapsed=0
    log_info "Waiting for pod ${PENDING_POD_NAME} to be scheduled and Running (timeout: ${timeout}s)..."
    while [ $elapsed -lt $timeout ]; do
        local phase
        phase=$(kubectl get pod "${PENDING_POD_NAME}" -n "${TEST_NAMESPACE}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
        if [ "$phase" = "Running" ]; then
            local node
            node=$(kubectl get pod "${PENDING_POD_NAME}" -n "${TEST_NAMESPACE}" -o jsonpath='{.spec.nodeName}' 2>/dev/null || echo "")
            log_info "✓ Pod is Running on node: $node"
            return 0
        fi
        echo -n "."
        sleep 10
        elapsed=$((elapsed + 10))
    done
    log_error "Timeout: pod never reached Running"
    kubectl describe pod "${PENDING_POD_NAME}" -n "${TEST_NAMESPACE}" 2>/dev/null || true
    return 1
}

# ── Scale-Down Helpers ────────────────────────────────────────────────────────

# Delete the pending pod and confirm deletion
delete_pending_pod() {
    log_info "Deleting pod ${PENDING_POD_NAME} to trigger scale-down..."
    kubectl delete pod "${PENDING_POD_NAME}" -n "${TEST_NAMESPACE}" --grace-period=0 --force 2>/dev/null || true

    # Wait for the pod to actually be gone
    local elapsed=0
    while [ $elapsed -lt 60 ]; do
        if ! kubectl get pod "${PENDING_POD_NAME}" -n "${TEST_NAMESPACE}" &>/dev/null; then
            log_info "✓ Pod ${PENDING_POD_NAME} deleted"
            return 0
        fi
        sleep 5
        elapsed=$((elapsed + 5))
    done
    log_warn "Pod may still be terminating, continuing..."
}

# Wait for the controller to delete the NodeClaim (phase → Deleting → gone)
wait_for_nodeclaim_deletion() {
    local timeout=${1:-300}
    local elapsed=0
    log_info "Waiting for controller to remove NodeClaim ${NODECLAIM_NAME} (timeout: ${timeout}s)..."
    log_info "(ttlSecondsAfterEmpty=30, node should be detected as empty shortly)"

    while [ $elapsed -lt $timeout ]; do
        # Check if NodeClaim still exists
        if ! kubectl get nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}" &>/dev/null; then
            log_info "✓ NodeClaim ${NODECLAIM_NAME} has been deleted by the controller"
            return 0
        fi

        # Show current phase for progress
        local phase
        phase=$(kubectl get nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "unknown")
        if [ "$phase" = "Deleting" ]; then
            log_info "NodeClaim phase is Deleting, waiting for GCP instance cleanup..."
        fi

        echo -n "."
        sleep 10
        elapsed=$((elapsed + 10))
    done

    log_error "Timeout: NodeClaim was not deleted"
    log_info "NodeClaim state:"
    kubectl describe nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}" 2>/dev/null || true
    log_info "Controller logs (last 60 lines):"
    kubectl logs -n "${CONTROLLER_NAMESPACE}" -l app=cloudburst-controller --tail=60 2>/dev/null || true
    return 1
}

# Wait for the GCP VM to be fully gone
wait_for_gcp_vm_deletion() {
    local instance_name=$1
    local timeout=${2:-180}
    local elapsed=0
    log_info "Verifying GCP instance ${instance_name} is deleted (timeout: ${timeout}s)..."

    while [ $elapsed -lt $timeout ]; do
        if ! gcloud compute instances describe "$instance_name" --zone="${GCP_ZONE}" --project="${GCP_PROJECT}" &>/dev/null; then
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

# ── Cleanup ────────────────────────────────────────────────────────────────────

# Do NOT delete the Kind cluster until pod is gone, NodeClaim is gone, and GCP VM is deleted.
# If any step times out (pod: 60s, nodeclaim: SCALEDOWN_TIMEOUT, vm: VM_DELETION_TIMEOUT), force teardown: delete all cb-* VMs, then delete Kind.

CLEANUP_DONE=""
cleanup() {
    [ -n "${CLEANUP_DONE:-}" ] && return
    CLEANUP_DONE=1
    declare -f stop_ui &>/dev/null && stop_ui
    log_info "Cleaning up test resources (will not delete Kind until pod, NodeClaim, and GCP VM are gone)..."
    local instance_id instance_name instance_zone force_teardown=0
    local kind_ctx="kind-${KIND_CLUSTER_NAME:-cloudburst}"
    # If cluster is unreachable (e.g. never created or already gone), skip graceful wait
    if ! kubectl cluster-info --context "${kind_ctx}" &>/dev/null; then
        log_warn "Kind cluster unreachable - will force teardown (VMs only)"
        force_teardown=1
    fi
    # Capture instance info BEFORE any deletions (NodeClaim may be deleted by controller)
    # Format: projects/PROJECT/zones/ZONE/instances/NAME
    instance_id=$(kubectl get nodeclaim "${NODECLAIM_NAME:-none}" -n "${TEST_NAMESPACE}" -o jsonpath='{.status.instanceID}' 2>/dev/null || echo "")
    instance_name=""
    instance_zone="${GCP_ZONE}"
    if [ -n "$instance_id" ]; then
        instance_name=$(echo "$instance_id" | awk -F'/' '{print $NF}')
        # Parse zone from projects/PROJECT/zones/ZONE/instances/NAME
        if [[ "$instance_id" == *"/zones/"*"/instances/"* ]]; then
            instance_zone=$(echo "$instance_id" | sed -E 's|.*/zones/([^/]+)/instances/.*|\1|')
            [[ "$instance_zone" == "$instance_id" ]] && instance_zone="${GCP_ZONE}"
        fi
    fi

    # Steps 1-4: only when cluster is reachable
    if [ "$force_teardown" -eq 0 ]; then
    # Step 1: Ensure pod is deleted
    log_info "Ensuring pod ${PENDING_POD_NAME} is deleted..."
    kubectl delete pod "${PENDING_POD_NAME}" -n "${TEST_NAMESPACE}" --ignore-not-found=true --grace-period=0 --force 2>/dev/null || true
    local waited=0
    local pod_timeout=60
    while [ $waited -lt $pod_timeout ]; do
        if ! kubectl get pod "${PENDING_POD_NAME}" -n "${TEST_NAMESPACE}" &>/dev/null; then
            log_info "✓ Pod deleted"
            break
        fi
        sleep 5
        waited=$((waited + 5))
    done
    if kubectl get pod "${PENDING_POD_NAME}" -n "${TEST_NAMESPACE}" &>/dev/null; then
        log_warn "Pod still exists after ${pod_timeout}s - will force teardown"
        force_teardown=1
    fi

    # Step 2: Delete NodeClaim(s) to trigger controller to delete GCP VM
    if [ -n "${NODECLAIM_NAME:-}" ] && [ "$NODECLAIM_NAME" != "none" ]; then
        kubectl delete nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}" --ignore-not-found=true 2>/dev/null || true
    fi
    kubectl delete nodeclaims --all -n "${TEST_NAMESPACE}" --ignore-not-found=true 2>/dev/null || true

    # Step 3: Wait for NodeClaim(s) to be deleted
    local nc_timeout=${SCALEDOWN_TIMEOUT:-300}
    waited=0
    log_info "Waiting for NodeClaim(s) to be deleted (timeout: ${nc_timeout}s)..."
    while [ $waited -lt $nc_timeout ]; do
        local claims
        claims=$(kubectl get nodeclaims -n "${TEST_NAMESPACE}" --no-headers 2>/dev/null | wc -l | tr -d ' ')
        if [ "${claims:-0}" -eq 0 ]; then
            log_info "✓ NodeClaim(s) deleted"
            break
        fi
        sleep 10
        waited=$((waited + 10))
    done
    claims=$(kubectl get nodeclaims -n "${TEST_NAMESPACE}" --no-headers 2>/dev/null | wc -l | tr -d ' ')
    if [ "${claims:-0}" -gt 0 ]; then
        log_warn "NodeClaim(s) still exist after ${nc_timeout}s - will force teardown"
        force_teardown=1
    fi

    # Step 4: Wait for GCP VM to be deleted
    if [ "$force_teardown" -eq 0 ] && [ -n "$instance_name" ] && command -v gcloud &>/dev/null; then
        local term_timeout=${VM_DELETION_TIMEOUT:-180}
        waited=0
        log_info "Waiting for GCP instance $instance_name (zone: $instance_zone) to be deleted (timeout: ${term_timeout}s)..."
        while [ $waited -lt $term_timeout ]; do
            if ! gcloud compute instances describe "$instance_name" --zone="${instance_zone}" --project="${GCP_PROJECT}" &>/dev/null; then
                log_info "✓ GCP instance deleted"
                break
            fi
            sleep 15
            waited=$((waited + 15))
        done
        if gcloud compute instances describe "$instance_name" --zone="${instance_zone}" --project="${GCP_PROJECT}" &>/dev/null; then
            log_warn "GCP instance still exists after ${term_timeout}s - will force teardown"
            force_teardown=1
        fi
    fi
    fi

    if [ "$force_teardown" -eq 1 ]; then
        log_warn "=== Teardown timeout - forcing manual cleanup (Kind cluster + all cb-* GCP VMs) ==="
        delete_idle_gcp_vms 2>/dev/null || true
    fi

    # Final sweep: any orphaned cb-* in this test's zone before teardown
    log_info "Sweeping orphaned cb-* VMs in zone used by this test..."
    delete_idle_gcp_vms 2>/dev/null || true

    # Delete Kind cluster and remaining K8s resources
    kubectl delete nodepool "${NODEPOOL_NAME}" -n "${TEST_NAMESPACE}" --ignore-not-found=true 2>/dev/null || true
    kubectl delete nodeclass "${NODECLASS_NAME}" -n "${TEST_NAMESPACE}" --ignore-not-found=true 2>/dev/null || true
    kubectl delete secret tailscale-auth -n "${TEST_NAMESPACE}" --ignore-not-found=true 2>/dev/null || true
    kubectl delete namespace "${CONTROLLER_NAMESPACE}" --ignore-not-found=true 2>/dev/null || true
    kind delete cluster --name "${KIND_CLUSTER_NAME}" 2>/dev/null || true
    log_info "✓ Cleanup complete"
}

# ── Main ───────────────────────────────────────────────────────────────────────

main() {
    log_info "=== CloudBurst E2E Integration Test (In-Cluster Controller) ==="
    log_info "GCP Project: ${GCP_PROJECT}"
    log_info "GCP Zone: ${GCP_ZONE}"
    log_info "Kind Cluster: ${KIND_CLUSTER_NAME}"
    echo ""

    check_prerequisites
    echo ""

    ensure_cloudbroker
    echo ""

    delete_idle_gcp_vms
    echo ""

    # 1. Kind cluster
    setup_kind_cluster
    echo ""

    # 2. CRDs
    install_crds
    echo ""

    # 3. Build & load controller image
    build_and_load_controller_image
    echo ""

    # 4. Deploy controller as Deployment
    deploy_controller
    echo ""
    if declare -f deploy_ui &>/dev/null; then deploy_ui || exit 1; echo ""; verify_ui_ready || exit 1; echo ""; fi
    # 5. Create NodeClass, Tailscale secret, NodePool
    create_tailscale_secret
    echo ""
    create_nodeclass
    echo ""
    create_nodepool
    echo ""
    if declare -f run_playwright_screenshots_phase &>/dev/null; then run_playwright_screenshots_phase "empty" "gcp"; echo ""; fi

    # 6. Create a pending pod that is unschedulable -> triggers autoscaling
    create_pending_pod
    echo ""
    if declare -f run_playwright_screenshots_phase &>/dev/null; then run_playwright_screenshots_phase "pending" "gcp"; echo ""; fi

    # 7. Wait for the controller to auto-create a NodeClaim
    log_info "=== Monitoring Autoscaling Lifecycle ==="
    if ! wait_for_nodeclaim_creation "${NODECLAIM_CREATE_TIMEOUT}"; then
        log_error "Controller did not create a NodeClaim"
        exit 1
    fi

    # 8. Monitor NodeClaim lifecycle
    # Capture provisioning screenshots while NodeClaim is exactly in Provisioning phase
    # Must run before wait_for_phase (which returns when already Joining/Ready)
    if declare -f run_playwright_screenshots_phase &>/dev/null; then
        if wait_for_phase_exact "Provisioning" 90; then
            run_playwright_screenshots_phase "provisioning" "gcp"
            echo ""
        else
            log_warn "Skipping provisioning screenshots (NodeClaim already past Provisioning)"
        fi
    fi

    if ! wait_for_phase "Provisioning" "${PROVISIONING_TIMEOUT}"; then
        log_error "Failed to reach Provisioning phase"
        log_info "Controller logs (last 60 lines):"
        kubectl logs -n "${CONTROLLER_NAMESPACE}" -l app=cloudburst-controller --tail=60 2>/dev/null || true
        exit 1
    fi

    log_info "Verifying GCP instance..."
    instance_id=$(kubectl get nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}" -o jsonpath='{.status.instanceID}' 2>/dev/null || echo "")
    if [ -n "$instance_id" ]; then
        instance_name=$(echo "$instance_id" | awk -F'/' '{print $NF}')
        log_info "✓ GCP instance: $instance_name"
    fi

    if ! wait_for_phase "Joining" 600; then
        log_error "Failed to reach Joining phase"
        exit 1
    fi

    if ! wait_for_phase "Ready" "${READY_TIMEOUT}"; then
        log_error "Failed to reach Ready phase"
        if [ -n "${instance_name:-}" ]; then
            log_info "GCP serial port output (last 150 lines):"
            gcloud compute instances get-serial-port-output "$instance_name" --zone="${GCP_ZONE}" --project="${GCP_PROJECT}" 2>/dev/null | tail -150 || true
        fi
        log_info "Controller logs (last 60 lines):"
        kubectl logs -n "${CONTROLLER_NAMESPACE}" -l app=cloudburst-controller --tail=60 2>/dev/null || true
        exit 1
    fi
    if declare -f run_playwright_screenshots_phase &>/dev/null; then run_playwright_screenshots_phase "ready" "gcp"; echo ""; fi

    # 9. Verify the pending pod gets scheduled onto the new node
    echo ""
    if wait_for_pod_running "${POD_SCHEDULED_TIMEOUT}"; then
        log_info "✓ Pod scheduled onto GCP node"
        if declare -f run_playwright_screenshots_phase &>/dev/null; then run_playwright_screenshots_phase "scheduled" "gcp"; echo ""; fi
    else
        log_error "Pod was not scheduled after node became Ready"
        kubectl get pods -n "${TEST_NAMESPACE}" -o wide
        exit 1
    fi

    # 10. Capture state before scale-down
    echo ""
    log_info "=== Cluster State (before scale-down) ==="
    kubectl get nodes -o wide
    echo ""
    kubectl get nodeclaims -n "${TEST_NAMESPACE}" -o wide 2>/dev/null || true
    echo ""
    kubectl get pods -n "${TEST_NAMESPACE}" -o wide

    if declare -f run_playwright_ui_assertions &>/dev/null; then
        run_playwright_ui_assertions "gcp" || exit 1
        echo ""
    fi

    # Save the GCP instance name for later verification
    local gcp_instance_id gcp_instance_name
    gcp_instance_id=$(kubectl get nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}" -o jsonpath='{.status.instanceID}' 2>/dev/null || echo "")
    gcp_instance_name=""
    if [ -n "$gcp_instance_id" ]; then
        gcp_instance_name=$(echo "$gcp_instance_id" | awk -F'/' '{print $NF}')
    fi

    # 11. Delete the pod to trigger scale-down
    echo ""
    log_info "=== Scale-Down: Deleting Pod to Trigger NodeClaim Removal ==="
    delete_pending_pod
    echo ""

    # 12. Wait for the controller to detect the empty node and delete the NodeClaim
    if ! wait_for_nodeclaim_deletion "${SCALEDOWN_TIMEOUT}"; then
        log_error "Scale-down failed: NodeClaim was not removed"
        exit 1
    fi
    echo ""

    # 13. Verify the GCP VM is also deleted
    if [ -n "$gcp_instance_name" ]; then
        if ! wait_for_gcp_vm_deletion "$gcp_instance_name" "${VM_DELETION_TIMEOUT}"; then
            log_error "Scale-down failed: GCP VM was not deleted"
            exit 1
        fi
    else
        log_warn "Could not determine GCP instance name, skipping VM deletion check"
    fi

    # 14. Final verification
    echo ""
    log_info "=== Final Cluster State (after scale-down) ==="
    kubectl get nodes -o wide
    echo ""
    local remaining_claims
    remaining_claims=$(kubectl get nodeclaims -n "${TEST_NAMESPACE}" --no-headers 2>/dev/null | wc -l | tr -d ' ')
    if [ "$remaining_claims" -eq 0 ]; then
        log_info "✓ No NodeClaims remaining (scaled to zero)"
    else
        log_warn "Unexpected: ${remaining_claims} NodeClaim(s) still exist"
        kubectl get nodeclaims -n "${TEST_NAMESPACE}" -o wide 2>/dev/null || true
    fi

    echo ""
    log_info "=== E2E Integration Test PASSED (Scale-Up + Scale-Down) ==="
    log_info "Scale-Up: Pod triggered NodeClaim creation → GCP VM provisioned → node joined → pod scheduled"
    log_info "Scale-Down: Pod deleted → node empty → controller removed NodeClaim → GCP VM deleted"
    echo ""
}

# Trap cleanup
trap 'cleanup' EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

main "$@"
