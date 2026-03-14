#!/bin/bash
# hack/e2e-common.sh — Shared E2E test library for all providers.
#
# Usage: source this file from a provider script, define the required callbacks,
# set provider variables, then call main_e2e.
#
# Required variables (set before sourcing or before main_e2e):
#   PROVIDER_NAME       — e.g. "gcp", "azure", "hetzner"
#   NODECLASS_NAME      — e.g. "e2e-nodeclass"
#   NODEPOOL_NAME       — e.g. "e2e-gcp-nodepool"
#   PENDING_POD_NAME    — e.g. "e2e-resource-hungry-pod"
#
# Optional variables:
#   PROVIDER_MAX_PRICE  — NodePool maxPriceEurPerHour (default: 0.50)
#   PROVIDER_POD_CPU    — pending pod CPU request     (default: "1500m")
#   PROVIDER_POD_MEMORY — pending pod memory request  (default: "2Gi")
#
# Required provider callbacks (functions):
#   provider_check_credentials   — check provider-specific env vars, increment $missing
#   provider_create_controller_secrets — create K8s secrets in $CONTROLLER_NAMESPACE
#   provider_controller_env_block — echo pre-indented env YAML for controller deployment
#   provider_nodeclass_spec       — echo provider-specific NodeClass spec YAML
#   provider_cleanup_vms          — delete stale cb-* VMs from the cloud
#   provider_verify_vm_deleted    — verify a specific VM is deleted ($1=instance_id, $2=timeout)
#
# Optional provider callbacks:
#   provider_create_test_secrets       — create extra secrets in $TEST_NAMESPACE
#   provider_controller_volume_mounts  — echo volumeMounts YAML for controller container
#   provider_controller_volumes        — echo volumes YAML for controller pod spec
#   provider_cloudbroker_ingest        — run provider-specific CloudBroker ingestion
#   provider_pre_check                 — run after setup, before creating resources
#   provider_post_scaledown_cleanup    — run after scale-down (e.g. orphan cleanup)
#   provider_nodepool_requirements_extra — echo extra YAML for requirements block (e.g. excludedInstanceTypes)

# ── Paths ─────────────────────────────────────────────────────────────────────

_E2E_LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${_E2E_LIB_DIR}/.." && pwd)"
ENV_FILE="${PROJECT_ROOT}/.env"

# Source .env if it exists
if [ -f "${ENV_FILE}" ]; then
    echo -e "\033[0;32m[INFO]\033[0m Loading environment variables from .env file"
    set -a
    # shellcheck source=/dev/null
    source "${ENV_FILE}"
    set +a
fi

# ── Colors ────────────────────────────────────────────────────────────────────

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

# ── Common Variables ──────────────────────────────────────────────────────────

CLOUDBROKER_DIR="${CLOUDBROKER_DIR:-${PROJECT_ROOT}/../cloudbroker}"
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-cloudburst}"
TEST_NAMESPACE="${TEST_NAMESPACE:-default}"
CONTROLLER_NAMESPACE="cloudburst-system"
CONTROLLER_IMAGE="cloudburst-autoscaler:e2e"
TAILSCALE_AUTHKEY="${TAILSCALE_AUTHKEY:-}"

NODECLAIM_CREATE_TIMEOUT="${NODECLAIM_CREATE_TIMEOUT:-300}"
PROVISIONING_TIMEOUT="${PROVISIONING_TIMEOUT:-600}"
READY_TIMEOUT="${READY_TIMEOUT:-600}"
POD_SCHEDULED_TIMEOUT="${POD_SCHEDULED_TIMEOUT:-120}"
SCALEDOWN_TIMEOUT="${SCALEDOWN_TIMEOUT:-300}"
VM_DELETION_TIMEOUT="${VM_DELETION_TIMEOUT:-180}"

PROVIDER_MAX_PRICE="${PROVIDER_MAX_PRICE:-0.50}"
PROVIDER_POD_CPU="${PROVIDER_POD_CPU:-1500m}"
PROVIDER_POD_MEMORY="${PROVIDER_POD_MEMORY:-2Gi}"

NODECLAIM_NAME=""

