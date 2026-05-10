#!/usr/bin/env bash
# Run ce-proxy as a local macOS process on port 9100, using the Mac's healthy
# network for watsonx calls instead of the kind VM NAT.
#
# Secrets (WATSONX_API_KEY, WATSONX_PROJECT_ID, WATSONX_URL) are read from the
# kubernetes secret `ce-watsonx` in namespace `ibac` when the cluster is
# running; on a fresh machine without a cluster, they are read from
# ibac/ce-demo/.env.
#
# The local ce-proxy posts observer events to the observer's NodePort on
# localhost:30071 (same NodePort the UI uses).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
CE_DEMO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"                 # ibac/ce-demo/
REPO_ROOT="$(cd "$CE_DEMO_DIR/../.." && pwd)"               # ibac-ce/
VENV="$REPO_ROOT/.runtime/ce-proxy-venv"
LOG="$REPO_ROOT/.runtime/ce-proxy-mac.log"
PIDFILE="$REPO_ROOT/.runtime/ce-proxy-mac.pid"

cmd="${1:-up}"

case "$cmd" in
  up)
    if [ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; then
      echo "ce-proxy-mac already running (pid $(cat "$PIDFILE"))"
      exit 0
    fi

    if [ ! -x "$VENV/bin/uvicorn" ]; then
      echo "Mac venv missing — run: make -C ibac/ce-demo proxy-mac-install"
      exit 1
    fi

    # Prefer kubectl secret (when cluster is running), else fall back to .env.
    if kubectl -n ibac get secret ce-watsonx >/dev/null 2>&1; then
      WATSONX_API_KEY="$(kubectl -n ibac get secret ce-watsonx -o jsonpath='{.data.WATSONX_API_KEY}' | base64 -d)"
      WATSONX_PROJECT_ID="$(kubectl -n ibac get secret ce-watsonx -o jsonpath='{.data.WATSONX_PROJECT_ID}' | base64 -d)"
      WATSONX_URL="$(kubectl -n ibac get secret ce-watsonx -o jsonpath='{.data.WATSONX_URL}' | base64 -d)"
    elif [ -f "$CE_DEMO_DIR/.env" ]; then
      set -a
      . "$CE_DEMO_DIR/.env"
      set +a
      WATSONX_API_KEY="${WATSONX_API_KEY:-${WX_API_KEY:-}}"
      WATSONX_PROJECT_ID="${WATSONX_PROJECT_ID:-${WX_PROJECT_ID:-}}"
      WATSONX_URL="${WATSONX_URL:-${WX_URL:-https://us-south.ml.cloud.ibm.com}}"
    else
      echo "ERROR: no cluster secret 'ce-watsonx' and no $CE_DEMO_DIR/.env found."
      echo "Copy $CE_DEMO_DIR/.env.example to $CE_DEMO_DIR/.env and fill in your watsonx credentials."
      exit 1
    fi

    if [ -z "${WATSONX_API_KEY:-}" ] || [ -z "${WATSONX_PROJECT_ID:-}" ]; then
      echo "ERROR: WATSONX_API_KEY / WATSONX_PROJECT_ID not set."
      exit 1
    fi

    export WATSONX_API_KEY WATSONX_PROJECT_ID WATSONX_URL
    export WATSONX_MODEL="${WATSONX_MODEL:-watsonx/openai/gpt-oss-120b}"
    export ACCEPTED_MODEL="${ACCEPTED_MODEL:-openai/gpt-oss-120b}"
    export CE_MODE="${CE_MODE:-off}"
    export CE_MAX_TOKENS="${CE_MAX_TOKENS:-131072}"
    export CE_THRESHOLD_FRAC="${CE_THRESHOLD_FRAC:-0.65}"
    export CE_EMIT_DIFF_CONTENT="${CE_EMIT_DIFF_CONTENT:-true}"
    export CE_KEEP_LAST_N_TURNS="${CE_KEEP_LAST_N_TURNS:-2}"
    export CE_AGGRESSIVENESS="${CE_AGGRESSIVENESS:-0.8}"
    export CE_MAX_OUTPUT_CHARS="${CE_MAX_OUTPUT_CHARS:-120000}"
    # Observer reached via its NodePort on the Mac host.
    export OBSERVER_URL="${OBSERVER_URL:-http://localhost:30071}"

    mkdir -p "$(dirname "$LOG")"
    cd "$CE_DEMO_DIR/ce-proxy"
    nohup "$VENV/bin/uvicorn" app.main:app \
      --host 0.0.0.0 --port 9100 \
      >"$LOG" 2>&1 &
    echo $! > "$PIDFILE"
    echo "ce-proxy-mac started (pid $(cat "$PIDFILE")), logs: $LOG"

    # Wait for readiness.
    for i in $(seq 1 20); do
      if curl -sf -o /dev/null --max-time 1 http://localhost:9100/healthz; then
        echo "ce-proxy-mac healthy on http://localhost:9100"
        exit 0
      fi
      sleep 0.5
    done
    echo "ce-proxy-mac failed to become healthy — tail of log:"
    tail -30 "$LOG"
    exit 1
    ;;

  down)
    if [ ! -f "$PIDFILE" ]; then
      echo "ce-proxy-mac not running"
      exit 0
    fi
    PID="$(cat "$PIDFILE")"
    kill "$PID" 2>/dev/null || true
    sleep 0.3
    kill -9 "$PID" 2>/dev/null || true
    rm -f "$PIDFILE"
    echo "ce-proxy-mac stopped"
    ;;

  status)
    if [ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; then
      echo "ce-proxy-mac running (pid $(cat "$PIDFILE"))"
      curl -sS --max-time 2 http://localhost:9100/healthz
      echo
    else
      echo "ce-proxy-mac not running"
    fi
    ;;

  logs)
    tail -n 100 -f "$LOG"
    ;;

  *)
    echo "Usage: $0 up|down|status|logs"
    exit 2
    ;;
esac
