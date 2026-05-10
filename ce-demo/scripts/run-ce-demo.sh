#!/usr/bin/env bash
# End-to-end runner for the CE-Manager demo on localhost.
#
# Usage:
#   scripts/run-ce-demo.sh                 # full flow: setup → up → off → on
#   scripts/run-ce-demo.sh setup           # one-time venv + deps
#   scripts/run-ce-demo.sh up              # start observer + ce-proxy + ce-demo-agent
#   scripts/run-ce-demo.sh off             # run scenario with CE_MODE=off     (Q2 overflows)
#   scripts/run-ce-demo.sh on              # run scenario with CE_MODE=on      (masker compacts)
#   scripts/run-ce-demo.sh summary         # run scenario with CE_MODE=summary (LLM paraphrases old turns)
#   scripts/run-ce-demo.sh down            # stop all services
#   scripts/run-ce-demo.sh status          # show pids + health
#
# All three services log to $LOG_DIR (default /tmp/ce-logs/).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
WORKSPACE_ROOT="$(cd "$ROOT_DIR/.." && pwd)"
CE_MANAGER_DIR="${CE_MANAGER_DIR:-$WORKSPACE_ROOT/CE-Manager}"

VENV="${VENV:-/tmp/ce-venv}"
LOG_DIR="${LOG_DIR:-/tmp/ce-logs}"
OBSERVER_BIN="${OBSERVER_BIN:-/tmp/ce-observer}"
AGENT_BIN="${AGENT_BIN:-/tmp/ce-demo-agent}"

# The local-dev path uses the CE-dedicated observer (lanes are CE-only).
OBSERVER_PORT=7070
PROXY_PORT=9100
AGENT_PORT=8080

OBSERVER_URL="http://127.0.0.1:${OBSERVER_PORT}"
PROXY_URL="http://127.0.0.1:${PROXY_PORT}"
AGENT_URL="http://127.0.0.1:${AGENT_PORT}"

# --- colours ---
C_RESET=$'\033[0m'; C_BOLD=$'\033[1m'
C_BLUE=$'\033[34m'; C_GREEN=$'\033[32m'; C_YEL=$'\033[33m'; C_RED=$'\033[31m'
hdr() { printf '\n%s=== %s ===%s\n' "$C_BOLD" "$*" "$C_RESET"; }
ok()  { printf '%s✓%s %s\n' "$C_GREEN" "$C_RESET" "$*"; }
warn(){ printf '%s!%s %s\n' "$C_YEL" "$C_RESET" "$*"; }
err() { printf '%s✗%s %s\n' "$C_RED" "$C_RESET" "$*"; }
note(){ printf '%s•%s %s\n' "$C_BLUE" "$C_RESET" "$*"; }

mkdir -p "$LOG_DIR"

# --- helpers ---
kill_port() {
  local port="$1"
  local pids
  pids=$(lsof -ti ":$port" 2>/dev/null || true)
  [ -n "$pids" ] && kill -9 $pids 2>/dev/null || true
}

wait_for_http() {
  local url="$1" name="$2" timeout="${3:-30}"
  local i=0
  while [ $i -lt "$timeout" ]; do
    if curl -sf "$url" >/dev/null 2>&1; then
      ok "$name ready at $url"
      return 0
    fi
    i=$((i+1)); sleep 1
  done
  err "$name never became ready (url=$url)"
  return 1
}

find_go() {
  if command -v go >/dev/null 2>&1; then command -v go; return; fi
  # best-effort: pre-commit-managed golangenv
  local hit
  hit=$(find "$HOME/.cache/pre-commit" -type f -name go -path '*/golangenv-*/.go/bin/go' 2>/dev/null | head -n1 || true)
  [ -n "$hit" ] && { echo "$hit"; return; }
  err "go not found on PATH. Install Go 1.23+ and retry."
  return 1
}

require_env() {
  if [ ! -f "$ROOT_DIR/.env" ]; then
    err "$ROOT_DIR/.env is missing."
    err "Create it with WATSONX_API_KEY, WATSONX_PROJECT_ID, WATSONX_URL."
    return 1
  fi
  local missing=""
  # shellcheck disable=SC1090
  (set -a; . "$ROOT_DIR/.env"; set +a
   for v in WATSONX_API_KEY WATSONX_PROJECT_ID WATSONX_URL; do
     alias_="WX_${v#WATSONX_}"
     if [ -z "${!v:-}" ] && [ -z "${!alias_:-}" ]; then missing="$missing $v"; fi
   done
   if [ -n "$missing" ]; then
     err "Missing env vars in .env:$missing"
     exit 1
   fi
  )
}

