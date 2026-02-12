# End-to-End Manual Testing Guide

This document provides step-by-step instructions to manually test the Cloudburst Autoscaler with both **GCP** and **Scaleway** providers, including scale-up (triggered by unschedulable pods) and scale-down (triggered by pod deletion).

## Overview

The test flow:

```
1. Deploy Kind cluster + controller     (make kind-setup-and-deploy)
2. Create provider credentials           (Secrets)
3. Create NodeClass                      (provider-specific infrastructure config)
4. Create NodePool                       (scaling policy)
5. Create high-resource Pod              (triggers scale-up)
6. Observe: NodeClaim created -> CloudBroker recommendation -> VM provisioned -> Node joins -> Pod scheduled
7. Delete the Pod                        (triggers scale-down)
8. Observe: Node becomes empty -> TTL expires -> NodeClaim deleted -> VM terminated
```

## Prerequisites

- Docker
- [Kind](https://kind.sigs.k8s.io/)
- kubectl
- [Tailscale](https://tailscale.com/) connected to your tailnet
- [CloudBroker](https://github.com/braghettos/cloudbroker) running and accessible
- `.env` file configured (see `.env.example`)

**For GCP testing:**
- GCP service account JSON key with Compute Engine permissions
- GCP project ID and zone

**For Scaleway testing:**
- Scaleway API access key and secret key
- Scaleway project ID and zone

---

## Step 0: Environment Setup

### 0.1 Configure `.env`

```bash
cp .env.example .env
# Edit .env with your actual values
```

Required variables:
- `GOOGLE_APPLICATION_CREDENTIALS` -- path to GCP service account JSON
- `TAILSCALE_AUTHKEY` -- Tailscale auth key
- `SCW_SECRET_KEY` -- Scaleway secret key (for Scaleway testing)
- `SCW_ACCESS_KEY` -- Scaleway access key (for Scaleway testing)

### 0.2 Get Your Tailscale IP

```bash
tailscale ip -4
```

Save this IP -- you'll need it for the `hostApiServer` field in NodeClass resources.

### 0.3 Ensure CloudBroker is Running

CloudBroker must be accessible from your machine. By default the controller connects to `http://localhost:8000` or the URL in the `cloudbroker-config` ConfigMap.

```bash
# Verify CloudBroker is reachable
curl -s http://localhost:8000/health | jq .
```

---

## Step 1: Deploy the Kind Cluster and Controller

This single command creates the Kind cluster, builds the controller image, loads it locally, and deploys everything:

```bash
make kind-setup-and-deploy IMG=cloudburst-autoscaler:latest
```

### Verify

```bash
# Check nodes (should have control-plane + worker)
kubectl get nodes

# Check controller is running
kubectl get pods -n cloudburst-system

# Check controller logs
kubectl logs -n cloudburst-system -l control-plane=controller-manager --tail=20
```

Expected output:
```
NAME                       STATUS   ROLES           AGE   VERSION
cloudburst-control-plane   Ready    control-plane   1m    v1.34.3
cloudburst-worker          Ready    <none>          1m    v1.34.3
```

---

## Step 2: Testing with GCP Provider

### 2.1 Create GCP NodeClass

Edit `config/samples/nodeclass-gcp.yaml` with your actual values:

```bash
# Replace placeholders:
#   - your-gcp-project-id -> your actual GCP project ID
#   - <HOST_TAILSCALE_IP> -> your Tailscale IP (from step 0.2)
kubectl apply -f config/samples/nodeclass-gcp.yaml
```

Or create it inline:

```bash
TAILSCALE_IP=$(tailscale ip -4)

cat <<EOF | kubectl apply -f -
apiVersion: cloudburst.io/v1alpha1
kind: NodeClass
metadata:
  name: gcp-nodeclass
  namespace: default
spec:
  gcp:
    project: "YOUR_GCP_PROJECT"
    zone: "us-central1-a"
  join:
    hostApiServer: "https://${TAILSCALE_IP}:6443"
    kindClusterName: "cloudburst"
    tokenTtlMinutes: 60
  tailscale:
    authKeySecretRef:
      name: tailscale-auth
      key: authkey
  bootstrap:
    kubernetesVersion: "1.34.3"
EOF
```

### 2.2 Create GCP NodePool

```bash
kubectl apply -f config/samples/nodepool-gcp.yaml
```

### Verify Resources

```bash
kubectl get nodeclasses
kubectl get nodepools
```

### 2.3 Scale-Up: Create High-Resource Pod

This pod requests 1.5 CPU and 2Gi memory with a node affinity for GCP provider nodes.
Since no GCP-labeled node exists, it becomes **Unschedulable**, triggering the controller.

```bash
kubectl apply -f config/samples/workload-gcp.yaml
```

### 2.4 Observe Scale-Up

```bash
# Watch the pod status (should be Pending/Unschedulable initially)
kubectl get pods -w

# Watch NodeClaims being created
kubectl get nodeclaims -w

# Watch controller logs for the full flow
kubectl logs -n cloudburst-system -l control-plane=controller-manager -f
```

**Expected sequence in logs:**
1. `calculated desired capacity` -- controller detects unschedulable pod
2. `creating NodeClaim` -- controller creates a NodeClaim
3. `requesting recommendation from CloudBroker` -- calls CloudBroker API
4. `provisioning GCP instance` -- creates GCP VM
5. `instance provisioned` -- VM is running
6. `node joined cluster` -- VM has joined via kubeadm
7. `NodeClaim is Ready` -- node is ready

```bash
# After a few minutes, check nodes (new GCP node should appear)
kubectl get nodes -l cloudburst.io/provider=gcp

# Check pod is now scheduled
kubectl get pods -o wide
```

### 2.5 Scale-Down: Delete the Pod

Delete the workload pod to make the node empty:

```bash
kubectl delete pod gcp-workload
```

### 2.6 Observe Scale-Down

The NodePool controller periodically checks for empty nodes. After `ttlSecondsAfterEmpty` (60s in the sample), it deletes the NodeClaim, which triggers:

1. Node cordon
2. Node drain
3. Kubernetes Node object deletion
4. GCP VM deletion
5. NodeClaim CR removal

```bash
# Watch NodeClaims (should transition to Deleting, then disappear)
kubectl get nodeclaims -w

# Watch nodes (GCP node should be removed)
kubectl get nodes -w

# Watch controller logs
kubectl logs -n cloudburst-system -l control-plane=controller-manager -f | grep -E "scale-down|empty|deleting|deleted"
```

**Expected timeline:**
- Pod deleted -> node becomes empty immediately
- ~60 seconds later (ttlSecondsAfterEmpty) -> NodeClaim deletion starts
- ~30-60 seconds later -> VM deleted, node removed

### 2.7 Cleanup GCP Resources

```bash
kubectl delete -f config/samples/workload-gcp.yaml --ignore-not-found
kubectl delete -f config/samples/nodepool-gcp.yaml
kubectl delete -f config/samples/nodeclass-gcp.yaml
```

---

## Step 3: Testing with Scaleway Provider

### 3.1 Create Scaleway Credentials Secret

```bash
# Edit config/samples/secret-scaleway.yaml with your actual keys, then:
kubectl apply -f config/samples/secret-scaleway.yaml
```

Or create it inline:

```bash
kubectl create secret generic scaleway-credentials \
  --from-literal=SCW_SECRET_KEY="your-scaleway-secret-key" \
  --from-literal=SCW_ACCESS_KEY="your-scaleway-access-key" \
  --namespace=default \
  --dry-run=client -o yaml | kubectl apply -f -
```

### 3.2 Create Scaleway NodeClass

Edit `config/samples/nodeclass-scaleway.yaml` with your actual values:

```bash
# Replace placeholders:
#   - your-scaleway-project-id -> your actual Scaleway project ID
#   - <HOST_TAILSCALE_IP> -> your Tailscale IP (from step 0.2)
kubectl apply -f config/samples/nodeclass-scaleway.yaml
```

Or create it inline:

```bash
TAILSCALE_IP=$(tailscale ip -4)

cat <<EOF | kubectl apply -f -
apiVersion: cloudburst.io/v1alpha1
kind: NodeClass
metadata:
  name: scaleway-nodeclass
  namespace: default
spec:
  scaleway:
    zone: "fr-par-1"
    projectID: "YOUR_SCW_PROJECT_ID"
    image: "ubuntu_jammy"
    apiKeySecretRef:
      name: scaleway-credentials
      key: SCW_SECRET_KEY
  join:
    hostApiServer: "https://${TAILSCALE_IP}:6443"
    kindClusterName: "cloudburst"
    tokenTtlMinutes: 60
  tailscale:
    authKeySecretRef:
      name: tailscale-auth
      key: authkey
  bootstrap:
    kubernetesVersion: "1.34.3"
EOF
```

### 3.3 Create Scaleway NodePool

```bash
kubectl apply -f config/samples/nodepool-scaleway.yaml
```

### Verify Resources

```bash
kubectl get nodeclasses
kubectl get nodepools
```

### 3.4 Scale-Up: Create High-Resource Pod

```bash
kubectl apply -f config/samples/workload-scaleway.yaml
```

### 3.5 Observe Scale-Up

```bash
# Watch pods
kubectl get pods -w

# Watch NodeClaims
kubectl get nodeclaims -w

# Watch controller logs
kubectl logs -n cloudburst-system -l control-plane=controller-manager -f
```

```bash
# After provisioning, check Scaleway node
kubectl get nodes -l cloudburst.io/provider=scaleway

# Check pod is scheduled
kubectl get pods -o wide
```

### 3.6 Scale-Down: Delete the Pod

```bash
kubectl delete pod scaleway-workload
```

### 3.7 Observe Scale-Down

```bash
# Watch NodeClaims (should transition to Deleting)
kubectl get nodeclaims -w

# Watch nodes (Scaleway node should be removed)
kubectl get nodes -w

# Controller logs
kubectl logs -n cloudburst-system -l control-plane=controller-manager -f | grep -E "scale-down|empty|deleting|deleted"
```

### 3.8 Cleanup Scaleway Resources

```bash
kubectl delete -f config/samples/workload-scaleway.yaml --ignore-not-found
kubectl delete -f config/samples/nodepool-scaleway.yaml
kubectl delete -f config/samples/nodeclass-scaleway.yaml
kubectl delete -f config/samples/secret-scaleway.yaml
```

---

## Step 4: Multi-Provider Testing (Both GCP and Scaleway)

Test both providers simultaneously to verify Cloudburst handles multi-cloud provisioning.

### 4.1 Create All Resources

```bash
TAILSCALE_IP=$(tailscale ip -4)

# Secrets (Tailscale is already created by make deploy-all)
kubectl apply -f config/samples/secret-scaleway.yaml

# NodeClasses (edit with your values first)
kubectl apply -f config/samples/nodeclass-gcp.yaml
kubectl apply -f config/samples/nodeclass-scaleway.yaml

# NodePools
kubectl apply -f config/samples/nodepool-gcp.yaml
kubectl apply -f config/samples/nodepool-scaleway.yaml
```

### 4.2 Scale-Up: Create Both Workloads

```bash
kubectl apply -f config/samples/workload-gcp.yaml
kubectl apply -f config/samples/workload-scaleway.yaml
```

### 4.3 Observe Multi-Provider Scale-Up

```bash
# Watch NodeClaims from both providers
kubectl get nodeclaims -w

# Watch all nodes
kubectl get nodes -w

# Watch pods
kubectl get pods -o wide -w
```

After provisioning completes:

```bash
# Verify nodes from both providers
kubectl get nodes -l cloudburst.io/provider

# Expected: one GCP node + one Scaleway node
kubectl get nodes -l cloudburst.io/provider=gcp
kubectl get nodes -l cloudburst.io/provider=scaleway
```

### 4.4 Scale-Down: Delete Both Pods

```bash
kubectl delete pod gcp-workload scaleway-workload
```

### 4.5 Observe Multi-Provider Scale-Down

```bash
# Watch NodeClaims disappearing
kubectl get nodeclaims -w

# Watch nodes being removed
kubectl get nodes -w
```

After ~60 seconds (ttlSecondsAfterEmpty), both cloud VMs should be terminated.

### 4.6 Full Cleanup

```bash
kubectl delete -f config/samples/workload-gcp.yaml --ignore-not-found
kubectl delete -f config/samples/workload-scaleway.yaml --ignore-not-found
kubectl delete -f config/samples/nodepool-gcp.yaml --ignore-not-found
kubectl delete -f config/samples/nodepool-scaleway.yaml --ignore-not-found
kubectl delete -f config/samples/nodeclass-gcp.yaml --ignore-not-found
kubectl delete -f config/samples/nodeclass-scaleway.yaml --ignore-not-found
kubectl delete -f config/samples/secret-scaleway.yaml --ignore-not-found
```

---

## Troubleshooting

### Pod stays Pending but no NodeClaim is created

```bash
# Check if NodePool exists and matches
kubectl get nodepools
kubectl describe nodepool <name>

# Check controller logs for demand detection
kubectl logs -n cloudburst-system -l control-plane=controller-manager | grep -E "demand|capacity|unschedulable"
```

### NodeClaim stuck in Provisioning

```bash
# Check NodeClaim status
kubectl describe nodeclaim <name>

# Check controller logs for provisioning errors
kubectl logs -n cloudburst-system -l control-plane=controller-manager | grep -E "provision|error|CloudBroker"

# Verify CloudBroker is reachable from the controller
kubectl exec -n cloudburst-system deploy/cloudburst-controller-manager -- wget -qO- http://localhost:8000/health 2>/dev/null || echo "CloudBroker not reachable"
```

### Node joined but Pod still Pending

```bash
# Check node status and labels
kubectl describe node <node-name>

# Check if node has the expected provider label
kubectl get nodes --show-labels | grep cloudburst

# Check pod events
kubectl describe pod <pod-name>
```

### Scale-down not happening

```bash
# Check NodePool disruption settings
kubectl get nodepool <name> -o jsonpath='{.spec.disruption}'

# Check if minNodes is preventing scale-down (must be 0 for full scale-down)
kubectl get nodepool <name> -o jsonpath='{.spec.limits.minNodes}'

# Check controller logs for scale-down activity
kubectl logs -n cloudburst-system -l control-plane=controller-manager | grep -E "scale-down|empty|ttl"
```

### VM not deleted after NodeClaim removal

```bash
# GCP: Check if instance still exists
gcloud compute instances list --filter="name~cb-" --project=YOUR_PROJECT

# Scaleway: Check instances
scw instance server list zone=fr-par-1
```

---

## Tear Down Everything

```bash
# Delete all Cloudburst resources
kubectl delete nodeclaims --all
kubectl delete nodepools --all
kubectl delete nodeclasses --all

# Undeploy the controller
make undeploy

# Delete the Kind cluster
kind delete cluster --name cloudburst
```

---

## Sample Files Reference

| File | Description |
|------|-------------|
| `config/samples/nodeclass-gcp.yaml` | GCP NodeClass |
| `config/samples/nodeclass-scaleway.yaml` | Scaleway NodeClass |
| `config/samples/nodepool-gcp.yaml` | GCP NodePool (allowedProviders: gcp) |
| `config/samples/nodepool-scaleway.yaml` | Scaleway NodePool (allowedProviders: scaleway) |
| `config/samples/secret-scaleway.yaml` | Scaleway API credentials Secret |
| `config/samples/workload-gcp.yaml` | High-resource pod targeting GCP nodes |
| `config/samples/workload-scaleway.yaml` | High-resource pod targeting Scaleway nodes |
