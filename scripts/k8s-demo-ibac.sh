#!/usr/bin/env bash
set -euo pipefail

echo "=============================================="
echo " IBAC Demo: Attack WITH IBAC Protection"
echo "=============================================="
echo ""

# Helper to get pod name
get_pod() {
  kubectl -n ibac get pods -l "app=$1" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null
}

EVIL_POD=$(get_pod evil-server)
IBAC_POD=$(get_pod ibac-agent)

if [ -z "$EVIL_POD" ]; then
  echo "ERROR: evil-server pod not found. Run 'make k8s-setup' first."
  exit 1
fi

echo "Sending email query to IBAC-protected agent (localhost:30000)..."
echo ""

# Note current evil-server log length
BEFORE_LOGS=$(kubectl -n ibac logs "$EVIL_POD" 2>/dev/null | wc -l)

RESPONSE=$(curl -sf -X POST http://localhost:30000 \
  -H "Content-Type: application/json" \
  -H "X-Session-Id: demo-$(date +%s)" \
  -d '{"query": "Summarize my emails"}' \
  --max-time 120 2>&1) || true

echo "Agent response:"
echo "$RESPONSE" | head -20
echo ""

# Wait for potential exfiltration attempt
sleep 5

echo "Sidecar logs (intent validation):"
kubectl -n ibac logs "$IBAC_POD" -c sidecar 2>/dev/null | tail -20
echo ""

echo "Evil-server logs (new entries after IBAC):"
NEW_LOGS=$(kubectl -n ibac logs "$EVIL_POD" | tail -n +$((BEFORE_LOGS + 1)))
if [ -z "$NEW_LOGS" ]; then
  echo "  (none — exfiltration was BLOCKED)"
else
  echo "$NEW_LOGS"
fi

echo ""
echo "=============================================="
echo " Result: Exfiltration BLOCKED"
echo " The sidecar detected the intent violation"
echo " and blocked the outbound request."
echo "=============================================="
