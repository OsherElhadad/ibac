#!/usr/bin/env bash
set -euo pipefail

CLUSTER_NAME="ibac-demo"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
HASH_DIR="$ROOT_DIR/.build-hashes"
mkdir -p "$HASH_DIR"

echo "=== Deploying IBAC demo resources ==="

# Check ollama is running
if ! curl -sf http://localhost:11434/api/tags &>/dev/null; then
  echo "ERROR: ollama is not running on localhost:11434"
  echo "Start it with: ollama serve"
  exit 1
fi

# needs_rebuild computes a hash of the source files for a component
# and returns 0 (true) if the image needs rebuilding.
needs_rebuild() {
  local name="$1"
  shift
  local current_hash
  current_hash=$(cat "$@" 2>/dev/null | shasum -a 256 | cut -d' ' -f1)
  local saved_hash
  saved_hash=$(cat "$HASH_DIR/$name" 2>/dev/null || echo "")
  if [ "$current_hash" = "$saved_hash" ]; then
    return 1  # no rebuild needed
  fi
  echo "$current_hash" > "$HASH_DIR/$name"
  return 0  # rebuild needed
}

# Build and load images only when source changes
IMAGES=()

if needs_rebuild agent "$ROOT_DIR"/agent/*.go "$ROOT_DIR"/go.mod "$ROOT_DIR"/go.sum "$ROOT_DIR"/Dockerfile.agent "$ROOT_DIR"/testdata/*; then
  echo "Building agent image..."
  podman build -t localhost/ibac-agent:latest -f "$ROOT_DIR/Dockerfile.agent" "$ROOT_DIR"
  IMAGES+=(localhost/ibac-agent:latest)
else
  echo "Agent image up to date, skipping."
fi

if needs_rebuild sidecar "$ROOT_DIR"/sidecar/*.go "$ROOT_DIR"/go.mod "$ROOT_DIR"/go.sum "$ROOT_DIR"/Dockerfile.sidecar; then
  echo "Building sidecar image..."
  podman build -t localhost/ibac-sidecar:latest -f "$ROOT_DIR/Dockerfile.sidecar" "$ROOT_DIR"
  IMAGES+=(localhost/ibac-sidecar:latest)
else
  echo "Sidecar image up to date, skipping."
fi

if needs_rebuild evil-server "$ROOT_DIR"/evil-server/*.go "$ROOT_DIR"/go.mod "$ROOT_DIR"/go.sum "$ROOT_DIR"/Dockerfile.evil-server; then
  echo "Building evil-server image..."
  podman build -t localhost/ibac-evil-server:latest -f "$ROOT_DIR/Dockerfile.evil-server" "$ROOT_DIR"
  IMAGES+=(localhost/ibac-evil-server:latest)
else
  echo "Evil-server image up to date, skipping."
fi

if needs_rebuild email-server "$ROOT_DIR"/email-server/*.go "$ROOT_DIR"/go.mod "$ROOT_DIR"/go.sum "$ROOT_DIR"/Dockerfile.email-server; then
  echo "Building email-server image..."
  podman build -t localhost/ibac-email-server:latest -f "$ROOT_DIR/Dockerfile.email-server" "$ROOT_DIR"
  IMAGES+=(localhost/ibac-email-server:latest)
else
  echo "Email-server image up to date, skipping."
fi

# Load only rebuilt images into kind
if [ ${#IMAGES[@]} -gt 0 ]; then
  echo "Loading ${#IMAGES[@]} image(s) into kind cluster..."
  for img in "${IMAGES[@]}"; do
    kind load docker-image "$img" --name "$CLUSTER_NAME"
  done
else
  echo "All images up to date, nothing to load."
fi

# Apply manifests (agent.yaml first — it creates the ibac namespace)
echo "Applying Kubernetes manifests..."
kubectl apply -f "$ROOT_DIR/k8s/agent.yaml"
kubectl apply -f "$ROOT_DIR/k8s/envoy-config.yaml"
kubectl apply -f "$ROOT_DIR/k8s/evil-server.yaml"
kubectl apply -f "$ROOT_DIR/k8s/email-server.yaml"

# Restart pods if images were rebuilt so they pick up the new images
if [ ${#IMAGES[@]} -gt 0 ]; then
  echo "Restarting pods to pick up new images..."
  for img in "${IMAGES[@]}"; do
    case "$img" in
      *agent:*)      kubectl -n ibac delete pod -l app=ibac-agent --ignore-not-found
                     kubectl -n ibac delete pod -l app=agent-no-ibac --ignore-not-found ;;
      *sidecar:*)    kubectl -n ibac delete pod -l app=ibac-agent --ignore-not-found ;;
      *evil-server:*) kubectl -n ibac delete pod -l app=evil-server --ignore-not-found ;;
      *email-server:*) kubectl -n ibac delete pod -l app=email-server --ignore-not-found ;;
    esac
  done
fi

# Wait for pods to be ready
echo "Waiting for pods to be ready..."
kubectl -n ibac wait --for=condition=Ready pod -l app=evil-server --timeout=120s
kubectl -n ibac wait --for=condition=Ready pod -l app=email-server --timeout=120s
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
