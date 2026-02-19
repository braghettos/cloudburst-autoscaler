#!/bin/bash
# Terminate orphaned cb-* EC2 instances across regions.
# Use after E2E runs where scale-down may have left instances behind.
#
# Env: AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY (or default creds)
#      AWS_REGIONS: space-separated regions to check (default: major regions CloudBroker may use)
#      Or run from project root with .env loaded: source .env && ./hack/aws-cleanup.sh

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
[ -f "${PROJECT_ROOT}/.env" ] && set -a && source "${PROJECT_ROOT}/.env" && set +a

# When AWS_REGIONS is set (e.g. from e2e: AWS_REGIONS="${AWS_REGION}"), only those regions are checked.
# Default: broad list to catch orphaned instances from prior runs when no cluster exists.
AWS_REGIONS="${AWS_REGIONS:-us-east-1 us-east-2 us-west-1 us-west-2 eu-west-1 eu-central-1 ap-southeast-1 ap-northeast-1 ap-south-1 sa-east-1}"
AWS_REGION="${AWS_REGION:-eu-west-1}"

if ! command -v aws &>/dev/null; then
    echo "aws CLI not found; skipping AWS cleanup"
    exit 0
fi

if [ -z "${AWS_ACCESS_KEY_ID:-}" ] && [ -z "${AWS_PROFILE:-}" ] && ! aws sts get-caller-identity &>/dev/null; then
    echo "AWS credentials not configured; skipping AWS cleanup"
    exit 0
fi

total=0
for region in $AWS_REGIONS; do
    ids=$(aws ec2 describe-instances --region "$region" \
        --filters "Name=tag:Name,Values=cb-*" "Name=instance-state-name,Values=pending,running,stopping,stopped" \
        --query 'Reservations[*].Instances[*].InstanceId' --output text 2>/dev/null || true)
    if [ -n "$ids" ]; then
        for id in $ids; do
            echo "Terminating $id (region: $region)..."
            aws ec2 terminate-instances --region "$region" --instance-ids "$id" 2>/dev/null || true
            total=$((total + 1))
        done
    fi
done

if [ $total -gt 0 ]; then
    echo "Terminated $total cb-* instance(s)"
else
    echo "No cb-* EC2 instances found in regions: $AWS_REGIONS"
fi
