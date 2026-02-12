#!/bin/bash
set -euo pipefail

# Multi-provider test: Creates pods that trigger provisioning on both GCP and Scaleway.
# This demonstrates that Cloudburst can provision nodes from multiple cloud providers
# simultaneously based on NodePool configuration and pod requirements.
#
# Environment Variables:
#   This script will automatically source .env file if it exists in the project root.
#   See .env.example for all available configuration options.
#
#   Required env vars:
#     GCP_PROJECT, GOOGLE_APPLICATION_CREDENTIALS, GCP_ZONE
#     SCW_PROJECT_ID, SCW_SECRET_KEY (or SCW_ACCESS_KEY + SCW_SECRET_KEY)
#     TAILSCALE_AUTHKEY
#   Optional env vars:
#     SCW_DEFAULT_ZONE, CLOUDBROKER_BASEURL, KIND_CLUSTER_NAME, TEST_NAMESPACE
#     NODECLAIM_CREATE_TIMEOUT, PROVISIONING_TIMEOUT, READY_TIMEOUT, POD_SCHEDULED_TIMEOUT

# Colors (define early for use in .env loading message)
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
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
GCP_PROJECT="${GCP_PROJECT:?GCP_PROJECT must be set (e.g. in .env or environment)}"
GCP_ZONE="${GCP_ZONE:-us-central1-a}"
SCW_ZONE="${SCW_DEFAULT_ZONE:-fr-par-1}"
TAILSCALE_AUTHKEY="${TAILSCALE_AUTHKEY:-}"
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-cloudburst}"
TEST_NAMESPACE="${TEST_NAMESPACE:-default}"
CONTROLLER_NAMESPACE="cloudburst-system"
CONTROLLER_IMAGE="cloudburst-autoscaler:e2e"

# Allow users to export either SCALEWAY_PROJECT_ID or SCW_PROJECT_ID.
# SCW_PROJECT_ID is the canonical name used by the script and NodeClass spec.
if [ -z "${SCW_PROJECT_ID:-}" ] && [ -n "${SCALEWAY_PROJECT_ID:-}" ]; then
    export SCW_PROJECT_ID="${SCALEWAY_PROJECT_ID}"
fi

# NodeClass names
GCP_NODECLASS_NAME="gcp-nodeclass"
SCW_NODECLASS_NAME="scaleway-nodeclass"

# NodePool names
GCP_NODEPOOL_NAME="gcp-nodepool"
SCW_NODEPOOL_NAME="scaleway-nodepool"

# Pod names
GCP_POD_NAME="gcp-workload-pod"
SCW_POD_NAME="scaleway-workload-pod"

# Timeouts
NODECLAIM_CREATE_TIMEOUT="${NODECLAIM_CREATE_TIMEOUT:-300}"
PROVISIONING_TIMEOUT="${PROVISIONING_TIMEOUT:-600}"
READY_TIMEOUT="${READY_TIMEOUT:-600}"
POD_SCHEDULED_TIMEOUT="${POD_SCHEDULED_TIMEOUT:-120}"

log_info()  { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }
log_provider() { echo -e "${BLUE}[PROVIDER]${NC} $1"; }

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

    # GCP credentials
    [ -z "${GOOGLE_APPLICATION_CREDENTIALS:-}" ] && { log_error "GOOGLE_APPLICATION_CREDENTIALS not set"; missing=$((missing+1)); }
    [ -n "${GOOGLE_APPLICATION_CREDENTIALS:-}" ] && [ ! -f "${GOOGLE_APPLICATION_CREDENTIALS}" ] && { log_error "GCP key not found: $GOOGLE_APPLICATION_CREDENTIALS"; missing=$((missing+1)); }
    
    # Scaleway credentials (can be from env or secret)
    if [ -z "${SCW_SECRET_KEY:-}" ] && [ -z "${SCW_ACCESS_KEY:-}" ]; then
        log_warn "SCW_SECRET_KEY not set - Scaleway provisioning may fail if not configured via secret"
    else
        log_info "✓ Scaleway credentials (from env)"
    fi

    # Scaleway project ID (required so the SDK can create servers)
    if [ -z "${SCW_PROJECT_ID:-}" ]; then
        log_error "SCW_PROJECT_ID not set (required for Scaleway NodeClass spec.scaleway.projectID)"
        missing=$((missing+1))
    fi

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

# ── Setup (reuse from integration-test-e2e.sh) ────────────────────────────────

