# E2E All-Providers Test: Controller Log Analysis Plan

## Overview

The all-providers integration test (`hack/integration-test-e2e-all-providers.sh`) runs 8 cloud providers (GCP, Scaleway, Hetzner, DigitalOcean, Aruba, OVH, AWS, Azure) simultaneously. Controller logs are dumped to `controller-e2e-all-providers.log` at the end of the test (during cleanup, before namespace deletion).

## Test Status

- **Script**: `bash hack/integration-test-e2e-all-providers.sh`
- **Duration**: Typically 60–90+ minutes (CloudBroker ingest ~30 min, 8 VM provisions, Playwright, scale-down)
- **Log file**: `controller-e2e-all-providers.log` (or set `CONTROLLER_LOG_FILE` env)
- **Enabled providers**: Detected from env (gcp, scaleway, hetzner, digitalocean, aruba, ovh, aws, azure)

## Controller Log Analysis Checklist

### 1. Concurrency Issues

| Pattern | What to search | Action |
|---------|----------------|--------|
| **Reconciliation conflicts** | `"conflict"`, `"resource version"`, `"already exists"` | Check for optimistic locking / update conflicts when 8 NodeClaims reconcile in parallel |
| **Race conditions** | `"concurrent"`, `"race"`, `"panic"` | Look for goroutine panics or data races |
| **Duplicate work** | Same NodeClaim name reconciled multiple times in quick succession | May indicate queue flooding or redundant enqueues |
| **Stale cache** | `"not found"` after create, or outdated status | Controller-runtime cache consistency |

### 2. Graph / API Issues

| Pattern | What to search | Action |
|---------|----------------|--------|
| **Graph API errors** | `/api/clusters/.*/graph`, `graph`, `500`, `error` | UI graph endpoint failures during Playwright |
| **Missing resources** | NodeClaim/Node not in list when expected | Graph or dashboard API returning incomplete data |
| **CloudBroker** | `cloudbroker`, `CloudBroker`, `404`, `timeout` | CloudBroker client errors (config, instance types) |

### 3. Provider-Specific Errors

| Pattern | What to search | Action |
|---------|----------------|--------|
| **Provisioning failures** | `"failed to create"`, `"provision"`, `error` | Per-provider create/delete errors |
| **Provider names** | `gcp`, `scaleway`, `hetzner`, `digitalocean`, `aruba`, `ovh`, `aws`, `azure` | Correlate errors with provider |
| **Finalizer / delete** | `"failed to delete instance"`, `finalizer` | Scale-down / cleanup failures |

### 4. Structural Patterns

| Pattern | What to search | Action |
|---------|----------------|--------|
| **Requeue** | `"requeue"`, `"Requeue"` | Excessive requeues may indicate retry loops |
| **Backoff** | `"backoff"`, `"rate limit"` | Provider rate limiting |
| **Timeout** | `"timeout"`, `"context deadline"` | Slow cloud APIs or network issues |

## Known Controller Behavior

- **controller-runtime**: Default `MaxConcurrentReconciles` is 1 per controller. NodeClaim and NodePool reconciliations are serial per controller.
- **NodePool → NodeClaim**: One NodePool creates one NodeClaim per pending pod. With 8 NodePools, 8 NodeClaims are created in parallel (by different NodePool reconciliations).
- **NodeClaim**: Each NodeClaim reconciliation is independent (create VM, wait for ready, join cluster). No shared mutable state between NodeClaims.

## Fix Plan (If Issues Found)

### Concurrency / Conflict

1. **Update conflicts**: Ensure all status updates use `Patch` with `resourceVersion` or `StrategicMergePatch` to avoid overwriting concurrent updates.
2. **MaxConcurrentReconciles**: If conflicts persist, consider lowering to 1 for NodeClaim controller (already default) or adding per-NodePool rate limiting.
3. **Finalizer ordering**: Ensure instance deletion completes before removing finalizer to avoid orphaned VMs.

### Graph / API

1. **Graph endpoint**: Verify `/api/clusters/{id}/graph` aggregates NodeClaims and Nodes correctly under load.
2. **Caching**: If graph returns stale data, add cache invalidation or shorter TTL when NodeClaims change.
3. **CloudBroker**: Ensure CloudBroker URL is reachable from the controller (Tailscale/host network).

### Provider-Specific

1. **Retries**: Each provisioner has retry logic; verify backoff and max retries are appropriate.
2. **Timeouts**: Increase provisioning timeouts for slow providers (e.g. Azure, AWS).
3. **Credentials**: Ensure all 8 provider secrets are present and valid in `cloudburst-system` namespace.

## Running the Analysis

```bash
# After test completes, analyze the log
cd /Users/diegobraga/cloudburst-autoscaler

# Quick scan for errors
grep -iE "error|panic|conflict|failed" controller-e2e-all-providers.log | head -50

# Count errors by type
grep -c "error" controller-e2e-all-providers.log

# Provider-specific
for p in gcp scaleway hetzner digitalocean aruba ovh aws azure; do
  echo "=== $p ==="
  grep -i "$p" controller-e2e-all-providers.log | grep -iE "error|failed" | tail -5
done

# Reconcile flow (NodeClaim names)
grep "Reconciling NodeClaim" controller-e2e-all-providers.log | wc -l
```

## Optional: Keep Cluster for Manual Inspection

Run with `--keep-cluster` to skip Kind and instance cleanup, then inspect manually:

```bash
./hack/integration-test-e2e-all-providers.sh --keep-cluster
# On failure or after completion:
kubectl logs -n cloudburst-system -l app=cloudburst-controller --tail=10000 > controller-e2e-all-providers.log
```
