<p align="center">
  <img src="assets/logo.png" alt="Cloudburst Autoscaler" width="200">
</p>

<h1 align="center">Cloudburst Autoscaler</h1>

A Kubernetes controller that automatically provisions cloud VMs as worker nodes in response to unschedulable pod demand. When pods cannot be scheduled due to insufficient capacity, the controller requests an optimal instance from [CloudBroker](https://github.com/braghettos/cloudbroker), provisions a VM (GCP or Scaleway), bootstraps it into the cluster via `kubeadm join`, and removes it when no longer needed.

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
│   ├── scaleway/                    # Scaleway instance provisioner
│   │   └── provisioner.go
│   ├── planner/                     # Capacity planning utilities
│   │   └── planner.go
│   └── tailscale/                   # Tailscale API client
│       └── client.go
├── config/                          # Kubernetes manifests (kustomize)
│   ├── crd/                         # CRD definitions
│   ├── rbac/                        # RBAC rules
│   ├── manager/                     # Controller Deployment
│   ├── cloudbroker/                 # CloudBroker ConfigMap
│   ├── default/                     # Kustomize overlay (combines all)
│   └── samples/                     # Example CRs (GCP + Scaleway)
├── hack/                            # Development scripts
│   ├── create-kind.sh               # Create local Kind cluster
│   ├── integration-test.sh          # NodeClaim lifecycle test
│   ├── integration-test-e2e.sh      # Full E2E test (scale-up + scale-down)
│   └── test-multi-provider.sh       # Multi-provider test (GCP + Scaleway)
└── docs/                            # Additional documentation
    ├── E2E_MANUAL_TEST.md           # Manual E2E testing guide
    ├── architecture.md
    ├── provider-selection.md
    └── tailscale-kind-gcp.md
```

## Prerequisites

- Go 1.21+
- Docker
- kubectl
- [Kind](https://kind.sigs.k8s.io/) (for local development)
- [Tailscale](https://tailscale.com/) (for node-to-control-plane networking)
- GCP service account with Compute Engine permissions (for GCP provider)
- Scaleway API keys (for Scaleway provider)
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

**Prerequisites:**
Create a `.env` file from `.env.example` and configure required variables:
```bash
cp .env.example .env
# Edit .env with your values:
#   - GOOGLE_APPLICATION_CREDENTIALS (path to GCP service account JSON)
#   - TAILSCALE_AUTHKEY (Tailscale auth key)
#
# Note: GCP_PROJECT and GCP_ZONE are configured in NodeClass resources,
#       not in the .env file
```

**Local Kind Deploy (Recommended for development):**
```bash
# Create Kind cluster, build image locally, load into Kind, and deploy everything
make kind-setup-and-deploy IMG=cloudburst-autoscaler:latest
```

**Build & Deploy (auto-detects Kind vs registry):**
```bash
# Builds image, detects Kind cluster (loads locally) or pushes to registry, deploys
make build-and-deploy IMG=cloudburst-autoscaler:latest
```

**Deploy Only (image must already exist):**
```bash
make deploy-all IMG=cloudburst-autoscaler:latest
```

**Step-by-Step Deploy:**
```bash
make create-secrets        # Create namespace and secrets (from .env)
make install               # Install CRDs
make deploy IMG=...        # Deploy the controller
make undeploy              # Undeploy
```

**Override Variables:**
```bash
make deploy-all \
  IMG=my-registry/cloudburst:latest \
  GCP_CREDENTIALS_FILE=/custom/path/gcp-sa.json \
  TAILSCALE_AUTHKEY=tskey-auth-...
```

**Note:** The Makefile reads `GOOGLE_APPLICATION_CREDENTIALS` and `TAILSCALE_AUTHKEY` from `.env`. See [docs/E2E_MANUAL_TEST.md](docs/E2E_MANUAL_TEST.md) for full testing instructions.

### Run Locally (development)

```bash
# Create a Kind cluster with Tailscale connectivity
./hack/create-kind.sh

# Run the controller against the Kind cluster
export GOOGLE_APPLICATION_CREDENTIALS=/path/to/sa.json
export TAILSCALE_AUTHKEY=tskey-auth-...
export CLOUDBROKER_BASEURL=http://localhost:8000
make run
```

### Integration Tests

```bash
# Required environment variables (see .env.example)
export GOOGLE_APPLICATION_CREDENTIALS=/path/to/sa.json
export TAILSCALE_AUTHKEY=tskey-auth-...

# NodeClaim lifecycle test (manual NodeClaim creation)
./hack/integration-test.sh

# Full E2E test (pod triggers autoscaling, then scale-down)
./hack/integration-test-e2e.sh
```

## Configuration

### Controller Configuration

The controller requires the following Kubernetes resources:

#### CloudBroker ConfigMap (`cloudbroker-config`)

Configures the CloudBroker API connection:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: cloudbroker-config
  namespace: cloudburst-system
data:
  baseurl: "http://cloudbroker-service:8000"  # CloudBroker API URL
  timeout-seconds: "5"  # Optional: API request timeout
```

#### Secrets

- **GCP Credentials** (`gcp-credentials`): Secret containing GCP service account JSON key
  - Key: `credentials.json`
  - Mounted at: `/var/run/secrets/gcp/credentials.json`
- **Tailscale Auth Key** (`tailscale-auth`): Secret containing Tailscale auth key
  - Key: `authkey`

#### Provider Configuration

Provider-specific configuration (GCP project/zone, Scaleway project ID/zone/organization ID, etc.) is defined in **NodeClass** resources, not in the controller ConfigMap or environment variables. This allows multiple providers and configurations to coexist.

**Note**: Provider-specific settings like `GCP_PROJECT`, `GCP_ZONE`, `SCW_PROJECT_ID`, and `SCW_DEFAULT_ZONE` should be configured in NodeClass resources, not in the `.env` file.

### Environment Variables (Local Development Only)

When running locally with `make run`, these environment variables are used:

| Variable | Required | Description |
|----------|----------|-------------|
| `GOOGLE_APPLICATION_CREDENTIALS` | Yes | Path to GCP service account JSON key |
| `TAILSCALE_AUTHKEY` | Yes | Tailscale auth key for node registration |
| `CLOUDBROKER_BASEURL` | No | CloudBroker API URL (default: `http://localhost:8000`) |

**Note**: `GCP_PROJECT` and `GCP_ZONE` are no longer required as environment variables. These are now specified in NodeClass resources.

### Example Resources

```yaml
# GCP NodeClass example
apiVersion: cloudburst.io/v1alpha1
kind: NodeClass
metadata:
  name: gcp-standard
spec:
  gcp:
    project: my-project
    zone: us-central1-a
    imageFamily: ubuntu-2204-lts      # default
    imageProject: ubuntu-os-cloud     # default
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
# Scaleway NodeClass example
apiVersion: cloudburst.io/v1alpha1
kind: NodeClass
metadata:
  name: scaleway-standard
spec:
  scaleway:
    zone: fr-par-1
    projectID: "your-project-id"
    organizationID: "your-org-id"  # optional
    image: ubuntu_jammy  # optional, defaults to Ubuntu 22.04
    apiKeySecretRef:
      name: scaleway-credentials
      key: SCW_SECRET_KEY  # Secret must contain SCW_SECRET_KEY (required) and SCW_ACCESS_KEY (recommended)
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
# Example secret for Scaleway credentials
apiVersion: v1
kind: Secret
metadata:
  name: scaleway-credentials
  namespace: default
type: Opaque
stringData:
  SCW_SECRET_KEY: "your-scaleway-secret-key"
  SCW_ACCESS_KEY: "your-scaleway-access-key"  # Recommended for proper authentication
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

## Documentation

- [Architecture](docs/architecture.md) -- CRD design, controller interaction, bootstrap process, networking
- [Provider Selection Flow](docs/provider-selection.md) -- how providers are chosen end-to-end
- [Tailscale + Kind + GCP Networking](docs/tailscale-kind-gcp.md) -- how VMs reach a local Kind cluster over Tailscale
- [End-to-End Manual Testing Guide](docs/E2E_MANUAL_TEST.md) -- full step-by-step testing instructions
- [Multi-Provider Testing Guide](hack/MULTI_PROVIDER_TEST.md) -- testing with GCP and Scaleway simultaneously

## License

Apache License 2.0
