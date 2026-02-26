#!/usr/bin/env bash
set -euo pipefail

CLUSTER_NAME="ibac-demo"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
HASH_DIR="$ROOT_DIR/.build-hashes"
mkdir -p "$HASH_DIR"

echo "=== Deploying IBAC demo resources ==="

# Ensure kubectl targets the correct kind cluster
kubectl config use-context "kind-${CLUSTER_NAME}" &>/dev/null || {
  echo "ERROR: kubectl context 'kind-${CLUSTER_NAME}' not found."
  echo "Create the cluster first with: make create-cluster"
  exit 1
}

# Check ollama is running
if ! curl -sf http://localhost:11434/api/tags &>/dev/null; then
  echo "ERROR: ollama is not running on localhost:11434"
  echo "Start it with: ollama serve"
  exit 1
fi

# needs_build checks if an image needs rebuilding:
#   1. Image missing from podman → need build
#   2. Source hash changed → need build
needs_build() {
  local name="$1"; shift
  if ! podman image exists "localhost/ibac-${name}:latest" 2>/dev/null; then
    # Image missing from podman; recompute and save hash so future runs are correct
    local current_hash
    current_hash=$(cat "$@" 2>/dev/null | shasum -a 256 | cut -d' ' -f1)
    echo "$current_hash" > "$HASH_DIR/$name"
    return 0  # need build
  fi
  local current_hash
  current_hash=$(cat "$@" 2>/dev/null | shasum -a 256 | cut -d' ' -f1)
  local saved_hash
  saved_hash=$(cat "$HASH_DIR/$name" 2>/dev/null || echo "")
  if [ "$current_hash" != "$saved_hash" ]; then
    echo "$current_hash" > "$HASH_DIR/$name"
    return 0  # need build
  fi
  return 1
}

# needs_load checks if an image is missing from the kind cluster node.
needs_load() {
  local image="$1"
  docker exec "${CLUSTER_NAME}-control-plane" crictl images -o json 2>/dev/null \
    | grep -q "$image" && return 1
  return 0  # need load
}

# Two arrays: BUILD_IMAGES need build+load, LOAD_IMAGES only need load
BUILD_IMAGES=()
LOAD_IMAGES=()

# --- agent ---
if needs_build agent "$ROOT_DIR"/agent/*.go "$ROOT_DIR"/go.mod "$ROOT_DIR"/go.sum "$ROOT_DIR"/Dockerfile.agent "$ROOT_DIR"/testdata/*; then
  echo "Building agent image..."
  podman build -t localhost/ibac-agent:latest -f "$ROOT_DIR/Dockerfile.agent" "$ROOT_DIR"
  BUILD_IMAGES+=(localhost/ibac-agent:latest)
elif needs_load "localhost/ibac-agent:latest"; then
  echo "Agent image missing from kind cluster, will load."
  LOAD_IMAGES+=(localhost/ibac-agent:latest)
else
  echo "Agent image up to date, skipping."
fi

# --- sidecar ---
if needs_build sidecar "$ROOT_DIR"/sidecar/*.go "$ROOT_DIR"/sidecar/*.txt "$ROOT_DIR"/go.mod "$ROOT_DIR"/go.sum "$ROOT_DIR"/Dockerfile.sidecar; then
  echo "Building sidecar image..."
  podman build -t localhost/ibac-sidecar:latest -f "$ROOT_DIR/Dockerfile.sidecar" "$ROOT_DIR"
  BUILD_IMAGES+=(localhost/ibac-sidecar:latest)
elif needs_load "localhost/ibac-sidecar:latest"; then
  echo "Sidecar image missing from kind cluster, will load."
  LOAD_IMAGES+=(localhost/ibac-sidecar:latest)
else
  echo "Sidecar image up to date, skipping."
fi

# --- evil-server ---
if needs_build evil-server "$ROOT_DIR"/evil-server/*.go "$ROOT_DIR"/go.mod "$ROOT_DIR"/go.sum "$ROOT_DIR"/Dockerfile.evil-server; then
  echo "Building evil-server image..."
  podman build -t localhost/ibac-evil-server:latest -f "$ROOT_DIR/Dockerfile.evil-server" "$ROOT_DIR"
  BUILD_IMAGES+=(localhost/ibac-evil-server:latest)
elif needs_load "localhost/ibac-evil-server:latest"; then
  echo "Evil-server image missing from kind cluster, will load."
  LOAD_IMAGES+=(localhost/ibac-evil-server:latest)
else
  echo "Evil-server image up to date, skipping."
fi

# --- email-server ---
if needs_build email-server "$ROOT_DIR"/email-server/*.go "$ROOT_DIR"/go.mod "$ROOT_DIR"/go.sum "$ROOT_DIR"/Dockerfile.email-server; then
  echo "Building email-server image..."
  podman build -t localhost/ibac-email-server:latest -f "$ROOT_DIR/Dockerfile.email-server" "$ROOT_DIR"
  BUILD_IMAGES+=(localhost/ibac-email-server:latest)
elif needs_load "localhost/ibac-email-server:latest"; then
  echo "Email-server image missing from kind cluster, will load."
  LOAD_IMAGES+=(localhost/ibac-email-server:latest)
else
  echo "Email-server image up to date, skipping."
fi

# Combine both arrays for loading into kind
ALL_LOAD=()
ALL_LOAD+=(${BUILD_IMAGES[@]+"${BUILD_IMAGES[@]}"})
ALL_LOAD+=(${LOAD_IMAGES[@]+"${LOAD_IMAGES[@]}"})
if [ ${#ALL_LOAD[@]} -gt 0 ]; then
  echo "Loading ${#ALL_LOAD[@]} image(s) into kind cluster..."
  for img in "${ALL_LOAD[@]}"; do
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
# (only for BUILD_IMAGES — LOAD_IMAGES are just missing from the node, pods
#  will pick them up on next create via the manifests applied above)
if [ ${#BUILD_IMAGES[@]} -gt 0 ]; then
  echo "Restarting pods to pick up new images..."
  for img in "${BUILD_IMAGES[@]}"; do
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