# ---------------------------------------------------------------------------
cmd_setup() {
  hdr "setup: Python venv at $VENV"
  if [ ! -x "$VENV/bin/python" ]; then
    python3 -m venv "$VENV"
    "$VENV/bin/pip" install -U pip >/dev/null
  else
    note "venv already exists, skipping creation"
  fi

  hdr "setup: installing Python deps"
  "$VENV/bin/pip" install --quiet \
    fastapi 'uvicorn[standard]' litellm tiktoken httpx pydantic python-dotenv numpy \
    playwright 2>&1 | tail -5 || true

  if [ ! -d "$CE_MANAGER_DIR" ]; then
    err "CE-Manager not found at $CE_MANAGER_DIR"
    err "Clone it there or set CE_MANAGER_DIR=..."
    exit 1
  fi
  "$VENV/bin/pip" install --quiet --no-deps "$CE_MANAGER_DIR" 2>&1 | tail -3 || true

  hdr "setup: stubbing IBM-internal llm_client (local dev only)"
  local site
  site=$("$VENV/bin/python" -c "import sysconfig; print(sysconfig.get_paths()['purelib'])")
  if [ ! -f "$site/llm_client/__init__.py" ]; then
    mkdir -p "$site/llm_client/llm"
    cat > "$site/llm_client/__init__.py" <<'PY'
"""Local stub for IBM-internal llm-client. ce-proxy brings its own MaskerClient."""
def get_llm(*a, **k):
    raise RuntimeError("llm_client stub: ce-proxy should pass its own client to CE-Manager")
PY
    cat > "$site/llm_client/llm/__init__.py" <<'PY'
from .. import get_llm  # noqa: F401
PY
    cat > "$site/llm_client/llm/types.py" <<'PY'
class GenerationArgs:
    def __init__(self, *a, **k): pass
PY
    ok "llm_client stub written to $site/llm_client"
  else
    note "llm_client stub already present"
  fi

  hdr "setup: verifying CE-Manager imports"
  "$VENV/bin/python" -c "
from ce_manager.components.programmatic_context_engineering import compact_conversation
from ce_manager.config import ProgrammaticCEConfig
print('OK — compact_conversation importable')"

  hdr "setup: building Go binaries"
  local GO; GO=$(find_go)
  (cd "$ROOT_DIR" && "$GO" build -o "$OBSERVER_BIN" ./ce-observer/)
  ok "built $OBSERVER_BIN (ce-observer)"
  (cd "$ROOT_DIR" && "$GO" build -o "$AGENT_BIN" ./ce-demo-agent/)
  ok "built $AGENT_BIN"

  hdr "setup: regenerating tool fixtures (fast)"
  python3 "$ROOT_DIR/scripts/fixtures_gen.py" >/dev/null
  ok "fixtures in ce-demo-agent/fixtures/"

  ok "setup complete"
}

# ---------------------------------------------------------------------------
start_observer() {
  if curl -sf "$OBSERVER_URL/api/sessions" >/dev/null 2>&1; then
    note "observer already running"; return 0
  fi
  kill_port $OBSERVER_PORT
  nohup "$OBSERVER_BIN" > "$LOG_DIR/observer.log" 2>&1 &
  echo $! > "$LOG_DIR/observer.pid"
  wait_for_http "$OBSERVER_URL/api/sessions" "observer"
}

start_proxy() {
  local mode="${1:-off}"
  kill_port $PROXY_PORT
  (
    set -a
    # shellcheck disable=SC1090
    . "$ROOT_DIR/.env"
    set +a
    # LiteLLM's watsonx provider reads WX_* (not WATSONX_*). Mirror both directions.
    for b in API_KEY PROJECT_ID URL; do
      wa="WATSONX_$b"; wx="WX_$b"
      eval "[ -n \"\${$wa:-}\" ] && export $wx=\"\${$wa}\""
      eval "[ -n \"\${$wx:-}\" ] && export $wa=\"\${$wx}\""
    done
    export PYTHONPATH="$ROOT_DIR/ce-proxy"
    export CE_MODE="$mode"
    export CE_MAX_TOKENS="${CE_MAX_TOKENS:-131072}"
    export CE_THRESHOLD_FRAC="${CE_THRESHOLD_FRAC:-0.50}"
    export CE_EMIT_DIFF_CONTENT="${CE_EMIT_DIFF_CONTENT:-true}"
    export CE_KEEP_LAST_N_TURNS="${CE_KEEP_LAST_N_TURNS:-2}"
    export CE_AGGRESSIVENESS="${CE_AGGRESSIVENESS:-0.8}"
    export CE_MAX_OUTPUT_CHARS="${CE_MAX_OUTPUT_CHARS:-120000}"
    export OBSERVER_URL="$OBSERVER_URL"
    export WATSONX_MODEL="${WATSONX_MODEL:-watsonx/openai/gpt-oss-120b}"
    nohup "$VENV/bin/uvicorn" app.main:app --host 127.0.0.1 --port "$PROXY_PORT" \
      > "$LOG_DIR/ce-proxy.log" 2>&1 &
    echo $! > "$LOG_DIR/ce-proxy.pid"
  )
  wait_for_http "$PROXY_URL/healthz" "ce-proxy"
  note "ce-proxy CE_MODE=$mode"
}

