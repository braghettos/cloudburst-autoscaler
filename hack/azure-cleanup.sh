#!/bin/bash
# Delete orphaned cb-* Azure VMs subscription-wide (any resource group).
# Use after E2E runs where scale-down may have left instances behind.
# VMs in unexpected resource groups (e.g. from different NodeClass configs) are now found.
#
# Env: AZURE_TENANT_ID, AZURE_CLIENT_ID, AZURE_CLIENT_SECRET, AZURE_SUBSCRIPTION_ID
#      Or run from project root with .env loaded: source .env && ./hack/azure-cleanup.sh

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
[ -f "${PROJECT_ROOT}/.env" ] && set -a && source "${PROJECT_ROOT}/.env" && set +a

AZURE_SUBSCRIPTION_ID="${AZURE_SUBSCRIPTION_ID:-}"

if ! command -v az &>/dev/null; then
    echo "az CLI not found; skipping Azure cleanup"
    exit 0
fi

# Check we have enough Azure creds (need at least subscription for subscription-wide list)
if ! az account show &>/dev/null; then
    echo "Azure not logged in (run 'az login'); skipping Azure cleanup"
    exit 0
fi

# Set subscription if provided
[ -n "${AZURE_SUBSCRIPTION_ID}" ] && az account set --subscription "${AZURE_SUBSCRIPTION_ID}" 2>/dev/null || true

# List cb-* VMs subscription-wide (name, resourceGroup) - catches orphans in any RG
# Query returns [name, resourceGroup] per VM; -o tsv gives "name\tresourceGroup" per line
vms_tsv=$(az vm list --query "[?starts_with(name, 'cb-')].[name, resourceGroup]" -o tsv 2>/dev/null || true)

total=0
while IFS=$'\t' read -r vm_name rg; do
    [ -z "$vm_name" ] || [ -z "$rg" ] && continue
    echo "Deleting Azure VM: $vm_name (resource group: $rg)..."
    az vm delete --resource-group "$rg" --name "$vm_name" --yes --no-wait 2>/dev/null || true
    total=$((total + 1))
done <<< "$vms_tsv"

if [ $total -gt 0 ]; then
    echo "Delete requested for $total cb-* Azure VM(s) (async)"
else
    echo "No cb-* Azure VMs found in subscription"
fi
