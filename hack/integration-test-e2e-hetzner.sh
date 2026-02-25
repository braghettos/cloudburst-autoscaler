#!/bin/bash
set -euo pipefail

# End-to-end integration test for the HETZNER provider.
# Controller runs as a Deployment inside a Kind cluster.
# A pending pod (unschedulable due to resource requests) triggers the NodePool controller
# to auto-create a NodeClaim, which provisions a Hetzner Cloud VM, joins it to the cluster,
# and the pod gets scheduled onto the new node.
#
# After the pod is scheduled, it is deleted. The controller detects the node is empty
# and removes the NodeClaim (which also deletes the Hetzner VM), verifying scale-down to zero.
#
# Environment Variables:
#   This script will automatically source .env file if it exists in the project root.
#   Required: HETZNER_API_TOKEN, TAILSCALE_AUTHKEY
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
HETZNER_API_TOKEN="${HETZNER_API_TOKEN:?HETZNER_API_TOKEN must be set (e.g. in .env or environment)}"
HETZNER_LOCATION="${HETZNER_LOCATION:-fsn1}"
TAILSCALE_AUTHKEY="${TAILSCALE_AUTHKEY:-}"
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-cloudburst}"
TEST_NAMESPACE="${TEST_NAMESPACE:-default}"
CONTROLLER_NAMESPACE="cloudburst-system"
NODECLASS_NAME="e2e-hetzner-nodeclass"
NODEPOOL_NAME="e2e-hetzner-nodepool"
CONTROLLER_IMAGE="cloudburst-autoscaler:e2e"
PENDING_POD_NAME="e2e-hetzner-resource-hungry-pod"

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
if [ -f "${SCRIPT_DIR}/e2e-common-ui.sh" ]; then
    # shellcheck source=./e2e-common-ui.sh
    source "${SCRIPT_DIR}/e2e-common-ui.sh"
fi

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

    for cmd in docker kind kubectl; do
        if ! command -v $cmd &>/dev/null; then log_error "$cmd not found"; missing=$((missing+1)); else log_info "✓ $cmd"; fi
    done

    # hcloud CLI is optional (used for verification and cleanup, but not required)
    if command -v hcloud &>/dev/null; then
        log_info "✓ hcloud CLI (optional, used for verification)"
        HCLOUD_AVAILABLE=1
    else
        log_warn "hcloud CLI not found (optional, verification will use kubectl only)"
        HCLOUD_AVAILABLE=0
    fi

    [ -z "${HETZNER_API_TOKEN}" ] && { log_error "HETZNER_API_TOKEN not set"; missing=$((missing+1)); }
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

# ── Hetzner VM Cleanup ─────────────────────────────────────────────────────────

