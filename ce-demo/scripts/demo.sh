#!/usr/bin/env bash
# Shared runner for CE-Manager demo scenarios. Usage:
#   scripts/demo.sh off       # CE_MODE=off      — Q2 overflows
#   scripts/demo.sh on        # CE_MODE=on       — programmatic masker kicks in
#   scripts/demo.sh summary   # CE_MODE=summary  — LLM rewrites old turns as prose
#   scripts/demo.sh truncate  # CE_MODE=truncate — deterministic eviction
set -euo pipefail

MODE="${1:-on}"
case "$MODE" in
  off|on|summary|truncate) ;;
  *) echo "Usage: $0 off|on|summary|truncate"; exit 2 ;;
esac

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

AGENT_URL="${CE_AGENT_URL:-http://localhost:30021}"
OBSERVER_URL="${OBSERVER_URL:-http://localhost:30071}"
SESSION_ID="ce-demo-${MODE}-$(date +%s)"

QUERIES_FILE="$ROOT_DIR/testdata/ce-demo-queries.json"
Q1="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["q1"])' "$QUERIES_FILE")"
Q2="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["q2"])' "$QUERIES_FILE")"

echo "=============================================="
echo " CE-Manager Demo  (CE_MODE=$MODE)"
echo "=============================================="
echo ""
echo "Agent URL:    $AGENT_URL"
echo "Observer URL: $OBSERVER_URL/session/$SESSION_ID"
echo "Session ID:   $SESSION_ID"
echo ""

echo "Flipping CE_MODE=$MODE on the ce-proxy deployment..."
kubectl -n ibac set env deployment/ce-proxy "CE_MODE=$MODE" >/dev/null
kubectl -n ibac rollout status deployment/ce-proxy --timeout=120s >/dev/null

echo "Resetting CE-Manager state..."
kubectl -n ibac exec deploy/ce-proxy -- \
  python -c "import urllib.request; urllib.request.urlopen(urllib.request.Request('http://localhost:9100/admin/reset', method='POST', data=b''), timeout=5).read()" \
  >/dev/null 2>&1 || true
echo ""

register_event() {
  curl -sf -X POST "$OBSERVER_URL/api/events" \
    -H "Content-Type: application/json" \
    -d "$(printf '{"session_id":"%s","source":"%s","stage":"%s","status":"%s","title":"%s","summary":"%s"}' \
      "$SESSION_ID" "$1" "$2" "$3" "$4" "$5")" >/dev/null || true
}

post_turn() {
  local query="$1"
  local tmp http_code
  tmp="$(mktemp)"
  http_code=$(curl -sS -o "$tmp" -w "%{http_code}" \
    -X POST "$AGENT_URL" \
    -H "Content-Type: application/json" \
    -H "X-Session-Id: $SESSION_ID" \
    --max-time 600 \
    -d "$(python3 -c 'import json,sys; print(json.dumps({"query": sys.argv[1]}))' "$query")")
  echo "HTTP $http_code"
  cat "$tmp"
  echo ""
  rm -f "$tmp"
}

register_event "network" "session" "info" "CE demo ($MODE) started" "Scenario: IT incident investigation."

echo "--- Q1 ---"
echo "$Q1"
echo ""
post_turn "$Q1"
echo ""
sleep 2

echo "--- Q2 ---"
echo "$Q2"
echo ""
post_turn "$Q2"
echo ""

echo ""
echo "Open $OBSERVER_URL/#$SESSION_ID  (or the session list) to review events."
