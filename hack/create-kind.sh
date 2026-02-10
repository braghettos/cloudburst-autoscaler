#!/bin/bash
set -euo pipefail

# Default cluster name
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-cloudburst}"

echo "=== CloudBurst Kind Cluster Setup ==="
echo "Cluster name: $KIND_CLUSTER_NAME"

# HOST_TS_IP: API server address for kind cert SANs and optional reachability check. No Tailscale CLI used.
# Set HOST_TS_IP in the environment (e.g. 127.0.0.1); if unset, 127.0.0.1 is used.
# certSANs: kind expects an IP. If HOST_TS_IP is a hostname, use 127.0.0.1 for kind config.
if [[ -n "${HOST_TS_IP:-}" && "$HOST_TS_IP" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    : # valid IP
elif [ -n "${HOST_TS_IP:-}" ]; then
    echo "HOST_TS_IP is hostname (${HOST_TS_IP}); using 127.0.0.1 for kind API server SAN."
    HOST_TS_IP="127.0.0.1"
else
    HOST_TS_IP="127.0.0.1"
fi
export HOST_TS_IP

# Step 3: Template kind-config.yaml
echo ""
echo "Step 3: Generating kind-config.yaml..."
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEMPLATE_FILE="$SCRIPT_DIR/kind-config.yaml.tmpl"
OUTPUT_FILE="$SCRIPT_DIR/kind-config.yaml"

if [ ! -f "$TEMPLATE_FILE" ]; then
    echo "ERROR: Template file not found: $TEMPLATE_FILE"
    exit 1
fi

# Replace variables in template
export KIND_CLUSTER_NAME
envsubst < "$TEMPLATE_FILE" > "$OUTPUT_FILE"

echo "✓ Generated kind-config.yaml"

# Step 4: Recreate kind cluster
echo ""
echo "Step 4: Creating kind cluster..."
if kind get clusters | grep -q "^${KIND_CLUSTER_NAME}$"; then
    echo "Deleting existing cluster: $KIND_CLUSTER_NAME"
    kind delete cluster --name "$KIND_CLUSTER_NAME" || true
fi

kind create cluster --name "$KIND_CLUSTER_NAME" --config "$OUTPUT_FILE"

echo "✓ Kind cluster created"

# Step 4b: Fix kubeconfig so kubectl can connect (cert is valid for 127.0.0.1, not 0.0.0.0)
KIND_CONTEXT="kind-${KIND_CLUSTER_NAME}"
if kubectl config get-clusters 2>/dev/null | grep -q "^${KIND_CONTEXT}$"; then
    kubectl config set-cluster "${KIND_CONTEXT}" --server="https://127.0.0.1:6443" 2>/dev/null && echo "✓ Kubeconfig updated (server=https://127.0.0.1:6443)" || true
fi

# Step 5: Verify API server reachable via Tailscale
echo ""
echo "Step 5: Verifying API server accessibility via Tailscale..."
MAX_RETRIES=30
RETRY_COUNT=0

while [ $RETRY_COUNT -lt $MAX_RETRIES ]; do
    if curl -k -s -f "https://${HOST_TS_IP}:6443/healthz" > /dev/null 2>&1; then
        echo "✓ API server is reachable via Tailscale IP: https://${HOST_TS_IP}:6443"
        break
    fi
    
    RETRY_COUNT=$((RETRY_COUNT + 1))
    if [ $RETRY_COUNT -lt $MAX_RETRIES ]; then
        echo "  Waiting for API server... (attempt $RETRY_COUNT/$MAX_RETRIES)"
        sleep 2
    fi
done

if [ $RETRY_COUNT -eq $MAX_RETRIES ]; then
    echo "WARNING: API server not reachable via Tailscale IP after $MAX_RETRIES attempts"
    echo "This might be normal if the cluster is still starting. Please verify manually:"
    echo "  curl -k https://${HOST_TS_IP}:6443/healthz"
else
    # Step 5b: Align cluster-info ConfigMap server endpoint with HOST_TS_IP so kubeadm discovery sees a consistent API server URL
    echo ""
    echo "Step 5b: Patching cluster-info ConfigMap to use https://${HOST_TS_IP}:6443..."
    if kubectl -n kube-public get configmap cluster-info >/dev/null 2>&1; then
        kubectl -n kube-public get configmap cluster-info -o yaml \
          | sed "s|server: .*|server: https://${HOST_TS_IP}:6443|" \
          | kubectl apply -f - >/dev/null 2>&1 && \
          echo "✓ cluster-info server set to https://${HOST_TS_IP}:6443" || \
          echo "WARN: Failed to patch cluster-info server; kubeadm may log a discovery endpoint mismatch warning."
    else
        echo "WARN: cluster-info ConfigMap not found in kube-public; skipping patch."
    fi
    # Step 5c: Remove cgroupRoot from kubelet-config ConfigMap so GCP VMs (cgroup v2 + systemd) don't inherit kind's cgroup-v1 setting
    echo ""
    echo "Step 5c: Patching kubelet-config ConfigMap to remove cgroupRoot..."
    if kubectl -n kube-system get configmap kubelet-config >/dev/null 2>&1; then
        # Extract the kubelet config data, strip cgroupRoot line, and patch back
        KUBELET_CFG=$(kubectl -n kube-system get configmap kubelet-config -o jsonpath='{.data.kubelet}')
        if echo "$KUBELET_CFG" | grep -q 'cgroupRoot'; then
            PATCHED_CFG=$(echo "$KUBELET_CFG" | sed '/^cgroupRoot:/d; /^  cgroupRoot:/d')
            kubectl -n kube-system create configmap kubelet-config --from-literal=kubelet="$PATCHED_CFG" --dry-run=client -o yaml | kubectl apply -f - >/dev/null 2>&1 && \
              echo "✓ Removed cgroupRoot from kubelet-config ConfigMap" || \
              echo "WARN: Failed to patch kubelet-config; GCP VMs may crash with cgroup error."
        else
            echo "✓ kubelet-config already has no cgroupRoot"
        fi
    else
        echo "WARN: kubelet-config ConfigMap not found; skipping patch."
    fi

    echo ""
    echo "=== Setup Complete ==="
    echo "Cluster name: $KIND_CLUSTER_NAME"
    # At this point HOST_TS_IP is the API server address (e.g. Tailscale IP for kind API).
    echo "API server address: $HOST_TS_IP"
    echo "API Server: https://${HOST_TS_IP}:6443"
    echo ""
    echo "Next steps:"
    echo "1. Apply CRDs and controller"
    echo "2. Set NodeClass spec.join.hostApiServer to https://<HOST_TAILSCALE_IP>:6443 (e.g. tailscale ip -4)"
    echo "3. Create NodeClass and NodePool"
fi
