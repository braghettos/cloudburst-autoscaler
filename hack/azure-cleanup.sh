#!/bin/bash
# Delete orphaned cb-* Azure VMs in the resource group.
# Use after E2E runs where scale-down may have left instances behind.
#
# Env: AZURE_TENANT_ID, AZURE_CLIENT_ID, AZURE_CLIENT_SECRET, AZURE_SUBSCRIPTION_ID, AZURE_RESOURCE_GROUP
#      Or run from project root with .env loaded: source .env && ./hack/azure-cleanup.sh

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
[ -f "${PROJECT_ROOT}/.env" ] && set -a && source "${PROJECT_ROOT}/.env" && set +a

AZURE_RESOURCE_GROUP="${AZURE_RESOURCE_GROUP:-}"
AZURE_SUBSCRIPTION_ID="${AZURE_SUBSCRIPTION_ID:-}"

if [ -z "${AZURE_RESOURCE_GROUP}" ]; then
    echo "AZURE_RESOURCE_GROUP not set; skipping Azure cleanup"
    exit 0
fi

if ! command -v az &>/dev/null; then
    echo "az CLI not found; skipping Azure cleanup"
    exit 0
fi

# Set subscription if provided
[ -n "${AZURE_SUBSCRIPTION_ID}" ] && az account set --subscription "${AZURE_SUBSCRIPTION_ID}" 2>/dev/null || true

vms=$(az vm list --resource-group "${AZURE_RESOURCE_GROUP}" \
    --query "[?starts_with(name, 'cb-')].name" -o tsv 2>/dev/null || true)

total=0
for vm in $vms; do
    [ -z "$vm" ] && continue
    echo "Deleting Azure VM: $vm (resource group: ${AZURE_RESOURCE_GROUP})..."
    az vm delete --resource-group "${AZURE_RESOURCE_GROUP}" --name "$vm" --yes --no-wait 2>/dev/null || true
    total=$((total + 1))
done

if [ $total -gt 0 ]; then
    echo "Delete requested for $total cb-* Azure VM(s) (async)"
else
    echo "No cb-* Azure VMs found in resource group ${AZURE_RESOURCE_GROUP}"
fi
