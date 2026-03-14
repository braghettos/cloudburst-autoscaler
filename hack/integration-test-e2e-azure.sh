#!/bin/bash
set -euo pipefail

# End-to-end integration test for the Azure provider.
# See hack/e2e-common.sh for the shared test framework.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Skip UI/screenshots to focus on core VM lifecycle (set E2E_SKIP_UI=1)
export E2E_SKIP_UI="${E2E_SKIP_UI:-1}"

# Source shared library first (loads .env)
source "${SCRIPT_DIR}/e2e-common.sh"

# ── Provider Configuration ────────────────────────────────────────────────────

PROVIDER_NAME="azure"
NODECLASS_NAME="e2e-azure-nodeclass"
NODEPOOL_NAME="e2e-azure-nodepool"
PENDING_POD_NAME="e2e-azure-resource-hungry-pod"
PROVIDER_MAX_PRICE="0.15"

AZURE_SUBSCRIPTION_ID="${AZURE_SUBSCRIPTION_ID:?AZURE_SUBSCRIPTION_ID must be set}"
AZURE_RESOURCE_GROUP="${AZURE_RESOURCE_GROUP:?AZURE_RESOURCE_GROUP must be set}"
AZURE_LOCATION="${AZURE_LOCATION:-westeurope}"
# Optional: when empty, provisioner auto-creates VNet and subnet (cloudburst-vnet/default)
AZURE_SUBNET_ID="${AZURE_SUBNET_ID:-}"
AZURE_TENANT_ID="${AZURE_TENANT_ID:?AZURE_TENANT_ID must be set}"
AZURE_CLIENT_ID="${AZURE_CLIENT_ID:?AZURE_CLIENT_ID must be set}"
AZURE_CLIENT_SECRET="${AZURE_CLIENT_SECRET:?AZURE_CLIENT_SECRET must be set}"

# ── Provider Callbacks ────────────────────────────────────────────────────────

provider_check_credentials() {
    [ -z "${AZURE_SUBSCRIPTION_ID}" ] && { log_error "AZURE_SUBSCRIPTION_ID not set"; missing=$((missing+1)); }
    [ -z "${AZURE_RESOURCE_GROUP}" ] && { log_error "AZURE_RESOURCE_GROUP not set"; missing=$((missing+1)); }
    [ -z "${AZURE_TENANT_ID}" ] && { log_error "AZURE_TENANT_ID not set"; missing=$((missing+1)); }
    [ -z "${AZURE_CLIENT_ID}" ] && { log_error "AZURE_CLIENT_ID not set"; missing=$((missing+1)); }
    [ -z "${AZURE_CLIENT_SECRET}" ] && { log_error "AZURE_CLIENT_SECRET not set"; missing=$((missing+1)); }
}

provider_create_controller_secrets() {
    kubectl create secret generic azure-credentials \
        --from-literal=AZURE_TENANT_ID="${AZURE_TENANT_ID}" \
        --from-literal=AZURE_CLIENT_ID="${AZURE_CLIENT_ID}" \
        --from-literal=AZURE_CLIENT_SECRET="${AZURE_CLIENT_SECRET}" \
        --namespace="${CONTROLLER_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -
}

provider_create_test_secrets() {
    log_info "Creating Azure credentials secret in test namespace..."
    kubectl create secret generic azure-credentials \
        --from-literal=AZURE_TENANT_ID="${AZURE_TENANT_ID}" \
        --from-literal=AZURE_CLIENT_ID="${AZURE_CLIENT_ID}" \
        --from-literal=AZURE_CLIENT_SECRET="${AZURE_CLIENT_SECRET}" \
        --namespace="${TEST_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -
    log_info "✓ Azure credentials secret created"
}

provider_controller_env_block() {
    cat <<EOF
        - name: AZURE_SUBSCRIPTION_ID
          value: "${AZURE_SUBSCRIPTION_ID}"
        - name: AZURE_RESOURCE_GROUP
          value: "${AZURE_RESOURCE_GROUP}"
        - name: AZURE_LOCATION
          value: "${AZURE_LOCATION}"
        - name: AZURE_TENANT_ID
          valueFrom:
            secretKeyRef:
              name: azure-credentials
              key: AZURE_TENANT_ID
        - name: AZURE_CLIENT_ID
          valueFrom:
            secretKeyRef:
              name: azure-credentials
              key: AZURE_CLIENT_ID
        - name: AZURE_CLIENT_SECRET
          valueFrom:
            secretKeyRef:
              name: azure-credentials
              key: AZURE_CLIENT_SECRET
EOF
}

# Exclude Azure B-series (Standard_B*) - often 0 quota in free-tier subscriptions
provider_nodepool_requirements_extra() {
    cat <<EOF
    excludedInstanceTypePrefixes:
      - Standard_B
EOF
}

