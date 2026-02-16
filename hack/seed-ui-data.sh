#!/bin/bash
set -euo pipefail

# Seed the cluster with NodeClass, NodePool, and a workload pod to populate the UI.
# Uses existing cluster and controller. Requires .env with GCP + Tailscale for full provisioning.
# Without credentials, resources are still created and will show in the UI (NodeClaim may stay Pending/Failed).

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${PROJECT_ROOT}"

# Source .env if present
if [ -f ".env" ]; then
    echo "[INFO] Loading .env"
    set -a
    source .env
    set +a
fi

GCP_PROJECT="${GCP_PROJECT:-demo-project}"
GCP_ZONE="${GCP_ZONE:-us-central1-a}"
TAILSCALE_AUTHKEY="${TAILSCALE_AUTHKEY:-}"
HOST_TS_IP="${HOST_TS_IP:-127.0.0.1}"
TEST_NAMESPACE="${TEST_NAMESPACE:-default}"
NODECLASS_NAME="e2e-nodeclass"
NODEPOOL_NAME="e2e-nodepool"
PENDING_POD_NAME="e2e-resource-hungry-pod"

echo "[INFO] Seeding UI data (namespace=${TEST_NAMESPACE})"
echo "[INFO] GCP_PROJECT=${GCP_PROJECT} GCP_ZONE=${GCP_ZONE} HOST_TS_IP=${HOST_TS_IP}"

# Create Tailscale secret (required by NodeClass)
if [ -n "${TAILSCALE_AUTHKEY}" ]; then
    echo "[INFO] Creating tailscale-auth secret..."
    kubectl create secret generic tailscale-auth \
        --from-literal=authkey="${TAILSCALE_AUTHKEY}" \
        -n "${TEST_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -
else
    echo "[WARN] TAILSCALE_AUTHKEY not set - using placeholder (NodeClaim provisioning may fail)"
    kubectl create secret generic tailscale-auth \
        --from-literal=authkey="tskey-placeholder" \
        -n "${TEST_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -
fi

# Create NodeClass
echo "[INFO] Creating NodeClass ${NODECLASS_NAME}..."
kubectl apply -f - <<EOF
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
    kindClusterName: "cloudburst"
    tokenTtlMinutes: 60
  tailscale:
    authKeySecretRef:
      name: tailscale-auth
      key: authkey
  bootstrap:
    kubernetesVersion: "1.34.3"
EOF

# Create NodePool
echo "[INFO] Creating NodePool ${NODEPOOL_NAME}..."
kubectl apply -f - <<EOF
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
    ttlSecondsAfterEmpty: 60
    ttlSecondsUntilExpired: 3600
  weight: 1
EOF

# Create pending pod (triggers NodeClaim creation)
echo "[INFO] Creating workload pod ${PENDING_POD_NAME}..."
kubectl apply -f - <<EOF
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

echo "[INFO] Done. Run the UI (make run-ui or from cloudburst-autoscaler-ui) and open http://localhost:8082 to see:"
echo "  - NodePool: ${NODEPOOL_NAME}"
echo "  - NodeClass: ${NODECLASS_NAME}"
echo "  - Pod: ${PENDING_POD_NAME} (Pending/Unschedulable)"
echo "  - NodeClaim: (created by controller when it reconciles)"
