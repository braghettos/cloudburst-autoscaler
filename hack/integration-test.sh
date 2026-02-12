#!/bin/bash
set -euo pipefail

# Integration test script for CloudBurst Autoscaler
# This script tests the full lifecycle: kind cluster -> NodeClaim -> GCP VM -> Kubernetes Node
#
# Environment Variables:
#   This script will automatically source .env file if it exists in the project root.
#   See .env.example for all available configuration options.

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

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
TAILSCALE_AUTHKEY="${TAILSCALE_AUTHKEY:-}"
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-cloudburst}"
TEST_NAMESPACE="${TEST_NAMESPACE:-default}"
NODECLASS_NAME="${NODECLASS_NAME:-integration-test-nodeclass}"
NODEPOOL_NAME="${NODEPOOL_NAME:-integration-test-nodepool}"
NODECLAIM_NAME="${NODECLAIM_NAME:-cb-integration-test-nodeclaim}"

# Functions
log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

check_prerequisites() {
    log_info "Checking prerequisites..."
    
    local missing=0
    
    # Check Tailscale CLI and that the Tailscale service is running on the host
    TAILSCALE_CMD=""
    if command -v tailscale &> /dev/null; then
        TAILSCALE_CMD="tailscale"
    elif [ -f "/Applications/Tailscale.app/Contents/MacOS/Tailscale" ]; then
        TAILSCALE_CMD="/Applications/Tailscale.app/Contents/MacOS/Tailscale"
    elif [ -f "/usr/local/bin/tailscale" ]; then
        TAILSCALE_CMD="/usr/local/bin/tailscale"
    elif [ -f "/opt/homebrew/bin/tailscale" ]; then
        TAILSCALE_CMD="/opt/homebrew/bin/tailscale"
    fi

    if [ -z "$TAILSCALE_CMD" ]; then
        log_error "tailscale command not found. Install Tailscale on the host (e.g. brew install tailscale) and start the service."
        missing=$((missing + 1))
    else
        export TAILSCALE_CMD
        # Require Tailscale service to be running: status succeeds and we get an IPv4
        if ! "$TAILSCALE_CMD" status &> /dev/null; then
            log_error "Tailscale service is not running or not connected. Start Tailscale on the host (e.g. systemctl start tailscaled on Linux, or open Tailscale app on macOS)."
            missing=$((missing + 1))
        elif [ -z "$("$TAILSCALE_CMD" ip -4 2>/dev/null)" ]; then
            log_error "Tailscale is running but has no IPv4. Ensure the Tailscale daemon is connected to your tailnet."
            missing=$((missing + 1))
        else
            log_info "✓ Tailscale is installed and service is running on the host"
        fi
    fi
    
    # Check Docker
    if ! command -v docker &> /dev/null; then
        log_error "docker command not found"
        missing=$((missing + 1))
    else
        # Try multiple ways to check Docker
        if docker ps &> /dev/null 2>&1 || docker version &> /dev/null 2>&1; then
            log_info "✓ Docker is running"
        else
            log_warn "Docker check failed, but will attempt to continue..."
            # Don't fail - Docker might work during execution
        fi
    fi
    
    # Check kind
    if ! command -v kind &> /dev/null; then
        log_error "kind command not found. Install via: brew install kind"
        missing=$((missing + 1))
    else
        log_info "✓ kind is installed"
    fi
    
    # Check kubectl
    if ! command -v kubectl &> /dev/null; then
        log_error "kubectl command not found"
        missing=$((missing + 1))
    else
        log_info "✓ kubectl is installed"
    fi
    
    # Check GCP credentials
    if [ -z "${GOOGLE_APPLICATION_CREDENTIALS:-}" ]; then
        log_error "GOOGLE_APPLICATION_CREDENTIALS not set"
        missing=$((missing + 1))
    elif [ ! -f "${GOOGLE_APPLICATION_CREDENTIALS}" ]; then
        log_error "GCP service account key not found: ${GOOGLE_APPLICATION_CREDENTIALS}"
        missing=$((missing + 1))
    else
        log_info "✓ GCP credentials configured"
    fi
    
    # Check Tailscale auth key
    if [ -z "${TAILSCALE_AUTHKEY}" ]; then
        log_error "TAILSCALE_AUTHKEY not set"
        missing=$((missing + 1))
    else
        log_info "✓ Tailscale auth key configured"
    fi
    
    if [ $missing -gt 0 ]; then
        log_warn "Some prerequisites may be missing, but continuing with test..."
        log_info "Note: Test may fail if prerequisites are truly unavailable"
    fi
    
    log_info "All prerequisites met!"
}