# ── Logging ───────────────────────────────────────────────────────────────────

log_info()  { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }

# ── Phase Timing ──────────────────────────────────────────────────────────────

_phase_start_ts=""
_phase_name=""
_phase_times=()

phase_start() {
    _phase_name="$1"
    _phase_start_ts=$(date +%s)
}

phase_end() {
    if [ -n "${_phase_start_ts}" ]; then
        local elapsed=$(( $(date +%s) - _phase_start_ts ))
        _phase_times+=("${_phase_name}: ${elapsed}s")
        log_info "✓ ${_phase_name} (${elapsed}s)"
        _phase_start_ts=""
    fi
}

print_phase_timings() {
    echo ""
    log_info "=== Phase Timings ==="
    for t in "${_phase_times[@]}"; do
        log_info "  $t"
    done
}

# ── Source optional UI helpers ────────────────────────────────────────────────
# Set E2E_SKIP_UI=1 to skip UI deployment and screenshots (faster, avoids flaky Playwright timeouts)

[ -z "${E2E_SKIP_UI:-}" ] && [ -f "${_E2E_LIB_DIR}/e2e-common-ui.sh" ] && source "${_E2E_LIB_DIR}/e2e-common-ui.sh"

# ── Tailscale ─────────────────────────────────────────────────────────────────

_TAILSCALE_CMD=""

_find_tailscale() {
    [ -n "${_TAILSCALE_CMD}" ] && return 0
    for p in tailscale /Applications/Tailscale.app/Contents/MacOS/Tailscale /usr/local/bin/tailscale /opt/homebrew/bin/tailscale; do
        if command -v "$p" &>/dev/null || [ -f "$p" ]; then _TAILSCALE_CMD="$p"; return 0; fi
    done
    return 1
}

get_tailscale_ip() {
    _find_tailscale || { log_error "tailscale not found" >&2; return 1; }
    local ip
    ip=$("$_TAILSCALE_CMD" ip -4 2>/dev/null | tr -d '\r\n\t')
    [ -z "$ip" ] && { log_error "Could not get Tailscale IPv4" >&2; return 1; }
    echo "$ip"
}

# ── Prerequisites ─────────────────────────────────────────────────────────────

check_prerequisites() {
    log_info "Checking prerequisites..."
    local missing=0

    if ! _find_tailscale; then
        log_error "tailscale not found"; missing=$((missing+1))
    elif ! "$_TAILSCALE_CMD" status &>/dev/null; then
        log_error "Tailscale not running"; missing=$((missing+1))
    else
        log_info "✓ Tailscale"
    fi

    for cmd in docker kind kubectl; do
        if ! command -v $cmd &>/dev/null; then log_error "$cmd not found"; missing=$((missing+1)); else log_info "✓ $cmd"; fi
    done

    [ -z "${TAILSCALE_AUTHKEY}" ] && { log_error "TAILSCALE_AUTHKEY not set"; missing=$((missing+1)); }

    # Use "|| true" so provider_check_credentials' last [ -z ] test (returns 1 when var is set) doesn't trigger set -e
    provider_check_credentials || true

    [ $missing -gt 0 ] && { log_error "Prerequisites missing, aborting."; exit 1; }
    log_info "All prerequisites met!"
}

# ── CloudBroker ───────────────────────────────────────────────────────────────

ensure_cloudbroker() {
    if curl -sf "http://127.0.0.1:8000/health" >/dev/null 2>&1; then
        log_info "CloudBroker already running at :8000"
        return 0
    fi
    if [ ! -d "${CLOUDBROKER_DIR}" ]; then
        log_error "CloudBroker not running and CLOUDBROKER_DIR not found: ${CLOUDBROKER_DIR}"
        log_error "Start CloudBroker manually or set CLOUDBROKER_DIR"
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
    if declare -f provider_cloudbroker_ingest &>/dev/null; then
        provider_cloudbroker_ingest
    fi
    log_info "✓ CloudBroker ready"
}

# ── Kind Cluster ──────────────────────────────────────────────────────────────

