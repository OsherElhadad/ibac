#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
SPARC_PID_FILE="$ROOT_DIR/.runtime/sparc-worker.pid"

echo "Deleting ibac namespace and all resources..."
kubectl delete namespace ibac --wait=true
if [ -f "$SPARC_PID_FILE" ]; then
  PID="$(cat "$SPARC_PID_FILE" 2>/dev/null || true)"
  if [ -n "${PID:-}" ] && kill -0 "$PID" 2>/dev/null; then
    kill "$PID" >/dev/null 2>&1 || true
  fi
  rm -f "$SPARC_PID_FILE"
fi
echo "Done. Run 'make deploy' to redeploy."
