#!/usr/bin/env bash
# Create a fresh kind cluster for the CE-Manager demo.
# No Ollama, no ALTK, no SPARC — just the minimum needed for ce-proxy,
# ce-demo-agent, and ce-observer.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
CE_DEMO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

CLUSTER_NAME="${CLUSTER_NAME:-ibac-demo}"
KIND_CONFIG="${KIND_CONFIG:-$CE_DEMO_DIR/kind-config.yaml}"

if kind get clusters 2>/dev/null | grep -qx "$CLUSTER_NAME"; then
  echo "kind cluster '$CLUSTER_NAME' already exists — nothing to do."
  exit 0
fi

if [ ! -f "$KIND_CONFIG" ]; then
  echo "ERROR: kind-config.yaml not found at $KIND_CONFIG"
  exit 1
fi

echo "Creating kind cluster '$CLUSTER_NAME'..."
kind create cluster --name "$CLUSTER_NAME" --config "$KIND_CONFIG"

kubectl config use-context "kind-$CLUSTER_NAME" >/dev/null
kubectl get nodes

echo ""
echo "Cluster ready. Next:"
echo "  cp $CE_DEMO_DIR/.env.example $CE_DEMO_DIR/.env   # paste WATSONX creds"
echo "  make -C ibac/ce-demo deploy"