setup_kind_cluster() {
    log_info "Setting up Kind cluster..."
    HOST_TS_IP=$(get_tailscale_ip)
    export HOST_TS_IP
    log_info "Host Tailscale IP: $HOST_TS_IP"

    if [ -f "./hack/create-kind.sh" ]; then
        ./hack/create-kind.sh
    else
        # Fallback to direct kind create if script doesn't exist
        if kind get clusters | grep -q "^${KIND_CLUSTER_NAME}$"; then
            log_info "Kind cluster ${KIND_CLUSTER_NAME} already exists"
        else
            kind create cluster --name "${KIND_CLUSTER_NAME}" --config - <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  apiServerAddress: "0.0.0.0"
  apiServerPort: 6443
nodes:
- role: control-plane
  kubeadmConfigPatches:
  - |
    kind: InitConfiguration
    nodeRegistration:
      kubeletExtraArgs:
        node-labels: "node-role.kubernetes.io/control-plane="
- role: worker
  image: kindest/node:v1.34.3
  kubeadmConfigPatches:
  - |
    kind: JoinConfiguration
    nodeRegistration:
      kubeletExtraArgs:
        node-labels: "cloudburst.io/node=controller-worker"
EOF
            log_info "✓ Kind cluster created"
        fi
    fi

    KIND_CONTEXT="kind-${KIND_CLUSTER_NAME}"
    kubectl config set-cluster "${KIND_CONTEXT}" --server="https://127.0.0.1:6443" 2>/dev/null || true
    kubectl cluster-info --context "${KIND_CONTEXT}" &>/dev/null || { log_error "Kind cluster not accessible"; exit 1; }
    log_info "✓ Kind cluster ready"
}

install_crds() {
    log_info "Installing CRDs..."
    if make install 2>/dev/null; then
        log_info "✓ CRDs installed"
    else
        log_warn "make install failed, trying kubectl apply..."
        local applied=0
        for f in config/crd/bases/cloudburst.io_*.yaml; do
            [ -f "$f" ] && kubectl apply -f "$f" 2>/dev/null && applied=$((applied+1))
        done
        [ $applied -gt 0 ] && { log_info "✓ CRDs installed ($applied)"; return; }
        log_error "Could not install CRDs"; exit 1
    fi
}

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

    # Create Scaleway credentials secret if SCW_SECRET_KEY is set
    if [ -n "${SCW_SECRET_KEY:-}" ]; then
        kubectl create secret generic scaleway-credentials \
            --from-literal=SCW_SECRET_KEY="${SCW_SECRET_KEY}" \
            --namespace="${CONTROLLER_NAMESPACE}" \
            --dry-run=client -o yaml | kubectl apply -f -
        log_info "✓ Scaleway credentials secret created"
    fi

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
        $(if [ -n "${SCW_SECRET_KEY:-}" ]; then
            echo '- name: SCW_SECRET_KEY
          valueFrom:
            secretKeyRef:
              name: scaleway-credentials
              key: SCW_SECRET_KEY'
        fi)
        $(if [ -n "${SCW_ACCESS_KEY:-}" ]; then
            echo "- name: SCW_ACCESS_KEY
          value: \"${SCW_ACCESS_KEY}\""
        fi)
        $(if [ -n "${SCW_DEFAULT_ZONE:-}" ]; then
            echo "- name: SCW_DEFAULT_ZONE
          value: \"${SCW_DEFAULT_ZONE}\""
        fi)
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

# ── Create NodeClasses ──────────────────────────────────────────────────────────

create_gcp_nodeclass() {
    log_provider "Creating GCP NodeClass..."
    HOST_TS_IP=$(get_tailscale_ip)
    cat <<EOF | kubectl apply -f -
apiVersion: cloudburst.io/v1alpha1
kind: NodeClass
metadata:
  name: ${GCP_NODECLASS_NAME}
  namespace: ${TEST_NAMESPACE}
spec:
  gcp:
    project: "${GCP_PROJECT}"
    zone: "${GCP_ZONE}"
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
    log_provider "✓ GCP NodeClass created"
}

create_scaleway_nodeclass() {
    log_provider "Creating Scaleway NodeClass..."
    HOST_TS_IP=$(get_tailscale_ip)

    # Ensure Scaleway API key secret exists in the workload namespace (where NodeClaims live),
    # since the controller looks up APIKeySecretRef in nodeClaim.Namespace.
    if [ -n "${SCW_SECRET_KEY:-}" ]; then
        kubectl create secret generic scaleway-credentials \
            --from-literal=SCW_SECRET_KEY="${SCW_SECRET_KEY}" \
            --namespace="${TEST_NAMESPACE}" \
            --dry-run=client -o yaml | kubectl apply -f -
        log_info "✓ Scaleway credentials secret created in namespace ${TEST_NAMESPACE}"
    fi

    cat <<EOF | kubectl apply -f -
apiVersion: cloudburst.io/v1alpha1
kind: NodeClass
metadata:
  name: ${SCW_NODECLASS_NAME}
  namespace: ${TEST_NAMESPACE}
spec:
  scaleway:
    zone: "${SCW_ZONE}"
    projectID: "${SCW_PROJECT_ID}"
    image: "ubuntu_jammy"
    $(if [ -n "${SCW_SECRET_KEY:-}" ]; then
        echo 'apiKeySecretRef:
      name: scaleway-credentials
      key: SCW_SECRET_KEY'
    fi)
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
    log_provider "✓ Scaleway NodeClass created"
}

