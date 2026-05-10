#!/usr/bin/env bash
# One-time setup: create a Python venv on the Mac with the ce-proxy's
# runtime deps + the vendored CE-Manager + the committed llm_client stub.
# Safe to re-run (pip will no-op on already-satisfied requirements).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
CE_DEMO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"                 # ibac/ce-demo/
REPO_ROOT="$(cd "$CE_DEMO_DIR/../.." && pwd)"               # ibac-ce/
VENV="$REPO_ROOT/.runtime/ce-proxy-venv"
REQS="$CE_DEMO_DIR/ce-proxy/requirements.txt"
CE_MANAGER="$REPO_ROOT/CE-Manager"
STUB="$CE_DEMO_DIR/ce-proxy/stubs/llm_client"

# Clone CE-Manager via HTTPS if missing.
if [ ! -d "$CE_MANAGER" ]; then
  "$SCRIPT_DIR/fetch-ce-manager.sh"
fi

if [ ! -f "$REQS" ]; then
  echo "requirements.txt missing at $REQS"; exit 1
fi
if [ ! -d "$CE_MANAGER" ]; then
  echo "CE-Manager source missing at $CE_MANAGER"; exit 1
fi

mkdir -p "$REPO_ROOT/.runtime"

if [ ! -x "$VENV/bin/python" ]; then
  python3 -m venv "$VENV"
fi

"$VENV/bin/pip" install --quiet --disable-pip-version-check -r "$REQS"
"$VENV/bin/pip" install --quiet --disable-pip-version-check --no-deps "$CE_MANAGER"

SITE_PACKAGES="$("$VENV/bin/python" -c 'import site; print(site.getsitepackages()[0])')"
if [ ! -d "$SITE_PACKAGES/llm_client" ]; then
  cp -r "$STUB" "$SITE_PACKAGES/"
fi

"$VENV/bin/python" -c "import fastapi, uvicorn, litellm, ce_manager, llm_client; print('ce-proxy-mac venv: OK')"
