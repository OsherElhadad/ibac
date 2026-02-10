#!/usr/bin/env bash
set -euo pipefail

CLUSTER_NAME="ibac-demo"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

echo "=== Creating kind cluster ==="

# Check prerequisites
for cmd in kind kubectl podman; do
  if ! command -v "$cmd" &>/dev/null; then
    echo "ERROR: $cmd is required but not found"
    exit 1
  fi
done

# Delete existing cluster if present
if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
  echo "Deleting existing kind cluster '${CLUSTER_NAME}'..."
  kind delete cluster --name "$CLUSTER_NAME"
fi

# Create kind cluster
echo "Creating kind cluster '${CLUSTER_NAME}'..."
kind create cluster --name "$CLUSTER_NAME" --config "$ROOT_DIR/kind-config.yaml"

echo ""
echo "Cluster '${CLUSTER_NAME}' is ready."
echo "Run 'make deploy' to build images and deploy resources."
