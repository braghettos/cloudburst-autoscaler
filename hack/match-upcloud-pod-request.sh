#!/bin/bash
# Match the UpCloud E2E hungry pod request with CloudBroker UpCloud instances.
# Pod: 1 vCPU, 1Gi memory. NodePool: maxPriceEurPerHour=0.01, arch=x86_64, allowedProviders=[upcloud]
# This script calls CloudBroker with the same params the controller uses and shows the match.

set -euo pipefail

CLOUDBROKER_URL="${CLOUDBROKER_BASEURL:-http://localhost:8000}"

echo "=== UpCloud E2E Pod Request vs CloudBroker Match ==="
echo ""
echo "Pod request (from integration-test-e2e-upcloud.sh):"
echo "  - CPU: 1"
echo "  - Memory: 1Gi"
echo "  - nodeSelector: cloudburst.io/nodepool=e2e-upcloud-nodepool"
echo ""
echo "NodePool requirements (from create_nodepool):"
echo "  - regionConstraint: ANY"
echo "  - arch: x86_64"
echo "  - maxPriceEurPerHour: 0.01 (UPCLOUD_MAX_PRICE_EUR_PER_HOUR)"
echo "  - allowedProviders: [upcloud]"
echo ""
echo "CloudBroker request (same as controller sends):"
REQ='{
  "min_vcpu": 1,
  "min_ram_gb": 1,
  "arch": "x86_64",
  "region_constraint": "ANY",
  "max_price_eur_per_hour": 0.01,
  "limit": 10,
  "allowed_providers": ["upcloud"]
}'
echo "$REQ" | python3 -m json.tool
echo ""

echo "CloudBroker response:"
RESP=$(curl -s -X POST "${CLOUDBROKER_URL}/api/recommendations" \
  -H "Content-Type: application/json" \
  -d "$REQ")

echo "$RESP" | python3 -m json.tool 2>/dev/null || echo "$RESP"
echo ""

# Parse and summarize
COUNT=$(echo "$RESP" | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
    recs = d.get('recommendations', [])
    print(len(recs))
except: print(0)
" 2>/dev/null || echo "0")

if [ "${COUNT:-0}" -gt 0 ]; then
    echo "=== Match: YES — $COUNT UpCloud recommendation(s) returned ==="
    echo ""
    echo "Instance types CloudBroker would recommend (used by UpCloud provisioner as Plan):"
    echo "$RESP" | python3 -c "
import sys, json
d = json.load(sys.stdin)
for r in d.get('recommendations', []):
    it = r.get('instance_type_name', '?')
    prov = r.get('provider_slug', '?')
    reg = r.get('region_slug', '?')
    vcpu = r.get('vcpu', '?')
    ram = r.get('ram_gb', '?')
    explain = r.get('explain', {})
    price = explain.get('price_eur_per_hour') if explain else r.get('price_eur_per_hour')
    print(f'  - {it} (provider={prov} region={reg} vcpu={vcpu} ram_gb={ram} price={price} EUR/h)')
" 2>/dev/null || true
else
    echo "=== Match: NO — 0 recommendations returned ==="
    echo ""
    echo "Checking with higher max_price to see if UpCloud instances exist..."
    REQ2='{"min_vcpu":1,"min_ram_gb":1,"arch":"x86_64","region_constraint":"ANY","max_price_eur_per_hour":1.0,"limit":20,"allowed_providers":["upcloud"]}'
    RESP2=$(curl -s -X POST "${CLOUDBROKER_URL}/api/recommendations" -H "Content-Type: application/json" -d "$REQ2")
    COUNT2=$(echo "$RESP2" | python3 -c "import sys,json; d=json.load(sys.stdin); print(len(d.get('recommendations',[])))" 2>/dev/null || echo "0")
    if [ "${COUNT2:-0}" -gt 0 ]; then
        echo "With max_price=1.0 EUR/h, CloudBroker returns $COUNT2 UpCloud recommendations."
        echo "Smallest UpCloud plans (price):"
        echo "$RESP2" | python3 -c "
import sys, json
d = json.load(sys.stdin)
recs = d.get('recommendations', [])
# Sort by price
def price(r):
    ex = r.get('explain', {})
    return ex.get('price_eur_per_hour') or r.get('price_eur_per_hour') or 999
recs = sorted(recs, key=price)[:5]
for r in recs:
    it = r.get('instance_type_name', '?')
    p = price(r)
    print(f'  - {it}: {p:.4f} EUR/h')
" 2>/dev/null || true
        echo ""
        echo "Conclusion: max_price_eur_per_hour=0.01 may be too low. Smallest UpCloud (DEV-1xCPU-1GB-10GB) is ~0.0045 EUR/h."
    else
        echo "No UpCloud recommendations even with max_price=1.0. Is UpCloud ingested? Run: cd cloudbroker && make ingest-upcloud"
    fi
fi