# Outputs only the Tailscale IPv4 to stdout (logs go to stderr so they are not captured when used in HOST_TS_IP=$(get_tailscale_ip))
# Requires Tailscale service to be running on the host (no hardcoded IP fallback).
get_tailscale_ip() {
    log_info "Getting Tailscale IP..." >&2

    TAILSCALE_CMD="${TAILSCALE_CMD:-tailscale}"
    if ! command -v "$TAILSCALE_CMD" &> /dev/null; then
        if [ -f "/Applications/Tailscale.app/Contents/MacOS/Tailscale" ]; then
            TAILSCALE_CMD="/Applications/Tailscale.app/Contents/MacOS/Tailscale"
        elif [ -f "/usr/local/bin/tailscale" ]; then
            TAILSCALE_CMD="/usr/local/bin/tailscale"
        elif [ -f "/opt/homebrew/bin/tailscale" ]; then
            TAILSCALE_CMD="/opt/homebrew/bin/tailscale"
        fi
    fi

    HOST_TS_IP=$("$TAILSCALE_CMD" ip -4 2>/dev/null | tr -d '\r\n\t')
    if [ -z "$HOST_TS_IP" ]; then
        log_error "Could not get Tailscale IPv4. Ensure Tailscale is running on the host (e.g. systemctl start tailscaled, or Tailscale app connected)." >&2
        return 1
    fi
    log_info "Tailscale IP: $HOST_TS_IP" >&2
    echo "$HOST_TS_IP"
}

# Delete existing GCP VMs created by this test (name prefix cb-) so each run starts clean.
delete_idle_gcp_vms() {
    log_info "Deleting any existing cloudburst GCP VMs (prefix cb-) in ${GCP_PROJECT}/${GCP_ZONE}..."
    if ! command -v gcloud &>/dev/null; then
        log_warn "gcloud not found, skipping GCP VM cleanup"
        return 0
    fi
    local list
    list=$(gcloud compute instances list --project="${GCP_PROJECT}" --filter="zone:${GCP_ZONE} name~^cb-" --format="value(name)" 2>/dev/null || true)
    if [ -z "$list" ]; then
        log_info "No existing cb-* VMs to delete"
        return 0
    fi
    for name in $list; do
        log_info "Deleting VM: $name"
        gcloud compute instances delete "$name" --zone="${GCP_ZONE}" --project="${GCP_PROJECT}" --quiet 2>/dev/null || true
    done
    log_info "✓ Idle GCP VMs cleaned up"
}

setup_kind_cluster() {
    log_info "Setting up kind cluster..."
    
    # Ensure kind API server cert includes Tailscale IP so VMs can connect via Tailscale
    if [ -z "${HOST_TS_IP:-}" ]; then
        HOST_TS_IP=$(get_tailscale_ip | tr -d '\r\n\t')
        export HOST_TS_IP
        log_info "Using HOST_TS_IP=$HOST_TS_IP for kind cert SANs"
    fi
    
    # Run create-kind.sh script
    if [ -f "./hack/create-kind.sh" ]; then
        ./hack/create-kind.sh
    else
        log_error "hack/create-kind.sh not found"
        exit 1
    fi
    
    # Fix kubeconfig: kind sets server to 0.0.0.0:6443 but API server cert has 127.0.0.1
    KIND_CONTEXT="kind-${KIND_CLUSTER_NAME}"
    if kubectl config get-clusters 2>/dev/null | grep -q "^${KIND_CONTEXT}$"; then
        kubectl config set-cluster "${KIND_CONTEXT}" --server="https://127.0.0.1:6443" 2>/dev/null || true
    fi
    
    # Verify cluster is accessible
    if kubectl cluster-info --context "kind-${KIND_CLUSTER_NAME}" &> /dev/null; then
        log_info "✓ Kind cluster is accessible"
    else
        log_error "Kind cluster is not accessible"
        exit 1
    fi
}

