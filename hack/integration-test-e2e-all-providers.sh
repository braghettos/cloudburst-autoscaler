#!/bin/bash
set -euo pipefail

# Unified E2E test: run scale-up and scale-down for every provider that has
# credentials set in the environment. Uses a single Kind cluster and controller
# with all provider credentials; creates one NodeClass, NodePool, and pending pod
# per enabled provider, then verifies all NodeClaims reach Ready and all pods
# run, then deletes all pods and verifies scale-down to zero.
#
# This script invokes hack/integration-test-e2e-all-providers-full.sh which also
# manages CloudBroker (docker compose up, migrate, ingest-all).
#
# Environment: source .env and set credentials for each provider you want to test.
# CloudBroker ingest requires .env in ../cloudbroker with provider credentials.
# At least one provider must have credentials. TAILSCALE_AUTHKEY is required.
#
# Usage:
#   ./hack/integration-test-e2e-all-providers.sh [--no-scale-down] [--cloudbroker-dir /path]

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec "${SCRIPT_DIR}/integration-test-e2e-all-providers-full.sh" "$@"
