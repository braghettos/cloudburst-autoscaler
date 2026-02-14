#!/bin/bash
set -euo pipefail

# End-to-end integration test for the AWS EC2 provider.
# Controller runs as a Deployment inside a Kind cluster.
# A pending pod triggers the NodePool controller to auto-create a NodeClaim,
# which provisions an EC2 instance, joins it to the cluster, and the pod gets scheduled.
#
# After the pod is scheduled, it is deleted. The controller detects the node is empty
# and removes the NodeClaim (which also terminates the EC2 instance), verifying scale-down to zero.
#
# Environment Variables:
#   This script will automatically source .env file if it exists in the project root.
#   Required: AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_REGION, AWS_AMI, TAILSCALE_AUTHKEY
#   Optional: AWS_SUBNET_ID, AWS_SECURITY_GROUP_IDS (comma-separated)
#   See .env.example for all available configuration options.

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

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
AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID:?AWS_ACCESS_KEY_ID must be set}"
AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY:?AWS_SECRET_ACCESS_KEY must be set}"
AWS_REGION="${AWS_REGION:-eu-west-1}"
AWS_AMI="${AWS_AMI:-}"
AWS_SUBNET_ID="${AWS_SUBNET_ID:-}"
AWS_SECURITY_GROUP_IDS="${AWS_SECURITY_GROUP_IDS:-}"
TAILSCALE_AUTHKEY="${TAILSCALE_AUTHKEY:-}"
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-cloudburst}"
TEST_NAMESPACE="${TEST_NAMESPACE:-default}"
CONTROLLER_NAMESPACE="cloudburst-system"
NODECLASS_NAME="e2e-aws-nodeclass"
NODEPOOL_NAME="e2e-aws-nodepool"
CONTROLLER_IMAGE="cloudburst-autoscaler:e2e"
PENDING_POD_NAME="e2e-aws-resource-hungry-pod"

# Timeouts
NODECLAIM_CREATE_TIMEOUT="${NODECLAIM_CREATE_TIMEOUT:-300}"
PROVISIONING_TIMEOUT="${PROVISIONING_TIMEOUT:-600}"
READY_TIMEOUT="${READY_TIMEOUT:-600}"
POD_SCHEDULED_TIMEOUT="${POD_SCHEDULED_TIMEOUT:-120}"
SCALEDOWN_TIMEOUT="${SCALEDOWN_TIMEOUT:-300}"
VM_DELETION_TIMEOUT="${VM_DELETION_TIMEOUT:-300}"

log_info()  { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }

# ── AMI Resolution ─────────────────────────────────────────────────────────────

resolve_aws_ami() {
    if [ -n "${AWS_AMI}" ]; then
        log_info "Using configured AMI: ${AWS_AMI}"
        return 0
    fi
    log_info "AWS_AMI not set — auto-resolving latest Ubuntu 22.04 AMI for ${AWS_REGION}..."
    if ! command -v aws &>/dev/null; then
        log_error "aws CLI not found; cannot auto-resolve AMI. Set AWS_AMI manually."
        return 1
    fi
    local ami
    ami=$(aws ec2 describe-images \
        --region "${AWS_REGION}" \
        --owners 099720109477 \
        --filters "Name=name,Values=ubuntu/images/hvm-ssd/ubuntu-jammy-22.04-amd64-server-*" \
                  "Name=state,Values=available" \
        --query 'Images | sort_by(@, &CreationDate) | [-1].ImageId' \
        --output text 2>/dev/null)
    if [ -z "$ami" ] || [ "$ami" = "None" ]; then
        log_error "Could not resolve Ubuntu 22.04 AMI in ${AWS_REGION}. Set AWS_AMI in .env (e.g. from https://cloud-images.ubuntu.com/locator/ec2/). If using IAM without ec2:DescribeImages, you must set AWS_AMI manually."
        return 1
    fi
    AWS_AMI="$ami"
    export AWS_AMI
    log_info "✓ Resolved AMI: ${AWS_AMI}"
}