install_crds() {
    log_info "Installing CRDs..."
    if make install 2>/dev/null; then
        log_info "✓ CRDs installed (via make install)"
        return
    fi
    log_warn "make install failed (e.g. controller-gen build). Trying kubectl apply -f config/crd/bases/..."
    APPLIED=0
    for f in config/crd/bases/cloudburst.io_*.yaml; do
        if [ -f "$f" ] && [ "$(wc -l < "$f")" -gt 3 ]; then
            if kubectl apply -f "$f" 2>/dev/null; then
                APPLIED=$((APPLIED + 1))
            fi
        fi
    done
    if [ $APPLIED -gt 0 ]; then
        log_info "✓ CRDs installed ($APPLIED CRDs via config/crd/bases)"
        return
    fi
    log_error "Could not install CRDs. Fix 'make install' (e.g. install controller-gen) or ensure config/crd/bases/*.yaml contain CRD definitions."
    exit 1
}

create_tailscale_secret() {
    log_info "Creating Tailscale auth key secret..."
    kubectl create secret generic tailscale-auth \
        --from-literal=authkey="${TAILSCALE_AUTHKEY}" \
        --namespace="${TEST_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -
    log_info "✓ Tailscale secret created"
}

create_nodeclass() {
    log_info "Creating NodeClass..."
    
    HOST_TS_IP=$(get_tailscale_ip | tr -d '\r\n\t')
    GCP_PROJECT_CLEAN=$(echo -n "${GCP_PROJECT}" | tr -d '\r\n\t')
    GCP_ZONE_CLEAN=$(echo -n "${GCP_ZONE}" | tr -d '\r\n\t')
    KIND_CLUSTER_NAME_CLEAN=$(echo -n "${KIND_CLUSTER_NAME}" | tr -d '\r\n\t')
    NODECLASS_NAME_CLEAN=$(echo -n "${NODECLASS_NAME}" | tr -d '\r\n\t')
    TEST_NAMESPACE_CLEAN=$(echo -n "${TEST_NAMESPACE}" | tr -d '\r\n\t')
    
    NODECLASS_FILE=$(mktemp)
    trap "rm -f ${NODECLASS_FILE}" RETURN
    cat <<EOF > "${NODECLASS_FILE}"
apiVersion: cloudburst.io/v1alpha1
kind: NodeClass
metadata:
  name: ${NODECLASS_NAME_CLEAN}
  namespace: ${TEST_NAMESPACE_CLEAN}
spec:
  gcp:
    project: "${GCP_PROJECT_CLEAN}"
    zone: "${GCP_ZONE_CLEAN}"
  imageFamily: "ubuntu-2204-lts"
  imageProject: "ubuntu-os-cloud"
  join:
    hostApiServer: "https://${HOST_TS_IP}:6443"
    kindClusterName: "${KIND_CLUSTER_NAME_CLEAN}"
    tokenTtlMinutes: 60
  tailscale:
    authKeySecretRef:
      name: tailscale-auth
      key: authkey
  bootstrap:
    kubernetesVersion: "1.34.3"
EOF
    # Remove control characters that break YAML parsing (keep newline \012)
    tr -d '\000-\011\013-\037' < "${NODECLASS_FILE}" > "${NODECLASS_FILE}.clean" && mv "${NODECLASS_FILE}.clean" "${NODECLASS_FILE}"
    kubectl apply -f "${NODECLASS_FILE}"
    
    log_info "✓ NodeClass created"
}

start_controller() {
    log_info "Building and starting controller in background..."
    if ! go build -o bin/cloudburst-autoscaler . 2>/dev/null; then
        log_warn "go build failed, trying existing binary..."
    fi
    if [ ! -f "bin/cloudburst-autoscaler" ]; then
        log_error "Controller binary not found. Run: go build -o bin/cloudburst-autoscaler ."
        exit 1
    fi
    export KUBECONFIG="${KUBECONFIG:-$HOME/.kube/config}"
    # Ensure controller has GCP and Tailscale env (inherited by child process)
    export GCP_PROJECT="${GCP_PROJECT}"
    export GCP_ZONE="${GCP_ZONE}"
    export GOOGLE_APPLICATION_CREDENTIALS="${GOOGLE_APPLICATION_CREDENTIALS:-}"
    CONTROLLER_LOG="${CONTROLLER_LOG:-/tmp/cloudburst-controller.log}"
    # Use non-default ports so multiple runs or other tools don't conflict (8081/8080 often in use)
    ./bin/cloudburst-autoscaler --health-probe-bind-address=:18081 --metrics-bind-address=:18080 2>>"$CONTROLLER_LOG" &
    CONTROLLER_PID=$!
    log_info "Controller started (PID: $CONTROLLER_PID, log: $CONTROLLER_LOG)"
    # Allow controller to register and start reconciling
    sleep 5
}