setup_kind_cluster() {
    log_info "Setting up kind cluster..."
    HOST_TS_IP=$(get_tailscale_ip)
    export HOST_TS_IP
    log_info "Host Tailscale IP: $HOST_TS_IP"

    # Use temp kubeconfig to avoid "failed to lock config file" when ~/.kube is restricted
    E2E_KUBECONFIG="${E2E_KUBECONFIG:-/tmp/cloudburst-e2e-kubeconfig-$$.yaml}"
    export KUBECONFIG="${E2E_KUBECONFIG}"

    ./hack/create-kind.sh

    KIND_CONTEXT="kind-${KIND_CLUSTER_NAME}"
    kubectl config set-cluster "${KIND_CONTEXT}" --server="https://127.0.0.1:6443" 2>/dev/null || true
    kubectl cluster-info --context "${KIND_CONTEXT}" &>/dev/null || { log_error "Kind cluster not accessible"; exit 1; }
    log_info "✓ Kind cluster ready"
}

# ── CRDs ──────────────────────────────────────────────────────────────────────

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

# ── Build & Load Controller Image ────────────────────────────────────────────

build_and_load_controller_image() {
    log_info "Building controller Docker image: ${CONTROLLER_IMAGE}..."
    docker build -t "${CONTROLLER_IMAGE}" . 2>&1 | tail -5

    log_info "Loading image into kind cluster..."
    local nodes=("${KIND_CLUSTER_NAME}-control-plane" "${KIND_CLUSTER_NAME}-worker")
    for node in "${nodes[@]}"; do
        if docker ps --format '{{.Names}}' | grep -q "^${node}$"; then
            log_info "Importing image into ${node}..."
            docker save "${CONTROLLER_IMAGE}" | docker exec -i "${node}" ctr --namespace=k8s.io images import - 2>&1 | grep -v "^$" || true
            if docker exec "${node}" crictl images 2>/dev/null | grep -q "cloudburst-autoscaler"; then
                log_info "✓ Image verified in ${node}"
            else
                log_warn "Image not found via crictl in ${node}"
            fi
        fi
    done
}

# ── Deploy Controller ────────────────────────────────────────────────────────