# ── Prerequisites ──────────────────────────────────────────────────────────────

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

    [ -z "${AWS_ACCESS_KEY_ID}" ] && { log_error "AWS_ACCESS_KEY_ID not set"; missing=$((missing+1)); }
    [ -z "${AWS_SECRET_ACCESS_KEY}" ] && { log_error "AWS_SECRET_ACCESS_KEY not set"; missing=$((missing+1)); }
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

# ── AWS EC2 cleanup (cb-* tagged or named instances) ─────────────────────────────

delete_idle_aws_instances() {
    log_info "Cleaning up existing cb-* EC2 instances..."
    if ! command -v aws &>/dev/null; then
        log_warn "aws CLI not found; skipping pre-cleanup of EC2 instances"
        return 0
    fi
    local ids
    ids=$(aws ec2 describe-instances --region "${AWS_REGION}" \
        --filters "Name=tag:Name,Values=cb-*" "Name=instance-state-name,Values=pending,running,stopping,stopped" \
        --query 'Reservations[*].Instances[*].InstanceId' --output text 2>/dev/null || true)
    if [ -z "$ids" ]; then
        log_info "No cb-* EC2 instances found"
        return 0
    fi
    for id in $ids; do
        log_info "Terminating EC2 instance: $id"
        aws ec2 terminate-instances --region "${AWS_REGION}" --instance-ids "$id" 2>/dev/null || true
    done
    log_info "✓ Idle EC2 instances cleanup requested"
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
            docker save "${CONTROLLER_IMAGE}" | docker exec -i "${node}" ctr --namespace=k8s.io images import - 2>&1 | grep -v "^$" || true
            log_info "✓ Image imported into ${node}"
        fi
    done
}

# ── Deploy Controller ──────────────────────────────────────────────────────────

deploy_controller() {
    log_info "Deploying controller into cluster..."
    HOST_TS_IP=$(get_tailscale_ip)
    local cloudbroker_url="${CLOUDBROKER_BASEURL:-http://${HOST_TS_IP}:8000}"
    log_info "CloudBroker URL: ${cloudbroker_url}"

    kubectl create namespace "${CONTROLLER_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

    kubectl create secret generic aws-credentials \
        --from-literal=AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID}" \
        --from-literal=AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY}" \
        --namespace="${CONTROLLER_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -

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
        - name: AWS_ACCESS_KEY_ID
          valueFrom:
            secretKeyRef:
              name: aws-credentials
              key: AWS_ACCESS_KEY_ID
        - name: AWS_SECRET_ACCESS_KEY
          valueFrom:
            secretKeyRef:
              name: aws-credentials
              key: AWS_SECRET_ACCESS_KEY
        - name: AWS_REGION
          value: "${AWS_REGION}"
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

    log_info "Waiting for controller deployment to be ready..."
    if ! kubectl rollout status deployment/cloudburst-controller -n "${CONTROLLER_NAMESPACE}" --timeout=120s; then
        log_error "Controller deployment failed to become ready"
        kubectl get pods -n "${CONTROLLER_NAMESPACE}" -o wide
        kubectl logs -n "${CONTROLLER_NAMESPACE}" -l app=cloudburst-controller --tail=50 2>/dev/null || true
        exit 1
    fi
    log_info "✓ Controller deployment is ready"
}

# ── Create NodeClass & NodePool ────────────────────────────────────────────────

create_aws_secret() {
    log_info "Creating AWS credentials secret in test namespace..."
    kubectl create secret generic aws-credentials \
        --from-literal=AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID}" \
        --from-literal=AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY}" \
        --namespace="${TEST_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -
    log_info "✓ AWS credentials secret created"
}