start_agent() {
  if curl -sf "$AGENT_URL/healthz" >/dev/null 2>&1; then
    note "ce-demo-agent already running"; return 0
  fi
  kill_port $AGENT_PORT
  (
    export LLM_URL="$PROXY_URL"
    export LLM_MODEL="${LLM_MODEL:-openai/gpt-oss-120b}"
    export OBSERVER_URL="$OBSERVER_URL"
    nohup "$AGENT_BIN" > "$LOG_DIR/ce-demo-agent.log" 2>&1 &
    echo $! > "$LOG_DIR/ce-demo-agent.pid"
  )
  wait_for_http "$AGENT_URL/healthz" "ce-demo-agent"
}

# Restart proxy in-place (CE-Manager state + env var toggle)
switch_mode() {
  local mode="$1"
  note "switching ce-proxy to CE_MODE=$mode (proxy will restart)"
  start_proxy "$mode"
  curl -s -X POST "$PROXY_URL/admin/reset" >/dev/null || true
  ok "CE_MODE=$mode ready"
}

cmd_up() {
  require_env
  hdr "starting services"
  start_observer
  start_proxy "off"
  start_agent
  hdr "services up"
  note "Observer:   $OBSERVER_URL"
  note "ce-proxy:   $PROXY_URL    (CE_MODE=off by default — flip with './run-ce-demo.sh on')"
  note "Agent:      $AGENT_URL"
  note "Logs:       $LOG_DIR/"
}

cmd_down() {
  hdr "stopping services"
  for svc in observer ce-proxy ce-demo-agent; do
    if [ -f "$LOG_DIR/$svc.pid" ]; then
      local pid; pid=$(cat "$LOG_DIR/$svc.pid" 2>/dev/null || true)
      [ -n "$pid" ] && kill "$pid" 2>/dev/null || true
      rm -f "$LOG_DIR/$svc.pid"
    fi
  done
  kill_port $OBSERVER_PORT
  kill_port $PROXY_PORT
  kill_port $AGENT_PORT
  ok "stopped"
}

cmd_status() {
  for port_name in "$OBSERVER_PORT observer" "$PROXY_PORT ce-proxy" "$AGENT_PORT ce-demo-agent"; do
    set -- $port_name
    if lsof -ti ":$1" >/dev/null 2>&1; then
      ok "$2 UP on :$1"
    else
      err "$2 DOWN"
    fi
  done
  if curl -sf "$PROXY_URL/healthz" >/dev/null 2>&1; then
    note "proxy health: $(curl -sf $PROXY_URL/healthz)"
  fi
}

