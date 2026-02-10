#!/usr/bin/env bash
set -euo pipefail

CLUSTER_NAME="ibac-demo"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

echo "=== Deploying IBAC demo resources ==="

# Check ollama is running
if ! curl -sf http://localhost:11434/api/tags &>/dev/null; then
  echo "ERROR: ollama is not running on localhost:11434"
  echo "Start it with: ollama serve"
  exit 1
fi

# Build images
echo "Building container images..."
podman build -t localhost/ibac-agent:latest -f "$ROOT_DIR/Dockerfile.agent" "$ROOT_DIR"
podman build -t localhost/ibac-sidecar:latest -f "$ROOT_DIR/Dockerfile.sidecar" "$ROOT_DIR"
podman build -t localhost/ibac-evil-server:latest -f "$ROOT_DIR/Dockerfile.evil-server" "$ROOT_DIR"
podman build -t localhost/ibac-weather-server:latest -f "$ROOT_DIR/Dockerfile.weather-server" "$ROOT_DIR"

# Load images into kind
echo "Loading images into kind cluster..."
kind load docker-image localhost/ibac-agent:latest --name "$CLUSTER_NAME"
kind load docker-image localhost/ibac-sidecar:latest --name "$CLUSTER_NAME"
kind load docker-image localhost/ibac-evil-server:latest --name "$CLUSTER_NAME"
kind load docker-image localhost/ibac-weather-server:latest --name "$CLUSTER_NAME"

# Apply manifests (agent.yaml first — it creates the ibac namespace)
echo "Applying Kubernetes manifests..."
kubectl apply -f "$ROOT_DIR/k8s/agent.yaml"
kubectl apply -f "$ROOT_DIR/k8s/envoy-config.yaml"
kubectl apply -f "$ROOT_DIR/k8s/evil-server.yaml"
kubectl apply -f "$ROOT_DIR/k8s/weather-server.yaml"

# Wait for pods to be ready
echo "Waiting for pods to be ready..."
kubectl -n ibac wait --for=condition=Ready pod -l app=evil-server --timeout=120s
kubectl -n ibac wait --for=condition=Ready pod -l app=weather-server --timeout=120s
kubectl -n ibac wait --for=condition=Ready pod -l app=agent-no-ibac --timeout=120s
kubectl -n ibac wait --for=condition=Ready pod -l app=ibac-agent --timeout=120s

echo ""
echo "=== Deploy Complete ==="
echo "Pods:"
kubectl -n ibac get pods
echo ""
echo "Services:"
kubectl -n ibac get svc
echo ""
echo "Access points:"
echo "  Without IBAC: curl -X POST http://localhost:30080 -d '{\"query\": \"...\"}'"
echo "  With IBAC:    curl -X POST http://localhost:30000 -d '{\"query\": \"...\"}'"
echo ""
echo "Run 'make demo-no-ibac' or 'make demo-ibac' to execute the attack demo"