deploy_controller() {
    log_info "Deploying controller into cluster..."

    HOST_TS_IP=$(get_tailscale_ip)
    local cloudbroker_url="${CLOUDBROKER_BASEURL:-http://${HOST_TS_IP}:8000}"
    log_info "CloudBroker URL: ${cloudbroker_url}"

    kubectl create namespace "${CONTROLLER_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

    # Generate self-signed certs for webhook server (controller-runtime expects tls.crt/tls.key in CertDir)
    log_info "Creating webhook TLS certs..."
    cert_dir=$(mktemp -d)
    openssl req -x509 -newkey rsa:2048 -keyout "${cert_dir}/tls.key" -out "${cert_dir}/tls.crt" \
        -days 365 -nodes -subj "/CN=cloudburst-webhook.cloudburst-system.svc" \
        -addext "subjectAltName=DNS:cloudburst-webhook,DNS:cloudburst-webhook.${CONTROLLER_NAMESPACE},DNS:cloudburst-webhook.${CONTROLLER_NAMESPACE}.svc"
    kubectl create secret generic webhook-certs \
        --from-file=tls.crt="${cert_dir}/tls.crt" \
        --from-file=tls.key="${cert_dir}/tls.key" \
        --namespace="${CONTROLLER_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -
    rm -rf "${cert_dir}"
    log_info "✓ Webhook certs created"

    provider_create_controller_secrets

    # RBAC
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
- apiGroups: ["apps"]
  resources: ["daemonsets"]
  verbs: ["get","list","watch"]
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

    # Provider-specific fragments
    local env_block=""
    declare -f provider_controller_env_block &>/dev/null && env_block=$(provider_controller_env_block)

    local volume_mounts=""
    declare -f provider_controller_volume_mounts &>/dev/null && volume_mounts=$(provider_controller_volume_mounts)

    local volumes=""
    declare -f provider_controller_volumes &>/dev/null && volumes=$(provider_controller_volumes)

    local deploy_file
    deploy_file=$(mktemp)
    cat > "${deploy_file}" <<DEPLOYEOF
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
${env_block}
        - name: CLOUDBROKER_BASEURL
          value: "${cloudbroker_url}"
        - name: TAILSCALE_AUTHKEY
          value: "${TAILSCALE_AUTHKEY}"
        volumeMounts:
        - name: webhook-certs
          mountPath: /tmp/k8s-webhook-server/serving-certs
          readOnly: true
${volume_mounts}
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
      - name: webhook-certs
        secret:
          secretName: webhook-certs
${volumes}
DEPLOYEOF
    kubectl apply -f "${deploy_file}"
    rm -f "${deploy_file}"

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

# ── Create Resources ──────────────────────────────────────────────────────────

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
    HOST_TS_IP=$(get_tailscale_ip)
    local provider_spec
    provider_spec=$(provider_nodeclass_spec)
    cat <<EOF | kubectl apply -f -
apiVersion: cloudburst.io/v1alpha1
kind: NodeClass
metadata:
  name: ${NODECLASS_NAME}
  namespace: ${TEST_NAMESPACE}
spec:
${provider_spec}
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

create_nodepool() {
    log_info "Creating NodePool..."
    _req_extra=""
    if declare -f provider_nodepool_requirements_extra >/dev/null 2>&1; then
        _req_extra=$(provider_nodepool_requirements_extra)
    fi
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
    maxPriceEurPerHour: ${PROVIDER_MAX_PRICE}
    allowedProviders: ["${PROVIDER_NAME}"]
${_req_extra}
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
        cpu: "${PROVIDER_POD_CPU}"
        memory: "${PROVIDER_POD_MEMORY}"
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

# ── Wait Helpers ──────────────────────────────────────────────────────────────

wait_for_nodeclaim_creation() {
    local timeout=${1:-${NODECLAIM_CREATE_TIMEOUT}}
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

wait_for_phase() {
    local phase=$1
    local timeout=${2:-${READY_TIMEOUT}}
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
    log_error "Timeout waiting for phase $phase (last: $(kubectl get nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}" -o jsonpath='{.status.phase}' 2>/dev/null || echo 'unknown'))"
    kubectl describe nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}" 2>/dev/null || true
    return 1
}

