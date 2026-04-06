#!/usr/bin/env bash
set -euo pipefail

CLUSTER_NAME="ibac-demo"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
SPARC_PID_FILE="$ROOT_DIR/.runtime/sparc-worker.pid"

echo "Deleting kind cluster '${CLUSTER_NAME}'..."
kind delete cluster --name "$CLUSTER_NAME"
if [ -f "$SPARC_PID_FILE" ]; then
  PID="$(cat "$SPARC_PID_FILE" 2>/dev/null || true)"
  if [ -n "${PID:-}" ] && kill -0 "$PID" 2>/dev/null; then
    kill "$PID" >/dev/null 2>&1 || true
  fi
  rm -f "$SPARC_PID_FILE"
fi
echo "Done."