create_tailscale_secret() {
    log_info "Creating Tailscale auth key secret..."
    kubectl create secret generic tailscale-auth \
        --from-literal=authkey="${TAILSCALE_AUTHKEY}" \
        --namespace="${TEST_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -
    log_info "✓ Tailscale secret created"
}

# ── Create NodePools ────────────────────────────────────────────────────────────

create_gcp_nodepool() {
    log_provider "Creating GCP NodePool..."
    # Note: Controller selects first NodeClass found in namespace.
    # We ensure only GCP NodeClass exists when GCP NodePool is created.
    cat <<EOF | kubectl apply -f -
apiVersion: cloudburst.io/v1alpha1
kind: NodePool
metadata:
  name: ${GCP_NODEPOOL_NAME}
  namespace: ${TEST_NAMESPACE}
spec:
  requirements:
    regionConstraint: "ANY"
    arch: ["x86_64"]
    maxPriceEurPerHour: 0.15
    allowedProviders: ["gcp"]
  limits:
    maxNodes: 3
    minNodes: 0
  template:
    labels:
      cloudburst.io/nodepool: "${GCP_NODEPOOL_NAME}"
      cloudburst.io/provider: "gcp"
  disruption:
    ttlSecondsAfterEmpty: 30
    ttlSecondsUntilExpired: 3600
  weight: 1
EOF
    log_provider "✓ GCP NodePool created"
}

create_scaleway_nodepool() {
    log_provider "Creating Scaleway NodePool..."
    # Note: Controller selects first NodeClass found in namespace.
    # We ensure only Scaleway NodeClass exists when Scaleway NodePool is created.
    cat <<EOF | kubectl apply -f -
apiVersion: cloudburst.io/v1alpha1
kind: NodePool
metadata:
  name: ${SCW_NODEPOOL_NAME}
  namespace: ${TEST_NAMESPACE}
spec:
  requirements:
    regionConstraint: "ANY"
    arch: ["x86_64"]
    maxPriceEurPerHour: 0.15
    allowedProviders: ["scaleway"]
  limits:
    maxNodes: 3
    minNodes: 0
  template:
    labels:
      cloudburst.io/nodepool: "${SCW_NODEPOOL_NAME}"
      cloudburst.io/provider: "scaleway"
  disruption:
    ttlSecondsAfterEmpty: 30
    ttlSecondsUntilExpired: 3600
  weight: 1
EOF
    log_provider "✓ Scaleway NodePool created"
}

# ── Create Pods ────────────────────────────────────────────────────────────────

create_gcp_pod() {
    log_provider "Creating pod that will trigger GCP provisioning..."
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: ${GCP_POD_NAME}
  namespace: ${TEST_NAMESPACE}
  labels:
    app: gcp-workload
spec:
  containers:
  - name: workload
    image: busybox:1.36
    command: ["sleep", "infinity"]
    resources:
      requests:
        cpu: "1500m"
        memory: "2Gi"
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
        - matchExpressions:
          - key: node-role.kubernetes.io/control-plane
            operator: DoesNotExist
          - key: cloudburst.io/provider
            operator: In
            values: ["gcp"]
EOF
    log_provider "✓ GCP workload pod created"
}

create_scaleway_pod() {
    log_provider "Creating pod that will trigger Scaleway provisioning..."
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: ${SCW_POD_NAME}
  namespace: ${TEST_NAMESPACE}
  labels:
    app: scaleway-workload
spec:
  containers:
  - name: workload
    image: busybox:1.36
    command: ["sleep", "infinity"]
    resources:
      requests:
        cpu: "1500m"
        memory: "2Gi"
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
        - matchExpressions:
          - key: node-role.kubernetes.io/control-plane
            operator: DoesNotExist
          - key: cloudburst.io/provider
            operator: In
            values: ["scaleway"]
EOF
    log_provider "✓ Scaleway workload pod created"
}

# ── Wait Helpers ───────────────────────────────────────────────────────────────

