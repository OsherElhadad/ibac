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

# Add host.docker.internal to /etc/hosts in the kind node
echo "Configuring host.docker.internal..."

# On macOS with Docker Desktop, we need to use the special host.docker.internal that Docker provides
# First, try to get it from the Docker VM
if docker run --rm alpine getent hosts host.docker.internal &>/dev/null; then
  # Docker Desktop provides host.docker.internal automatically
  HOST_IP=$(docker run --rm alpine getent hosts host.docker.internal | awk '{print $1}')
  echo "Using Docker Desktop's host.docker.internal: $HOST_IP"
else
  # Fallback: use the gateway IP
  HOST_IP=$(docker network inspect kind | jq -r '.[0].IPAM.Config[0].Gateway')
  if [ -z "$HOST_IP" ] || [ "$HOST_IP" = "null" ]; then
    HOST_IP=$(docker exec "${CLUSTER_NAME}-control-plane" ip route | grep default | awk '{print $3}')
  fi
  echo "Using gateway IP: $HOST_IP"
fi

if [ -n "$HOST_IP" ] && [ "$HOST_IP" != "null" ]; then
  echo "Adding host.docker.internal -> $HOST_IP to kind node..."
  docker exec "${CLUSTER_NAME}-control-plane" sh -c "echo '$HOST_IP host.docker.internal' >> /etc/hosts"
  
  # Also test if we can reach the host
  echo "Testing connectivity to host..."
  if docker exec "${CLUSTER_NAME}-control-plane" sh -c "command -v nc >/dev/null && nc -zv $HOST_IP 11434 2>&1" | grep -q succeeded; then
    echo "✓ Successfully connected to ollama at $HOST_IP:11434"
  else
    echo "⚠ Warning: Could not connect to ollama at $HOST_IP:11434"
    echo "  Make sure ollama is running with: OLLAMA_HOST=0.0.0.0:11434 ollama serve"
  fi
else
  echo "ERROR: Could not determine host IP. host.docker.internal may not work."
fi

echo "Configuring CoreDNS upstream resolvers..."
kubectl -n kube-system patch configmap coredns --type merge -p '{"data":{"Corefile":".:53 {\n    errors\n    health {\n       lameduck 5s\n    }\n    ready\n    kubernetes cluster.local in-addr.arpa ip6.arpa {\n       pods insecure\n       fallthrough in-addr.arpa ip6.arpa\n       ttl 30\n    }\n    prometheus :9153\n    forward . 8.8.8.8 1.1.1.1 {\n       max_concurrent 1000\n    }\n    cache 30 {\n       disable success cluster.local\n       disable denial cluster.local\n    }\n    loop\n    reload\n    loadbalance\n}\n"}}' >/dev/null
kubectl -n kube-system rollout restart deployment/coredns >/dev/null
kubectl -n kube-system rollout status deployment/coredns --timeout=120s >/dev/null

echo ""
echo "Cluster '${CLUSTER_NAME}' is ready."
echo "Run 'make deploy' to build images and deploy resources."
