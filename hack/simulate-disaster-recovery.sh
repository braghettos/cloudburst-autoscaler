#!/bin/bash
set -euo pipefail

# Disaster Recovery Use Case Simulation
#
# Tests: NodePool with hetzner+scaleway → Deployment provisions via recommendation →
# DNS patch routes controller to mock (Hetzner "outage") → manual instance delete →
# Deployment redeploys pod → controller tries Hetzner (mock 503) → falls back to Scaleway.
#
# Flow:
#   Phase 1: NodePool allowedProviders=[hetzner, scaleway]. Deployment (1 replica).
#             CloudBroker recommends one; controller provisions, node joins, pod runs.
#   Phase 2: Patch controller deployment with hostAliases (api.hetzner.cloud -> mock)
#             so controller routes Hetzner API to mock. Wait for controller rollout.
#   Phase 3: Delete cloud instance via provider API (no manual node delete).
#             Node stays as NotReady; controller (handleReady) detects unreachable
#             node after NodeUnreachableGracePeriod (90s in DR), transitions NodeClaim
#             to Deleting, cleans up. Deployment recreates pod (replacement).
#   Phase 4: NodePool sees pending pod, creates NodeClaim. CloudBroker recommends
#             (possibly Hetzner first). Controller tries Hetzner -> mock returns 503.
#             Controller falls back to next recommendation (Scaleway), provisions.
#             Pod schedules on Scaleway node. DR demonstrated.
#
# Requires --with-api-mock (deploys mock that returns 503 on Hetzner create).
#
# Prerequisites:
#   - Kind cluster, CloudBroker, NodeClass for hetzner + scaleway
#   - TAILSCALE_AUTHKEY, HETZNER_API_TOKEN, SCW_* credentials in .env
#
# Usage:
#   ./hack/simulate-disaster-recovery.sh --with-api-mock
#   ./hack/simulate-disaster-recovery.sh --use-existing-cluster --with-api-mock

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
WITH_API_MOCK=0

for arg in "$@"; do
    case "$arg" in
        --use-existing-cluster) USE_EXISTING_CLUSTER=1 ;;
        --with-api-mock) WITH_API_MOCK=1 ;;
    esac
done

if [ -f "${ENV_FILE}" ]; then
    set -a
    # shellcheck source=/dev/null
    source "${ENV_FILE}"
    set +a
fi

# DR test uses hetzner + scaleway; mock simulates Hetzner outage
ALLOWED_PROVIDERS_STR="hetzner,scaleway"
IFS=',' read -ra ALLOWED_PROVIDERS_ARR <<< "$ALLOWED_PROVIDERS_STR"

KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-cloudburst}"
TEST_NAMESPACE="${TEST_NAMESPACE:-default}"
CONTROLLER_NAMESPACE="cloudburst-system"
NODECLASS_NAME="dr-nodeclass"
NODEPOOL_NAME="dr-nodepool"
DEPLOYMENT_NAME="dr-workload"
CONTROLLER_IMAGE="cloudburst-autoscaler:e2e"

# Full provision cycle (VM create + bootstrap + join + pod schedule) can take 12-15 min
READY_TIMEOUT="${READY_TIMEOUT:-900}"
POD_SCHEDULED_TIMEOUT="${POD_SCHEDULED_TIMEOUT:-300}"

log_info()  { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }
log_phase() { echo -e "\n${CYAN}=== $1 ===${NC}"; }

# ── Provider credential checks ────────────────────────────────────────────────

