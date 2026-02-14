#!/usr/bin/env bash
# align-env.sh — Add any keys from .env.example that are missing in .env.
# Existing values in .env are never changed. Run from project root.
#
# Usage: ./hack/align-env.sh   or   make env-align

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
ENV_EXAMPLE="${PROJECT_ROOT}/.env.example"
ENV_FILE="${PROJECT_ROOT}/.env"

if [ ! -f "${ENV_EXAMPLE}" ]; then
  echo "No .env.example found at ${ENV_EXAMPLE}" >&2
  exit 1
fi

if [ ! -f "${ENV_FILE}" ]; then
  cp "${ENV_EXAMPLE}" "${ENV_FILE}"
  echo "Created .env from .env.example. Edit .env with your values."
  exit 0
fi

missing=()
while IFS= read -r line; do
  if [[ "$line" =~ ^([A-Za-z_][A-Za-z0-9_]*)= ]]; then
    key="${BASH_REMATCH[1]}"
    if ! grep -q "^${key}=" "${ENV_FILE}" 2>/dev/null; then
      missing+=("$line")
    fi
  fi
done < "${ENV_EXAMPLE}"

if [ ${#missing[@]} -eq 0 ]; then
  echo ".env already has all keys from .env.example. Nothing to add."
  exit 0
fi

echo "Adding ${#missing[@]} missing key(s) to .env:"
printf '%s\n' "${missing[@]}"
{
  echo ""
  echo "# ── Added by align-env.sh ($(date +%Y-%m-%d)) ─────────────────────────────"
  printf '%s\n' "${missing[@]}"
} >> "${ENV_FILE}"
echo "Done. Review .env and replace placeholder values if needed."