create_nodepool() {
    log_info "Creating NodePool..."
    
    cat <<EOF | tr -d '\r' | kubectl apply -f -
apiVersion: cloudburst.io/v1alpha1
kind: NodePool
metadata:
  name: ${NODEPOOL_NAME}
  namespace: ${TEST_NAMESPACE}
spec:
  requirements:
    regionConstraint: "ANY"
    arch:
    - "x86_64"
    maxPriceEurPerHour: 0.15
    allowedProviders:
    - "gcp"
  limits:
    maxNodes: 5
  template:
    labels:
      cloudburst.io/nodepool: "${NODEPOOL_NAME}"
  disruption:
    ttlSecondsAfterEmpty: 300
    ttlSecondsUntilExpired: 3600
  weight: 1
EOF
    
    log_info "✓ NodePool created"
}

create_nodeclaim() {
    log_info "Creating NodeClaim..."
    
    # Include finalizer so that when we delete the NodeClaim, the controller gets a reconcile
    # and can delete the GCP VM before the CR is removed (otherwise the CR is removed immediately
    # and the controller never runs handleDeletion).
    cat <<EOF | tr -d '\r' | kubectl apply -f -
apiVersion: cloudburst.io/v1alpha1
kind: NodeClaim
metadata:
  name: ${NODECLAIM_NAME}
  namespace: ${TEST_NAMESPACE}
  finalizers:
  - cloudburst.io/nodeclaim-protection
spec:
  nodePoolRef: "${NODEPOOL_NAME}"
  nodeClassRef: "${NODECLASS_NAME}"
  chosen:
    provider: "gcp"
    region: "us-central1"
    instanceType: "e2-medium"
    vcpu: 2
    ramGb: 4
    priceEurPerHour: 0.05
status:
  phase: Pending
EOF
    
    log_info "✓ NodeClaim created"
}

# Phase order: Pending < Provisioning < Joining < Ready
phase_reached() {
    local current=$1
    local target=$2
    case "$target" in
        Pending)     [ -n "$current" ];;
        Provisioning) [ "$current" = "Provisioning" ] || [ "$current" = "Joining" ] || [ "$current" = "Ready" ];;
        Joining)     [ "$current" = "Joining" ] || [ "$current" = "Ready" ];;
        Ready)       [ "$current" = "Ready" ];;
        *)           [ "$current" = "$target" ];;
    esac
}

wait_for_phase() {
    local phase=$1
    local timeout=${2:-600}  # Default 10 minutes
    local elapsed=0
    local interval=10
    
    log_info "Waiting for NodeClaim to reach phase: $phase (timeout: ${timeout}s)"
    
    while [ $elapsed -lt $timeout ]; do
        current_phase=$(kubectl get nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
        
        if phase_reached "$current_phase" "$phase"; then
            log_info "✓ NodeClaim reached phase: $phase (current: $current_phase)"
            return 0
        fi
        
        if [ "$current_phase" == "Failed" ]; then
            log_error "NodeClaim entered Failed phase"
            kubectl describe nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}"
            return 1
        fi
        
        echo -n "."
        sleep $interval
        elapsed=$((elapsed + interval))
    done
    
    log_error "Timeout waiting for phase $phase (last phase: $current_phase)"
    kubectl describe nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}"
    return 1
}

verify_gcp_instance() {
    log_info "Verifying GCP instance..."
    
    # Get instance ID from NodeClaim
    instance_id=$(kubectl get nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}" -o jsonpath='{.status.instanceID}' 2>/dev/null || echo "")
    
    if [ -z "$instance_id" ]; then
        log_error "Instance ID not found in NodeClaim status"
        return 1
    fi
    
    log_info "Instance ID: $instance_id"
    
    # Extract instance name from instance ID
    instance_name=$(echo "$instance_id" | awk -F'/' '{print $NF}')
    
    # Check if instance exists in GCP
    if gcloud compute instances describe "$instance_name" --zone="${GCP_ZONE}" --project="${GCP_PROJECT}" &> /dev/null; then
        log_info "✓ GCP instance exists: $instance_name"
        
        # Check instance status
        status=$(gcloud compute instances describe "$instance_name" --zone="${GCP_ZONE}" --project="${GCP_PROJECT}" --format="value(status)")
        log_info "Instance status: $status"
        
        return 0
    else
        log_error "GCP instance not found: $instance_name"
        return 1
    fi
}

