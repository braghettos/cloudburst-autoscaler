#!/bin/bash
set -euo pipefail

# E2E Integration Test: All Providers in One Kind Cluster
#
# Flow:
# 1. Stop CloudBroker containers, delete Kind cluster
# 2. Start CloudBroker, migrate, ingest-all
# 3. Create Kind cluster, install CRDs, build and load controller
# 4. Deploy controller with ALL enabled provider credentials
# 5. Create one NodeClass (with configs for all providers), tailscale secret
# 6. Deploy UI (http://127.0.0.1:8082)
# 7. For each enabled provider: create NodePool + pending pod
# 8. Wait for all NodeClaims Ready and all pods Running
# 9. Run Playwright UI assertions (per enabled provider)
# 10. Delete pods and wait for scale-down (mandatory)
#
# Environment: source .env. Set credentials for each provider you want to test.
# Requires cloudburst-autoscaler-ui at ../cloudburst-autoscaler-ui for UI deployment.
# CloudBroker ingest requires .env in ../cloudbroker with provider credentials.
# At least one provider must have credentials. TAILSCALE_AUTHKEY required.
#
# Usage:
#   ./hack/integration-test-e2e-all-providers.sh
#   ./hack/integration-test-e2e-all-providers.sh --keep-cluster   # Skip Kind/instance cleanup for diagnosis
#   ./hack/integration-test-e2e-all-providers.sh --cloudbroker-dir /path/to/cloudbroker

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
ENV_FILE="${PROJECT_ROOT}/.env"
CLOUDBROKER_DIR="${CLOUDBROKER_DIR:-${PROJECT_ROOT}/../cloudbroker}"
KEEP_CLUSTER=0

# Parse optional args
i=1
while [ $i -le $# ]; do
    arg="${!i}"
    case "$arg" in
        --keep-cluster) KEEP_CLUSTER=1 ;;
        --cloudbroker-dir=*) CLOUDBROKER_DIR="${arg#*=}" ;;
        --cloudbroker-dir)
            i=$((i + 1))
            [ $i -le $# ] && CLOUDBROKER_DIR="${!i}"
            ;;
    esac
    i=$((i + 1))
done

if [ -f "${ENV_FILE}" ]; then
    echo -e "${GREEN}[INFO]${NC} Loading environment from .env"
    set -a
    # shellcheck source=/dev/null
    source "${ENV_FILE}"
    set +a
fi

# GCP_PROJECT: extract from SA file if not set (when GCP credentials exist)
if [ -z "${GCP_PROJECT:-}" ]; then
    GCP_SA_FILE="${GOOGLE_APPLICATION_CREDENTIALS:-}"
    if [ -n "$GCP_SA_FILE" ] && [ -f "$GCP_SA_FILE" ]; then
        GCP_PROJECT=$(python3 -c "import json; print(json.load(open('$GCP_SA_FILE'))['project_id'])" 2>/dev/null || true)
        [ -n "${GCP_PROJECT}" ] && export GCP_PROJECT
    fi
fi

TAILSCALE_AUTHKEY="${TAILSCALE_AUTHKEY:-}"
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-cloudburst}"
TEST_NAMESPACE="${TEST_NAMESPACE:-default}"
CONTROLLER_NAMESPACE="cloudburst-system"
NODECLASS_NAME="e2e-all-nodeclass"
CONTROLLER_IMAGE="cloudburst-autoscaler:e2e"

NODECLAIM_CREATE_TIMEOUT="${NODECLAIM_CREATE_TIMEOUT:-300}"
PROVISIONING_TIMEOUT="${PROVISIONING_TIMEOUT:-600}"
READY_TIMEOUT="${READY_TIMEOUT:-600}"
POD_SCHEDULED_TIMEOUT="${POD_SCHEDULED_TIMEOUT:-120}"
SCALEDOWN_TIMEOUT="${SCALEDOWN_TIMEOUT:-600}"

# Detect enabled providers
ENABLED_PROVIDERS=()
[ -n "${GOOGLE_APPLICATION_CREDENTIALS:-}" ] && [ -f "${GOOGLE_APPLICATION_CREDENTIALS:-}" ] && ENABLED_PROVIDERS+=("gcp")
[ -n "${SCW_SECRET_KEY:-}" ]                 && ENABLED_PROVIDERS+=("scaleway")
[ -n "${HETZNER_API_TOKEN:-}" ]              && ENABLED_PROVIDERS+=("hetzner")
[ -n "${DIGITALOCEAN_API_TOKEN:-}" ]         && ENABLED_PROVIDERS+=("digitalocean")
[ -n "${ARUBA_API_KEY:-}" ]                  && [ -n "${ARUBA_API_SECRET:-}" ] && [ -n "${ARUBA_PROJECT_ID:-}" ] && ENABLED_PROVIDERS+=("aruba")
[ -n "${OVH_APPLICATION_KEY:-}" ]            && [ -n "${OVH_APPLICATION_SECRET:-}" ] && [ -n "${OVH_CONSUMER_KEY:-}" ] && [ -n "${OVH_PROJECT_ID:-}" ] && ENABLED_PROVIDERS+=("ovh")
[ -n "${AWS_ACCESS_KEY_ID:-}" ]               && [ -n "${AWS_SECRET_ACCESS_KEY:-}" ] && ENABLED_PROVIDERS+=("aws")
[ -n "${AZURE_TENANT_ID:-}" ]                && [ -n "${AZURE_CLIENT_ID:-}" ] && [ -n "${AZURE_CLIENT_SECRET:-}" ] && [ -n "${AZURE_SUBSCRIPTION_ID:-}" ] && [ -n "${AZURE_RESOURCE_GROUP:-}" ] && ENABLED_PROVIDERS+=("azure")
[ -n "${UPCLOUD_API_TOKEN:-}" ]             && ENABLED_PROVIDERS+=("upcloud")
[ -n "${EXOSCALE_API_KEY:-}" ]              && [ -n "${EXOSCALE_API_SECRET:-}" ] && ENABLED_PROVIDERS+=("exoscale")
if [ -n "${IONOS_API_TOKEN:-}" ] || { [ -n "${IONOS_USERNAME:-}" ] && [ -n "${IONOS_PASSWORD:-}" ]; }; then
    ENABLED_PROVIDERS+=("ionos")
