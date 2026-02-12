# Provider Selection Flow

This document explains how the cloud provider (GCP, Scaleway, etc.) is chosen when pods become unschedulable and trigger node provisioning.

## Overview

The provider selection happens at the **NodePool level**, not at the pod level. The flow is:

1. **Pods become unschedulable** (due to resource constraints)
2. **NodePool controllers reconcile** (each independently)
3. **Each NodePool** calculates demand from unschedulable pods
4. **Each NodePool** calls CloudBroker with its `allowedProviders` constraint
5. **CloudBroker** returns a recommendation with a specific provider
6. **Controller** matches NodeClass to the provider
7. **NodeClaim** is created with the chosen provider

## Detailed Flow

### 1. Pod Detection

When pods become unschedulable:

```yaml
status:
  phase: Pending
  conditions:
  - type: PodScheduled
    status: "False"
    reason: "Unschedulable"
```

The NodePool controller detects these pods by:
- Filtering out `kube-system` namespace pods
- Filtering out pods with `cloudburst.io/ignore=true` annotation
- Checking for `Pending` phase with `Unschedulable` condition

**Important**: Currently, **all NodePools see ALL unschedulable pods**. There's no filtering based on pod node selectors matching NodePool template labels.

### 2. NodePool Reconciliation

Each NodePool controller reconciles independently:

```go
// Each NodePool controller:
1. Lists ALL unschedulable pods in cluster
2. Calculates desired capacity (CPU, RAM, pod count)
3. Determines if nodes are needed
4. If needed, calls CloudBroker with NodePool constraints
```

### 3. CloudBroker Request

The NodePool sends a request to CloudBroker with:

```go
RecommendationRequest{
    MinVCPU:            calculated from pod demands,
    MinRAMGb:           calculated from pod demands,
    Arch:               extracted from pod nodeSelector/nodeAffinity (kubernetes.io/arch),
    RegionConstraint:   from NodePool.Requirements.RegionConstraint,
    MaxPriceEurPerHour: from NodePool.Requirements.MaxPriceEurPerHour,
    AllowedProviders:   from NodePool.Requirements.AllowedProviders,  // KEY!
}
```

**The `AllowedProviders` field is critical** - it restricts which providers CloudBroker can recommend.

### 4. CloudBroker Recommendation

CloudBroker returns a recommendation:

```go
RecommendationResult{
    Provider:        "gcp" or "scaleway",  // Based on AllowedProviders + pricing/availability
    Region:         "europe-west1",
    InstanceType:   "n1-standard-2",
    VCPU:           2,
    RAMGb:          8,
    PriceEurPerHour: 0.05,
}
```

**How CloudBroker chooses the provider**:
- If `AllowedProviders` contains only one provider → returns that provider
- If `AllowedProviders` contains multiple providers → CloudBroker selects based on:
  - Price (lowest cost)
  - Availability
  - Region constraints
  - Other business logic

### 5. NodeClass Matching

The controller matches a NodeClass to the provider:

```go
// For each NodeClass in namespace:
if provider == "gcp" {
    // Check if NodeClass has GCP config
    if nodeClass.Spec.GCP.Project != "" && nodeClass.Spec.GCP.Zone != "" {
        return nodeClass  // Match!
    }
}
if provider == "scaleway" {
    // Check if NodeClass has Scaleway config
    if nodeClass.Spec.Scaleway != nil && nodeClass.Spec.Scaleway.Zone != "" {
        return nodeClass  // Match!
    }
}
```

### 6. NodeClaim Creation

A NodeClaim is created with:

```yaml
spec:
  nodePoolRef: "my-nodepool"
  nodeClassRef: "matched-nodeclass"
  chosen:
    provider: "gcp"  # From CloudBroker recommendation
    region: "europe-west1"
    instanceType: "n1-standard-2"
    # ... other fields
```

## Example Scenarios

### Scenario 1: Single Provider NodePool

```yaml
apiVersion: cloudburst.io/v1alpha1
kind: NodePool
metadata:
  name: gcp-pool
spec:
  requirements:
    allowedProviders: ["gcp"]  # Only GCP allowed
```

**Result**: CloudBroker will always return `provider: "gcp"` (if GCP instances are available).

### Scenario 2: Multi-Provider NodePool

```yaml
apiVersion: cloudburst.io/v1alpha1
kind: NodePool
metadata:
  name: multi-provider-pool
spec:
  requirements:
    allowedProviders: ["gcp", "scaleway"]  # Both allowed
```

**Result**: CloudBroker chooses between GCP and Scaleway based on price/availability. The controller then matches the appropriate NodeClass.

### Scenario 3: Multiple NodePools

```yaml
# NodePool 1: GCP only
apiVersion: cloudburst.io/v1alpha1
kind: NodePool
metadata:
  name: gcp-pool
spec:
  requirements:
    allowedProviders: ["gcp"]
  template:
    labels:
      cloudburst.io/provider: "gcp"

---
# NodePool 2: Scaleway only
apiVersion: cloudburst.io/v1alpha1
kind: NodePool
metadata:
  name: scaleway-pool
spec:
  requirements:
    allowedProviders: ["scaleway"]
  template:
    labels:
      cloudburst.io/provider: "scaleway"
```

**Pods with node selectors**:

```yaml
# Pod targeting GCP
apiVersion: v1
kind: Pod
spec:
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
        - matchExpressions:
          - key: cloudburst.io/provider
            operator: In
            values: ["gcp"]
```

**Current Behavior**:
- Both NodePools see the unschedulable pod
- Both NodePools try to create nodes
- The first one to succeed "wins"
- Pod gets scheduled on whichever node becomes ready first

**Ideal Behavior** (future enhancement):
- Only the NodePool whose template labels match the pod's node selectors should create nodes
- This requires filtering pods by node selector compatibility

## Key Points

1. **Provider selection is NodePool-driven**: Each NodePool specifies `allowedProviders`
2. **CloudBroker makes the final decision**: When multiple providers are allowed, CloudBroker chooses based on price/availability
3. **NodeClass matching is automatic**: Controller finds NodeClass that supports the provider
4. **Multiple NodePools can compete**: Currently, all NodePools see all pods (no pod filtering)

## Future Enhancements

1. **Pod-to-NodePool Matching**: Filter pods based on node selectors matching NodePool template labels
2. **NodePool Priority**: When multiple NodePools match, use weight/priority to choose
3. **Provider Preference**: Allow specifying provider preference/priority in NodePool
4. **Cost-Based Selection**: Expose CloudBroker's cost comparison to users

## Code References

- **NodePool Controller**: `internal/controller/nodepool_controller.go`
  - `calculateDesiredCapacity()`: Detects unschedulable pods
  - `filterEligiblePods()`: Filters pods for demand detection
  - `createNodeClaim()`: Creates NodeClaim with provider from CloudBroker
  
- **CloudBroker Client**: `internal/cloudbroker/client.go`
  - `Recommend()`: Calls CloudBroker API with `AllowedProviders`

- **NodeClass Matching**: `internal/controller/nodepool_controller.go`
  - `nodeClassSupportsProvider()`: Checks if NodeClass supports provider
