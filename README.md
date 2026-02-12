<p align="center">
  <img src="assets/logo.png" alt="Cloudburst Autoscaler" width="200">
</p>

<h1 align="center">Cloudburst Autoscaler</h1>

<p align="center">
  Kubernetes controller that automatically provisions cloud VMs as worker nodes in response to unschedulable pod demand &mdash; and removes them when no longer needed.
</p>

## Architecture

```mermaid
graph TB
    POD["Pending Pod<br/>(unschedulable)"]

    subgraph Controllers ["Cloudburst Controllers"]
        DD["Demand Detector<br/>(watches pods)"]
        NPC["NodePool Controller<br/>(scale-up / scale-down)"]
        NCC["NodeClaim Controller<br/>(VM lifecycle)"]
    end

    CB["CloudBroker API<br/>(instance recommendation)"]

    subgraph Providers ["Cloud Providers"]
        GCP["GCP<br/>Compute Engine"]
        SCW["Scaleway<br/>Instances"]
        HTZ["Hetzner<br/>Cloud"]
    end

    BS["Bootstrap<br/>(cloud-init + kubeadm join<br/>over Tailscale)"]
    NODE["New K8s Node<br/>(Ready)"]

    POD -->|"detected"| DD
    DD -->|"enqueue"| NPC
    NPC -->|"POST /api/recommendations"| CB
    CB -->|"cheapest instance"| NPC
    NPC -->|"create NodeClaim"| NCC
    NCC -->|"provision VM"| Providers
    Providers --> BS
    BS -->|"node joins cluster"| NODE
    NODE -->|"pod scheduled"| POD

    NPC -->|"scale-down: delete NodeClaim"| NCC
    NCC -->|"cordon + drain + delete VM"| Providers

    style Controllers fill:#e8f4f8,stroke:#2196F3,color:#000
    style Providers fill:#fff3e0,stroke:#FF9800,color:#000
    style CB fill:#f3e5f5,stroke:#9C27B0,color:#000
    style BS fill:#e8f5e9,stroke:#4CAF50,color:#000
```

### How it works

**Scale-up:**