verify_kubernetes_node() {
    log_info "Verifying Kubernetes node..."
    
    # Wait for node to appear
    local timeout=900  # 15 minutes
    local elapsed=0
    local interval=15
    
    while [ $elapsed -lt $timeout ]; do
        node_name=$(kubectl get nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}" -o jsonpath='{.status.nodeName}' 2>/dev/null || echo "")
        
        if [ -n "$node_name" ]; then
            # Check if node exists
            if kubectl get node "$node_name" &> /dev/null; then
                log_info "✓ Node found: $node_name"
                
                # Check node readiness
                ready=$(kubectl get node "$node_name" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')
                if [ "$ready" == "True" ]; then
                    log_info "✓ Node is Ready"
                    return 0
                else
                    log_info "Node not ready yet, status: $ready"
                fi
            fi
        fi
        
        echo -n "."
        sleep $interval
        elapsed=$((elapsed + interval))
    done
    
    log_error "Timeout waiting for node to appear"
    return 1
}

# Verify the kind cluster has the provisioned node (control-plane + at least one worker)
verify_kind_has_provisioned_node() {
    log_info "Checking kind cluster for provisioned node..."
    
    local node_count
    node_count=$(kubectl get nodes --no-headers 2>/dev/null | wc -l | tr -d ' ')
    if [ "${node_count}" -lt 2 ]; then
        log_error "Expected at least 2 nodes (control-plane + worker), got: $node_count"
        kubectl get nodes -o wide
        return 1
    fi
    
    log_info "✓ Kind cluster has $node_count node(s)"
    
    local nodeclaim_node
    nodeclaim_node=$(kubectl get nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}" -o jsonpath='{.status.nodeName}' 2>/dev/null || echo "")
    if [ -n "$nodeclaim_node" ]; then
        if kubectl get node "$nodeclaim_node" &>/dev/null; then
            log_info "✓ Provisioned node present: $nodeclaim_node"
            kubectl get nodes -o wide
            return 0
        fi
    fi
    
    # List nodes so we can see control-plane + worker(s)
    kubectl get nodes -o wide
    log_info "✓ Kind cluster node count: $node_count (provisioned node may still be joining)"
    return 0
}

# Wait for GCP instance to be deleted (so VM and Tailscale removal can complete)
wait_for_gcp_instance_gone() {
    local instance_name=$1
    local timeout=${2:-300}
    local elapsed=0
    local interval=15
    log_info "Waiting for GCP instance to be deleted: $instance_name (timeout: ${timeout}s)"
    while [ $elapsed -lt $timeout ]; do
        if ! gcloud compute instances describe "$instance_name" --zone="${GCP_ZONE}" --project="${GCP_PROJECT}" &>/dev/null; then
            log_info "✓ GCP instance deleted"
            return 0
        fi
        echo -n "."
        sleep $interval
        elapsed=$((elapsed + interval))
    done
    log_warn "Timeout waiting for GCP instance to disappear (may still be deleting)"
    return 0
}

# Guard so cleanup runs only once (e.g. when both INT and EXIT fire)
CLEANUP_DONE=""

cleanup() {
    [ -n "${CLEANUP_DONE:-}" ] && return
    CLEANUP_DONE=1
    
    log_info "Cleaning up test resources..."
    
    # Get instance name from NodeClaim before deleting (controller needs it to delete VM)
    local instance_id instance_name
    instance_id=$(kubectl get nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}" -o jsonpath='{.status.instanceID}' 2>/dev/null || echo "")
    if [ -n "$instance_id" ]; then
        instance_name=$(echo "$instance_id" | awk -F'/' '{print $NF}')
    else
        instance_name=""
    fi
    
    # If we never got an instance, dump controller log to help debug
    if [ -z "$instance_id" ]; then
        log_info "NodeClaim had no instanceID; controller log (last 80 lines):"
        tail -80 "${CONTROLLER_LOG:-/tmp/cloudburst-controller.log}" 2>/dev/null || true
    fi
    # 1. Delete NodeClaim first (controller must still be running to delete GCP VM)
    log_info "Deleting NodeClaim so controller can delete the GCP VM..."
    kubectl delete nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}" --ignore-not-found=true
    
    # 2. Wait for GCP instance to be deleted (VM shutdown runs tailscale logout)
    if [ -n "$instance_name" ]; then
        wait_for_gcp_instance_gone "$instance_name" 300
    else
        log_info "No instance ID from NodeClaim, waiting 60s for controller to process..."
        sleep 60
    fi
    
    # 3. Stop controller
    [ -n "${CONTROLLER_PID:-}" ] && kill "${CONTROLLER_PID}" 2>/dev/null || true
    sleep 2
    
    # 4. Delete other CRs and secret
    kubectl delete nodepool "${NODEPOOL_NAME}" -n "${TEST_NAMESPACE}" --ignore-not-found=true
    kubectl delete nodeclass "${NODECLASS_NAME}" -n "${TEST_NAMESPACE}" --ignore-not-found=true
    kubectl delete secret tailscale-auth -n "${TEST_NAMESPACE}" --ignore-not-found=true
    
    # 5. Delete kind cluster last (after VM is gone)
    if command -v kind &>/dev/null; then
        log_info "Deleting kind cluster: ${KIND_CLUSTER_NAME}"
        kind delete cluster --name "${KIND_CLUSTER_NAME}" || true
    fi
    
    log_info "✓ Cleanup complete"
}

