# Disaster Recovery Use Case

When your primary cloud or region has an outage, Cloudburst can burst to nodes on alternative providers. No manual failover—the controller provisions where capacity exists.

## How It Works

Cloudburst uses **NodePool** `allowedProviders` to control which clouds it may use. In a disaster scenario:

1. **Normal state**: NodePool allows your primary provider (e.g. GCP).
2. **Outage detected**: You update the NodePool to exclude the affected provider and allow fallback providers (e.g. Hetzner, Scaleway).
3. **Automatic failover**: Pending pods trigger new NodeClaims. CloudBroker recommends the cheapest instance from the remaining providers. The controller provisions on the fallback cloud.

Because Cloudburst is **multi-cloud by design**, switching providers is a policy change—no code changes, no manual VM creation.

## Simulation

The script `hack/simulate-disaster-recovery.sh` demonstrates this flow:

1. **Phase 1 (primary)**: NodePool allows only the primary provider. A pending pod triggers provisioning on that cloud.
2. **Phase 2 (simulate outage)**: NodePool is patched to remove the primary and allow fallback providers.
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

# Option B: Use existing cluster (after running integration-test-e2e-all-providers.sh)
# Use this for GCP, AWS, Azure, or other provider combinations.
./hack/integration-test-e2e-all-providers.sh --no-scale-down   # one-time setup
./hack/simulate-disaster-recovery.sh --use-existing-cluster

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

- **Detecting outages**: Use monitoring (e.g. provider status pages, health checks) to trigger NodePool updates. Consider automation: when primary provider APIs return errors, patch NodePool via a small operator or CI job.
- **Region-level failover**: For region-specific outages, use `regionConstraint` (e.g. `EU`) and ensure fallback providers have capacity in the same region.
- **Cost**: Fallback providers may have different pricing. CloudBroker always returns the cheapest option within the allowed set.