1. A pod is created that cannot be scheduled (insufficient CPU/memory).
2. `DemandDetector` detects the unschedulable pod and enqueues the `NodePool`.
3. `NodePool` controller calculates required capacity and calls [CloudBroker](https://github.com/braghettos/cloudbroker) for the cheapest matching instance.
4. A `NodeClaim` CR is created with the recommended instance details.
5. `NodeClaim` controller provisions a VM on the selected provider (GCP, Scaleway, or Hetzner).
6. The VM runs a cloud-init script that installs containerd, kubelet, kubeadm, joins Tailscale, and runs `kubeadm join`.
7. Once the node reports `Ready`, the pending pod is scheduled onto it.

**Scale-down:**

1. `NodePool` controller periodically checks active nodes (every 30s).
2. If a node is empty (no non-DaemonSet pods) and `ttlSecondsAfterEmpty` has elapsed, deletion is triggered.
3. `NodeClaim` controller cordons the node, drains it, deletes the Kubernetes node object, and deletes the cloud VM.

## Features

- **Multi-provider** &mdash; GCP Compute Engine, Scaleway Instances, Hetzner Cloud (more planned)
- **Cost-optimised** &mdash; CloudBroker picks the cheapest instance across all configured providers
- **Automatic lifecycle** &mdash; full scale-up and scale-down without manual intervention
- **Tailscale networking** &mdash; VMs join the cluster over a private Tailscale mesh (works with Kind, EKS, GKE, etc.)
- **CRD-driven** &mdash; NodePool, NodeClass, and NodeClaim resources for declarative configuration
- **Safe scale-down** &mdash; cordon, drain, then delete; respects TTL policies

### Custom Resource Definitions

| CRD | Purpose |
|-----|---------|
| **NodePool** | Scaling policy: instance requirements (CPU, RAM, price, arch, allowed providers), limits (min/max nodes), disruption settings (TTL after empty, TTL until expired). |
| **NodeClass** | Infrastructure config: provider credentials and settings (GCP project/zone, Scaleway zone/project, Hetzner location/token), cluster join config, Tailscale auth, Kubernetes version. |
| **NodeClaim** | Represents a single provisioned node. Created automatically by the NodePool controller. Lifecycle: `Pending` &rarr; `Provisioning` &rarr; `Joining` &rarr; `Ready` &rarr; `Deleting`. |

### Controllers

| Controller | Watches | Responsibility |
|-----------|---------|---------------|
| **NodePool** | Pods, NodeClaims, NodePools | Detects unschedulable pods, calculates required capacity, creates/deletes NodeClaims. Periodically re-checks for scale-down. |
| **NodeClaim** | NodeClaims, Nodes | Manages full VM lifecycle: calls CloudBroker, provisions VM with bootstrap script, monitors node join, handles deletion (cordon, drain, delete VM). |
| **NodeClass** | NodeClasses | Validates and reconciles NodeClass configuration. |
| **DemandDetector** | Pods | Enqueues NodePool reconciliation when pod scheduling state changes. |

### Naming Convention

All identifiers for a provisioned node are identical:

```
cb-<nodepool-name>-<random-suffix>
```

- **NodeClaim name** = **K8s Node name** = **Cloud VM instance name**
- The `cb-` prefix identifies Cloudburst-managed resources
- Example: `cb-production-pool-x7kq2`

## Tech Stack

| Layer | Technology |
|-------|-----------|
| Language | Go 1.21+ |
| Framework | controller-runtime (Kubebuilder) |
| CRDs | NodePool, NodeClass, NodeClaim |
| Cloud SDKs | google-cloud-go, scaleway-sdk-go, hcloud-go |
| Networking | Tailscale |
| Bootstrap | kubeadm + cloud-init |
| Containerisation | Docker |
| CI | GitHub Actions (lint, test, Docker build + push to GHCR) |

## Quick Start

### Prerequisites

- Go 1.21+
- Docker
- kubectl
- [Kind](https://kind.sigs.k8s.io/) (for local development)
- [Tailscale](https://tailscale.com/) (for node-to-control-plane networking)
- [CloudBroker](https://github.com/braghettos/cloudbroker) instance running
- Provider credentials (at least one): GCP service account, Scaleway API keys, or Hetzner API token

### Build

```bash
make build              # Build the manager binary
make docker-build       # Build the Docker image
make manifests          # Generate CRD manifests
make generate           # Generate DeepCopy methods
```

### Deploy to a Cluster

Create a `.env` file from `.env.example`:

```bash
cp .env.example .env
# Edit .env with your values:
#   - TAILSCALE_AUTHKEY (required)
#   - GOOGLE_APPLICATION_CREDENTIALS (for GCP)
#   - HETZNER_API_TOKEN (for Hetzner)
#   - SCW_ACCESS_KEY / SCW_SECRET_KEY (for Scaleway)
```

**Local Kind deploy (recommended for development):**

```bash
make kind-setup-and-deploy IMG=cloudburst-autoscaler:latest
```

**Build and deploy (auto-detects Kind vs registry):**

```bash
make build-and-deploy IMG=cloudburst-autoscaler:latest
```

**Step-by-step:**

```bash
make create-secrets        # Create namespace and secrets (from .env)
make install               # Install CRDs
make deploy IMG=...        # Deploy the controller
```

### Run Locally

```bash
./hack/create-kind.sh
export TAILSCALE_AUTHKEY=tskey-auth-...
export CLOUDBROKER_BASEURL=http://localhost:8000
make run
```

### Integration Tests

```bash
# Full E2E test (GCP): pod triggers autoscaling, then scale-down
./hack/integration-test-e2e.sh

# Full E2E test (Hetzner Cloud)
./hack/integration-test-e2e-hetzner.sh

# Multi-provider test (GCP + Scaleway)
./hack/test-multi-provider.sh

# NodeClaim lifecycle test (manual NodeClaim creation)
./hack/integration-test.sh
```

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
│   │   ├── nodepool_controller.go   # Scale-up / scale-down decisions
│   │   ├── nodeclaim_controller.go  # VM lifecycle management
│   │   ├── nodeclaim_deletion.go    # Cordon, drain, delete logic
│   │   ├── nodeclass_controller.go  # NodeClass validation
│   │   └── demand_detector_controller.go
│   ├── bootstrap/                   # VM bootstrap script generation
│   │   ├── join.go                  # kubeadm token + discovery kubeconfig
│   │   └── startup_script.go        # Full cloud-init script builder
│   ├── cloudbroker/                 # CloudBroker API client
│   ├── gcp/                         # GCP Compute Engine provisioner
│   ├── scaleway/                    # Scaleway Instances provisioner
│   ├── hetzner/                     # Hetzner Cloud provisioner
│   ├── planner/                     # Capacity planning utilities
│   └── tailscale/                   # Tailscale API client
├── config/                          # Kubernetes manifests (Kustomize)
│   ├── crd/                         # CRD definitions
│   ├── rbac/                        # RBAC rules
│   ├── manager/                     # Controller Deployment
│   ├── cloudbroker/                 # CloudBroker ConfigMap
│   └── samples/                     # Example CRs
├── hack/                            # Development and test scripts
│   ├── create-kind.sh               # Create local Kind cluster
│   ├── integration-test.sh          # NodeClaim lifecycle test
│   ├── integration-test-e2e.sh      # Full E2E test (GCP)
│   ├── integration-test-e2e-hetzner.sh  # Full E2E test (Hetzner)
│   └── test-multi-provider.sh       # Multi-provider test
└── docs/                            # Additional documentation
```

## Configuration

### Controller Kubernetes Resources

**CloudBroker ConfigMap** (`cloudbroker-config`):

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: cloudbroker-config
  namespace: cloudburst-system
data:
  baseurl: "http://cloudbroker-service:8000"
  timeout-seconds: "5"
```

**Secrets** (created automatically by `make create-secrets` from `.env`):

| Secret | Key | Description |
|--------|-----|-------------|
| `gcp-credentials` | `credentials.json` | GCP service account JSON key |
| `tailscale-auth` | `authkey` | Tailscale auth key |
| `hetzner-api-token` | `HETZNER_API_TOKEN` | Hetzner Cloud API token |
| `scaleway-credentials` | `SCW_SECRET_KEY`, `SCW_ACCESS_KEY` | Scaleway API keys |

### Environment Variables (local development)

| Variable | Required | Description |
|----------|----------|-------------|
| `TAILSCALE_AUTHKEY` | Yes | Tailscale auth key for node registration |
| `CLOUDBROKER_BASEURL` | No | CloudBroker API URL (default: `http://localhost:8000`) |
| `GOOGLE_APPLICATION_CREDENTIALS` | GCP only | Path to GCP service account JSON key |
| `HETZNER_API_TOKEN` | Hetzner only | Hetzner Cloud API token |
| `SCW_ACCESS_KEY` / `SCW_SECRET_KEY` | Scaleway only | Scaleway API credentials |

### Example Resources

```yaml
# Hetzner NodeClass
apiVersion: cloudburst.io/v1alpha1
kind: NodeClass
metadata:
  name: hetzner-standard
spec:
  hetzner:
    location: fsn1
    apiTokenSecretRef:
      name: hetzner-api-token
      key: HETZNER_API_TOKEN
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
# GCP NodeClass
apiVersion: cloudburst.io/v1alpha1
kind: NodeClass
metadata:
  name: gcp-standard
spec:
  gcp:
    project: my-project
    zone: us-central1-a
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
# Scaleway NodeClass
apiVersion: cloudburst.io/v1alpha1
kind: NodeClass
metadata:
  name: scaleway-standard
spec:
  scaleway:
    zone: fr-par-1
    projectID: "your-project-id"
    apiKeySecretRef:
      name: scaleway-credentials
      key: SCW_SECRET_KEY
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
# NodePool (works with any provider)
apiVersion: cloudburst.io/v1alpha1
kind: NodePool
metadata:
  name: burst-pool
spec:
  requirements:
    arch: ["x86_64"]
    maxPriceEurPerHour: 0.15
    allowedProviders: ["gcp", "hetzner", "scaleway"]
  limits:
    maxNodes: 5
    minNodes: 0
  disruption:
    ttlSecondsAfterEmpty: 300
    ttlSecondsUntilExpired: 3600
```

## Documentation

- [Architecture](docs/architecture.md) &mdash; CRD design, controller interaction, bootstrap process, networking
- [Provider Selection Flow](docs/provider-selection.md) &mdash; how providers are chosen end-to-end
- [Tailscale + Kind + GCP Networking](docs/tailscale-kind-gcp.md) &mdash; how VMs reach a local Kind cluster over Tailscale
- [End-to-End Manual Testing Guide](docs/E2E_MANUAL_TEST.md) &mdash; full step-by-step testing instructions
- [Multi-Provider Testing Guide](hack/MULTI_PROVIDER_TEST.md) &mdash; testing with GCP and Scaleway simultaneously

## License

Apache License 2.0
