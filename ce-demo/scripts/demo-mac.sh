#!/usr/bin/env bash
# Run the CE demo with the ce-proxy running on macOS instead of in the kind
# cluster. Bypasses the Rancher Desktop VM NAT for all watsonx traffic — when
# the VM network is flaky, this is the fastest path available.
#
# Pipeline:
#   1. Ensure Mac-side ce-proxy is up (via run-proxy-mac.sh up).
#   2. Detect the macOS IP that is reachable from kind pods (Rancher assigns
#      the kind bridge a route to the Mac; typically the VPN/tunnel IP).
#   3. Override CE_MODE on the Mac ce-proxy to match the requested scenario.
#   4. Point the in-cluster ce-demo-agent at http://<mac-ip>:9100.
#   5. Wait for the agent rollout, then run Q1 + Q2 as usual.
set -euo pipefail

MODE="${1:-on}"
case "$MODE" in
  off|on|summary|truncate) ;;
  *) echo "Usage: $0 off|on|summary|truncate"; exit 2 ;;
esac

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
CE_DEMO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"                 # ibac/ce-demo/

AGENT_URL="${CE_AGENT_URL:-http://localhost:30021}"
OBSERVER_URL="${OBSERVER_URL:-http://localhost:30071}"
SESSION_ID="ce-demo-${MODE}-$(date +%s)"

QUERIES_FILE="$CE_DEMO_DIR/testdata/ce-demo-queries.json"
Q1="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["q1"])' "$QUERIES_FILE")"
Q2="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["q2"])' "$QUERIES_FILE")"

echo "=============================================="
echo " CE-Manager Demo (mac proxy · CE_MODE=$MODE)"
echo "=============================================="

# Step 1: start Mac ce-proxy with the right CE_MODE. Always restart so the
# CE_MODE env reflects the requested scenario (run-proxy-mac.sh up is a
# no-op if the process is already alive).
"$SCRIPT_DIR/run-proxy-mac.sh" down >/dev/null 2>&1 || true
CE_MODE="$MODE" "$SCRIPT_DIR/run-proxy-mac.sh" up

# Step 2: find a macOS IP the pod can reach. Probe known candidates.
echo "Finding pod-reachable Mac IP..."
MAC_CANDIDATES=$(
  ifconfig | awk '/inet / && $2 !~ /^127\./ {print $2}' | sort -u
)

REACHABLE_IP=""
for ip in $MAC_CANDIDATES; do
  if kubectl -n ibac exec deploy/ce-demo-agent -- nc -zvw 2 "$ip" 9100 >/dev/null 2>&1; then
    REACHABLE_IP="$ip"
    break
  fi
done

if [ -z "$REACHABLE_IP" ]; then
  echo "ERROR: no Mac IP is reachable from the kind pod on port 9100."
  echo "Candidates tried: $MAC_CANDIDATES"
  echo "Possible fixes:"
  echo "  - Check VPN is connected (kind pods only see you via VPN tunnel IP)."
  echo "  - Verify ce-proxy-mac is listening on 0.0.0.0:9100."
  exit 1
fi
echo "Mac IP reachable from pod: $REACHABLE_IP"

# Step 3: scale the in-cluster ce-proxy to 0 (if running) and point the
# agent at the Mac proxy. Wait for agent rollout.
kubectl -n ibac scale deploy/ce-proxy --replicas=0 >/dev/null 2>&1 || true

# Create / update a pure-endpoints Service that the agent already points at
# (ce-proxy.ibac.svc.cluster.local:9100) but that now routes to the Mac.
# Removing the selector lets us manage Endpoints manually.
kubectl -n ibac patch svc ce-proxy --type=json -p='[{"op":"remove","path":"/spec/selector"}]' 2>/dev/null || true
cat <<EOF | kubectl -n ibac apply -f - >/dev/null
apiVersion: v1
kind: Endpoints
metadata:
  name: ce-proxy
  namespace: ibac
subsets:
  - addresses:
      - ip: ${REACHABLE_IP}
    ports:
      - name: http
        port: 9100
        protocol: TCP
EOF
echo "Service ce-proxy.ibac now routes to ${REACHABLE_IP}:9100"

# Wait for kube-proxy to pick up the new Endpoints and for the ClusterIP
# to actually route to the Mac proxy. Without this wait, Q1 can race the
# endpoint rollout and get "connection refused" from the stale ClusterIP.
echo "Waiting for ce-proxy Service to become reachable from the pod..."
for i in $(seq 1 30); do
  if kubectl -n ibac exec deploy/ce-demo-agent -- \
       wget -qO- --timeout=2 http://ce-proxy.ibac.svc.cluster.local:9100/healthz \
       >/dev/null 2>&1 ||
     kubectl -n ibac exec deploy/ce-demo-agent -- \
       nc -zvw 2 ce-proxy.ibac.svc.cluster.local 9100 >/dev/null 2>&1; then
    echo "  ce-proxy Service ready (after ${i}s)"
    break
  fi
  sleep 1
  if [ "$i" -eq 30 ]; then
    echo "WARNING: ce-proxy Service never became reachable — proceeding anyway"
  fi
done

# Reset ce-proxy-mac state so the new session starts clean.
curl -sSf -X POST http://localhost:9100/admin/reset >/dev/null || true

echo ""
echo "Agent URL:    $AGENT_URL"
echo "Observer URL: $OBSERVER_URL/#${SESSION_ID}"
echo "Session ID:   $SESSION_ID"
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

register_event "network" "session" "info" "CE demo (mac/$MODE) started" "watsonx via Mac proxy at ${REACHABLE_IP}:9100."

echo "--- Q1 ---"
echo "$Q1"
echo ""
START_Q1=$(date +%s)
post_turn "$Q1"
END_Q1=$(date +%s)
echo "Q1 wall time: $((END_Q1 - START_Q1)) s"
echo ""
sleep 2

echo "--- Q2 ---"
echo "$Q2"
echo ""
START_Q2=$(date +%s)
post_turn "$Q2"
END_Q2=$(date +%s)
echo "Q2 wall time: $((END_Q2 - START_Q2)) s"
echo ""
echo "Total wall time: $((END_Q2 - START_Q1)) s"
echo ""
echo "Open $OBSERVER_URL/#$SESSION_ID to review events."
