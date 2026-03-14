#!/bin/bash
set -euo pipefail

# End-to-end integration test for the AWS provider.
# See hack/e2e-common.sh for the shared test framework.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ── Provider Configuration ────────────────────────────────────────────────────

PROVIDER_NAME="aws"
NODECLASS_NAME="e2e-aws-nodeclass"
NODEPOOL_NAME="e2e-aws-nodepool"
PENDING_POD_NAME="e2e-aws-resource-hungry-pod"
VM_DELETION_TIMEOUT="${VM_DELETION_TIMEOUT:-300}"

AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID:?AWS_ACCESS_KEY_ID must be set}"
AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY:?AWS_SECRET_ACCESS_KEY must be set}"
AWS_REGION="${AWS_REGION:-eu-west-1}"
AWS_AMI="${AWS_AMI:-}"
AWS_SUBNET_ID="${AWS_SUBNET_ID:-}"
AWS_SECURITY_GROUP_IDS="${AWS_SECURITY_GROUP_IDS:-}"

# Source shared library
source "${SCRIPT_DIR}/e2e-common.sh"

# ── AMI Resolution ────────────────────────────────────────────────────────────

resolve_aws_ami() {
    if [ -n "${AWS_AMI}" ]; then return; fi
    if ! command -v aws &>/dev/null; then
        log_error "AWS_AMI not set and aws CLI not available for auto-resolution"
        exit 1
    fi
    log_info "Resolving latest Ubuntu 22.04 AMI for ${AWS_REGION}..."
    AWS_AMI=$(aws ec2 describe-images --region "${AWS_REGION}" --owners 099720109477 \
        --filters "Name=name,Values=ubuntu/images/hvm-ssd/ubuntu-jammy-22.04-amd64-server-*" "Name=state,Values=available" \
        --query 'Images | sort_by(@, &CreationDate) | [-1].ImageId' --output text 2>/dev/null || true)
    [ -z "${AWS_AMI}" ] && { log_error "Could not auto-resolve AWS AMI. Set AWS_AMI in .env"; exit 1; }
    log_info "✓ Resolved AMI: ${AWS_AMI}"
}

# ── Provider Callbacks ────────────────────────────────────────────────────────

provider_check_credentials() {
    [ -z "${AWS_ACCESS_KEY_ID}" ] && { log_error "AWS_ACCESS_KEY_ID not set"; missing=$((missing+1)); }
    [ -z "${AWS_SECRET_ACCESS_KEY}" ] && { log_error "AWS_SECRET_ACCESS_KEY not set"; missing=$((missing+1)); }
    if ! command -v aws &>/dev/null; then log_warn "aws CLI not found; cleanup/verify may be limited"; fi
    resolve_aws_ami
}

provider_create_controller_secrets() {
    kubectl create secret generic aws-credentials \
        --from-literal=AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID}" \
        --from-literal=AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY}" \
        --namespace="${CONTROLLER_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -
}

provider_create_test_secrets() {
    log_info "Creating AWS credentials secret in test namespace..."
    kubectl create secret generic aws-credentials \
        --from-literal=AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID}" \
        --from-literal=AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY}" \
        --namespace="${TEST_NAMESPACE}" \
        --dry-run=client -o yaml | kubectl apply -f -
    log_info "✓ AWS secret created"
}

provider_controller_env_block() {
    cat <<EOF
        - name: AWS_ACCESS_KEY_ID
          valueFrom:
            secretKeyRef:
              name: aws-credentials
              key: AWS_ACCESS_KEY_ID
        - name: AWS_SECRET_ACCESS_KEY
          valueFrom:
            secretKeyRef:
              name: aws-credentials
              key: AWS_SECRET_ACCESS_KEY
        - name: AWS_REGION
          value: "${AWS_REGION}"
EOF
}

provider_nodeclass_spec() {
    {
        echo "  aws:"
        echo "    region: \"${AWS_REGION}\""
        echo "    ami: \"${AWS_AMI}\""
        [ -n "${AWS_SUBNET_ID}" ] && echo "    subnetID: \"${AWS_SUBNET_ID}\""
        if [ -n "${AWS_SECURITY_GROUP_IDS}" ]; then
            echo "    securityGroupIDs:"
            IFS=',' read -ra sgs <<< "${AWS_SECURITY_GROUP_IDS}"
            for sg in "${sgs[@]}"; do
                echo "    - \"$(echo "$sg" | xargs)\""
            done
        fi
        echo "    accessKeyIDSecretRef:"
        echo "      name: aws-credentials"
        echo "      key: AWS_ACCESS_KEY_ID"
        echo "    secretAccessKeySecretRef:"
        echo "      name: aws-credentials"
        echo "      key: AWS_SECRET_ACCESS_KEY"
    }
}

provider_cleanup_vms() {
    log_info "Cleaning up existing cb-* AWS instances (region: ${AWS_REGION})..."
    if ! command -v aws &>/dev/null; then
        log_warn "aws CLI not found; skipping AWS cleanup"
        return 0
    fi
    local regions="${AWS_CLEANUP_REGIONS:-${AWS_REGION}}"
    for region in $regions; do
        local ids
        ids=$(aws ec2 describe-instances --region "$region" \
            --filters "Name=tag:Name,Values=cb-*" "Name=instance-state-name,Values=pending,running,stopping,stopped" \
            --query 'Reservations[].Instances[].InstanceId' --output text 2>/dev/null || true)
        if [ -n "$ids" ]; then
            log_info "Terminating AWS instances in ${region}: ${ids}"
            aws ec2 terminate-instances --region "$region" --instance-ids $ids 2>/dev/null || true
        fi
    done
    log_info "✓ AWS cleanup done"
}

provider_verify_vm_deleted() {
    local instance_id="$1"
    local timeout="${2:-${VM_DELETION_TIMEOUT}}"
    # instance_id format: aws/<region>/<instance-id>
    local region ec2_id
    region=$(echo "$instance_id" | cut -d'/' -f2)
    ec2_id=$(echo "$instance_id" | cut -d'/' -f3)
    [ -z "$ec2_id" ] && { log_warn "Could not parse EC2 instance ID"; return 0; }
    [ -z "$region" ] && region="${AWS_REGION}"

    if ! command -v aws &>/dev/null; then
        log_warn "aws CLI not found; skipping verification"
        return 0
    fi

    log_info "Verifying AWS instance ${ec2_id} is terminated (timeout: ${timeout}s)..."
    local elapsed=0
    while [ $elapsed -lt $timeout ]; do
        local state
        state=$(aws ec2 describe-instances --region "$region" --instance-ids "$ec2_id" \
            --query 'Reservations[0].Instances[0].State.Name' --output text 2>/dev/null || echo "terminated")
        if [ "$state" = "terminated" ]; then
            log_info "✓ AWS instance ${ec2_id} confirmed terminated"
            return 0
        fi
        echo -n "."
        sleep 15
        elapsed=$((elapsed + 15))
    done
    log_error "Timeout: AWS instance ${ec2_id} still exists (state: ${state:-unknown})"
    return 1
}

# ── Run ───────────────────────────────────────────────────────────────────────

main_e2e