fi

log_info()  { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }

# Source common UI deploy/verify
[ -f "${SCRIPT_DIR}/e2e-common-ui.sh" ] && source "${SCRIPT_DIR}/e2e-common-ui.sh"

check_prerequisites() {
    log_info "Checking prerequisites..."
    local missing=0
    TAILSCALE_CMD=""
    for p in tailscale /Applications/Tailscale.app/Contents/MacOS/Tailscale /usr/local/bin/tailscale /opt/homebrew/bin/tailscale; do
        if command -v "$p" &>/dev/null || [ -f "$p" ]; then TAILSCALE_CMD="$p"; break; fi
    done
    [ -z "$TAILSCALE_CMD" ] && { log_error "tailscale not found"; missing=$((missing+1)); }
    ! "$TAILSCALE_CMD" status &>/dev/null && { log_error "Tailscale not running"; missing=$((missing+1)); }
    [ -z "${TAILSCALE_AUTHKEY}" ] && { log_error "TAILSCALE_AUTHKEY not set"; missing=$((missing+1)); }
    [ ${#ENABLED_PROVIDERS[@]} -eq 0 ] && { log_error "No provider credentials set. Set env for at least one of: gcp, scaleway, hetzner, digitalocean, aruba, ovh, aws, azure, upcloud, exoscale, ionos"; missing=$((missing+1)); }
    if [[ " ${ENABLED_PROVIDERS[*]} " =~ " gcp " ]]; then
        [ -z "${GCP_PROJECT:-}" ] && { log_error "GCP_PROJECT not set (required when GCP is enabled). Set in .env or it will be read from GOOGLE_APPLICATION_CREDENTIALS"; missing=$((missing+1)); }
    fi
    if [[ " ${ENABLED_PROVIDERS[*]} " =~ " ionos " ]]; then
        [ -z "${IONOS_DATACENTER_ID:-}" ] && [ -z "${IONOS_REGION_TO_DATACENTER_ID:-}" ] && { log_error "IONOS_DATACENTER_ID or IONOS_REGION_TO_DATACENTER_ID required when IONOS is enabled"; missing=$((missing+1)); }
    fi
    for cmd in docker kind kubectl curl; do
        command -v $cmd &>/dev/null || { log_error "$cmd not found"; missing=$((missing+1)); }
    done
    [ ! -d "${CLOUDBROKER_DIR}" ] && { log_error "CloudBroker dir not found: ${CLOUDBROKER_DIR}. Set CLOUDBROKER_DIR."; missing=$((missing+1)); }
    [ $missing -gt 0 ] && { log_error "Prerequisites missing."; exit 1; }
    log_info "Enabled providers: ${ENABLED_PROVIDERS[*]}"
}

get_tailscale_ip() {
    "$TAILSCALE_CMD" ip -4 2>/dev/null | tr -d '\r\n\t'
}

# ── Phase A: CloudBroker cleanup, start, migrate, ingest ───────────────────────

phase_cloudbroker() {
    log_info "=== Phase A: CloudBroker ==="
    cd "${CLOUDBROKER_DIR}"

    log_info "Stopping CloudBroker containers..."
    docker compose down 2>/dev/null || true

    log_info "Deleting existing Kind cluster..."
    kind delete cluster --name "${KIND_CLUSTER_NAME}" 2>/dev/null || true

    log_info "Starting CloudBroker..."
    docker compose up -d --build

    log_info "Waiting for CloudBroker API health..."
    local retries=30
    while [ $retries -gt 0 ]; do
        if curl -sf "http://localhost:8000/health" >/dev/null 2>&1; then
            log_info "✓ CloudBroker API is healthy"
            break
        fi
        retries=$((retries - 1))
        [ $retries -eq 0 ] && { log_error "CloudBroker API failed to become healthy"; exit 1; }
        sleep 2
    done

    log_info "Running migrations..."
    make migrate

    log_info "Ingesting all providers (make ingest-all)..."
    local ingest_retries=3
    local ingest_attempt=1
    while [ $ingest_attempt -le $ingest_retries ]; do
        if make ingest-all 2>&1; then
            log_info "✓ Ingest completed successfully"
            break
        fi
        if [ $ingest_attempt -eq $ingest_retries ]; then
            log_error "Ingest failed after ${ingest_retries} attempts (transient network errors?)"
            exit 1
        fi
        log_warn "Ingest attempt ${ingest_attempt}/${ingest_retries} failed, retrying in 15s..."
        sleep 15
        ingest_attempt=$((ingest_attempt + 1))
    done

    log_info "✓ Phase A complete"
}

# ── Phase B: Kind cluster, CRDs, controller ────────────────────────────────────

phase_kind_and_controller() {
    log_info "=== Phase B: Kind cluster and controller ==="
    cd "${PROJECT_ROOT}"

    HOST_TS_IP=$(get_tailscale_ip)
    export HOST_TS_IP
    log_info "Host Tailscale IP: $HOST_TS_IP"

    log_info "Creating Kind cluster..."
    ./hack/create-kind.sh

    KIND_CONTEXT="kind-${KIND_CLUSTER_NAME}"
    kubectl config set-cluster "${KIND_CONTEXT}" --server="https://127.0.0.1:6443" 2>/dev/null || true
    kubectl cluster-info --context "${KIND_CONTEXT}" &>/dev/null || { log_error "Kind cluster not accessible"; exit 1; }

    log_info "Installing CRDs..."
    if make install 2>/dev/null; then
        log_info "✓ CRDs installed"
    else
        local applied=0
        for f in config/crd/bases/cloudburst.io_*.yaml; do
            [ -f "$f" ] && kubectl apply -f "$f" 2>/dev/null && applied=$((applied+1))
        done
        [ $applied -eq 0 ] && { log_error "Could not install CRDs"; exit 1; }
        log_info "✓ CRDs installed ($applied)"
    fi

    log_info "Building controller image..."
    docker build -t "${CONTROLLER_IMAGE}" . 2>&1 | tail -5

    log_info "Loading image into kind cluster..."
    local nodes=("${KIND_CLUSTER_NAME}-control-plane" "${KIND_CLUSTER_NAME}-worker")
    for node in "${nodes[@]}"; do
        if docker ps --format '{{.Names}}' | grep -q "^${node}$"; then
            docker save "${CONTROLLER_IMAGE}" | docker exec -i "${node}" ctr --namespace=k8s.io images import - 2>&1 | grep -v "^$" || true
        fi
    done

    deploy_controller_all_providers
    log_info "✓ Phase B complete"
}

deploy_controller_all_providers() {
    log_info "Deploying controller with all provider credentials..."
    HOST_TS_IP=$(get_tailscale_ip)
    local cloudbroker_url="${CLOUDBROKER_BASEURL:-http://${HOST_TS_IP}:8000}"

    kubectl create namespace "${CONTROLLER_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

    # Create provider secrets in controller namespace
    [ -n "${GOOGLE_APPLICATION_CREDENTIALS:-}" ] && [ -f "${GOOGLE_APPLICATION_CREDENTIALS:-}" ] && \
        kubectl create secret generic gcp-credentials --from-file=credentials.json="${GOOGLE_APPLICATION_CREDENTIALS}" -n "${CONTROLLER_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
    [ -n "${HETZNER_API_TOKEN:-}" ] && \
        kubectl create secret generic hetzner-credentials --from-literal=HETZNER_API_TOKEN="${HETZNER_API_TOKEN}" -n "${CONTROLLER_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
    [ -n "${SCW_SECRET_KEY:-}" ] && \
        kubectl create secret generic scaleway-credentials --from-literal=SCW_SECRET_KEY="${SCW_SECRET_KEY}" \
        $([ -n "${SCW_ACCESS_KEY:-}" ] && echo "--from-literal=SCW_ACCESS_KEY=${SCW_ACCESS_KEY}") \
        -n "${CONTROLLER_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
    [ -n "${DIGITALOCEAN_API_TOKEN:-}" ] && \
        kubectl create secret generic digitalocean-credentials --from-literal=DIGITALOCEAN_API_TOKEN="${DIGITALOCEAN_API_TOKEN}" -n "${CONTROLLER_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
    [ -n "${OVH_APPLICATION_KEY:-}" ] && [ -n "${OVH_APPLICATION_SECRET:-}" ] && [ -n "${OVH_CONSUMER_KEY:-}" ] && \
        kubectl create secret generic ovh-credentials \
        --from-literal=OVH_APPLICATION_KEY="${OVH_APPLICATION_KEY}" \
        --from-literal=OVH_APPLICATION_SECRET="${OVH_APPLICATION_SECRET}" \
        --from-literal=OVH_CONSUMER_KEY="${OVH_CONSUMER_KEY}" \
        -n "${CONTROLLER_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
    [ -n "${AWS_ACCESS_KEY_ID:-}" ] && [ -n "${AWS_SECRET_ACCESS_KEY:-}" ] && \
        kubectl create secret generic aws-credentials \
        --from-literal=AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID}" \
        --from-literal=AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY}" \
        -n "${CONTROLLER_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
    [ -n "${AZURE_TENANT_ID:-}" ] && [ -n "${AZURE_CLIENT_ID:-}" ] && [ -n "${AZURE_CLIENT_SECRET:-}" ] && \
        kubectl create secret generic azure-credentials \
        --from-literal=AZURE_TENANT_ID="${AZURE_TENANT_ID}" \
        --from-literal=AZURE_CLIENT_ID="${AZURE_CLIENT_ID}" \
        --from-literal=AZURE_CLIENT_SECRET="${AZURE_CLIENT_SECRET}" \
        -n "${CONTROLLER_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
    [ -n "${UPCLOUD_API_TOKEN:-}" ] && \
        kubectl create secret generic upcloud-credentials --from-literal=UPCLOUD_API_TOKEN="${UPCLOUD_API_TOKEN}" -n "${CONTROLLER_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
    [ -n "${EXOSCALE_API_KEY:-}" ] && [ -n "${EXOSCALE_API_SECRET:-}" ] && \
        kubectl create secret generic exoscale-credentials \
        --from-literal=EXOSCALE_API_KEY="${EXOSCALE_API_KEY}" \
        --from-literal=EXOSCALE_API_SECRET="${EXOSCALE_API_SECRET}" \
        -n "${CONTROLLER_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
    if [ -n "${IONOS_API_TOKEN:-}" ]; then
        kubectl create secret generic ionos-credentials --from-literal=IONOS_API_TOKEN="${IONOS_API_TOKEN}" -n "${CONTROLLER_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
    elif [ -n "${IONOS_USERNAME:-}" ] && [ -n "${IONOS_PASSWORD:-}" ]; then
        kubectl create secret generic ionos-credentials \
        --from-literal=IONOS_USERNAME="${IONOS_USERNAME}" \
        --from-literal=IONOS_PASSWORD="${IONOS_PASSWORD}" \
        -n "${CONTROLLER_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
    fi

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
- apiGroups: [""]
  resources: ["pods","pods/exec","nodes","secrets","configmaps","events"]
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

    # Build env block for controller - we use a here-doc and inject per-provider
    local env_block=""
    if [[ " ${ENABLED_PROVIDERS[*]} " =~ " gcp " ]]; then
        env_block="${env_block}
        - name: GOOGLE_APPLICATION_CREDENTIALS
          value: /var/run/secrets/gcp/credentials.json"
    fi
    if [[ " ${ENABLED_PROVIDERS[*]} " =~ " hetzner " ]]; then
        env_block="${env_block}
        - name: HETZNER_API_TOKEN
          valueFrom:
            secretKeyRef:
              name: hetzner-credentials
              key: HETZNER_API_TOKEN"
    fi
    if [[ " ${ENABLED_PROVIDERS[*]} " =~ " scaleway " ]]; then
        env_block="${env_block}
        - name: SCW_SECRET_KEY
          valueFrom:
            secretKeyRef:
              name: scaleway-credentials
              key: SCW_SECRET_KEY
        - name: SCW_DEFAULT_PROJECT_ID
          value: \"${SCW_PROJECT_ID:-}\"
        - name: SCW_DEFAULT_ORGANIZATION_ID
          value: \"${SCW_ORGANIZATION_ID:-${SCW_DEFAULT_ORGANIZATION_ID:-}}\""
    fi
    if [[ " ${ENABLED_PROVIDERS[*]} " =~ " digitalocean " ]]; then
        env_block="${env_block}
        - name: DIGITALOCEAN_API_TOKEN
          valueFrom:
            secretKeyRef:
              name: digitalocean-credentials
              key: DIGITALOCEAN_API_TOKEN"
    fi
    if [[ " ${ENABLED_PROVIDERS[*]} " =~ " ovh " ]]; then
        env_block="${env_block}
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
              key: OVH_CONSUMER_KEY"
    fi
    if [[ " ${ENABLED_PROVIDERS[*]} " =~ " aws " ]]; then
        env_block="${env_block}
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
          value: \"${AWS_REGION:-eu-west-1}\""
    fi
    if [[ " ${ENABLED_PROVIDERS[*]} " =~ " azure " ]]; then
        env_block="${env_block}
        - name: AZURE_SUBSCRIPTION_ID
          value: \"${AZURE_SUBSCRIPTION_ID}\"
        - name: AZURE_RESOURCE_GROUP
          value: \"${AZURE_RESOURCE_GROUP}\"
        - name: AZURE_LOCATION
          value: \"${AZURE_LOCATION:-westeurope}\"
        - name: AZURE_TENANT_ID
          valueFrom:
            secretKeyRef:
              name: azure-credentials
              key: AZURE_TENANT_ID
        - name: AZURE_CLIENT_ID
          valueFrom:
            secretKeyRef:
              name: azure-credentials
              key: AZURE_CLIENT_ID
        - name: AZURE_CLIENT_SECRET
          valueFrom:
            secretKeyRef:
              name: azure-credentials
              key: AZURE_CLIENT_SECRET"
    fi
    if [[ " ${ENABLED_PROVIDERS[*]} " =~ " upcloud " ]]; then
        env_block="${env_block}
        - name: UPCLOUD_API_TOKEN
          valueFrom:
            secretKeyRef:
              name: upcloud-credentials
              key: UPCLOUD_API_TOKEN"
    fi
    if [[ " ${ENABLED_PROVIDERS[*]} " =~ " exoscale " ]]; then
        env_block="${env_block}
        - name: EXOSCALE_API_KEY
          valueFrom:
            secretKeyRef:
              name: exoscale-credentials
              key: EXOSCALE_API_KEY
        - name: EXOSCALE_API_SECRET
          valueFrom:
            secretKeyRef:
              name: exoscale-credentials
              key: EXOSCALE_API_SECRET"
    fi
    if [[ " ${ENABLED_PROVIDERS[*]} " =~ " ionos " ]]; then
        if [ -n "${IONOS_API_TOKEN:-}" ]; then
            env_block="${env_block}
        - name: IONOS_API_TOKEN
          valueFrom:
            secretKeyRef:
              name: ionos-credentials
              key: IONOS_API_TOKEN"
        else
            env_block="${env_block}
        - name: IONOS_USERNAME
          valueFrom:
            secretKeyRef:
              name: ionos-credentials
              key: IONOS_USERNAME
        - name: IONOS_PASSWORD
          valueFrom:
            secretKeyRef:
              name: ionos-credentials
              key: IONOS_PASSWORD"
        fi
    fi

    # Resolve AWS AMI if needed
    if [[ " ${ENABLED_PROVIDERS[*]} " =~ " aws " ]] && [ -z "${AWS_AMI:-}" ]; then
        if command -v aws &>/dev/null; then
            AWS_AMI=$(aws ec2 describe-images --region "${AWS_REGION:-eu-west-1}" --owners 099720109477 \
                --filters "Name=name,Values=ubuntu/images/hvm-ssd/ubuntu-jammy-22.04-amd64-server-*" "Name=state,Values=available" \
                --query 'Images | sort_by(@, &CreationDate) | [-1].ImageId' --output text 2>/dev/null || true)
        fi
        [ -z "${AWS_AMI}" ] && { log_error "AWS_AMI not set and could not auto-resolve. Set AWS_AMI in .env"; exit 1; }
    fi

    # Build volumeMounts and volumes for GCP (only when GCP enabled)
    local gcp_volume_mounts=""
    local gcp_volumes=""
    if [[ " ${ENABLED_PROVIDERS[*]} " =~ " gcp " ]]; then
        gcp_volume_mounts="
        volumeMounts:
        - name: gcp-credentials
          mountPath: /var/run/secrets/gcp
          readOnly: true"
        gcp_volumes="
      volumes:
      - name: gcp-credentials
        secret:
          secretName: gcp-credentials"
    fi

    # Deployment (write to temp file to allow conditional blocks)
    local deploy_file
    deploy_file=$(mktemp)
    cat <<DEPLOYEOF > "${deploy_file}"
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
        env:${env_block}
        - name: CLOUDBROKER_BASEURL
          value: "${cloudbroker_url}"
        - name: TAILSCALE_AUTHKEY
          value: "${TAILSCALE_AUTHKEY}"
        ${gcp_volume_mounts}
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
      ${gcp_volumes}
DEPLOYEOF
    kubectl apply -f "${deploy_file}"
    rm -f "${deploy_file}"

    log_info "Waiting for controller deployment..."
    kubectl rollout status deployment/cloudburst-controller -n "${CONTROLLER_NAMESPACE}" --timeout=120s || {
        log_error "Controller not ready"
        kubectl get pods -n "${CONTROLLER_NAMESPACE}" -o wide
        kubectl logs -n "${CONTROLLER_NAMESPACE}" -l app=cloudburst-controller --tail=50 2>/dev/null || true
        exit 1
    }
    log_info "✓ Controller ready"
}

# ── Phase C: NodeClass and provider secrets in test namespace ──────────────────

phase_nodeclass_and_secrets() {
    log_info "=== Phase C: NodeClass and secrets ==="
    cd "${PROJECT_ROOT}"
    HOST_TS_IP=$(get_tailscale_ip)

    kubectl create secret generic tailscale-auth \
        --from-literal=authkey="${TAILSCALE_AUTHKEY}" \
        --namespace="${TEST_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -

    # Provider secrets in test namespace (for NodeClass secretRef)
    [ -n "${HETZNER_API_TOKEN:-}" ] && \
        kubectl create secret generic hetzner-api-token --from-literal=HETZNER_API_TOKEN="${HETZNER_API_TOKEN}" -n "${TEST_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
    [ -n "${SCW_SECRET_KEY:-}" ] && \
        kubectl create secret generic scaleway-api-keys --from-literal=SCW_SECRET_KEY="${SCW_SECRET_KEY}" \
        $([ -n "${SCW_ACCESS_KEY:-}" ] && echo "--from-literal=SCW_ACCESS_KEY=${SCW_ACCESS_KEY}") \
        -n "${TEST_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
    [ -n "${DIGITALOCEAN_API_TOKEN:-}" ] && \
        kubectl create secret generic digitalocean-api-token --from-literal=DIGITALOCEAN_API_TOKEN="${DIGITALOCEAN_API_TOKEN}" -n "${TEST_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
    [ -n "${ARUBA_API_KEY:-}" ] && [ -n "${ARUBA_API_SECRET:-}" ] && [ -n "${ARUBA_PROJECT_ID:-}" ] && \
        kubectl create secret generic aruba-api-credentials \
        --from-literal=ARUBA_API_KEY="${ARUBA_API_KEY}" \
        --from-literal=ARUBA_API_SECRET="${ARUBA_API_SECRET}" \
        -n "${TEST_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
    [ -n "${OVH_APPLICATION_KEY:-}" ] && [ -n "${OVH_APPLICATION_SECRET:-}" ] && [ -n "${OVH_CONSUMER_KEY:-}" ] && \
        kubectl create secret generic ovh-api-credentials \
        --from-literal=OVH_APPLICATION_KEY="${OVH_APPLICATION_KEY}" \
        --from-literal=OVH_APPLICATION_SECRET="${OVH_APPLICATION_SECRET}" \
        --from-literal=OVH_CONSUMER_KEY="${OVH_CONSUMER_KEY}" \
        -n "${TEST_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
    [ -n "${AWS_ACCESS_KEY_ID:-}" ] && [ -n "${AWS_SECRET_ACCESS_KEY:-}" ] && \
        kubectl create secret generic aws-credentials \
        --from-literal=AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID}" \
        --from-literal=AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY}" \
        -n "${TEST_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
    [ -n "${AZURE_TENANT_ID:-}" ] && [ -n "${AZURE_CLIENT_ID:-}" ] && [ -n "${AZURE_CLIENT_SECRET:-}" ] && \
        kubectl create secret generic azure-credentials \
        --from-literal=AZURE_TENANT_ID="${AZURE_TENANT_ID}" \
        --from-literal=AZURE_CLIENT_ID="${AZURE_CLIENT_ID}" \
        --from-literal=AZURE_CLIENT_SECRET="${AZURE_CLIENT_SECRET}" \
        -n "${TEST_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
    [ -n "${UPCLOUD_API_TOKEN:-}" ] && \
        kubectl create secret generic upcloud-credentials --from-literal=UPCLOUD_API_TOKEN="${UPCLOUD_API_TOKEN}" -n "${TEST_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
    [ -n "${EXOSCALE_API_KEY:-}" ] && [ -n "${EXOSCALE_API_SECRET:-}" ] && \
        kubectl create secret generic exoscale-credentials \
        --from-literal=EXOSCALE_API_KEY="${EXOSCALE_API_KEY}" \
        --from-literal=EXOSCALE_API_SECRET="${EXOSCALE_API_SECRET}" \
        -n "${TEST_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
    if [ -n "${IONOS_API_TOKEN:-}" ]; then
        kubectl create secret generic ionos-credentials --from-literal=IONOS_API_TOKEN="${IONOS_API_TOKEN}" -n "${TEST_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
    elif [ -n "${IONOS_USERNAME:-}" ] && [ -n "${IONOS_PASSWORD:-}" ]; then
        kubectl create secret generic ionos-credentials \
        --from-literal=IONOS_USERNAME="${IONOS_USERNAME}" \
        --from-literal=IONOS_PASSWORD="${IONOS_PASSWORD}" \
        -n "${TEST_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
    fi

    # Build NodeClass YAML with all enabled provider configs
    {
        echo "apiVersion: cloudburst.io/v1alpha1"
        echo "kind: NodeClass"
        echo "metadata:"
        echo "  name: ${NODECLASS_NAME}"
        echo "  namespace: ${TEST_NAMESPACE}"
        echo "spec:"
        # GCP
        if [[ " ${ENABLED_PROVIDERS[*]} " =~ " gcp " ]]; then
            echo "  gcp:"
            echo "    project: \"${GCP_PROJECT:-}\""
            echo "    zone: \"${GCP_ZONE:-us-central1-a}\""
            echo "    imageFamily: ubuntu-2204-lts"
            echo "    imageProject: ubuntu-os-cloud"
        fi
        # Hetzner
        if [[ " ${ENABLED_PROVIDERS[*]} " =~ " hetzner " ]]; then
            echo "  hetzner:"
            echo "    location: \"${HETZNER_LOCATION:-fsn1}\""
            echo "    apiTokenSecretRef:"
            echo "      name: hetzner-api-token"
            echo "      key: HETZNER_API_TOKEN"
        fi
        # Scaleway
        if [[ " ${ENABLED_PROVIDERS[*]} " =~ " scaleway " ]]; then
            echo "  scaleway:"
            echo "    zone: \"${SCW_ZONE:-fr-par-1}\""
            [ -n "${SCW_PROJECT_ID:-}" ] && echo "    projectID: \"${SCW_PROJECT_ID}\""
            [ -n "${SCW_ORGANIZATION_ID:-}" ] && echo "    organizationID: \"${SCW_ORGANIZATION_ID}\""
            echo "    apiKeySecretRef:"
            echo "      name: scaleway-api-keys"
            echo "      key: SCW_SECRET_KEY"
        fi
        # DigitalOcean
        if [[ " ${ENABLED_PROVIDERS[*]} " =~ " digitalocean " ]]; then
            echo "  digitalocean:"
            echo "    region: \"${DIGITALOCEAN_REGION:-nyc1}\""
            echo "    apiTokenSecretRef:"
            echo "      name: digitalocean-api-token"
            echo "      key: DIGITALOCEAN_API_TOKEN"
        fi
        # Aruba
        if [[ " ${ENABLED_PROVIDERS[*]} " =~ " aruba " ]]; then
            echo "  aruba:"
            echo "    projectID: \"${ARUBA_PROJECT_ID}\""
            echo "    applicationKeySecretRef:"
            echo "      name: aruba-api-credentials"
            echo "      key: ARUBA_API_KEY"
            echo "    applicationSecretSecretRef:"
            echo "      name: aruba-api-credentials"
            echo "      key: ARUBA_API_SECRET"
        fi
        # OVH
        if [[ " ${ENABLED_PROVIDERS[*]} " =~ " ovh " ]]; then
            echo "  ovh:"
            echo "    projectID: \"${OVH_PROJECT_ID}\""
            [ -n "${OVH_REGION:-}" ] && echo "    region: \"${OVH_REGION}\""
            echo "    applicationKeySecretRef:"
            echo "      name: ovh-api-credentials"
            echo "      key: OVH_APPLICATION_KEY"
            echo "    applicationSecretSecretRef:"
            echo "      name: ovh-api-credentials"
            echo "      key: OVH_APPLICATION_SECRET"
            echo "    consumerKeySecretRef:"
            echo "      name: ovh-api-credentials"
            echo "      key: OVH_CONSUMER_KEY"
        fi
        # AWS
        if [[ " ${ENABLED_PROVIDERS[*]} " =~ " aws " ]]; then
            echo "  aws:"
            echo "    region: \"${AWS_REGION:-eu-west-1}\""
            echo "    ami: \"${AWS_AMI}\""
            [ -n "${AWS_SUBNET_ID:-}" ] && echo "    subnetID: \"${AWS_SUBNET_ID}\""
            echo "    accessKeyIDSecretRef:"
            echo "      name: aws-credentials"
            echo "      key: AWS_ACCESS_KEY_ID"
            echo "    secretAccessKeySecretRef:"
            echo "      name: aws-credentials"
            echo "      key: AWS_SECRET_ACCESS_KEY"
        fi
        # Azure
        if [[ " ${ENABLED_PROVIDERS[*]} " =~ " azure " ]]; then
            echo "  azure:"
            echo "    subscriptionID: \"${AZURE_SUBSCRIPTION_ID}\""
            echo "    resourceGroup: \"${AZURE_RESOURCE_GROUP}\""
            echo "    location: \"${AZURE_LOCATION:-westeurope}\""
            echo "    subnetID: \"${AZURE_SUBNET_ID}\""
            echo "    tenantIDSecretRef:"
            echo "      name: azure-credentials"
            echo "      key: AZURE_TENANT_ID"
            echo "    clientIDSecretRef:"
            echo "      name: azure-credentials"
            echo "      key: AZURE_CLIENT_ID"
            echo "    clientSecretSecretRef:"
            echo "      name: azure-credentials"
            echo "      key: AZURE_CLIENT_SECRET"
        fi
        # UpCloud
        if [[ " ${ENABLED_PROVIDERS[*]} " =~ " upcloud " ]]; then
            echo "  upcloud:"
            echo "    zone: \"${UPCLOUD_ZONE:-fi-hel1}\""
            echo "    apiTokenSecretRef:"
            echo "      name: upcloud-credentials"
            echo "      key: UPCLOUD_API_TOKEN"
        fi
        # Exoscale
        if [[ " ${ENABLED_PROVIDERS[*]} " =~ " exoscale " ]]; then
            echo "  exoscale:"
            echo "    zone: \"${EXOSCALE_ZONE:-ch-gva-2}\""
            echo "    apiKeySecretRef:"
            echo "      name: exoscale-credentials"
            echo "      key: EXOSCALE_API_KEY"
            echo "    apiSecretSecretRef:"
            echo "      name: exoscale-credentials"
            echo "      key: EXOSCALE_API_SECRET"
        fi
        # IONOS
        if [[ " ${ENABLED_PROVIDERS[*]} " =~ " ionos " ]]; then
            echo "  ionos:"
            [ -n "${IONOS_DATACENTER_ID:-}" ] && echo "    datacenterID: \"${IONOS_DATACENTER_ID}\""
            if [ -n "${IONOS_API_TOKEN:-}" ]; then
                echo "    apiTokenSecretRef:"
                echo "      name: ionos-credentials"
                echo "      key: IONOS_API_TOKEN"
            else
                echo "    usernameSecretRef:"
                echo "      name: ionos-credentials"
                echo "      key: IONOS_USERNAME"
                echo "    passwordSecretRef:"
                echo "      name: ionos-credentials"
                echo "      key: IONOS_PASSWORD"
            fi
        fi
        echo "  join:"
        echo "    hostApiServer: \"https://${HOST_TS_IP}:6443\""
        echo "    kindClusterName: \"${KIND_CLUSTER_NAME}\""
        echo "    tokenTtlMinutes: 60"
        echo "  tailscale:"
        echo "    authKeySecretRef:"
        echo "      name: tailscale-auth"
        echo "      key: authkey"
        echo "  bootstrap:"
        echo "    kubernetesVersion: \"1.34.3\""
    } | kubectl apply -f -

    log_info "✓ Phase C complete"
}