create_nodeclass() {
    log_info "Creating AWS NodeClass..."
    HOST_TS_IP=$(get_tailscale_ip)
    {
        echo "apiVersion: cloudburst.io/v1alpha1"
        echo "kind: NodeClass"
        echo "metadata:"
        echo "  name: ${NODECLASS_NAME}"
        echo "  namespace: ${TEST_NAMESPACE}"
        echo "spec:"
        echo "  aws:"
        echo "    region: \"${AWS_REGION}\""
        echo "    ami: \"${AWS_AMI}\""
        [ -n "${AWS_SUBNET_ID}" ] && echo "    subnetID: \"${AWS_SUBNET_ID}\""
        if [ -n "${AWS_SECURITY_GROUP_IDS}" ]; then
            echo "    securityGroupIDs:"
            for sg in $(echo "${AWS_SECURITY_GROUP_IDS}" | tr ',' ' '); do
                echo "    - \"${sg}\""
            done
        fi
        cat <<INNER
    accessKeyIDSecretRef:
      name: aws-credentials
      key: AWS_ACCESS_KEY_ID
    secretAccessKeySecretRef:
      name: aws-credentials
      key: AWS_SECRET_ACCESS_KEY
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
INNER
    } | kubectl apply -f -
    log_info "✓ AWS NodeClass created"
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
    log_info "Creating AWS NodePool..."
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
    allowedProviders: ["aws"]
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
    log_info "✓ AWS NodePool created"
}

# ── Create Pending Pod ─────────────────────────────────────────────────────────

create_pending_pod() {
    log_info "Creating resource-hungry pod to trigger autoscaling..."
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
            *)            [ "$current" = "$phase" ]                            && { log_info "✓ Phase reached: $phase (current: $current)"; return 0; } ;;
        esac
        [ "$current" = "Failed" ] && { log_error "NodeClaim entered Failed phase"; kubectl describe nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}"; return 1; }
        echo -n "."
        sleep 10
        elapsed=$((elapsed + 10))
    done
    log_error "Timeout waiting for phase $phase"
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
    while [ $elapsed -lt $timeout ]; do
        if ! kubectl get nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}" &>/dev/null; then
            log_info "✓ NodeClaim ${NODECLAIM_NAME} has been deleted by the controller"
            return 0
        fi
        local phase
        phase=$(kubectl get nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "unknown")
        [ "$phase" = "Deleting" ] && log_info "NodeClaim phase is Deleting, waiting for EC2 instance cleanup..."
        echo -n "."
        sleep 10
        elapsed=$((elapsed + 10))
    done
    log_error "Timeout: NodeClaim was not deleted"
    kubectl describe nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}" 2>/dev/null || true
    kubectl logs -n "${CONTROLLER_NAMESPACE}" -l app=cloudburst-controller --tail=60 2>/dev/null || true
    return 1
}

