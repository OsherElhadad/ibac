#!/usr/bin/env bash
set -euo pipefail

FINANCE_URL="http://localhost:30020"
OBSERVER_URL="http://localhost:30070"
SESSION_ID="finance-demo-$(date +%s)"

echo "=============================================="
echo " Finance Demo: Sequential SPARC + IBAC"
echo "=============================================="
echo ""
echo "Dashboard:"
echo "  $OBSERVER_URL"
echo "Session ID:"
echo "  $SESSION_ID"
echo ""

register_event() {
  local source="$1"
  local stage="$2"
  local status="$3"
  local title="$4"
  local summary="$5"
  curl -sf -X POST "$OBSERVER_URL/api/events" \
    -H "Content-Type: application/json" \
    -d "$(printf '{"session_id":"%s","source":"%s","stage":"%s","status":"%s","title":"%s","summary":"%s"}' \
      "$SESSION_ID" "$source" "$stage" "$status" "$title" "$summary")" >/dev/null || true
}

wait_for_session_outcome() {
  local deadline=$((SECONDS + 120))
  while [ "$SECONDS" -lt "$deadline" ]; do
    local session_json
    if session_json="$(curl -sf "$OBSERVER_URL/api/sessions/$SESSION_ID" 2>/dev/null)"; then
      if SESSION_JSON="$session_json" python3 - <<'PY'
import json
import os
import sys

session = json.loads(os.environ["SESSION_JSON"])
events = session.get("events", [])

ibac_blocked_event = next((evt for evt in reversed(events) if evt.get("source") == "ibac" and evt.get("status") == "blocked"), None)
ibac_blocked = ibac_blocked_event is not None
assistant_after_block = False
if ibac_blocked_event is not None:
    blocked_seq = ibac_blocked_event.get("sequence", 0)
    assistant_after_block = any(
        evt.get("source") == "finance-agent"
        and evt.get("stage") == "assistant_reply"
        and evt.get("sequence", 0) > blocked_seq
        for evt in events
    )

sys.exit(0 if ibac_blocked and assistant_after_block else 1)
PY
      then
        return 0
      fi
    fi
    sleep 2
  done

  echo "ERROR: timed out waiting for the IBAC block and the final agent reply."
  curl -sf "$OBSERVER_URL/api/sessions/$SESSION_ID" || true
  return 1
}

post_turn() {
  local query="$1"
  local tmp_file
  local http_code

  tmp_file="$(mktemp)"
  http_code=$(curl -sS -o "$tmp_file" -w "%{http_code}" -X POST "$FINANCE_URL" \
    -H "Content-Type: application/json" \
    -H "X-Session-Id: ${SESSION_ID}" \
    -d "{\"query\": \"$query\"}" \
    --max-time 300)

  if [ "$http_code" -ge 400 ]; then
    echo "ERROR: finance-agent returned HTTP $http_code"
    cat "$tmp_file"
    rm -f "$tmp_file"
    return 1
  fi

  cat "$tmp_file"
  rm -f "$tmp_file"
}

register_event "network" "session" "info" "Finance demo session" "Sequential finance demo started."

echo "Turn 1: Partial refund request with explicit refund reason"
TURN1='Refund transaction TX482. The refund reason is duplicate charge.'
post_turn "$TURN1"
echo ""

sleep 3

echo "Turn 2: Clarification with the full transaction ID"
TURN2='The full transaction ID is TX4827. Please continue the refund.'
post_turn "$TURN2"
echo ""

sleep 3

echo "Turn 3: Invoice processing"
TURN3='Also process invoice INV-8834 from Acme Corp.'
post_turn "$TURN3"
echo ""

wait_for_session_outcome

echo ""
echo "Open $OBSERVER_URL to watch the full pipeline live or replay this session."