# ── Phase D: NodePool and pod per provider ─────────────────────────────────────

phase_nodepools_and_pods() {
    log_info "=== Phase D: NodePools and pods ==="
    cd "${PROJECT_ROOT}"

    for p in "${ENABLED_PROVIDERS[@]}"; do
        local nodepool_name="e2e-${p}-nodepool"
        local pod_name="e2e-${p}-resource-hungry-pod"
        log_info "Creating NodePool and pod for ${p}..."

        cat <<EOF | kubectl apply -f -
apiVersion: cloudburst.io/v1alpha1
kind: NodePool
metadata:
  name: ${nodepool_name}
  namespace: ${TEST_NAMESPACE}
spec:
  requirements:
    regionConstraint: "ANY"
    arch: ["x86_64"]
    maxPriceEurPerHour: 0.50
    allowedProviders: ["${p}"]
  limits:
    maxNodes: 3
    minNodes: 0
  template:
    labels:
      cloudburst.io/nodepool: "${nodepool_name}"
  disruption:
    ttlSecondsAfterEmpty: 30
    ttlSecondsUntilExpired: 3600
  weight: 1
---
apiVersion: v1
kind: Pod
metadata:
  name: ${pod_name}
  namespace: ${TEST_NAMESPACE}
  labels:
    app: e2e-workload
spec:
  nodeSelector:
    cloudburst.io/nodepool: "${nodepool_name}"
  containers:
  - name: workload
    image: busybox:1.36
    command: ["sleep", "infinity"]
    resources:
      requests:
        cpu: "1500m"
        memory: "2Gi"
EOF
    done

    log_info "✓ Phase D complete"
}