provider_nodeclass_spec() {
    cat <<EOF
  azure:
    subscriptionID: "${AZURE_SUBSCRIPTION_ID}"
    resourceGroup: "${AZURE_RESOURCE_GROUP}"
    location: "${AZURE_LOCATION}"
    subnetID: "${AZURE_SUBNET_ID}"
    tenantIDSecretRef:
      name: azure-credentials
      key: AZURE_TENANT_ID
    clientIDSecretRef:
      name: azure-credentials
      key: AZURE_CLIENT_ID
    clientSecretSecretRef:
      name: azure-credentials
      key: AZURE_CLIENT_SECRET
EOF
}

provider_cleanup_vms() {
    log_info "Cleaning up existing cb-* Azure VMs in resource group ${AZURE_RESOURCE_GROUP}..."
    if ! command -v az &>/dev/null; then
        log_warn "az CLI not found; skipping Azure VM cleanup"
        return 0
    fi

    # Clean up VMs
    local vms
    vms=$(az vm list --resource-group "${AZURE_RESOURCE_GROUP}" --query "[?starts_with(name, 'cb-')].name" -o tsv 2>/dev/null || true)
    if [ -n "$vms" ]; then
        for vm in $vms; do
            log_info "Deleting Azure VM: $vm"
            az vm delete --resource-group "${AZURE_RESOURCE_GROUP}" --name "$vm" --yes --force-deletion true 2>/dev/null || true
        done
        log_info "✓ Azure VM cleanup requested"
    else
        log_info "No cb-* VMs in resource group"
    fi

    # Sweep orphaned NICs
    local nics
    nics=$(az network nic list --resource-group "${AZURE_RESOURCE_GROUP}" --query "[?starts_with(name, 'cb-')].name" -o tsv 2>/dev/null || true)
    if [ -n "$nics" ]; then
        for nic in $nics; do
            log_info "Deleting orphaned NIC: $nic"
            az network nic delete --resource-group "${AZURE_RESOURCE_GROUP}" --name "$nic" 2>/dev/null || true
        done
    fi

    # Sweep orphaned OS disks
    local disks
    disks=$(az disk list --resource-group "${AZURE_RESOURCE_GROUP}" --query "[?starts_with(name, 'cb-')].name" -o tsv 2>/dev/null || true)
    if [ -n "$disks" ]; then
        for disk in $disks; do
            log_info "Deleting orphaned disk: $disk"
            az disk delete --resource-group "${AZURE_RESOURCE_GROUP}" --name "$disk" --yes 2>/dev/null || true
        done
    fi
}

provider_verify_vm_deleted() {
    local instance_id="$1"
    local timeout="${2:-${VM_DELETION_TIMEOUT}}"
    local vm_name

    vm_name=$(echo "$instance_id" | awk -F'/' '{print $NF}')
    [ -z "$vm_name" ] && { log_warn "Could not parse VM name from instance ID"; return 0; }

    if ! command -v az &>/dev/null; then
        log_warn "az CLI not found; skipping Azure VM deletion verification"
        return 0
    fi

    log_info "Verifying Azure VM ${vm_name} is deleted (timeout: ${timeout}s)..."
    local elapsed=0
    while [ $elapsed -lt $timeout ]; do
        if ! az vm show --resource-group "${AZURE_RESOURCE_GROUP}" --name "${vm_name}" &>/dev/null; then
            log_info "✓ Azure VM ${vm_name} confirmed deleted"

            # Verify associated resources are also cleaned up
            local nic_name="${vm_name}-nic"
            local disk_name="${vm_name}-osdisk"
            local orphans=0

            if az network nic show --resource-group "${AZURE_RESOURCE_GROUP}" --name "${nic_name}" &>/dev/null 2>&1; then
                log_error "Orphaned NIC found: ${nic_name}"
                orphans=$((orphans+1))
            else
                log_info "✓ NIC ${nic_name} cleaned up"
            fi

            if az disk show --resource-group "${AZURE_RESOURCE_GROUP}" --name "${disk_name}" &>/dev/null 2>&1; then
                log_error "Orphaned OS disk found: ${disk_name}"
                orphans=$((orphans+1))
            else
                log_info "✓ OS disk ${disk_name} cleaned up"
            fi

            [ $orphans -gt 0 ] && { log_error "Azure orphaned resources detected"; return 1; }
            return 0
        fi
        echo -n "."
        sleep 15
        elapsed=$((elapsed + 15))
    done
    log_error "Timeout: Azure VM ${vm_name} still exists"
    return 1
}

# ── Run ───────────────────────────────────────────────────────────────────────

main_e2e