run_scenario() {
  local mode="$1"
  switch_mode "$mode"

  if ! curl -sf "$AGENT_URL/healthz" >/dev/null 2>&1; then
    err "agent not running — did you call 'up' first?"
    exit 1
  fi

  local sess="demo-${mode}-$(date +%s)"
  hdr "scenario: CE_MODE=$mode — session $sess"
  note "watch live at: $OBSERVER_URL  (click the matching session)"

  local q1 q2
  q1=$(python3 -c 'import json; print(json.load(open("'"$ROOT_DIR"'/testdata/ce-demo-queries.json"))["q1"])')
  q2=$(python3 -c 'import json; print(json.load(open("'"$ROOT_DIR"'/testdata/ce-demo-queries.json"))["q2"])')

  echo
  echo "$C_BOLD--- Q1 ---$C_RESET"
  echo "$q1" | fold -s -w 100
  echo
  local body
  body=$(python3 -c 'import json,sys; print(json.dumps({"query": sys.argv[1]}))' "$q1")
  local resp
  resp=$(curl -sS --max-time 900 -X POST "$AGENT_URL/" \
    -H "Content-Type: application/json" \
    -H "X-Session-Id: $sess" \
    -d "$body")
  echo "$C_BOLD→ Q1 reply:$C_RESET"
  python3 -c 'import json,sys; d=json.loads(sys.argv[1] or "{}"); print((d.get("response") or d.get("error") or "(empty)")[:600])' "$resp"

  echo
  echo "$C_BOLD--- Q2 ---$C_RESET"
  echo "$q2" | fold -s -w 100
  echo
  body=$(python3 -c 'import json,sys; print(json.dumps({"query": sys.argv[1]}))' "$q2")
  resp=$(curl -sS --max-time 900 -X POST "$AGENT_URL/" \
    -H "Content-Type: application/json" \
    -H "X-Session-Id: $sess" \
    -d "$body")
  echo "$C_BOLD→ Q2 reply:$C_RESET"
  python3 -c 'import json,sys; d=json.loads(sys.argv[1] or "{}"); print((d.get("response") or d.get("error") or "(empty)")[:600])' "$resp"

  echo
  ok "session complete — $sess"
  echo "$LOG_DIR/sess-${mode}.txt <- session id"
  echo "SESSION_ID=$sess" > "$LOG_DIR/sess-${mode}.txt"

  # Summary from the observer (fetched by Python via urllib so shell quoting
  # can't swallow JSON literals like `false`).
  echo
  hdr "observer event summary for $sess"
  SESS="$sess" OBS="$OBSERVER_URL" python3 - <<'PY'
import json, os, urllib.request
sess = os.environ["SESS"]; obs = os.environ["OBS"]
try:
    data = json.loads(urllib.request.urlopen(f"{obs}/api/sessions/{sess}").read())
except Exception as e:
    print(f"  (observer fetch failed: {e})"); raise SystemExit(0)
events = data.get("events", [])
print(f"  total events: {len(events)}")
rel = ("user_turn","assistant_reply","threshold_crossed","mask_started",
       "mask_codegen","context_diff","mask_applied","mask_fallback","mask_skipped",
       "session_cache_hit","session_cache_save","context_overflow","agent_blocked")
cnt = {}
for e in events:
    stage = e.get("stage",""); status = e.get("status","")
    if stage in rel:
        cnt[(stage, status)] = cnt.get((stage, status), 0) + 1
for k in sorted(cnt): print(f"    {k[0]:22s} {k[1]:8s} x{cnt[k]}")
# Mask reductions
reds = [round((e.get("data") or {}).get("reduction_pct"), 1)
        for e in events
        if e.get("stage") == "mask_applied" and e.get("status") == "success"
        and (e.get("data") or {}).get("reduction_pct") is not None]
if reds:
    print(f"  mask reductions (%): {reds}")
# Cache savings
savings = 0; hits = 0
for e in events:
    if e.get("stage") == "proxy_request":
        d = e.get("data") or {}
        if d.get("cache_hit"):
            hits += 1
            savings += max(0, (d.get("raw_tokens") or 0) - (d.get("tokens_in") or 0))
if hits:
    print(f"  session cache: {hits} hit(s), total tokens avoided: ~{savings}")
PY
  echo
  echo "Open in browser: $OBSERVER_URL  →  click '$sess'"
  echo "Look for:"
  if [ "$mode" = "off" ]; then
    echo "  • ce-manager lane:    'proxy_passthrough' cards, then a RED 'Context overflow' card on Q2"
    echo "  • ce-demo-agent lane: final RED 'Context overflow — cannot call LLM' card"
    echo "  • Conversation feed:  ends with the blocked Q2"
  else
    echo "  • ce-manager lane:    'threshold_crossed' (yellow) → 'mask_started' → 'mask_codegen'"
    echo "                        (click the 'LLM-generated distill()' card to see the Python code)"
    echo "                        then 'context_diff' (click it to see per-message before/after)"
    echo "                        and green 'Context reduced N%' mask_applied cards"
    echo "  • ce-demo-agent lane: BOTH Q1 and Q2 end in 'Agent reply' (no blocks)"
    echo "  • Conversation feed:  'Context masked' entries from CE Manager between tool calls"
  fi
}

cmd_all() {
  cmd_setup
  cmd_up
  run_scenario off
  run_scenario on
  echo
  hdr "all done"
  echo
  echo "Compare the two sessions in $OBSERVER_URL:"
  [ -f "$LOG_DIR/sess-off.txt" ] && echo "  CE_MODE=off:  $(cat $LOG_DIR/sess-off.txt)"
  [ -f "$LOG_DIR/sess-on.txt"  ] && echo "  CE_MODE=on:   $(cat $LOG_DIR/sess-on.txt)"
  echo
  echo "Services still running — stop with:  scripts/run-ce-demo.sh down"
}

# ---------------------------------------------------------------------------
main() {
  local cmd="${1:-all}"
  case "$cmd" in
    setup)  cmd_setup ;;
    up)     cmd_up ;;
    off)      cmd_up; run_scenario off ;;
    on)       cmd_up; run_scenario on ;;
    summary)  cmd_up; run_scenario summary ;;
    truncate) cmd_up; run_scenario truncate ;;
    both|all) cmd_all ;;
    down|stop) cmd_down ;;
    status) cmd_status ;;
    help|-h|--help)
      grep -E '^#( |$)' "$0" | sed 's/^#//' ;;
    *) err "unknown command: $cmd"; echo; grep -E '^#( |$)' "$0" | sed 's/^#//' ; exit 2 ;;
  esac
}

main "$@"