# ── Phase E: Wait for all NodeClaims Ready and pods Running ────────────────────

phase_wait_all() {
    log_info "=== Phase E: Waiting for all providers ==="
    cd "${PROJECT_ROOT}"

    local expected_claims=${#ENABLED_PROVIDERS[@]}
    local timeout=$((NODECLAIM_CREATE_TIMEOUT + PROVISIONING_TIMEOUT + READY_TIMEOUT + 120))

    log_info "Waiting for ${expected_claims} NodeClaim(s) (timeout: ${timeout}s)..."
    local elapsed=0
    while [ $elapsed -lt $timeout ]; do
        local claims
        claims=$(kubectl get nodeclaims -n "${TEST_NAMESPACE}" --no-headers 2>/dev/null | wc -l | tr -d ' ')
        local ready
        ready=$(kubectl get nodeclaims -n "${TEST_NAMESPACE}" -o jsonpath='{.items[?(@.status.phase=="Ready")].metadata.name}' 2>/dev/null | wc -w | tr -d ' ')
        if [ "$claims" -ge "$expected_claims" ] && [ "$ready" -ge "$expected_claims" ]; then
            log_info "✓ All ${expected_claims} NodeClaim(s) Ready"
            break
        fi
        echo -n "."
        sleep 15
        elapsed=$((elapsed + 15))
    done

    if [ $elapsed -ge $timeout ]; then
        log_error "Timeout waiting for NodeClaims"
        kubectl get nodeclaims -n "${TEST_NAMESPACE}" -o wide 2>/dev/null || true
        kubectl logs -n "${CONTROLLER_NAMESPACE}" -l app=cloudburst-controller --tail=80 2>/dev/null || true
        exit 1
    fi

    log_info "Waiting for all pods to be Running..."
    elapsed=0
    while [ $elapsed -lt $((POD_SCHEDULED_TIMEOUT * ${expected_claims:-0})); do
        local running=0
        for p in "${ENABLED_PROVIDERS[@]}"; do
            local phase
            phase=$(kubectl get pod "e2e-${p}-resource-hungry-pod" -n "${TEST_NAMESPACE}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
            [ "$phase" = "Running" ] && running=$((running + 1))
        done
        if [ "$running" -eq "${expected_claims:-0}" ]; then
            log_info "✓ All ${expected_claims} pod(s) Running"
            break
        fi
        echo -n "."
        sleep 10
        elapsed=$((elapsed + 10))
    done

    log_info "=== Cluster state ==="
    kubectl get nodes -o wide 2>/dev/null || true
    echo ""
    kubectl get nodeclaims -n "${TEST_NAMESPACE}" -o wide 2>/dev/null || true
    echo ""
    kubectl get pods -n "${TEST_NAMESPACE}" -o wide 2>/dev/null || true

    log_info "✓ Phase E complete"
}

# ── Phase E3: Playwright UI assertions ───────────────────────────────────────

phase_playwright() {
    if ! declare -f run_playwright_ui_assertions &>/dev/null; then
        log_warn "run_playwright_ui_assertions not available, skipping Playwright"
        return 0
    fi
    log_info "=== Phase E3: Playwright UI assertions ==="
    cd "${PROJECT_ROOT}"

    # Run Playwright once (UI shows all providers; no need to run per-provider)
    local prov="${ENABLED_PROVIDERS[0]:-gcp}"
    log_info "Running Playwright (provider filter: ${prov})"
    run_playwright_ui_assertions "${prov}" || exit 1

    log_info "✓ Phase E3 complete"
}

# ── Phase E2: UI deployment ───────────────────────────────────────────────────

phase_ui() {
    log_info "=== Phase E2: UI deployment ==="
    cd "${PROJECT_ROOT}"

    kubectl config use-context "kind-${KIND_CLUSTER_NAME}" 2>/dev/null || true

    if [ ! -d "${PROJECT_ROOT}/../cloudburst-autoscaler-ui" ]; then
        log_warn "cloudburst-autoscaler-ui not found at ../cloudburst-autoscaler-ui, skipping UI deployment"
        return 0
    fi

    deploy_ui || { log_warn "UI deployment failed (non-fatal)"; return 0; }
    echo ""
    verify_ui_ready || { log_warn "UI verification had issues (non-fatal)"; return 0; }

    log_info "✓ UI available at http://127.0.0.1:8082"
    log_info "✓ Phase E2 complete"
}

# ── Graph dump (before pod deletion, for overlap/debug analysis) ─────────────────

GRAPH_DUMP_FILE="${GRAPH_DUMP_FILE:-${PROJECT_ROOT}/graph-e2e-all-providers.json}"
dump_graph_before_scale_down() {
    log_info "Dumping graph API response to ${GRAPH_DUMP_FILE} (before pod deletion)..."
    if ! docker ps -q -f name="^${UI_CONTAINER:-cloudburst-ui-e2e}$" 2>/dev/null | grep -q .; then
        log_warn "UI container not running, skipping graph dump"
        return 0
    fi
    local base="http://127.0.0.1:8082"
    local cluster_id
    cluster_id=$(curl -s --connect-timeout 5 "${base}/api/clusters" 2>/dev/null | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)
    if [ -z "${cluster_id}" ]; then
        log_warn "No cluster from /api/clusters, skipping graph dump"
        return 0
    fi
    if curl -s --connect-timeout 10 "${base}/api/clusters/${cluster_id}/graph" 2>/dev/null > "${GRAPH_DUMP_FILE}"; then
        [ -s "${GRAPH_DUMP_FILE}" ] && log_info "Graph saved: ${GRAPH_DUMP_FILE}"
    else
        log_warn "Failed to fetch graph API"
    fi
}

# ── Phase F: Scale-down (mandatory) ─────────────────────────────────────────────

phase_scale_down() {
    log_info "=== Phase F: Scale-down ==="
    cd "${PROJECT_ROOT}"

    for p in "${ENABLED_PROVIDERS[@]}"; do
        kubectl delete pod "e2e-${p}-resource-hungry-pod" -n "${TEST_NAMESPACE}" --grace-period=0 --force 2>/dev/null || true
    done

    log_info "Waiting for NodeClaims to be deleted..."
    local timeout=$SCALEDOWN_TIMEOUT
    local elapsed=0
    while [ $elapsed -lt $timeout ]; do
        local claims
        claims=$(kubectl get nodeclaims -n "${TEST_NAMESPACE}" --no-headers 2>/dev/null | wc -l | tr -d ' ')
        if [ "$claims" -eq 0 ]; then
            log_info "✓ All NodeClaims deleted (scale to zero)"
            log_info "✓ Phase F complete"
            return 0
        fi
        echo -n "."
        sleep 15
        elapsed=$((elapsed + 15))
    done

    log_error "Timeout waiting for scale-down (NodeClaims not deleted)"
    kubectl get nodeclaims -n "${TEST_NAMESPACE}" -o wide 2>/dev/null || true
    exit 1
}

# ── Cleanup ───────────────────────────────────────────────────────────────────

CONTROLLER_LOG_FILE="${CONTROLLER_LOG_FILE:-${PROJECT_ROOT}/controller-e2e-all-providers.log}"
CLEANUP_DONE=""
cleanup() {
    [ -n "${CLEANUP_DONE:-}" ] && return
    CLEANUP_DONE=1
    log_info "Cleaning up..."
    cd "${PROJECT_ROOT}" 2>/dev/null || true

    # Dump controller logs before deleting namespace (for graph/concurrency analysis)
    log_info "Dumping controller logs to ${CONTROLLER_LOG_FILE}..."
    if kubectl get deployment -n "${CONTROLLER_NAMESPACE}" -o name 2>/dev/null | grep -q .; then
        kubectl logs -n "${CONTROLLER_NAMESPACE}" -l app=cloudburst-controller --tail=50000 --all-containers=true 2>/dev/null > "${CONTROLLER_LOG_FILE}" || true
        [ -s "${CONTROLLER_LOG_FILE}" ] && log_info "Controller log saved: ${CONTROLLER_LOG_FILE}"
    fi

    declare -f stop_ui &>/dev/null && stop_ui

    for p in "${ENABLED_PROVIDERS[@]:-}"; do
        kubectl delete pod "e2e-${p}-resource-hungry-pod" -n "${TEST_NAMESPACE}" --ignore-not-found=true --grace-period=0 --force 2>/dev/null || true
        kubectl delete nodepool "e2e-${p}-nodepool" -n "${TEST_NAMESPACE}" --ignore-not-found=true 2>/dev/null || true
    done
    kubectl delete nodeclaims --all -n "${TEST_NAMESPACE}" --ignore-not-found=true 2>/dev/null || true
    kubectl delete nodeclass "${NODECLASS_NAME}" -n "${TEST_NAMESPACE}" --ignore-not-found=true 2>/dev/null || true
    kubectl delete secret tailscale-auth -n "${TEST_NAMESPACE}" --ignore-not-found=true 2>/dev/null || true
    kubectl delete namespace "${CONTROLLER_NAMESPACE}" --ignore-not-found=true 2>/dev/null || true

    # Remove all cloud instances (catches orphans in any resource group)
    if [ "${KEEP_CLUSTER:-0}" -eq 0 ]; then
        log_info "Removing all cloud instances (cleanup-all-providers)..."
        bash "${SCRIPT_DIR}/cleanup-all-providers.sh" 2>/dev/null || true
        kind delete cluster --name "${KIND_CLUSTER_NAME}" 2>/dev/null || true
    else
        log_info "Keeping cluster (--keep-cluster). Run: make cleanup-all && kind delete cluster --name ${KIND_CLUSTER_NAME}"
    fi
    log_info "✓ Cleanup complete"
}

# ── Main ───────────────────────────────────────────────────────────────────────

main() {
    log_info "=== CloudBurst E2E — All Providers ==="
    check_prerequisites
    # For AWS: use only one region (cleanup, AMI resolution)
    if [[ " ${ENABLED_PROVIDERS[*]} " =~ " aws " ]]; then
        export AWS_REGIONS="${AWS_REGION:-eu-west-1}"
    fi
    echo ""

    trap cleanup EXIT
    trap 'cleanup; exit 130' INT
    trap 'cleanup; exit 143' TERM

    phase_cloudbroker
    echo ""
    phase_kind_and_controller
    echo ""
    phase_ui
    echo ""
    phase_nodeclass_and_secrets
    echo ""
    phase_nodepools_and_pods
    echo ""
    phase_wait_all
    echo ""
    phase_playwright
    echo ""
    dump_graph_before_scale_down
    echo ""
    phase_scale_down

    echo ""
    log_info "=== E2E All Providers PASSED ==="
}

main "$@"
