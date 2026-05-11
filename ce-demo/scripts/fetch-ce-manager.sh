#!/usr/bin/env bash
# Ensure CE-Manager is available as a sibling of ibac/ before building the
# ce-proxy image. If it's already present, do nothing. Otherwise clone it
# over HTTPS (no SSH required).
#
# Override the source with CE_MANAGER_REMOTE if you host a fork.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"                 # ibac/ce-demo/
WORKSPACE_ROOT="$(cd "$ROOT_DIR/../.." && pwd)"          # ibac-ce/ (sibling of ibac/)
CE_MANAGER_DIR="${CE_MANAGER_DIR:-$WORKSPACE_ROOT/CE-Manager}"
CE_MANAGER_REMOTE="${CE_MANAGER_REMOTE:-https://github.ibm.com/Context-Engineering/CE-Manager.git}"

if [ -d "$CE_MANAGER_DIR/src/ce_manager" ]; then
  echo "CE-Manager already at $CE_MANAGER_DIR — skipping fetch."
  exit 0
fi

echo "Cloning CE-Manager into $CE_MANAGER_DIR ..."
echo "  (source: $CE_MANAGER_REMOTE)"
if ! git clone --depth 1 "$CE_MANAGER_REMOTE" "$CE_MANAGER_DIR"; then
  cat <<EOF
ERROR: could not clone CE-Manager from $CE_MANAGER_REMOTE.

If you don't have access to the IBM GitHub Enterprise host, set
CE_MANAGER_REMOTE to a mirror you can reach, e.g.:

  CE_MANAGER_REMOTE=git@github.com:your-fork/CE-Manager.git ./scripts/fetch-ce-manager.sh

Or place the CE-Manager source manually at:
  $CE_MANAGER_DIR
EOF
  exit 1
fi

echo "CE-Manager ready at $CE_MANAGER_DIR"