# Hetzner has no location filter in API; cleans all cb-* servers (account-wide).
delete_idle_hetzner_vms() {
    log_info "Cleaning up existing cb-* Hetzner VMs..."

    if [ "$HCLOUD_AVAILABLE" -eq 1 ]; then
        export HCLOUD_TOKEN="${HETZNER_API_TOKEN}"
        local list
        list=$(hcloud server list -o noheader -o columns=id,name 2>/dev/null | grep "cb-" || true)
        [ -z "$list" ] && { log_info "No cb-* VMs"; return 0; }
        while IFS= read -r line; do
            local id name
            id=$(echo "$line" | awk '{print $1}')
            name=$(echo "$line" | awk '{print $2}')
            log_info "Deleting Hetzner VM: $name (ID: $id)"
            hcloud server delete "$id" 2>/dev/null || true
        done <<< "$list"
        log_info "✓ Idle Hetzner VMs cleaned up"
    else
        # Use curl with Hetzner API directly
        local response
        response=$(curl -s -H "Authorization: Bearer ${HETZNER_API_TOKEN}" \
            "https://api.hetzner.cloud/v1/servers?per_page=50" 2>/dev/null || true)
        local server_ids
        server_ids=$(echo "$response" | python3 -c "
import sys, json
try:
    data = json.load(sys.stdin)
    for s in data.get('servers', []):
        if s['name'].startswith('cb-'):
            print(s['id'], s['name'])
except: pass
" 2>/dev/null || true)
        [ -z "$server_ids" ] && { log_info "No cb-* VMs (or could not list)"; return 0; }
        while IFS= read -r line; do
            local id name
            id=$(echo "$line" | awk '{print $1}')
            name=$(echo "$line" | awk '{print $2}')
            log_info "Deleting Hetzner VM: $name (ID: $id)"
            curl -s -X DELETE -H "Authorization: Bearer ${HETZNER_API_TOKEN}" \
                "https://api.hetzner.cloud/v1/servers/$id" 2>/dev/null || true
        done <<< "$server_ids"
        log_info "✓ Idle Hetzner VMs cleaned up"
    fi
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
    
    local nodes=("${KIND_CLUSTER_NAME}-control-plane" "${KIND_CLUSTER_NAME}-worker")
    
    for node in "${nodes[@]}"; do
        if docker ps --format '{{.Names}}' | grep -q "^${node}$"; then
            log_info "Importing image into ${node} via docker save | ctr import..."
            docker save "${CONTROLLER_IMAGE}" | docker exec -i "${node}" ctr --namespace=k8s.io images import - 2>&1 | grep -v "^$" || true
            log_info "✓ Image imported into ${node}"
            
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

    local cloudbroker_url="${CLOUDBROKER_BASEURL:-http://${HOST_TS_IP}:8000}"
    log_info "CloudBroker URL: ${cloudbroker_url}"

    # Create namespace
    kubectl create namespace "${CONTROLLER_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

    # Create Hetzner API token secret in controller namespace
    kubectl create secret generic hetzner-credentials \
        --from-literal=HETZNER_API_TOKEN="${HETZNER_API_TOKEN}" \
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

    # Deploy controller with Hetzner API token as env var
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
      nodeSelector:
        cloudburst.io/node: controller-worker
      containers:
      - name: manager
        image: ${CONTROLLER_IMAGE}
        imagePullPolicy: Never
        command: ["/manager"]
        args: ["--leader-elect=false"]
        env:
        - name: HETZNER_API_TOKEN
          valueFrom:
            secretKeyRef:
              name: hetzner-credentials
              key: HETZNER_API_TOKEN
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

create_hetzner_api_token_secret() {
    log_info "Creating Hetzner API token secret in test namespace..."
    kubectl create secret generic hetzner-api-token \
        --from-literal=HETZNER_API_TOKEN="${HETZNER_API_TOKEN}" \
        --namespace="${TEST_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -
    log_info "✓ Hetzner API token secret created"
}

create_nodeclass() {
    log_info "Creating Hetzner NodeClass..."
    HOST_TS_IP=$(get_tailscale_ip)
    cat <<EOF | kubectl apply -f -
apiVersion: cloudburst.io/v1alpha1
kind: NodeClass
metadata:
  name: ${NODECLASS_NAME}
  namespace: ${TEST_NAMESPACE}
spec:
  hetzner:
    location: "${HETZNER_LOCATION}"
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
EOF
    log_info "✓ Hetzner NodeClass created"
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
    log_info "Creating Hetzner NodePool..."
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
    allowedProviders: ["hetzner"]
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
    log_info "✓ Hetzner NodePool created"
}

# ── Create Pending Pod (trigger) ───────────────────────────────────────────────

create_pending_pod() {
    log_info "Creating resource-hungry pod to trigger autoscaling..."

    # The pod must only be schedulable on cloudburst-provisioned nodes (not on
    # the Kind control-plane or the dedicated controller-worker node).
    # We use a nodeSelector label that the NodePool template assigns to new nodes.
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

delete_pending_pod() {
    log_info "Deleting pod ${PENDING_POD_NAME} to trigger scale-down..."
    kubectl delete pod "${PENDING_POD_NAME}" -n "${TEST_NAMESPACE}" --grace-period=0 --force 2>/dev/null || true

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

wait_for_nodeclaim_deletion() {
    local timeout=${1:-300}
    local elapsed=0
    log_info "Waiting for controller to remove NodeClaim ${NODECLAIM_NAME} (timeout: ${timeout}s)..."
    log_info "(ttlSecondsAfterEmpty=30, node should be detected as empty shortly)"

    while [ $elapsed -lt $timeout ]; do
        if ! kubectl get nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}" &>/dev/null; then
            log_info "✓ NodeClaim ${NODECLAIM_NAME} has been deleted by the controller"
            return 0
        fi

        local phase
        phase=$(kubectl get nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "unknown")
        if [ "$phase" = "Deleting" ]; then
            log_info "NodeClaim phase is Deleting, waiting for Hetzner instance cleanup..."
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

# Wait for the Hetzner VM to be fully gone
wait_for_hetzner_vm_deletion() {
    local server_id=$1
    local timeout=${2:-180}
    local elapsed=0
    log_info "Verifying Hetzner server ${server_id} is deleted (timeout: ${timeout}s)..."

    while [ $elapsed -lt $timeout ]; do
        local status_code
        status_code=$(curl -s -o /dev/null -w "%{http_code}" \
            -H "Authorization: Bearer ${HETZNER_API_TOKEN}" \
            "https://api.hetzner.cloud/v1/servers/${server_id}" 2>/dev/null || echo "000")
        if [ "$status_code" = "404" ]; then
            log_info "✓ Hetzner server ${server_id} confirmed deleted"
            return 0
        fi
        echo -n "."
        sleep 15
        elapsed=$((elapsed + 15))
    done

    log_error "Timeout: Hetzner server ${server_id} still exists"
    return 1
}

# ── Cleanup ────────────────────────────────────────────────────────────────────

# Do NOT delete Kind until pod, NodeClaim, and Hetzner instance are gone. On timeout: force teardown (delete_idle_hetzner_vms + kind).

CLEANUP_DONE=""
cleanup() {
    [ -n "${CLEANUP_DONE:-}" ] && return
    CLEANUP_DONE=1
    declare -f stop_ui &>/dev/null && stop_ui
    log_info "Cleaning up (will not delete Kind until pod, NodeClaim, and Hetzner instance are gone)..."
    local instance_id hetzner_server_id force_teardown=0
    local kind_ctx="kind-${KIND_CLUSTER_NAME:-cloudburst}"
    if ! kubectl cluster-info --context "${kind_ctx}" &>/dev/null; then
        log_warn "Kind cluster unreachable - will force teardown (VMs only)"
        force_teardown=1
    fi
    instance_id=$(kubectl get nodeclaim "${NODECLAIM_NAME:-none}" -n "${TEST_NAMESPACE}" -o jsonpath='{.status.instanceID}' 2>/dev/null || echo "")
    hetzner_server_id=""
    [ -n "$instance_id" ] && hetzner_server_id=$(echo "$instance_id" | awk -F'/' '{print $NF}')

    if [ "$force_teardown" -eq 0 ]; then
    log_info "Ensuring pod ${PENDING_POD_NAME} is deleted..."
    kubectl delete pod "${PENDING_POD_NAME}" -n "${TEST_NAMESPACE}" --ignore-not-found=true --grace-period=0 --force 2>/dev/null || true
    local waited=0 pod_timeout=60
    while [ $waited -lt $pod_timeout ]; do
        if ! kubectl get pod "${PENDING_POD_NAME}" -n "${TEST_NAMESPACE}" &>/dev/null; then log_info "✓ Pod deleted"; break; fi
        sleep 5; waited=$((waited + 5))
    done
    kubectl get pod "${PENDING_POD_NAME}" -n "${TEST_NAMESPACE}" &>/dev/null && { log_warn "Pod still exists after ${pod_timeout}s"; force_teardown=1; }

    if [ -n "${NODECLAIM_NAME:-}" ] && [ "$NODECLAIM_NAME" != "none" ]; then
        kubectl delete nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}" --ignore-not-found=true 2>/dev/null || true
    fi
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

    if [ "$force_teardown" -eq 0 ] && [ -n "$hetzner_server_id" ]; then
        local term_timeout=${VM_DELETION_TIMEOUT:-180}
        waited=0
        log_info "Waiting for Hetzner server $hetzner_server_id to be deleted (timeout: ${term_timeout}s)..."
        while [ $waited -lt $term_timeout ]; do
            local sc
            sc=$(curl -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer ${HETZNER_API_TOKEN}" \
                "https://api.hetzner.cloud/v1/servers/${hetzner_server_id}" 2>/dev/null || echo "000")
            [ "$sc" = "404" ] && { log_info "✓ Hetzner server deleted"; break; }
            sleep 15; waited=$((waited + 15))
        done
        sc=$(curl -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer ${HETZNER_API_TOKEN}" \
            "https://api.hetzner.cloud/v1/servers/${hetzner_server_id}" 2>/dev/null || echo "000")
        [ "$sc" != "404" ] && { log_warn "Hetzner server still exists after ${term_timeout}s"; force_teardown=1; }
    fi
    fi

    [ "$force_teardown" -eq 1 ] && { log_warn "=== Teardown timeout - forcing manual cleanup (Kind + cb-* Hetzner VMs) ==="; delete_idle_hetzner_vms 2>/dev/null || true; }
    # Final sweep: orphaned cb-* servers (Hetzner is account-wide; location not filterable via API)
    log_info "Sweeping orphaned cb-* Hetzner servers..."
    delete_idle_hetzner_vms 2>/dev/null || true
    kubectl delete nodepool "${NODEPOOL_NAME}" -n "${TEST_NAMESPACE}" --ignore-not-found=true 2>/dev/null || true
    kubectl delete nodeclass "${NODECLASS_NAME}" -n "${TEST_NAMESPACE}" --ignore-not-found=true 2>/dev/null || true
    kubectl delete secret tailscale-auth hetzner-api-token -n "${TEST_NAMESPACE}" --ignore-not-found=true 2>/dev/null || true
    kubectl delete namespace "${CONTROLLER_NAMESPACE}" --ignore-not-found=true 2>/dev/null || true
    kind delete cluster --name "${KIND_CLUSTER_NAME}" 2>/dev/null || true
    log_info "✓ Cleanup complete"
}

# ── Main ───────────────────────────────────────────────────────────────────────

main() {
    log_info "=== CloudBurst E2E Integration Test — Hetzner Cloud ==="
    log_info "Hetzner Location: ${HETZNER_LOCATION}"
    log_info "Kind Cluster: ${KIND_CLUSTER_NAME}"
    echo ""

    check_prerequisites
    echo ""

    delete_idle_hetzner_vms
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

    # 4b. Deploy and verify UI (if cloudburst-autoscaler-ui exists)
    if declare -f deploy_ui &>/dev/null; then
        deploy_ui || exit 1
        echo ""
        verify_ui_ready || exit 1
        echo ""
    fi

    # 5. Create secrets, NodeClass, NodePool
    create_tailscale_secret
    echo ""
    create_hetzner_api_token_secret
    echo ""
    create_nodeclass
    echo ""
    create_nodepool
    echo ""

    # 6. Create a pending pod that is unschedulable -> triggers autoscaling
    create_pending_pod
    echo ""

    # 7. Wait for the controller to auto-create a NodeClaim
    log_info "=== Monitoring Autoscaling Lifecycle ==="
    if ! wait_for_nodeclaim_creation "${NODECLAIM_CREATE_TIMEOUT}"; then
        log_error "Controller did not create a NodeClaim"
        exit 1
    fi
    echo ""

    # 8. Monitor NodeClaim lifecycle
    if ! wait_for_phase "Provisioning" "${PROVISIONING_TIMEOUT}"; then
        log_error "Failed to reach Provisioning phase"
        log_info "Controller logs (last 60 lines):"
        kubectl logs -n "${CONTROLLER_NAMESPACE}" -l app=cloudburst-controller --tail=60 2>/dev/null || true
        exit 1
    fi

    log_info "Verifying Hetzner instance..."
    instance_id=$(kubectl get nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}" -o jsonpath='{.status.instanceID}' 2>/dev/null || echo "")
    if [ -n "$instance_id" ]; then
        # Format: hetzner/<location>/<server-id>
        hetzner_server_id=$(echo "$instance_id" | awk -F'/' '{print $NF}')
        log_info "✓ Hetzner instance: $instance_id (server ID: $hetzner_server_id)"
    fi

    if ! wait_for_phase "Joining" 600; then
        log_error "Failed to reach Joining phase"
        exit 1
    fi

    if ! wait_for_phase "Ready" "${READY_TIMEOUT}"; then
        log_error "Failed to reach Ready phase"
        log_info "Controller logs (last 60 lines):"
        kubectl logs -n "${CONTROLLER_NAMESPACE}" -l app=cloudburst-controller --tail=60 2>/dev/null || true
        exit 1
    fi

    # 9. Verify the pending pod gets scheduled onto the new node
    echo ""
    if wait_for_pod_running "${POD_SCHEDULED_TIMEOUT}"; then
        log_info "✓ Pod scheduled onto Hetzner node"
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
        run_playwright_ui_assertions "hetzner" || exit 1
        echo ""
    fi

    # Save the Hetzner server ID for later verification
    local saved_instance_id saved_server_id
    saved_instance_id=$(kubectl get nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}" -o jsonpath='{.status.instanceID}' 2>/dev/null || echo "")
    saved_server_id=""
    if [ -n "$saved_instance_id" ]; then
        saved_server_id=$(echo "$saved_instance_id" | awk -F'/' '{print $NF}')
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

    # 13. Verify the Hetzner VM is also deleted
    if [ -n "$saved_server_id" ]; then
        if ! wait_for_hetzner_vm_deletion "$saved_server_id" "${VM_DELETION_TIMEOUT}"; then
            log_error "Scale-down failed: Hetzner VM was not deleted"
            exit 1
        fi
    else
        log_warn "Could not determine Hetzner server ID, skipping VM deletion check"
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
    log_info "=== E2E Integration Test PASSED (Hetzner Cloud — Scale-Up + Scale-Down) ==="
    log_info "Scale-Up: Pod triggered NodeClaim creation → Hetzner VM provisioned → node joined → pod scheduled"
    log_info "Scale-Down: Pod deleted → node empty → controller removed NodeClaim → Hetzner VM deleted"
    echo ""
}

# Trap cleanup
trap 'cleanup' EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

main "$@"
