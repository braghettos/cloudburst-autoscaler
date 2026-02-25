# Architecture

## Overview

Cloudburst Autoscaler is a Kubernetes operator built with [controller-runtime](https://github.com/kubernetes-sigs/controller-runtime) that extends a cluster by provisioning cloud VMs on demand. It is inspired by [Karpenter](https://karpenter.sh/) but focused on burst scenarios where workloads temporarily need external compute capacity.

The controller watches for unschedulable pods, consults [CloudBroker](https://github.com/braghettos/cloudbroker) for optimal instance pricing, provisions cloud VMs (GCP, Scaleway, Hetzner, DigitalOcean, OVH, AWS, Azure), and bootstraps them into the cluster using `kubeadm join` over a Tailscale network.

## CRD Design

### NodePool

The **NodePool** is the primary user-facing resource. It declares:

- **Requirements**: Constraints for instance selection (architecture, max price, **allowed providers** - determines which cloud providers can be used)
- **Limits**: Maximum and minimum node counts
- **Disruption**: Scale-down policies (TTL after empty, TTL until expired)
- **Template**: Labels and taints to apply to provisioned nodes

The NodePool controller is the brain of the system. It:
1. Watches all Pods for scheduling state changes
2. Calculates aggregate unschedulable demand (CPU millicore, memory bytes)
3. Calls CloudBroker with `allowedProviders` constraint to get instance recommendations
4. Matches NodeClass to the provider from CloudBroker recommendation
5. Creates NodeClaims when demand exceeds available capacity
6. Periodically checks for scale-down opportunities (every 30s requeue)

### NodeClass

The **NodeClass** defines infrastructure-level configuration:

- **GCP Config** (optional): Project and zone for GCP VM provisioning
- **Scaleway Config** (optional): Zone, image, and credentials for Scaleway server provisioning
- **Hetzner Config** (optional): Location and API token for Hetzner Cloud VMs
- **DigitalOcean Config** (optional): Region and API token for DigitalOcean Droplets
- **OVH Config** (optional): Project ID, region, and OAuth credentials for OVH Cloud
- **AWS Config** (optional): Region, AMI, subnet, security groups for AWS EC2
- **Azure Config** (optional): Subscription, resource group, location, subnet for Azure VM
- **Join Config**: Control plane API server address (Tailscale IP), token TTL
- **Tailscale**: Auth key secret reference for node registration
- **Bootstrap**: Kubernetes version to install on the VM

NodeClass is automatically matched to the provider from CloudBroker recommendation. A NodeClass must have configuration for at least one provider.

### NodeClaim

A **NodeClaim** represents a single provisioned node's full lifecycle. It is created by the NodePool controller (not by users directly) and progresses through phases:

```
Pending -> Provisioning -> Joining -> Ready -> Deleting
```

- **Pending**: CloudBroker recommendation acquired, ready to provision
- **Provisioning**: Cloud VM creation in progress (GCP, Scaleway, Hetzner, DigitalOcean, OVH, AWS, or Azure)
- **Joining**: VM is running, waiting for `kubeadm join` to complete and node to report Ready
- **Ready**: Node is a healthy member of the cluster
- **Deleting**: Node is being cordoned, drained, and the VM deleted

## Controller Interaction

```
 Pod events ──> DemandDetector ──> enqueues NodePool reconciliation
                                          │
                                   NodePool Controller
                                   ├── calculateDesiredCapacity()
                                   ├── checkScaleDown()
                                   └── createNodeClaim() ──> NodeClaim CR
                                                                  │
                                                          NodeClaim Controller
                                                          ├── handlePending()    → CloudBroker + provider create (GCP, Scaleway, Hetzner, DO, OVH, AWS, Azure)
                                                          ├── handleProvisioning() → wait for VM
                                                          ├── handleJoining()    → wait for K8s node Ready
                                                          └── handleDeleting()   → cordon + drain + provider delete
```

### DemandDetector

A lightweight controller that watches Pod create/update/delete events. For each pod change, it checks if there are any NodePools in the same namespace and enqueues them for reconciliation. This decouples pod-watching from the NodePool logic.

### NodePool Controller

The core scheduling brain:

1. **Demand Calculation**: Lists all pods across all namespaces, identifies unschedulable ones (Pending phase + PodScheduled=False + reason=Unschedulable), sums their CPU and memory requests.

2. **Scale-Up**: If `nodesNeeded > activeNodeClaims`, creates one NodeClaim per reconciliation (rate-limited to avoid over-provisioning while the cache is stale). The NodeClaim is sized to fit the aggregate demand.

3. **Scale-Down**: Checks each Ready NodeClaim:
   - `ttlSecondsAfterEmpty`: If node has no non-DaemonSet pods and has been ready longer than the TTL, delete it.
   - `ttlSecondsUntilExpired`: If node age exceeds the TTL, delete it regardless of occupancy.
   - Safety guard: Never scale below `minNodes`.

4. **Periodic Requeue**: When active NodeClaims exist but there's no demand, the controller requeues every 30 seconds to re-evaluate scale-down conditions (e.g., after pods finish terminating).

### NodeClaim Controller

Manages the full VM lifecycle:

1. **Pending -> Provisioning**: Generates a `kubeadm join` token via the bootstrap package, builds a cloud-init startup script, and calls the provisioner (created via `internal/provisioner.NewFromNodeClass`) to create the VM (e.g. GCP, Scaleway, Hetzner, DigitalOcean, OVH, AWS, Azure).

2. **Provisioning -> Joining**: Polls the cloud instance status until it's running.

3. **Joining -> Ready**: Lists K8s nodes and matches by name (hostname = NodeClaim name). Waits for the node to report `Ready` condition. Has a 20-minute timeout for the entire join process.

4. **Ready**: No-op; node is serving workloads.

5. **Deleting**: Cordons the node, drains non-DaemonSet pods, deletes the K8s Node object, calls the provisioner's `DeleteInstance()`, and finally deletes the NodeClaim CR.

## Bootstrap Process

The startup script (`internal/bootstrap/startup_script.go`) generates a comprehensive cloud-init script that runs on the GCP VM at first boot:

1. **System setup**: Sets hostname to NodeClaim name, configures `/etc/hosts`
2. **Tailscale**: Installs and authenticates Tailscale for network connectivity
3. **Container runtime**: Installs containerd with systemd cgroup driver
4. **Kubernetes packages**: Installs kubelet, kubeadm, kubectl at the specified version
5. **Kubelet config**: Sets node-ip to Tailscale IP, configures systemd cgroup driver
6. **kubeadm join**: Runs `kubeadm join` with a discovery kubeconfig (avoids insecure token discovery). Includes a background watcher that strips incompatible `cgroupRoot` settings from kubelet config as soon as it's written.
7. **Fallback CNI**: After join, if no CNI config appears within 30 seconds, queries the node's `podCIDR` and writes a minimal bridge CNI configuration. This ensures the node becomes Ready even when the cluster's DaemonSet-based CNI (e.g., Kindnet) doesn't support external nodes.

## Networking

The controller uses [Tailscale](https://tailscale.com/) as the network fabric between the control plane and GCP VMs:

- The Kind/local cluster's API server is exposed via the host's Tailscale IP
- GCP VMs install Tailscale and authenticate using a pre-shared auth key
- Kubelet on the VM uses the Tailscale IP as its `--node-ip`
- This provides encrypted, NAT-traversing connectivity without public IPs

## Naming Convention

A single identifier is shared across all three representations of a node:

| Resource | Name | Example |
|----------|------|---------|
| NodeClaim CR | `cb-<nodepool>-<suffix>` | `cb-burst-pool-x7kq2` |
| Kubernetes Node | Same (set via hostname) | `cb-burst-pool-x7kq2` |
| GCP VM Instance | Same (direct mapping) | `cb-burst-pool-x7kq2` |

The `cb-` prefix enables:
- Easy identification of cloudburst-managed resources
- Simple GCP cleanup queries (`name~^cb-`)
- Consistent correlation across `kubectl`, `gcloud`, and logs

## Provisioner Interface

All cloud providers implement the common `Provisioner` interface (`internal/provisioner/provisioner.go`): `CreateInstance`, `DeleteInstance`, `IsInstanceRunning`, and `Close`. The controller uses `provisioner.NewFromNodeClass()` to build the correct provisioner from the NodeClaim's chosen provider and the NodeClass spec (credentials are read from Kubernetes secrets or environment). This keeps the controller free of per-provider switch logic.

## E2E Tests

Per-provider E2E scripts in `hack/` run a full scale-up and scale-down cycle on a single Kind cluster:

- `integration-test-e2e-gcp.sh` (GCP)
- `integration-test-e2e-hetzner.sh`, `integration-test-e2e-digitalocean.sh`, `integration-test-e2e-ovh.sh`
- `integration-test-e2e-scaleway.sh`, `integration-test-e2e-aws.sh`, `integration-test-e2e-azure.sh`

`integration-test-e2e-all-providers.sh` detects which providers have credentials set and lists the corresponding per-provider scripts.

## External Dependencies

| Dependency | Purpose | Required |
|-----------|---------|----------|
| [CloudBroker](https://github.com/braghettos/cloudbroker) | Instance type recommendation (cheapest match) | Yes |
| GCP Compute Engine | VM provisioning | Yes |
| Tailscale | Control plane <-> VM networking | Yes |
| kubeadm | Cluster join mechanism | Yes (on VM) |