wait_for_pod_scheduled() {
    local pod_name=$1
    local provider=$2
    local timeout=${3:-120}
    local elapsed=0
    
    log_provider "Waiting for ${provider} pod '${pod_name}' to be scheduled (timeout: ${timeout}s)..."
    while [ $elapsed -lt $timeout ]; do
        local node_name
        node_name=$(kubectl get pod "${pod_name}" -n "${TEST_NAMESPACE}" -o jsonpath='{.spec.nodeName}' 2>/dev/null || echo "")
        if [ -n "$node_name" ] && [ "$node_name" != "null" ] && [ "$node_name" != "" ]; then
            log_provider "✓ ${provider} pod scheduled on node: ${node_name}"
            return 0
        fi
        sleep 5
        elapsed=$((elapsed + 5))
    done
    
    log_error "${provider} pod '${pod_name}' not scheduled after ${timeout}s"
    kubectl describe pod "${pod_name}" -n "${TEST_NAMESPACE}" || true
    return 1
}

wait_for_nodeclaim() {
    local nodepool_name=$1
    local provider=$2
    local timeout=${3:-300}
    local elapsed=0
    
    log_provider "Waiting for ${provider} NodeClaim to be created (timeout: ${timeout}s)..."
    while [ $elapsed -lt $timeout ]; do
        local claim
        claim=$(kubectl get nodeclaims -n "${TEST_NAMESPACE}" -l cloudburst.io/nodepool="${nodepool_name}" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")
        if [ -n "$claim" ]; then
            log_provider "✓ ${provider} NodeClaim created: ${claim}"
            return 0
        fi
        sleep 5
        elapsed=$((elapsed + 5))
    done
    
    log_error "${provider} NodeClaim not created after ${timeout}s"
    kubectl get nodeclaims -n "${TEST_NAMESPACE}" || true
    return 1
}

# ── Verification ────────────────────────────────────────────────────────────────

verify_nodes() {
    log_info "Verifying nodes from both providers..."
    
    local gcp_nodes
    gcp_nodes=$(kubectl get nodes -l cloudburst.io/provider=gcp --no-headers 2>/dev/null | wc -l | tr -d ' ')
    local scw_nodes
    scw_nodes=$(kubectl get nodes -l cloudburst.io/provider=scaleway --no-headers 2>/dev/null | wc -l | tr -d ' ')
    
    log_info "GCP nodes: ${gcp_nodes}"
    log_info "Scaleway nodes: ${scw_nodes}"
    
    if [ "$gcp_nodes" -gt 0 ] && [ "$scw_nodes" -gt 0 ]; then
        log_info "✓ Both providers have nodes!"
        kubectl get nodes -l cloudburst.io/provider --show-labels
        return 0
    else
        log_error "Not all providers have nodes (GCP: ${gcp_nodes}, Scaleway: ${scw_nodes})"
        return 1
    fi
}

# ── Main ───────────────────────────────────────────────────────────────────────

main() {
    log_info "=== Multi-Provider Test ==="
    log_info "This test will provision pods on both GCP and Scaleway"
    
    check_prerequisites
    setup_kind_cluster
    install_crds
    build_and_load_controller_image
    deploy_controller
    
    create_tailscale_secret
    
    # Create NodeClasses and NodePools in sequence to ensure correct matching
    # The controller selects the first NodeClass found, so we create them one at a time
    # with corresponding NodePools to ensure proper provider matching
    
    log_info "Creating GCP resources..."
    create_gcp_nodeclass
    create_gcp_nodepool
    
    log_info "Creating Scaleway resources..."
    create_scaleway_nodeclass
    create_scaleway_nodepool
    
    log_info "Creating pods to trigger provisioning..."
    create_gcp_pod
    create_scaleway_pod
    
    log_info "Waiting for NodeClaims to be created..."
    wait_for_nodeclaim "${GCP_NODEPOOL_NAME}" "GCP" "${NODECLAIM_CREATE_TIMEOUT}" &
    wait_for_nodeclaim "${SCW_NODEPOOL_NAME}" "Scaleway" "${NODECLAIM_CREATE_TIMEOUT}" &
    wait
    
    log_info "Waiting for pods to be scheduled..."
    wait_for_pod_scheduled "${GCP_POD_NAME}" "GCP" "${POD_SCHEDULED_TIMEOUT}" &
    wait_for_pod_scheduled "${SCW_POD_NAME}" "Scaleway" "${POD_SCHEDULED_TIMEOUT}" &
    wait
    
    verify_nodes
    
    log_info "=== Test Complete ==="
    log_info "GCP pod: ${GCP_POD_NAME}"
    log_info "Scaleway pod: ${SCW_POD_NAME}"
    log_info "Check nodes: kubectl get nodes -l cloudburst.io/provider"
    log_info "Check NodeClaims: kubectl get nodeclaims -n ${TEST_NAMESPACE}"
}

main "$@"
