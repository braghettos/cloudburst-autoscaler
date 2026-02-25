# Disaster Recovery Use Case

When your primary cloud or region has an outage, Cloudburst can burst to nodes on alternative providers. Two mechanisms work together:

## Automatic node-loss reconciliation

When a node leaves the cluster (e.g. VM terminated, provider failure), the NodeClaim controller:

1. **Detects** the node is gone (within ~60 seconds via periodic Ready-phase reconciliation).
2. **Transitions** the NodeClaim to Deleting and cleans it up, even if the provider API is unreachable.
3. **Frees capacity** so the NodePool can create a replacement when pending pods need scheduling.

No manual NodeClaim deletion is required. The controller reconciles automatically.

## Policy-based failover (NodePool)

To **steer** new capacity away from a failed provider, update **NodePool** `allowedProviders`:

1. **Normal state**: NodePool allows your primary provider (e.g. GCP).
2. **Outage detected**: Update the NodePool to exclude the affected provider and allow fallback providers (e.g. Hetzner, Scaleway).
3. **Automatic failover**: When the node is gone, the controller cleans up the stale NodeClaim. Pending pods trigger new NodeClaims. CloudBroker recommends from the updated allowed list. The controller provisions on the fallback cloud.

Because Cloudburst is **multi-cloud by design**, switching providers is a policy change—no code changes, no manual VM creation.

## Simulation

The script `hack/simulate-disaster-recovery.sh` demonstrates the full flow:

1. **Phase 1 (primary)**: NodePool allows only the primary provider. A pending pod triggers provisioning on that cloud.
2. **Phase 2 (simulate outage)**: NodePool is patched to exclude the primary and allow fallbacks. The burst node is deleted (simulates provider failure). The controller detects the node is gone and cleans up the NodeClaim automatically.
3. **Phase 3 (failover)**: A new pending pod triggers provisioning on a fallback provider.

### Prerequisites

- Kind cluster with Cloudburst controller deployed
- CloudBroker running and ingested for primary + fallback providers
- Credentials for at least two providers (e.g. GCP + Hetzner, or Scaleway + Hetzner)
- `TAILSCALE_AUTHKEY` set
- NodeClass with configs for all providers used in the test

### Quick Start

```bash
# From cloudburst-autoscaler repo root
cp .env.example .env
# Edit .env with primary + fallback provider credentials and TAILSCALE_AUTHKEY

# Option A: Full simulation with Scaleway + Hetzner (self-contained setup)
./hack/simulate-disaster-recovery.sh

# Option B: Use existing cluster (run simulate-disaster-recovery.sh first to create it)
# Use this for GCP, AWS, Azure, or other provider combinations.
./hack/simulate-disaster-recovery.sh   # creates cluster with nodes
./hack/simulate-disaster-recovery.sh --use-existing-cluster   # re-run DR simulation

# Customize primary and fallback
PRIMARY_PROVIDER=gcp FALLBACK_PROVIDERS=hetzner,scaleway ./hack/simulate-disaster-recovery.sh --use-existing-cluster
```

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `PRIMARY_PROVIDER` | `scaleway` | Provider used in Phase 1 (simulated primary) |
| `FALLBACK_PROVIDERS` | `hetzner` | Comma-separated fallback providers for Phase 3 |
| `CLOUDBROKER_DIR` | `../cloudbroker` | Path to CloudBroker repo |
| `KIND_CLUSTER_NAME` | `cloudburst` | Kind cluster name |
| `TAILSCALE_AUTHKEY` | (required) | Tailscale auth key for node join |

Provider-specific credentials (e.g. `SCW_SECRET_KEY`, `HETZNER_API_TOKEN`, `GOOGLE_APPLICATION_CREDENTIALS`) must be set in `.env` for each provider used.

## Production Considerations

- **Automatic recovery**: When a node disappears, the controller cleans up the NodeClaim and the NodePool creates a replacement. No NodePool patch is required for recovery.
- **Failover to a different provider**: To ensure new capacity goes to a different provider, patch NodePool `allowedProviders` when you know the primary is down. Use monitoring (e.g. provider status pages, health checks) or automation (e.g. when primary provider APIs return errors, patch NodePool via a small operator or CI job).
- **Alternatives fallback**: If the first provider in the CloudBroker recommendation fails at CreateInstance, the controller automatically tries the next alternative. So even without a NodePool patch, you may still get failover if the primary keeps failing.
- **Region-level failover**: For region-specific outages, use `regionConstraint` (e.g. `EU`) and ensure fallback providers have capacity in the same region.
- **Cost**: Fallback providers may have different pricing. CloudBroker always returns the cheapest option within the allowed set.