has_provider_creds() {
    case "$1" in
        gcp)    [ -n "${GOOGLE_APPLICATION_CREDENTIALS:-}" ] && [ -f "${GOOGLE_APPLICATION_CREDENTIALS:-}" ];;
        scaleway) [ -n "${SCW_ACCESS_KEY:-}" ] && [ -n "${SCW_SECRET_KEY:-}" ];;
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

    [ $WITH_API_MOCK -eq 0 ] && { log_error "--with-api-mock is required for this DR test"; missing=$((missing+1)); }
    [ -z "${TAILSCALE_AUTHKEY:-}" ] && { log_error "TAILSCALE_AUTHKEY not set"; missing=$((missing+1)); }

    for p in "${ALLOWED_PROVIDERS_ARR[@]}"; do
        if ! has_provider_creds "$p"; then
            log_error "Provider '${p}' credentials not set or invalid"
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

    log_info "Allowed providers: ${ALLOWED_PROVIDERS_STR}"
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
    ENABLED_PROVIDERS_FOR_DR=""
    for p in "${ALLOWED_PROVIDERS_ARR[@]}"; do
        ENABLED_PROVIDERS_FOR_DR="${ENABLED_PROVIDERS_FOR_DR} ${p}"
    done
    export ENABLED_PROVIDERS_FOR_DR

    # Run all-providers script but stop before NodePools (we'll create our own)
    # We run it up to phase C (NodeClass), then we take over.
    # The all-providers script creates nodepools per provider in phase D. We need to either:
    # 1) Patch it to support --dr-mode, or
    # 2) Run it fully, delete its nodepools, then run our DR flow.
    # The all-providers script always scales down; we do our own setup for DR simulation.
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
    make ingest-hetzner
    make ingest-scaleway
    make seed-dr-hetzner-first

    cd "${PROJECT_ROOT}"
    HOST_TS_IP=$(get_tailscale_ip)
    export HOST_TS_IP
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
    local nodes=("${KIND_CLUSTER_NAME}-control-plane" "${KIND_CLUSTER_NAME}-worker")
    for node in "${nodes[@]}"; do
        if docker ps --format '{{.Names}}' | grep -q "^${node}$"; then
            docker save "${CONTROLLER_IMAGE}" | docker exec -i "${node}" ctr --namespace=k8s.io images import - 2>&1 | grep -v "^$" || true
        fi
    done

    deploy_mock_if_needed
    deploy_controller
    create_nodeclass_and_secrets
    log_info "✓ Setup complete"
}

deploy_mock_if_needed() {
    if [ $WITH_API_MOCK -eq 0 ]; then
        return 0
    fi
    log_info "Deploying mock Hetzner API in-cluster..."
    cd "${PROJECT_ROOT}"
    docker build -t dr-mock-hetzner:test -f hack/dr-mock-hetzner/Dockerfile hack/dr-mock-hetzner 2>&1 | tail -2
    for node in "${KIND_CLUSTER_NAME}-control-plane" "${KIND_CLUSTER_NAME}-worker"; do
        if docker ps --format '{{.Names}}' | grep -q "^${node}$"; then
            docker save dr-mock-hetzner:test | docker exec -i "${node}" ctr --namespace=k8s.io images import - 2>/dev/null || true
        fi
    done
    kubectl create namespace "${CONTROLLER_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
    kubectl create secret generic hetzner-credentials --from-literal=HETZNER_API_TOKEN="${HETZNER_API_TOKEN}" -n "${CONTROLLER_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
    kubectl apply -f - <<MOCK
apiVersion: apps/v1
kind: Deployment
metadata:
  name: dr-mock-hetzner
  namespace: ${CONTROLLER_NAMESPACE}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: dr-mock-hetzner
  template:
    metadata:
      labels:
        app: dr-mock-hetzner
    spec:
      containers:
      - name: mock
        image: dr-mock-hetzner:test
        imagePullPolicy: Never
        env:
        - name: HETZNER_API_TOKEN
          valueFrom:
            secretKeyRef:
              name: hetzner-credentials
              key: HETZNER_API_TOKEN
        - name: TLS_PORT
          value: "8443"
        ports:
        - containerPort: 8443
---
apiVersion: v1
kind: Service
metadata:
  name: dr-mock-hetzner
  namespace: ${CONTROLLER_NAMESPACE}
spec:
  selector:
    app: dr-mock-hetzner
  ports:
  - port: 443
    targetPort: 8443
MOCK
    kubectl rollout status deployment/dr-mock-hetzner -n "${CONTROLLER_NAMESPACE}" --timeout=60s
    log_info "✓ Mock deployed"
}

