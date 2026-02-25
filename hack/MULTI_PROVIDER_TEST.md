# Multi-Provider Testing Guide

This guide explains how to test Cloudburst Autoscaler with pods provisioned from multiple cloud providers simultaneously in a single Kind cluster.

## Overview

The test script `integration-test-e2e-all-providers.sh` runs a full E2E flow that:

1. Stops CloudBroker, deletes any existing Kind cluster
2. Starts CloudBroker, runs migrations, ingests all providers
3. Creates a Kind cluster, installs CRDs, builds and loads the controller
4. Deploys the controller with **all enabled provider credentials** from `.env`
5. Creates one NodeClass (with configs for all enabled providers), Tailscale secret
6. For each enabled provider: creates NodePool + pending pod
7. Waits for all NodeClaims Ready and all pods Running
8. Deletes pods and waits for scale-down (mandatory)

Providers are auto-detected from environment variables. Set credentials in `.env` for each provider you want to test.

## Prerequisites

### Per-provider credentials

| Provider   | Required env vars |
|-----------|-------------------|
| GCP       | `GOOGLE_APPLICATION_CREDENTIALS`, `GCP_PROJECT` |
| Scaleway  | `SCW_SECRET_KEY` |
| Hetzner   | `HETZNER_API_TOKEN` |
| DigitalOcean | `DIGITALOCEAN_API_TOKEN` |
| OVH       | `OVH_APPLICATION_KEY`, `OVH_APPLICATION_SECRET`, `OVH_CONSUMER_KEY`, `OVH_PROJECT_ID` |
| AWS       | `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` (and `AWS_AMI` or AWS CLI for auto-resolve) |
| Azure     | `AZURE_TENANT_ID`, `AZURE_CLIENT_ID`, `AZURE_CLIENT_SECRET`, `AZURE_SUBSCRIPTION_ID`, `AZURE_RESOURCE_GROUP`, `AZURE_SUBNET_ID` |

### Common

- `TAILSCALE_AUTHKEY` for VPN connectivity
- Tailscale running and connected
- Docker, Kind, kubectl installed
- CloudBroker cloned and configurable (defaults to `../cloudbroker`)

## Running the Test

```bash
# Set required environment variables in .env (at least one provider + TAILSCALE_AUTHKEY)
cp .env.example .env
# Edit .env with your credentials

# Run the test (tests all providers with credentials in .env)
./hack/integration-test-e2e-all-providers.sh

# Skip scale-down phase
./hack/integration-test-e2e-all-providers.sh

# Custom CloudBroker directory
./hack/integration-test-e2e-all-providers.sh --cloudbroker-dir /path/to/cloudbroker
```

## How It Works

### NodePool per provider

For each enabled provider, the script creates:

- A NodePool with `allowedProviders: ["<provider>"]`
- A pod with `nodeSelector.cloudburst.io/nodepool: e2e-<provider>-nodepool`

The controller creates NodeClaims when pods are pending; CloudBroker returns recommendations, and the controller picks the matching NodeClass (single NodeClass has configs for all providers).

### Single NodeClass

One NodeClass is created with provider configs for all enabled providers. The controller matches the recommended provider from CloudBroker to the corresponding NodeClass spec (gcp, scaleway, hetzner, etc.).

## Alternative: Per-provider E2E scripts

For testing a single provider at a time:

- `./hack/integration-test-e2e-gcp.sh` — GCP only
- `./hack/integration-test-e2e-hetzner.sh`
- `./hack/integration-test-e2e-scaleway.sh`
- `./hack/integration-test-e2e-digitalocean.sh`
- `./hack/integration-test-e2e-ovh.sh`
- `./hack/integration-test-e2e-aws.sh`
- `./hack/integration-test-e2e-azure.sh`

Or run all per-provider tests sequentially:

```bash
./hack/run-all-e2e.sh
```

## Troubleshooting

- **No provider credentials set**: Ensure at least one provider’s env vars are set in `.env`
- **TAILSCALE_AUTHKEY not set**: Required for node join; add to `.env`
- **CloudBroker not healthy**: Ensure `../cloudbroker` (or `CLOUDBROKER_DIR`) is correct and `make migrate` + `make ingest-all` succeed
- **Timeout waiting for NodeClaims**: Check controller logs: `kubectl logs -n cloudburst-system -l app=cloudburst-controller --tail=80`
