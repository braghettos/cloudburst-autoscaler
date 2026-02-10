# Tailscale Networking: Kind + GCP VMs

How GCP VMs provisioned by the controller reach a local Kind cluster over Tailscale -- without public IPs, port forwarding, or an in-cluster proxy.

## How It Works

```
┌─────────────────┐         ┌──────────────────┐         ┌─────────────────┐
│  Developer PC   │         │   Tailscale      │         │   GCP VM        │
│  (behind NAT)   │◄───────►│   VPN Mesh       │◄───────►│  (provisioned)  │
│                 │         │                  │         │                 │
│  Kind cluster   │         │  Outbound-only   │         │  Worker node    │
│  API server     │         │  connectivity    │         │  joins cluster  │
└─────────────────┘         └──────────────────┘         └─────────────────┘
```

Three things make this work:

1. **Kind binds the API server to the host.**
   The Kind config uses `networking.apiServerAddress: "0.0.0.0"` and `apiServerPort: 6443`. Docker then binds the host's port 6443 to the control-plane container's API server. The host machine listens on `0.0.0.0:6443`.

2. **Tailscale runs on the same host.**
   The host gets a Tailscale IP (e.g. `100.x.x.x`). Any TCP connection to that Tailscale IP hits the host, and the host already has port 6443 bound by Kind. So `https://<host-tailscale-IP>:6443` routes: **VM → Tailscale mesh → host → host:6443 → Docker → Kind API server**.

3. **TLS certificate SANs.**
   The API server's serving certificate must include the Tailscale IP. `hack/create-kind.sh` injects `HOST_TS_IP` into the Kind config's `certSANs` so VMs can validate TLS when connecting via the Tailscale address.

No Kubernetes operator or in-cluster proxy is needed.

## Setting Up the Kind Cluster

```bash
# Set your Tailscale IP so the API server cert includes it
export HOST_TS_IP=$(tailscale ip -4)
./hack/create-kind.sh
```

The script:
- Renders `hack/kind-config.yaml.tmpl` with the Tailscale IP in `certSANs`
- Creates/recreates the Kind cluster named `cloudburst` (configurable via `KIND_CLUSTER_NAME`)
- Verifies the API server is reachable at `https://${HOST_TS_IP}:6443`

### Manual Alternative

```bash
export HOST_TS_IP=$(tailscale ip -4)
export KIND_CLUSTER_NAME=cloudburst
envsubst < hack/kind-config.yaml.tmpl > hack/kind-config.yaml
kind create cluster --name cloudburst --config hack/kind-config.yaml
```

Verify: `curl -k https://${HOST_TS_IP}:6443/healthz` should return `ok`.

## Tailscale Auth Key Options

**Static auth key** (simplest, used in integration tests):
1. Go to [Tailscale Admin Console → Keys](https://login.tailscale.com/admin/settings/keys)
2. Generate a reusable, ephemeral auth key
3. Store it in a Kubernetes Secret and reference it via `tailscale.authKeySecretRef` in the NodeClass

**OAuth client** (recommended for longer-lived setups):
1. Create an OAuth client in the Tailscale admin console
2. Set `TAILSCALE_OAUTH_CLIENT_ID` and `TAILSCALE_OAUTH_CLIENT_SECRET`
3. The controller exchanges these for a short-lived auth key per VM

Nodes using ephemeral keys are automatically removed from the tailnet when they shut down or run `tailscale logout`.

## Firewall Configuration

If the host has a firewall, allow Tailscale traffic on port 6443:

```bash
# UFW
sudo ufw allow in on tailscale0 to any port 6443

# iptables
sudo iptables -A INPUT -i tailscale0 -p tcp --dport 6443 -j ACCEPT
```

## What Happens on the GCP VM

The startup script (`internal/bootstrap/startup_script.go`) runs these networking steps:

1. **Tailscale install + auth** -- connects the VM to the same tailnet as the developer PC
2. **API server reachability check** -- `curl -k https://<HOST_TS_IP>:6443/healthz`
3. **kubeadm join** -- uses the Tailscale IP endpoint and a discovery kubeconfig (not insecure token discovery)
4. **kubelet node-ip** -- set to the VM's Tailscale IP so all cluster traffic routes through the mesh
5. **Fallback CNI** -- if no CNI config appears within 30 seconds after join (e.g., Kindnet doesn't support external nodes), a minimal bridge config is written using the node's `podCIDR`

## Troubleshooting

### TLS SAN Mismatch

**Symptom**: `x509: certificate is valid for ... not for <tailscale-ip>`

**Fix**: Ensure `HOST_TS_IP` was set to your Tailscale IP when the Kind cluster was created. Recreate with:

```bash
export HOST_TS_IP=$(tailscale ip -4)
./hack/create-kind.sh
```

### VM Cannot Reach API Server

**Symptom**: NodeClaim stuck in `Joining`, VM logs show connection timeouts.

**Checklist**:
1. Tailscale IP is correct: `tailscale ip -4` on host
2. Firewall allows port 6443 on `tailscale0` (see above)
3. Tailscale auth key is valid and not expired
4. SSH into the VM and test directly:
   ```bash
   gcloud compute ssh <instance-name> --zone=<zone>
   curl -k https://<HOST_TAILSCALE_IP>:6443/healthz
   ```

### Token Expired

**Symptom**: `kubeadm join` fails with token validation errors.

**Fix**: The controller generates a fresh bootstrap token per NodeClaim. If you see expiry errors, the VM took too long to start. Check GCP instance boot time and consider increasing `tokenTtlMinutes` in the NodeClass.

### NodeClaim Stuck in Provisioning

**Symptom**: NodeClaim stays in `Provisioning` phase.

**Checklist**:
1. Check GCP instance status: `gcloud compute instances describe <name> --zone=<zone>`
2. Verify GCP credentials are configured correctly
3. Check controller logs for API errors

### NodeClaim Stuck in Joining

**Symptom**: NodeClaim stays in `Joining` but the GCP VM is running.

**Checklist**:
1. `kubectl get nodes` -- check if the node has appeared
2. Check VM serial console for bootstrap errors:
   ```bash
   gcloud compute instances get-serial-port-output <name> --zone=<zone>
   ```
3. Look for:
   - Tailscale IP assignment
   - API server connectivity (healthz check)
   - kubeadm join output
   - CNI initialization

### Tailscale Auth Key Issues

**Symptom**: VM cannot join the Tailscale network.

**Checklist**:
1. Auth key is valid and not expired
2. Secret exists: `kubectl get secret tailscale-auth -o yaml`
3. Controller can read the Secret (check RBAC)
4. Fallback: set `TAILSCALE_AUTHKEY` env var on the controller

### Node Not Ready (CNI)

**Symptom**: Node appears in `kubectl get nodes` but stays `NotReady` with `NetworkPluginNotReady`.

**Fix**: The bootstrap script includes a fallback bridge CNI. If it didn't activate, SSH into the VM and check:
```bash
ls /etc/cni/net.d/
journalctl -u kubelet --no-pager | tail -50
```

The fallback CNI requires the node's `podCIDR` to be assigned. Verify with:
```bash
kubectl get node <name> -o jsonpath='{.spec.podCIDR}'
```
