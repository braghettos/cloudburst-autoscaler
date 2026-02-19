#!/bin/bash
# Delete orphaned cb-* GCP VMs across all zones in the project.
# Use after E2E runs where scale-down may have left instances behind.
#
# Env: GCP_PROJECT, GOOGLE_APPLICATION_CREDENTIALS (or gcloud auth)
#      Or run from project root with .env loaded: source .env && ./hack/gcp-cleanup.sh

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
[ -f "${PROJECT_ROOT}/.env" ] && set -a && source "${PROJECT_ROOT}/.env" && set +a

GCP_PROJECT="${GCP_PROJECT:-}"
# When GCP_ZONE is set, only instances in that zone are deleted (scope from NodeClass).
GCP_ZONE="${GCP_ZONE:-}"

if [ -z "${GCP_PROJECT}" ]; then
    echo "GCP_PROJECT not set; skipping GCP cleanup"
    exit 0
fi

if ! command -v gcloud &>/dev/null; then
    echo "gcloud CLI not found; skipping GCP cleanup"
    exit 0
fi

# List cb-* instances (project-wide, or in GCP_ZONE when set)
total=0
zones_arg=""
[ -n "${GCP_ZONE}" ] && zones_arg="--zones=${GCP_ZONE}"
while IFS=',' read -r name zone; do
    [ -z "$name" ] && continue
    echo "Deleting $name (zone: $zone)..."
    gcloud compute instances delete "$name" --zone="${zone}" --project="${GCP_PROJECT}" --quiet 2>/dev/null || true
    total=$((total + 1))
done < <(gcloud compute instances list --project="${GCP_PROJECT}" --filter="name~^cb-" ${zones_arg} --format="csv[no-heading](name,zone.basename())" 2>/dev/null || true)

if [ $total -gt 0 ]; then
    echo "Deleted $total cb-* VM(s)"
else
    echo "No cb-* GCP VMs found in project ${GCP_PROJECT}"
fi