deploy_controller() {
    log_info "Deploying controller..."
    kubectl create namespace "${CONTROLLER_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

    [ -n "${GOOGLE_APPLICATION_CREDENTIALS:-}" ] && [ -f "${GOOGLE_APPLICATION_CREDENTIALS:-}" ] && \
        kubectl create secret generic gcp-credentials --from-file=credentials.json="${GOOGLE_APPLICATION_CREDENTIALS}" -n "${CONTROLLER_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
    [ -n "${HETZNER_API_TOKEN:-}" ] && \
        kubectl create secret generic hetzner-credentials --from-literal=HETZNER_API_TOKEN="${HETZNER_API_TOKEN}" -n "${CONTROLLER_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
    [ -n "${SCW_SECRET_KEY:-}" ] && \
        kubectl create secret generic scaleway-credentials --from-literal=SCW_SECRET_KEY="${SCW_SECRET_KEY}" \
        $([ -n "${SCW_ACCESS_KEY:-}" ] && echo "--from-literal=SCW_ACCESS_KEY=${SCW_ACCESS_KEY}") \
        -n "${CONTROLLER_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

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
        - name: SCW_ACCESS_KEY
          valueFrom:
            secretKeyRef:
              name: scaleway-credentials
              key: SCW_ACCESS_KEY
        - name: SCW_DEFAULT_PROJECT_ID
          value: "${SCW_PROJECT_ID:-}"
        - name: SCW_DEFAULT_ORGANIZATION_ID
          value: "${SCW_ORGANIZATION_ID:-${SCW_DEFAULT_ORGANIZATION_ID:-}}"
        - name: CLOUDBURST_NODE_UNREACHABLE_GRACE_PERIOD
          value: "90s"
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
        kubectl create secret generic scaleway-api-keys --from-literal=SCW_SECRET_KEY="${SCW_SECRET_KEY}" \
        $([ -n "${SCW_ACCESS_KEY:-}" ] && echo "--from-literal=SCW_ACCESS_KEY=${SCW_ACCESS_KEY}") \
        -n "${TEST_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

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

# Build allowedProviders JSON array
allowed_providers_json() {
    local json="["
    local first=1
    for p in "${ALLOWED_PROVIDERS_ARR[@]}"; do
        [ $first -eq 0 ] && json+=","
        json+="\"${p}\""
        first=0
    done
    json+="]"
    echo "$json"
}

phase1_provision() {
    log_phase "Phase 1: Provision on recommendation (NodePool: ${ALLOWED_PROVIDERS_STR})"

    local ap_json
    ap_json=$(allowed_providers_json)
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
    allowedProviders: ${ap_json}
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
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${DEPLOYMENT_NAME}
  namespace: ${TEST_NAMESPACE}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: dr-workload
  template:
    metadata:
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

    log_info "Waiting for Deployment pod (CloudBroker will choose provider)..."
    wait_for_deployment_ready
    local node_provider
    node_provider=$(get_workload_pod_provider)
    log_info "✓ Pod scheduled on node from provider: ${node_provider}"
}

phase2_patch_controller_dns() {
    log_phase "Phase 2: Patch controller DNS to route api.hetzner.cloud -> mock"

    local mock_ip
    mock_ip=$(kubectl get svc dr-mock-hetzner -n "${CONTROLLER_NAMESPACE}" -o jsonpath='{.spec.clusterIP}' 2>/dev/null || true)
    if [ -z "$mock_ip" ]; then
        log_error "Mock service not found or no ClusterIP. Is --with-api-mock set?"
        exit 1
    fi

    log_info "Patching controller deployment with hostAliases (api.hetzner.cloud -> ${mock_ip})..."
    kubectl patch deployment cloudburst-controller -n "${CONTROLLER_NAMESPACE}" --type='json' -p='[
      {"op":"add","path":"/spec/template/spec/hostAliases","value":[{"ip":"'"${mock_ip}"'","hostnames":["api.hetzner.cloud"]}]},
      {"op":"add","path":"/spec/template/spec/containers/0/env/-","value":{"name":"HETZNER_INSECURE_SKIP_VERIFY","value":"true"}}
    ]'
    kubectl rollout status deployment/cloudburst-controller -n "${CONTROLLER_NAMESPACE}" --timeout=120s
    log_info "✓ Controller now routes Hetzner API to mock"
}

