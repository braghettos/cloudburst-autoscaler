# Multi-Provider Testing Guide

This guide explains how to test Cloudburst Autoscaler with pods provisioned on both GCP and Scaleway simultaneously.

## Overview

The test script `test-multi-provider.sh` demonstrates provisioning nodes from multiple cloud providers by:

1. Creating separate NodePools for GCP and Scaleway
2. Creating corresponding NodeClasses for each provider
3. Creating pods with node selectors that target specific providers
4. Verifying that nodes are provisioned from both providers

## Prerequisites

### GCP Setup
- `GCP_PROJECT` environment variable
- `GOOGLE_APPLICATION_CREDENTIALS` pointing to a service account key file
- `GCP_ZONE` (default: `us-central1-a`)

### Scaleway Setup
- `SCW_SECRET_KEY` environment variable (or configure via Kubernetes secret)
- `SCW_DEFAULT_ZONE` (optional, default: `fr-par-1`)

### Common Setup
- `TAILSCALE_AUTHKEY` for VPN connectivity
- Tailscale running and connected
- Docker, Kind, and kubectl installed
- CloudBroker service running (defaults to `http://<host-ip>:8000`)

## How It Works

### NodePool Selection

Each NodePool specifies `allowedProviders` to restrict which cloud providers can be used:

- **GCP NodePool**: `allowedProviders: ["gcp"]` → CloudBroker returns GCP recommendations
- **Scaleway NodePool**: `allowedProviders: ["scaleway"]` → CloudBroker returns Scaleway recommendations

### NodeClass Matching

**✅ Implemented**: The controller now automatically matches NodeClass based on the provider from CloudBroker recommendation:

1. **Automatic Matching**: When creating NodeClaims, the controller:
   - Gets CloudBroker recommendation first (which includes the provider)
   - Filters available NodeClasses to find one that supports the recommended provider
   - Uses the matching NodeClass for NodeClaim creation

2. **Provider Support Detection**:
   - **GCP**: NodeClass must have `spec.gcp.project` and `spec.gcp.zone` set
   - **Scaleway**: NodeClass must have `spec.scaleway` configured with `zone` set

3. **Fallback**: If no matching NodeClass is found, the controller logs an error and creates an event, preventing incorrect NodeClaim creation

### Pod Scheduling

Pods use node selectors to target specific providers:

```yaml
affinity:
  nodeAffinity:
    requiredDuringSchedulingIgnoredDuringExecution:
      nodeSelectorTerms:
      - matchExpressions:
        - key: cloudburst.io/provider
          operator: In
          values: ["gcp"]  # or ["scaleway"]
```

## Running the Test

```bash
# Set required environment variables
export GCP_PROJECT="your-project"
export GOOGLE_APPLICATION_CREDENTIALS="/path/to/gcp-key.json"
export GCP_ZONE="us-central1-a"
export SCW_SECRET_KEY="your-scaleway-secret-key"
export SCW_DEFAULT_ZONE="fr-par-1"  # optional
export TAILSCALE_AUTHKEY="your-tailscale-key"

# Run the test
./hack/test-multi-provider.sh
```

## Expected Behavior

1. **GCP Pod** (`gcp-workload-pod`):
   - Becomes unschedulable (no GCP nodes exist)
   - Triggers GCP NodePool controller
   - CloudBroker returns GCP recommendation
   - GCP NodeClaim created → GCP VM provisioned
   - Pod scheduled on GCP node

2. **Scaleway Pod** (`scaleway-workload-pod`):
   - Becomes unschedulable (no Scaleway nodes exist)
   - Triggers Scaleway NodePool controller
   - CloudBroker returns Scaleway recommendation
   - Scaleway NodeClaim created → Scaleway server provisioned
   - Pod scheduled on Scaleway node

3. **Verification**:
   - Both pods scheduled successfully
   - Nodes from both providers visible in cluster
   - Node labels show `cloudburst.io/provider=gcp` and `cloudburst.io/provider=scaleway`

## Troubleshooting

### NodeClass Mismatch

If you see errors like "missing GCP configuration" when provisioning Scaleway (or vice versa):

- **Cause**: Controller selected wrong NodeClass
- **Solution**: Use separate namespaces or ensure only matching NodeClass exists

### CloudBroker Not Returning Expected Provider

- Verify `allowedProviders` in NodePool matches desired provider
- Check CloudBroker logs to see what recommendations are returned
- Ensure CloudBroker supports both providers

### Pods Not Scheduling

- Check node labels: `kubectl get nodes --show-labels`
- Verify pod node selectors match node labels
- Check pod events: `kubectl describe pod <pod-name>`

## Future Improvements

1. **NodePool-NodeClass Binding**: Add explicit `nodeClassRef` to NodePool spec for explicit binding
2. **Provider Priority**: Support multiple NodeClasses per provider with priority/weight
3. **NodeClass Validation**: Add webhook validation to ensure NodeClass has at least one provider configured

## Example Output

```
[INFO] === Multi-Provider Test ===
[PROVIDER] Creating GCP NodeClass...
[PROVIDER] ✓ GCP NodeClass created
[PROVIDER] Creating GCP NodePool...
[PROVIDER] ✓ GCP NodePool created
[PROVIDER] Creating Scaleway NodeClass...
[PROVIDER] ✓ Scaleway NodeClass created
[PROVIDER] Creating Scaleway NodePool...
[PROVIDER] ✓ Scaleway NodePool created
[PROVIDER] Waiting for GCP NodeClaim to be created...
[PROVIDER] ✓ GCP NodeClaim created: cb-gcp-nodepool-abc123
[PROVIDER] Waiting for Scaleway NodeClaim to be created...
[PROVIDER] ✓ Scaleway NodeClaim created: cb-scaleway-nodepool-xyz789
[PROVIDER] Waiting for GCP pod 'gcp-workload-pod' to be scheduled...
[PROVIDER] ✓ GCP pod scheduled on node: cb-gcp-nodepool-abc123
[PROVIDER] Waiting for Scaleway pod 'scaleway-workload-pod' to be scheduled...
[PROVIDER] ✓ Scaleway pod scheduled on node: cb-scaleway-nodepool-xyz789
[INFO] GCP nodes: 1
[INFO] Scaleway nodes: 1
[INFO] ✓ Both providers have nodes!
```