wait_for_pod_running() {
    local timeout=${1:-${POD_SCHEDULED_TIMEOUT}}
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
    local timeout=${1:-${SCALEDOWN_TIMEOUT}}
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
            log_info "NodeClaim phase is Deleting, waiting for VM cleanup..."
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

# ── Cleanup ───────────────────────────────────────────────────────────────────

CLEANUP_DONE=""
_cleanup() {
    [ -n "${CLEANUP_DONE:-}" ] && return
    CLEANUP_DONE=1
    declare -f stop_ui &>/dev/null && stop_ui
    log_info "Cleaning up test resources..."
    local instance_id force_teardown=0
    local kind_ctx="kind-${KIND_CLUSTER_NAME:-cloudburst}"

    if ! kubectl cluster-info --context "${kind_ctx}" &>/dev/null; then
        log_warn "Kind cluster unreachable - will force teardown"
        force_teardown=1
    fi

    instance_id=$(kubectl get nodeclaim "${NODECLAIM_NAME:-none}" -n "${TEST_NAMESPACE}" -o jsonpath='{.status.instanceID}' 2>/dev/null || echo "")

    if [ "$force_teardown" -eq 0 ]; then
        # Ensure pod is deleted
        log_info "Ensuring pod ${PENDING_POD_NAME} is deleted..."
        kubectl delete pod "${PENDING_POD_NAME}" -n "${TEST_NAMESPACE}" --ignore-not-found=true --grace-period=0 --force 2>/dev/null || true
        local waited=0 pod_timeout=60
        while [ $waited -lt $pod_timeout ]; do
            if ! kubectl get pod "${PENDING_POD_NAME}" -n "${TEST_NAMESPACE}" &>/dev/null; then
                log_info "✓ Pod deleted"; break
            fi
            sleep 5; waited=$((waited + 5))
        done
        kubectl get pod "${PENDING_POD_NAME}" -n "${TEST_NAMESPACE}" &>/dev/null && { log_warn "Pod still exists after ${pod_timeout}s"; force_teardown=1; }

        # Delete NodeClaims
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

        # Wait for VM deletion
        if [ "$force_teardown" -eq 0 ] && [ -n "$instance_id" ]; then
            provider_verify_vm_deleted "$instance_id" "${VM_DELETION_TIMEOUT}" || force_teardown=1
        fi
    fi

    if [ "$force_teardown" -eq 1 ]; then
        log_warn "=== Teardown timeout - forcing manual cleanup ==="
        provider_cleanup_vms 2>/dev/null || true
    fi

    log_info "Sweeping orphaned cb-* VMs..."
    provider_cleanup_vms 2>/dev/null || true

    kubectl delete nodepool "${NODEPOOL_NAME}" -n "${TEST_NAMESPACE}" --ignore-not-found=true 2>/dev/null || true
    kubectl delete nodeclass "${NODECLASS_NAME}" -n "${TEST_NAMESPACE}" --ignore-not-found=true 2>/dev/null || true
    kubectl delete secret tailscale-auth -n "${TEST_NAMESPACE}" --ignore-not-found=true 2>/dev/null || true
    kubectl delete namespace "${CONTROLLER_NAMESPACE}" --ignore-not-found=true 2>/dev/null || true
    kind delete cluster --name "${KIND_CLUSTER_NAME}" 2>/dev/null || true
    log_info "✓ Cleanup complete"
}

# ── Main E2E Flow ─────────────────────────────────────────────────────────────

main_e2e() {
    log_info "=== CloudBurst E2E Integration Test — ${PROVIDER_NAME} ==="
    echo ""

    phase_start "Prerequisites"
    check_prerequisites
    phase_end
    echo ""

    phase_start "CloudBroker"
    ensure_cloudbroker
    phase_end
    echo ""

    phase_start "VM Cleanup (pre-test)"
    provider_cleanup_vms
    phase_end
    echo ""

    phase_start "Kind Cluster"
    setup_kind_cluster
    phase_end
    echo ""

    phase_start "CRDs"
    install_crds
    phase_end
    echo ""

    phase_start "Build & Load Image"
    build_and_load_controller_image
    phase_end
    echo ""

    phase_start "Deploy Controller"
    deploy_controller
    phase_end
    echo ""

    if declare -f deploy_ui &>/dev/null; then deploy_ui || exit 1; echo ""; verify_ui_ready || exit 1; echo ""; fi

    phase_start "Create Resources"
    create_tailscale_secret
    echo ""
    if declare -f provider_create_test_secrets &>/dev/null; then
        provider_create_test_secrets
        echo ""
    fi
    create_nodeclass
    echo ""
    create_nodepool
    phase_end
    echo ""

    if declare -f provider_pre_check &>/dev/null; then
        provider_pre_check
        echo ""
    fi

    if declare -f run_playwright_screenshots_phase &>/dev/null; then run_playwright_screenshots_phase "empty" "${PROVIDER_NAME}"; echo ""; fi

    phase_start "Create Pending Pod"
    create_pending_pod
    phase_end
    echo ""

    if declare -f run_playwright_screenshots_phase &>/dev/null; then run_playwright_screenshots_phase "pending" "${PROVIDER_NAME}"; echo ""; fi

    # Monitor autoscaling lifecycle
    log_info "=== Monitoring Autoscaling Lifecycle ==="
    phase_start "NodeClaim Creation"
    if ! wait_for_nodeclaim_creation "${NODECLAIM_CREATE_TIMEOUT}"; then
        log_error "Controller did not create a NodeClaim"
        exit 1
    fi
    phase_end

    if declare -f run_playwright_screenshots_phase &>/dev/null; then
        if wait_for_phase_exact "Provisioning" 90; then
            run_playwright_screenshots_phase "provisioning" "${PROVIDER_NAME}"
            echo ""
        else
            log_warn "Skipping provisioning screenshots (NodeClaim already past Provisioning)"
        fi
    fi

    phase_start "Provisioning"
    if ! wait_for_phase "Provisioning" "${PROVISIONING_TIMEOUT}"; then
        log_error "Failed to reach Provisioning phase"
        log_info "NodeClaim status:"
        kubectl describe nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}" 2>/dev/null || true
        log_info "Controller logs (last 120 lines):"
        kubectl logs -n "${CONTROLLER_NAMESPACE}" -l app=cloudburst-controller --tail=120 2>/dev/null || true
        exit 1
    fi
    phase_end

    log_info "Verifying cloud instance..."
    local instance_id
    instance_id=$(kubectl get nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}" -o jsonpath='{.status.instanceID}' 2>/dev/null || echo "")
    if [ -n "$instance_id" ]; then
        log_info "✓ Cloud instance: $instance_id"
    fi

    phase_start "Joining"
    if ! wait_for_phase "Joining" 600; then
        log_error "Failed to reach Joining phase"
        exit 1
    fi
    phase_end

    phase_start "Ready"
    if ! wait_for_phase "Ready" "${READY_TIMEOUT}"; then
        log_error "Failed to reach Ready phase"
        kubectl logs -n "${CONTROLLER_NAMESPACE}" -l app=cloudburst-controller --tail=60 2>/dev/null || true
        exit 1
    fi
    phase_end

    if declare -f run_playwright_screenshots_phase &>/dev/null; then run_playwright_screenshots_phase "ready" "${PROVIDER_NAME}"; echo ""; fi

    phase_start "Pod Scheduled"
    echo ""
    if wait_for_pod_running "${POD_SCHEDULED_TIMEOUT}"; then
        log_info "✓ Pod scheduled onto ${PROVIDER_NAME} node"
        if declare -f run_playwright_screenshots_phase &>/dev/null; then run_playwright_screenshots_phase "scheduled" "${PROVIDER_NAME}"; echo ""; fi
    else
        log_error "Pod was not scheduled after node became Ready"
        kubectl get pods -n "${TEST_NAMESPACE}" -o wide
        exit 1
    fi
    phase_end

    echo ""
    log_info "=== Cluster State (before scale-down) ==="
    kubectl get nodes -o wide
    echo ""
    kubectl get nodeclaims -n "${TEST_NAMESPACE}" -o wide 2>/dev/null || true
    echo ""
    kubectl get pods -n "${TEST_NAMESPACE}" -o wide

    if declare -f run_playwright_ui_assertions &>/dev/null; then
        run_playwright_ui_assertions "${PROVIDER_NAME}" || exit 1
        echo ""
    fi

    # Save instance info for VM deletion verification
    local saved_instance_id
    saved_instance_id=$(kubectl get nodeclaim "${NODECLAIM_NAME}" -n "${TEST_NAMESPACE}" -o jsonpath='{.status.instanceID}' 2>/dev/null || echo "")

    # Scale-down
    echo ""
    log_info "=== Scale-Down: Deleting Pod to Trigger NodeClaim Removal ==="
    delete_pending_pod
    echo ""

    phase_start "Scale-Down"
    if ! wait_for_nodeclaim_deletion "${SCALEDOWN_TIMEOUT}"; then
        log_error "Scale-down failed: NodeClaim was not removed"
        exit 1
    fi
    phase_end
    echo ""

    # Verify VM deletion
    if [ -n "$saved_instance_id" ]; then
        phase_start "VM Deletion Verification"
        if ! provider_verify_vm_deleted "$saved_instance_id" "${VM_DELETION_TIMEOUT}"; then
            log_error "Scale-down failed: cloud VM was not deleted"
            exit 1
        fi
        phase_end
    else
        log_warn "Could not determine instance ID, skipping VM deletion check"
    fi

    # Provider-specific post scale-down cleanup
    if declare -f provider_post_scaledown_cleanup &>/dev/null; then
        provider_post_scaledown_cleanup
    fi

    # Final verification
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

    print_phase_timings

    echo ""
    log_info "=== E2E Integration Test PASSED (${PROVIDER_NAME} — Scale-Up + Scale-Down) ==="
    echo ""
}

# ── Trap Setup ────────────────────────────────────────────────────────────────

trap '_cleanup' EXIT
trap '_cleanup; exit 130' INT
trap '_cleanup; exit 143' TERM
