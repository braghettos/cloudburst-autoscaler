<p align="center">
  <img src="assets/logo.svg" alt="Cloudburst Autoscaler" width="200">
</p>

<h1 align="center">Cloudburst Autoscaler</h1>

A Kubernetes controller that automatically provisions cloud VMs as worker nodes in response to unschedulable pod demand. When pods cannot be scheduled due to insufficient capacity, the controller requests an optimal instance from [CloudBroker](https://github.com/braghettos/cloudbroker), provisions a GCP VM, bootstraps it into the cluster via `kubeadm join`, and removes it when no longer needed.

## Architecture

```
                          +------------------+
                          |  Pending Pod     |
                          |  (unschedulable) |
                          +--------+---------+
                                   |
                          watches pods
                                   |
                    +--------------v--------------+
                    |   NodePool Controller       |
                    |   (demand detection +       |
                    |    scale-up / scale-down)   |
                    +--------------+--------------+
                                   |
                     creates / deletes NodeClaim
                                   |
                    +--------------v--------------+
                    |   NodeClaim Controller      |
                    |   (lifecycle management)    |
                    +--------------+--------------+
                           |               |
              +------------+    +----------+----------+
              |                 |                      |
     +--------v-------+ +------v--------+   +---------v--------+
     | CloudBroker    | | GCP           |   | Bootstrap        |
     | (instance      | | Provisioner   |   | (kubeadm join    |
     |  selection)    | | (VM create/   |   |  startup script) |
     +----------------+ |  delete)      |   +------------------+
                        +---------------+
```

### Custom Resource Definitions

| CRD | Purpose |
|-----|---------|
| **NodePool** | Defines scaling policy: instance requirements (CPU, RAM, price, arch), limits (min/max nodes), and disruption settings (TTL after empty, TTL until expired). |
| **NodeClass** | Defines infrastructure: GCP project/zone, image family, cluster join config (API server address, token TTL), Tailscale auth, and Kubernetes version. |
| **NodeClaim** | Represents a single provisioned node. Created automatically by the NodePool controller when demand is detected. Lifecycle: `Pending` -> `Provisioning` -> `Joining` -> `Ready` -> `Deleting`. |

### Controllers

| Controller | Watches | Responsibility |
|-----------|---------|---------------|
| **NodePool** | Pods, NodeClaims, NodePools | Detects unschedulable pods, calculates required capacity, creates/deletes NodeClaims. Periodically re-checks for scale-down when active nodes exist. |
| **NodeClaim** | NodeClaims, Nodes | Manages the full VM lifecycle: calls CloudBroker for instance recommendation, provisions GCP VM with bootstrap script, monitors node join, handles deletion (cordon, drain, delete VM). |
| **NodeClass** | NodeClasses | Validates and reconciles NodeClass configuration. |
| **DemandDetector** | Pods | Enqueues NodePool reconciliation when pod scheduling state changes. |

### Naming Convention

All identifiers for a provisioned node are identical:

```
cb-<nodepool-name>-<k8s-suffix>
```

- **NodeClaim name** = **K8s Node name** = **GCP VM instance name**
- The `cb-` prefix identifies cloudburst-managed resources
- Example: `cb-production-pool-x7kq2`

### Scale-Up Flow

1. A pod is created that cannot be scheduled (insufficient CPU/memory)
2. `DemandDetector` detects the unschedulable pod and enqueues the `NodePool`
3. `NodePool` controller calculates required capacity from pod requests
4. Controller calls CloudBroker API to find the cheapest matching instance
5. A `NodeClaim` CR is created with the recommended instance details
6. `NodeClaim` controller provisions a GCP VM with a cloud-init script that:
   - Installs containerd, kubelet, kubeadm
   - Configures Tailscale for network connectivity
   - Runs `kubeadm join` to join the cluster
   - Sets up fallback CNI if needed
7. Once the node reports `Ready`, the NodeClaim transitions to `Ready`
8. The previously pending pod is scheduled onto the new node

### Scale-Down Flow

1. `NodePool` controller periodically checks active nodes (every 30s)
2. For each `Ready` NodeClaim, checks if the node is empty (no non-DaemonSet pods)
3. If empty and `ttlSecondsAfterEmpty` has elapsed since `ReadyAt`, initiates deletion
4. `NodeClaim` controller cordons the node, drains remaining pods, deletes the K8s Node object, deletes the GCP VM, and removes the NodeClaim CR

## Project Structure

```
.
├── main.go                          # Controller manager entry point
├── api/v1alpha1/                    # CRD type definitions
│   ├── nodeclaim_types.go
│   ├── nodeclass_types.go
│   └── nodepool_types.go
├── internal/
│   ├── controller/                  # Reconciliation logic
│   │   ├── nodepool_controller.go   # Scale-up/down decisions
│   │   ├── nodeclaim_controller.go  # VM lifecycle management
│   │   ├── nodeclaim_deletion.go    # Cordon, drain, delete logic
│   │   ├── nodeclass_controller.go  # NodeClass validation
│   │   └── demand_detector_controller.go  # Pod watch -> NodePool enqueue
│   ├── bootstrap/                   # VM bootstrap script generation
│   │   ├── join.go                  # kubeadm token + discovery kubeconfig
│   │   └── startup_script.go        # Full cloud-init script builder
│   ├── cloudbroker/                 # CloudBroker API client
│   │   └── client.go
│   ├── gcp/                         # GCP Compute Engine provisioner
│   │   ├── provisioner.go
│   │   └── instance_status.go
│   ├── planner/                     # Capacity planning utilities
│   │   └── planner.go
│   └── tailscale/                   # Tailscale API client
│       └── client.go
├── config/                          # Kubernetes manifests (kustomize)
│   ├── crd/                         # CRD definitions
│   ├── rbac/                        # RBAC rules
│   ├── manager/                     # Controller Deployment
│   ├── default/                     # Kustomize overlay (combines all)
│   └── samples/                     # Example CRs
├── hack/                            # Development scripts
│   ├── create-kind.sh               # Create local Kind cluster
│   ├── integration-test.sh          # NodeClaim lifecycle test
│   ├── integration-test-e2e.sh      # Full E2E test (scale-up + scale-down)
│   └── get-join.sh                  # Helper: get kubeadm join token
└── docs/                            # Additional documentation
```

## Prerequisites

- Go 1.21+
- Docker
- kubectl
- [Kind](https://kind.sigs.k8s.io/) (for local development)
- [Tailscale](https://tailscale.com/) (for node-to-control-plane networking)
- GCP service account with Compute Engine permissions
- [CloudBroker](https://github.com/braghettos/cloudbroker) instance running

## Quick Start

### Build

```bash
make build              # Build the manager binary
make docker-build       # Build the Docker image
make manifests          # Generate CRD manifests
make generate           # Generate DeepCopy methods
```

### Deploy to a Cluster

```bash
# Install CRDs
make install

# Deploy the controller (uses kustomize)
make deploy IMG=ghcr.io/braghettos/cloudburst-autoscaler:latest

# Undeploy
make undeploy
```

### Run Locally (development)

```bash
# Create a Kind cluster with Tailscale connectivity
./hack/create-kind.sh

# Run the controller against the Kind cluster
export GCP_PROJECT=my-project
export GCP_ZONE=us-central1-a
export GOOGLE_APPLICATION_CREDENTIALS=/path/to/sa.json
export TAILSCALE_AUTHKEY=tskey-auth-...
export CLOUDBROKER_BASEURL=http://localhost:8000
make run
```

### Integration Tests

```bash
# Required environment variables (see .env.example)
export GCP_PROJECT=my-project
export GOOGLE_APPLICATION_CREDENTIALS=/path/to/sa.json
export TAILSCALE_AUTHKEY=tskey-auth-...
export GCP_ZONE=us-central1-a

# NodeClaim lifecycle test (manual NodeClaim creation)
./hack/integration-test.sh

# Full E2E test (pod triggers autoscaling, then scale-down)
./hack/integration-test-e2e.sh
```

## Configuration

### Environment Variables

| Variable | Required | Description |
|----------|----------|-------------|
| `GCP_PROJECT` | Yes | GCP project ID |
| `GCP_ZONE` | Yes | GCP zone for VM provisioning |
| `GOOGLE_APPLICATION_CREDENTIALS` | Yes | Path to GCP service account JSON key |
| `TAILSCALE_AUTHKEY` | Yes | Tailscale auth key for node registration |
| `CLOUDBROKER_BASEURL` | No | CloudBroker API URL (default: `http://localhost:8000`) |

### Example Resources

```yaml
apiVersion: cloudburst.io/v1alpha1
kind: NodeClass
metadata:
  name: gcp-standard
spec:
  gcp:
    project: my-project
    zone: us-central1-a
  imageFamily: ubuntu-2204-lts
  imageProject: ubuntu-os-cloud
  join:
    hostApiServer: "https://<tailscale-ip>:6443"
    tokenTtlMinutes: 60
  tailscale:
    authKeySecretRef:
      name: tailscale-auth
      key: authkey
  bootstrap:
    kubernetesVersion: "1.34.3"
---
apiVersion: cloudburst.io/v1alpha1
kind: NodePool
metadata:
  name: burst-pool
spec:
  requirements:
    arch: ["x86_64"]
    maxPriceEurPerHour: 0.15
    allowedProviders: ["gcp"]
  limits:
    maxNodes: 5
    minNodes: 0
  disruption:
    ttlSecondsAfterEmpty: 300
    ttlSecondsUntilExpired: 3600
```

## License

Apache License 2.0