# Instance ID format: aws/<region>/<instance-id>. Pass full id or just instance-id; region is optional (default: AWS_REGION).
wait_for_aws_instance_terminated() {
    local instance_id=$1
    local timeout=${2:-${VM_DELETION_TIMEOUT:-300}}
    local region=${3:-$AWS_REGION}
    # If instance_id looks like aws/region/i-xxx, extract region and instance id
    if [[ "$instance_id" == */*/* ]]; then
        region=$(echo "$instance_id" | awk -F'/' '{print $2}')
        instance_id=$(echo "$instance_id" | awk -F'/' '{print $NF}')
    fi
    local elapsed=0
    log_info "Verifying EC2 instance ${instance_id} (region: ${region}) is terminated (timeout: ${timeout}s)..."
    while [ $elapsed -lt $timeout ]; do
        local state
        state=$(aws ec2 describe-instances --region "${region}" --instance-ids "${instance_id}" \
            --query 'Reservations[0].Instances[0].State.Name' --output text 2>/dev/null || echo "unknown")
        if [ "$state" = "terminated" ]; then
            log_info "✓ EC2 instance ${instance_id} confirmed terminated"
            return 0
        fi
        echo -n "."
        sleep 15
        elapsed=$((elapsed + 15))
    done
    log_error "Timeout: EC2 instance ${instance_id} state is ${state}"
    return 1
}

# ── Cleanup ────────────────────────────────────────────────────────────────────

CLEANUP_DONE=""
cleanup() {
    [ -n "${CLEANUP_DONE:-}" ] && return
    CLEANUP_DONE=1
    log_info "Cleaning up test resources..."
    local instance_id ec2_id
    instance_id=$(kubectl get nodeclaim "${NODECLAIM_NAME:-none}" -n "${TEST_NAMESPACE}" -o jsonpath='{.status.instanceID}' 2>/dev/null || echo "")
    ec2_id=""
    ec2_region="${AWS_REGION}"
    if [ -n "$instance_id" ] && [[ "$instance_id" == */*/* ]]; then
        ec2_id=$(echo "$instance_id" | awk -F'/' '{print $NF}')
        ec2_region=$(echo "$instance_id" | awk -F'/' '{print $2}')
    elif [ -n "$instance_id" ]; then
        ec2_id="$instance_id"
    fi
    kubectl delete pod "${PENDING_POD_NAME}" -n "${TEST_NAMESPACE}" --ignore-not-found=true --grace-period=0 --force 2>/dev/null || true
    if [ -n "${NODECLAIM_NAME:-}" ] && [ "$NODECLAIM_NAME" != "none" ]; then
        kubectl delete nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}" --ignore-not-found=true 2>/dev/null || true
    fi
    kubectl delete nodeclaims --all -n "${TEST_NAMESPACE}" --ignore-not-found=true 2>/dev/null || true
    if [ -n "$ec2_id" ] && command -v aws &>/dev/null; then
        log_info "Waiting for EC2 instance $ec2_id (region: $ec2_region) to terminate..."
        local waited=0
        local state
        local term_timeout=${VM_DELETION_TIMEOUT:-300}
        while [ $waited -lt "$term_timeout" ]; do
            state=$(aws ec2 describe-instances --region "${ec2_region}" --instance-ids "${ec2_id}" --query 'Reservations[0].Instances[0].State.Name' --output text 2>/dev/null || echo "unknown")
            [ "$state" = "terminated" ] && { log_info "✓ EC2 instance terminated"; break; }
            sleep 15; waited=$((waited+15))
        done
    fi
    delete_idle_aws_instances 2>/dev/null || true
    kubectl delete nodepool "${NODEPOOL_NAME}" -n "${TEST_NAMESPACE}" --ignore-not-found=true 2>/dev/null || true
    kubectl delete nodeclass "${NODECLASS_NAME}" -n "${TEST_NAMESPACE}" --ignore-not-found=true 2>/dev/null || true
    kubectl delete secret tailscale-auth -n "${TEST_NAMESPACE}" --ignore-not-found=true 2>/dev/null || true
    kubectl delete secret aws-credentials -n "${TEST_NAMESPACE}" --ignore-not-found=true 2>/dev/null || true
    kubectl delete namespace "${CONTROLLER_NAMESPACE}" --ignore-not-found=true 2>/dev/null || true
    kind delete cluster --name "${KIND_CLUSTER_NAME}" 2>/dev/null || true
    log_info "✓ Cleanup complete"
}

# ── Main ───────────────────────────────────────────────────────────────────────

main() {
    log_info "=== CloudBurst E2E Integration Test — AWS EC2 ==="
    log_info "AWS Region: ${AWS_REGION}, AMI: ${AWS_AMI}"
    log_info "Kind Cluster: ${KIND_CLUSTER_NAME}"
    echo ""

    check_prerequisites
    echo ""

    # Auto-resolve AMI if not configured
    if ! resolve_aws_ami; then
        log_error "Cannot proceed without a valid AMI"
        exit 1
    fi
    echo ""
    delete_idle_aws_instances
    echo ""
    setup_kind_cluster
    echo ""
    install_crds
    echo ""
    build_and_load_controller_image
    echo ""
    deploy_controller
    echo ""
    create_tailscale_secret
    echo ""
    create_aws_secret
    echo ""
    create_nodeclass
    echo ""
    create_nodepool
    echo ""
    create_pending_pod
    echo ""

    log_info "=== Monitoring Autoscaling Lifecycle ==="
    if ! wait_for_nodeclaim_creation "${NODECLAIM_CREATE_TIMEOUT}"; then
        log_error "Controller did not create a NodeClaim"
        exit 1
    fi
    echo ""
    if ! wait_for_phase "Provisioning" "${PROVISIONING_TIMEOUT}"; then
        log_error "Failed to reach Provisioning phase"
        kubectl logs -n "${CONTROLLER_NAMESPACE}" -l app=cloudburst-controller --tail=60 2>/dev/null || true
        exit 1
    fi
    instance_id=$(kubectl get nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}" -o jsonpath='{.status.instanceID}' 2>/dev/null || echo "")
    if [ -n "$instance_id" ]; then
        ec2_id=$(echo "$instance_id" | awk -F'/' '{print $NF}')
        log_info "✓ AWS instance: $instance_id (EC2 ID: $ec2_id)"
    fi
    if ! wait_for_phase "Joining" 600; then
        log_error "Failed to reach Joining phase"
        exit 1
    fi
    if ! wait_for_phase "Ready" "${READY_TIMEOUT}"; then
        log_error "Failed to reach Ready phase"
        kubectl logs -n "${CONTROLLER_NAMESPACE}" -l app=cloudburst-controller --tail=60 2>/dev/null || true
        exit 1
    fi
    echo ""
    if ! wait_for_pod_running "${POD_SCHEDULED_TIMEOUT}"; then
        log_error "Pod was not scheduled after node became Ready"
        kubectl get pods -n "${TEST_NAMESPACE}" -o wide
        exit 1
    fi
    log_info "✓ Pod scheduled onto AWS node"
    echo ""
    log_info "=== Cluster State (before scale-down) ==="
    kubectl get nodes -o wide
    kubectl get nodeclaims -n "${TEST_NAMESPACE}" -o wide 2>/dev/null || true
    kubectl get pods -n "${TEST_NAMESPACE}" -o wide
    saved_instance_id=$(kubectl get nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}" -o jsonpath='{.status.instanceID}' 2>/dev/null || echo "")
    saved_ec2_id=""
    [ -n "$saved_instance_id" ] && saved_ec2_id=$(echo "$saved_instance_id" | awk -F'/' '{print $NF}')
    echo ""
    log_info "=== Scale-Down: Deleting Pod to Trigger NodeClaim Removal ==="
    delete_pending_pod
    echo ""
    if ! wait_for_nodeclaim_deletion "${SCALEDOWN_TIMEOUT}"; then
        log_error "Scale-down failed: NodeClaim was not removed"
        exit 1
    fi
    echo ""
    if [ -n "$saved_instance_id" ] && command -v aws &>/dev/null; then
        if ! wait_for_aws_instance_terminated "$saved_instance_id" "${VM_DELETION_TIMEOUT}"; then
            log_error "Scale-down failed: EC2 instance was not terminated"
            exit 1
        fi
    else
        [ -z "$saved_ec2_id" ] && log_warn "Could not determine EC2 instance ID, skipping termination check"
    fi
    echo ""
    log_info "=== Final Cluster State (after scale-down) ==="
    kubectl get nodes -o wide
    local remaining_claims
    remaining_claims=$(kubectl get nodeclaims -n "${TEST_NAMESPACE}" --no-headers 2>/dev/null | wc -l | tr -d ' ')
    if [ "$remaining_claims" -eq 0 ]; then
        log_info "✓ No NodeClaims remaining (scaled to zero)"
    else
        log_warn "Unexpected: ${remaining_claims} NodeClaim(s) still exist"
    fi
    echo ""
    log_info "=== E2E Integration Test PASSED (AWS EC2 — Scale-Up + Scale-Down) ==="
    echo ""
}

trap 'cleanup' EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

main "$@"