# Main execution
main() {
    log_info "=== CloudBurst Autoscaler Integration Test ==="
    log_info "GCP Project: ${GCP_PROJECT}"
    log_info "GCP Zone: ${GCP_ZONE}"
    log_info "Kind Cluster: ${KIND_CLUSTER_NAME}"
    echo ""
    
    # Check prerequisites
    check_prerequisites
    echo ""
    
    # Clean up any leftover GCP VMs from previous runs
    delete_idle_gcp_vms
    echo ""
    
    # Setup
    setup_kind_cluster
    echo ""
    
    install_crds
    echo ""
    
    create_tailscale_secret
    echo ""
    
    create_nodeclass
    echo ""
    
    create_nodepool
    echo ""
    
    start_controller
    echo ""
    
    create_nodeclaim
    echo ""
    
    # Monitor lifecycle
    log_info "=== Monitoring NodeClaim Lifecycle ==="
    
    if wait_for_phase "Provisioning" "${PROVISIONING_TIMEOUT:-600}"; then
        log_info "✓ NodeClaim entered Provisioning phase"
    else
        log_error "Failed to reach Provisioning phase"
        log_info "Controller log (last 60 lines):"
        tail -60 "${CONTROLLER_LOG:-/tmp/cloudburst-controller.log}" 2>/dev/null || true
        kubectl get nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}" -o yaml 2>/dev/null | tail -50 || true
        exit 1
    fi
    
    if verify_gcp_instance; then
        log_info "✓ GCP instance verified"
    else
        log_error "GCP instance verification failed"
        exit 1
    fi
    
    if wait_for_phase "Joining" 600; then
        log_info "✓ NodeClaim entered Joining phase"
    else
        log_error "Failed to reach Joining phase"
        exit 1
    fi
    
    if wait_for_phase "Ready" "${READY_TIMEOUT:-600}"; then
        log_info "✓ NodeClaim entered Ready phase"
    else
        log_error "Failed to reach Ready phase"
        instance_id=$(kubectl get nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}" -o jsonpath='{.status.instanceID}' 2>/dev/null || echo "")
        if [ -n "$instance_id" ]; then
            instance_name=$(echo "$instance_id" | awk -F'/' '{print $NF}')
            log_info "GCP serial port output (last 150 lines) for $instance_name:"
            gcloud compute instances get-serial-port-output "$instance_name" --zone="${GCP_ZONE}" --project="${GCP_PROJECT}" 2>/dev/null | tail -150 || true
        fi
        exit 1
    fi
    
    if verify_kubernetes_node; then
        log_info "✓ Kubernetes node verified"
    else
        log_error "Kubernetes node verification failed"
        exit 1
    fi
    
    if verify_kind_has_provisioned_node; then
        log_info "✓ Kind cluster has the provisioned node"
    else
        log_error "Kind cluster node check failed"
        exit 1
    fi
    
    echo ""
    log_info "=== Integration Test PASSED ==="
    log_info "NodeClaim: ${NODECLAIM_NAME}"
    log_info "GCP Instance: $(kubectl get nodeclaim ${NODECLAIM_NAME} -n ${TEST_NAMESPACE} -o jsonpath='{.status.instanceID}')"
    log_info "Kubernetes Node: $(kubectl get nodeclaim ${NODECLAIM_NAME} -n ${TEST_NAMESPACE} -o jsonpath='{.status.nodeName}')"
    echo ""
    
    if [ -t 0 ]; then
        read -p "Press Enter to cleanup test resources..." || true
    fi
}

# Always run cleanup on exit (success, failure, or timeout) so the NodeClaim and GCP VM are deleted
CONTROLLER_PID=""
trap 'cleanup' EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

# Run main function
main "$@"