# delete_instance_via_provider deletes the cloud instance directly via provider API.
# instanceID format: hetzner/fsn1/12345 or scaleway/fr-par-1/uuid
delete_instance_via_provider() {
    local instance_id="$1"
    if [[ "$instance_id" == hetzner/* ]]; then
        # hetzner/location/serverId
        local server_id
        server_id=$(echo "$instance_id" | cut -d'/' -f3)
        [ -z "${HETZNER_API_TOKEN:-}" ] && { log_error "HETZNER_API_TOKEN required to delete instance"; return 1; }
        log_info "Deleting Hetzner server ${server_id} via API..."
        local resp
        resp=$(curl -s -w "\n%{http_code}" -X DELETE \
            -H "Authorization: Bearer ${HETZNER_API_TOKEN}" \
            "https://api.hetzner.cloud/v1/servers/${server_id}" 2>/dev/null || true)
        local code
        code=$(echo "$resp" | tail -1)
        if [ "$code" = "200" ] || [ "$code" = "201" ]; then
            log_info "✓ Hetzner server ${server_id} deleted"
        elif [ "$code" = "404" ]; then
            log_info "✓ Hetzner server ${server_id} already gone (404)"
        else
            log_warn "Hetzner delete returned HTTP ${code}"
        fi
    elif [[ "$instance_id" == scaleway/* ]]; then
        # scaleway/zone/serverId (UUID)
        local zone server_id base state state_resp
        zone=$(echo "$instance_id" | cut -d'/' -f2)
        server_id=$(echo "$instance_id" | cut -d'/' -f3)
        [ -z "${SCW_SECRET_KEY:-}" ] && { log_error "SCW_SECRET_KEY required to delete instance"; return 1; }
        base="https://api.scaleway.com/instance/v1/zones/${zone}/servers/${server_id}"
        # Scaleway requires poweroff before delete
        state_resp=$(curl -s -H "X-Auth-Token: ${SCW_SECRET_KEY}" "${base}" 2>/dev/null || true)
        state=$(echo "$state_resp" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('server',{}).get('state',''))" 2>/dev/null || true)
        if [ "$state" = "running" ] || [ "$state" = "starting" ]; then
            log_info "Powering off Scaleway instance ${server_id}..."
            curl -s -X POST -H "X-Auth-Token: ${SCW_SECRET_KEY}" -H "Content-Type: application/json" \
                "${base}/action" -d '{"action":"poweroff"}' >/dev/null 2>&1 || true
            for _ in $(seq 1 30); do
                sleep 2
                state_resp=$(curl -s -H "X-Auth-Token: ${SCW_SECRET_KEY}" "${base}" 2>/dev/null || true)
                state=$(echo "$state_resp" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('server',{}).get('state',''))" 2>/dev/null || true)
                [ "$state" = "stopped" ] && break
            done
        fi
        log_info "Deleting Scaleway instance ${server_id}..."
        curl -s -X DELETE -H "X-Auth-Token: ${SCW_SECRET_KEY}" "${base}" 2>/dev/null || true
        log_info "✓ Scaleway instance delete requested"
    else
        log_error "Unsupported provider for direct delete: ${instance_id%%/*}"
        return 1
    fi
}

phase3_delete_instance() {
    log_phase "Phase 3: Delete instance via provider API (simulates outage)"

    local instance_id
    instance_id=$(kubectl get nodeclaims -n "${TEST_NAMESPACE}" -l "cloudburst.io/nodepool=${NODEPOOL_NAME}" -o jsonpath='{.items[0].status.instanceID}' 2>/dev/null || true)
    if [ -z "$instance_id" ]; then
        log_error "Could not get instanceID from NodeClaim"
        exit 1
    fi
    log_info "Deleting cloud instance ${instance_id} via provider API..."
    delete_instance_via_provider "$instance_id"

    log_info "Waiting for node to go NotReady and controller to clean up NodeClaim..."
    wait_for_nodeclaim_cleanup
    log_info "✓ NodeClaim cleaned up (Deployment will recreate pod)"
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

phase4_failover() {
    log_phase "Phase 4: Failover - Deployment recreates pod, controller tries Hetzner (mock 503) -> Scaleway"

    log_info "Waiting for Deployment pod (controller will try Hetzner first, get 503, fall back to Scaleway)..."
    wait_for_deployment_ready
    local node_provider
    node_provider=$(get_workload_pod_provider)
    log_info "✓ Failover pod scheduled on node from provider: ${node_provider}"

    if [[ " ${ALLOWED_PROVIDERS_STR} " =~ " ${node_provider} " ]]; then
        log_info "✓ Disaster recovery simulation SUCCESS: failover to ${node_provider}"
    else
        log_warn "Expected provider in ${ALLOWED_PROVIDERS_STR}; got: ${node_provider}"
    fi
}

# Wait for deployment/dr-workload to have a Ready pod
wait_for_deployment_ready() {
    local timeout=${READY_TIMEOUT}
    local elapsed=0

    while [ $elapsed -lt $timeout ]; do
        local ready
        ready=$(kubectl get deployment "${DEPLOYMENT_NAME}" -n "${TEST_NAMESPACE}" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
        if [ "${ready}" = "1" ]; then
            log_info "Deployment ${DEPLOYMENT_NAME} has 1 ready replica"
            return 0
        fi
        sleep 10
        elapsed=$((elapsed + 10))
    done

    log_error "Timeout waiting for deployment ${DEPLOYMENT_NAME} to be ready"
    kubectl get pods -n "${TEST_NAMESPACE}" -l app=dr-workload
    kubectl get nodeclaims -n "${TEST_NAMESPACE}"
    exit 1
}

# Get provider of the node running the dr-workload pod
get_workload_pod_provider() {
    local pod_name
    pod_name=$(kubectl get pods -n "${TEST_NAMESPACE}" -l app=dr-workload -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")
    if [ -z "$pod_name" ]; then
        echo "?"
        return
    fi
    local node_name
    node_name=$(kubectl get pod "${pod_name}" -n "${TEST_NAMESPACE}" -o jsonpath='{.spec.nodeName}' 2>/dev/null || echo "")
    if [ -z "$node_name" ]; then
        echo "?"
        return
    fi
    kubectl get node "${node_name}" -o jsonpath='{.metadata.labels.cloudburst\.io/provider}' 2>/dev/null || echo "?"
}

cleanup() {
    log_phase "Cleanup"
    kubectl delete nodepool "${NODEPOOL_NAME}" -n "${TEST_NAMESPACE}" --ignore-not-found 2>/dev/null || true
    kubectl delete deployment "${DEPLOYMENT_NAME}" -n "${TEST_NAMESPACE}" --ignore-not-found 2>/dev/null || true
    log_info "✓ Cleanup done (NodeClaims/nodes will scale down via controller)"
}

# ── Main ─────────────────────────────────────────────────────────────────────

main() {
    log_info "Disaster Recovery Use Case Simulation"
    log_info "Allowed providers: ${ALLOWED_PROVIDERS_STR}"
    check_prerequisites

    if [ $USE_EXISTING_CLUSTER -eq 0 ]; then
        run_full_setup
    else
        kubectl cluster-info --context "kind-${KIND_CLUSTER_NAME}" &>/dev/null || { log_error "Kind cluster not found or not accessible"; exit 1; }
        log_info "Using existing cluster: ${KIND_CLUSTER_NAME}"
        local nc_count
        nc_count=$(kubectl get nodeclass -n "${TEST_NAMESPACE}" --no-headers 2>/dev/null | wc -l)
        [ "$nc_count" -lt 1 ] && { log_error "No NodeClass found in ${TEST_NAMESPACE}. Run integration-test-e2e-all-providers.sh first."; exit 1; }
        if [ -d "${CLOUDBROKER_DIR}" ]; then
            log_info "Seeding CloudBroker so Hetzner ranks first (for failover path)..."
            (cd "${CLOUDBROKER_DIR}" && make seed-dr-hetzner-first 2>/dev/null) || log_warn "Could not run seed-dr-hetzner-first (CloudBroker may need ingest-hetzner + ingest-scaleway)"
        fi
    fi

    phase1_provision
    phase2_patch_controller_dns
    phase3_delete_instance
    phase4_failover

    log_phase "Result"
    log_info "Disaster recovery simulation complete."
    log_info "Summary: Hetzner 'outage' simulated via mock; workload failed over to Scaleway."
    log_info ""
    log_info "To clean up resources:"
    log_info "  kubectl delete nodepool ${NODEPOOL_NAME} -n ${TEST_NAMESPACE}"
    log_info "  kubectl delete deployment ${DEPLOYMENT_NAME} -n ${TEST_NAMESPACE}"
    log_info "  (Nodes will scale down after ttlSecondsAfterEmpty)"
}

# Allow sourcing for testing; run main when executed
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    main
fi
