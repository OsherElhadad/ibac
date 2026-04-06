#!/usr/bin/env bash
set -euo pipefail

CLUSTER_NAME="ibac-demo"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
WORKSPACE_ROOT="$(cd "$ROOT_DIR/.." && pwd)"
HASH_DIR="$ROOT_DIR/.build-hashes"
RUNTIME_DIR="$ROOT_DIR/.runtime"
SPARC_WORKER_PIDFILE="$RUNTIME_DIR/sparc-worker.pid"
SPARC_WORKER_LOG="$RUNTIME_DIR/sparc-worker.log"
SPARC_WORKER_VENV="$RUNTIME_DIR/sparc-worker-venv"
SPARC_WORKER_VENV_HASH="$RUNTIME_DIR/sparc-worker-venv.hash"
mkdir -p "$HASH_DIR"
mkdir -p "$RUNTIME_DIR"

HOST_PYTHON_BIN=""
for candidate in python3.12 python python3; do
  if command -v "$candidate" >/dev/null 2>&1; then
    HOST_PYTHON_BIN="$(command -v "$candidate")"
    break
  fi
done

if [ -z "$HOST_PYTHON_BIN" ]; then
  echo "ERROR: could not find a host Python interpreter for the SPARC worker"
  exit 1
fi

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

if [ ! -f "$ROOT_DIR/.env" ]; then
  echo "ERROR: missing $ROOT_DIR/.env"
  exit 1
fi

set -a
. "$ROOT_DIR/.env"
set +a

WX_API_KEY="${WX_API_KEY:-${WATSONX_API_KEY:-}}"
WX_PROJECT_ID="${WX_PROJECT_ID:-${WATSONX_PROJECT_ID:-}}"
WX_URL="${WX_URL:-${WATSONX_URL:-https://us-south.ml.cloud.ibm.com}}"

if [ -z "$WX_API_KEY" ] || [ -z "$WX_PROJECT_ID" ]; then
  echo "ERROR: Watsonx credentials are missing in ibac/.env"
  echo "Set WX_API_KEY/WX_PROJECT_ID (or WATSONX_API_KEY/WATSONX_PROJECT_ID)."
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

restart_host_sparc_worker() {
  if [ -f "$SPARC_WORKER_PIDFILE" ]; then
    local existing_pid
    existing_pid="$(cat "$SPARC_WORKER_PIDFILE" 2>/dev/null || true)"
    if [ -n "$existing_pid" ] && kill -0 "$existing_pid" 2>/dev/null; then
      kill "$existing_pid" 2>/dev/null || true
      sleep 1
    fi
    rm -f "$SPARC_WORKER_PIDFILE"
  fi

  echo "Starting host-side SPARC worker..."
  ensure_host_sparc_worker_env
  (
    cd "$ROOT_DIR"
    set -a
    . "$ROOT_DIR/.env"
    set +a
    export PYTHONPATH="$WORKSPACE_ROOT/agent-lifecycle-toolkit${PYTHONPATH:+:$PYTHONPATH}"
    export OBSERVER_URL="http://localhost:30070"
    export SPARC_WORKER_ID="host-sparc"
    nohup "$SPARC_WORKER_VENV/bin/python" "$ROOT_DIR/sparc-reflector/worker.py" >"$SPARC_WORKER_LOG" 2>&1 &
    echo $! >"$SPARC_WORKER_PIDFILE"
  )

  sleep 2
  if ! kill -0 "$(cat "$SPARC_WORKER_PIDFILE")" 2>/dev/null; then
    echo "ERROR: host-side SPARC worker failed to start"
    if [ -f "$SPARC_WORKER_LOG" ]; then
      tail -n 50 "$SPARC_WORKER_LOG"
    fi
    exit 1
  fi
}

ensure_host_sparc_worker_env() {
  local current_hash
  current_hash=$(cat \
    "$ROOT_DIR"/sparc-reflector/*.py \
    "$WORKSPACE_ROOT"/agent-lifecycle-toolkit/pyproject.toml \
    | shasum -a 256 | cut -d' ' -f1)
  current_hash="${current_hash}-$(basename "$HOST_PYTHON_BIN")"
  local saved_hash
  saved_hash=$(cat "$SPARC_WORKER_VENV_HASH" 2>/dev/null || true)

  local host_python_version
  host_python_version=$("$HOST_PYTHON_BIN" -c 'import sys; print(f"{sys.version_info.major}.{sys.version_info.minor}")')
  local venv_python_version=""
  if [ -x "$SPARC_WORKER_VENV/bin/python" ]; then
    venv_python_version=$("$SPARC_WORKER_VENV/bin/python" -c 'import sys; print(f"{sys.version_info.major}.{sys.version_info.minor}")' 2>/dev/null || true)
  fi

  if [ -n "$venv_python_version" ] && [ "$venv_python_version" != "$host_python_version" ]; then
    rm -rf "$SPARC_WORKER_VENV"
    saved_hash=""
  fi

  if [ ! -x "$SPARC_WORKER_VENV/bin/python" ]; then
    echo "Creating host-side SPARC worker virtualenv..."
    "$HOST_PYTHON_BIN" -m venv "$SPARC_WORKER_VENV"
    saved_hash=""
  fi

  if [ "$current_hash" != "$saved_hash" ]; then
    echo "Installing host-side SPARC worker dependencies..."
    "$SPARC_WORKER_VENV/bin/pip" install --upgrade pip >/dev/null
    "$SPARC_WORKER_VENV/bin/pip" install --no-cache-dir "$WORKSPACE_ROOT/agent-lifecycle-toolkit"
    echo "$current_hash" > "$SPARC_WORKER_VENV_HASH"
  fi
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
if needs_build sidecar "$ROOT_DIR"/sidecar/*.go "$ROOT_DIR"/sidecar/*.txt "$ROOT_DIR"/internal/demo/*.go "$ROOT_DIR"/go.mod "$ROOT_DIR"/go.sum "$ROOT_DIR"/Dockerfile.sidecar; then
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

# --- iptables-init ---
if needs_build iptables-init "$ROOT_DIR"/Dockerfile.iptables-init; then
  echo "Building iptables-init image..."
  podman build -t localhost/ibac-iptables-init:latest -f "$ROOT_DIR/Dockerfile.iptables-init" "$ROOT_DIR"
  BUILD_IMAGES+=(localhost/ibac-iptables-init:latest)
elif needs_load "localhost/ibac-iptables-init:latest"; then
  echo "Iptables-init image missing from kind cluster, will load."
  LOAD_IMAGES+=(localhost/ibac-iptables-init:latest)
else
  echo "Iptables-init image up to date, skipping."
fi

# --- finance-agent ---
if needs_build finance-agent "$ROOT_DIR"/finance-agent/*.go "$ROOT_DIR"/internal/demo/*.go "$ROOT_DIR"/go.mod "$ROOT_DIR"/go.sum "$ROOT_DIR"/Dockerfile.finance-agent; then
  echo "Building finance-agent image..."
  podman build -t localhost/ibac-finance-agent:latest -f "$ROOT_DIR/Dockerfile.finance-agent" "$ROOT_DIR"
  BUILD_IMAGES+=(localhost/ibac-finance-agent:latest)
elif needs_load "localhost/ibac-finance-agent:latest"; then
  echo "Finance-agent image missing from kind cluster, will load."
  LOAD_IMAGES+=(localhost/ibac-finance-agent:latest)
else
  echo "Finance-agent image up to date, skipping."
fi

# --- finance-backend ---
if needs_build finance-backend "$ROOT_DIR"/finance-backend/*.go "$ROOT_DIR"/internal/demo/*.go "$ROOT_DIR"/go.mod "$ROOT_DIR"/go.sum "$ROOT_DIR"/Dockerfile.finance-backend; then
  echo "Building finance-backend image..."
  podman build -t localhost/ibac-finance-backend:latest -f "$ROOT_DIR/Dockerfile.finance-backend" "$ROOT_DIR"
  BUILD_IMAGES+=(localhost/ibac-finance-backend:latest)
elif needs_load "localhost/ibac-finance-backend:latest"; then
  echo "Finance-backend image missing from kind cluster, will load."
  LOAD_IMAGES+=(localhost/ibac-finance-backend:latest)
else
  echo "Finance-backend image up to date, skipping."
fi

# --- observer ---
if needs_build observer "$ROOT_DIR"/observer/main.go "$ROOT_DIR"/observer/static/* "$ROOT_DIR"/internal/demo/*.go "$ROOT_DIR"/go.mod "$ROOT_DIR"/go.sum "$ROOT_DIR"/Dockerfile.observer; then
  echo "Building observer image..."
  podman build -t localhost/ibac-observer:latest -f "$ROOT_DIR/Dockerfile.observer" "$ROOT_DIR"
  BUILD_IMAGES+=(localhost/ibac-observer:latest)
elif needs_load "localhost/ibac-observer:latest"; then
  echo "Observer image missing from kind cluster, will load."
  LOAD_IMAGES+=(localhost/ibac-observer:latest)
else
  echo "Observer image up to date, skipping."
fi

# --- sparc-reflector ---
if needs_build sparc-reflector "$ROOT_DIR"/sparc-reflector/*.py "$ROOT_DIR"/Dockerfile.sparc-reflector "$WORKSPACE_ROOT"/agent-lifecycle-toolkit/pyproject.toml "$WORKSPACE_ROOT"/agent-lifecycle-toolkit/altk/core/llm/providers/litellm/*.py "$WORKSPACE_ROOT"/agent-lifecycle-toolkit/altk/core/llm/*.py "$WORKSPACE_ROOT"/agent-lifecycle-toolkit/altk/pre_tool/core/*.py "$WORKSPACE_ROOT"/agent-lifecycle-toolkit/altk/pre_tool/sparc/*.py; then
  echo "Building sparc-reflector image..."
  podman build -t localhost/ibac-sparc-reflector:latest -f "$ROOT_DIR/Dockerfile.sparc-reflector" "$WORKSPACE_ROOT"
  BUILD_IMAGES+=(localhost/ibac-sparc-reflector:latest)
elif needs_load "localhost/ibac-sparc-reflector:latest"; then
  echo "Sparc-reflector image missing from kind cluster, will load."
  LOAD_IMAGES+=(localhost/ibac-sparc-reflector:latest)
else
  echo "Sparc-reflector image up to date, skipping."
fi

# --- External images (envoy) ---
# Check if external images need to be loaded into kind
EXTERNAL_IMAGES=()

if needs_load "envoyproxy/envoy:v1.28-latest"; then
  echo "Envoy image missing from kind cluster, will load."
  # Pull to podman if not present
  if ! podman image exists "envoyproxy/envoy:v1.28-latest" 2>/dev/null; then
    echo "Pulling envoyproxy/envoy:v1.28-latest to podman..."
    podman pull envoyproxy/envoy:v1.28-latest
  fi
  EXTERNAL_IMAGES+=(envoyproxy/envoy:v1.28-latest)
else
  echo "Envoy image present in kind cluster, skipping."
fi

# Combine all arrays for loading into kind
ALL_LOAD=()
ALL_LOAD+=(${BUILD_IMAGES[@]+"${BUILD_IMAGES[@]}"})
ALL_LOAD+=(${LOAD_IMAGES[@]+"${LOAD_IMAGES[@]}"})
ALL_LOAD+=(${EXTERNAL_IMAGES[@]+"${EXTERNAL_IMAGES[@]}"})

if [ ${#ALL_LOAD[@]} -gt 0 ]; then
  echo "Loading ${#ALL_LOAD[@]} image(s) into kind cluster..."
  for img in "${ALL_LOAD[@]}"; do
    tmp_tar="$(mktemp "/tmp/ibac-image-XXXXXX.tar")"
    echo "Saving $img to archive..."
    podman save -o "$tmp_tar" "$img"
    echo "Loading $img into kind cluster..."
    kind load image-archive "$tmp_tar" --name "$CLUSTER_NAME"
    rm -f "$tmp_tar"
  done
else
  echo "All images up to date, nothing to load."
fi

# Get the ollama container IP
OLLAMA_IP=$(docker inspect ibac-ollama -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' 2>/dev/null)

if [ -z "$OLLAMA_IP" ]; then
  echo "ERROR: ibac-ollama container not found. Please run: docker run -d --name ibac-ollama --network kind -p 11434:11434 -v ollama-data:/root/.ollama ollama/ollama:latest"
  exit 1
fi

echo "Using ollama container IP $OLLAMA_IP"

# Ensure namespace exists before creating secrets
kubectl get namespace ibac >/dev/null 2>&1 || kubectl create namespace ibac

# Create/update the Watsonx secret for SPARC
kubectl -n ibac create secret generic sparc-watsonx \
  --from-literal=WX_API_KEY="$WX_API_KEY" \
  --from-literal=WX_PROJECT_ID="$WX_PROJECT_ID" \
  --from-literal=WX_URL="$WX_URL" \
  --dry-run=client -o yaml | kubectl apply -f -

# Apply manifests with IP substitution
echo "Applying Kubernetes manifests..."
sed "s/172\.20\.0\.3/$OLLAMA_IP/g" "$ROOT_DIR/k8s/agent.yaml" | kubectl apply -f -
sed "s/172\.20\.0\.3/$OLLAMA_IP/g" "$ROOT_DIR/k8s/finance-agent.yaml" | kubectl apply -f -
kubectl apply -f "$ROOT_DIR/k8s/envoy-config.yaml"
kubectl apply -f "$ROOT_DIR/k8s/evil-server.yaml"
kubectl apply -f "$ROOT_DIR/k8s/email-server.yaml"
kubectl apply -f "$ROOT_DIR/k8s/demo-observer.yaml"
kubectl apply -f "$ROOT_DIR/k8s/sparc-service.yaml"

echo "Refreshing protected agent pods to pick up config changes..."
kubectl -n ibac delete pod -l app=ibac-agent --ignore-not-found
kubectl -n ibac delete pod -l app=finance-agent --ignore-not-found

# Restart pods if images were rebuilt so they pick up the new images
# (only for BUILD_IMAGES — LOAD_IMAGES are just missing from the node, pods
#  will pick them up on next create via the manifests applied above)
if [ ${#BUILD_IMAGES[@]} -gt 0 ]; then
  echo "Restarting pods to pick up new images..."
  for img in "${BUILD_IMAGES[@]}"; do
    case "$img" in
      *finance-agent:*) kubectl -n ibac delete pod -l app=finance-agent --ignore-not-found ;;
      *agent:*)         kubectl -n ibac delete pod -l app=ibac-agent --ignore-not-found
                        kubectl -n ibac delete pod -l app=agent-no-ibac --ignore-not-found ;;
      *sidecar:*)       kubectl -n ibac delete pod -l app=ibac-agent --ignore-not-found
                        kubectl -n ibac delete pod -l app=finance-agent --ignore-not-found ;;
      *iptables-init:*) kubectl -n ibac delete pod -l app=ibac-agent --ignore-not-found
                        kubectl -n ibac delete pod -l app=finance-agent --ignore-not-found ;;
      *evil-server:*)   kubectl -n ibac delete pod -l app=evil-server --ignore-not-found ;;
      *email-server:*)  kubectl -n ibac delete pod -l app=email-server --ignore-not-found ;;
      *finance-backend:*) kubectl -n ibac delete pod -l app=finance-backend --ignore-not-found ;;
      *observer:*)      kubectl -n ibac delete pod -l app=demo-observer --ignore-not-found ;;
      *sparc-reflector:*) kubectl -n ibac delete pod -l app=sparc-reflector --ignore-not-found ;;
    esac
  done
fi

# Wait for pods to be ready
echo "Waiting for pods to be ready..."
kubectl -n ibac wait --for=condition=Ready pod -l app=evil-server --timeout=120s
kubectl -n ibac wait --for=condition=Ready pod -l app=email-server --timeout=120s
kubectl -n ibac wait --for=condition=Ready pod -l app=agent-no-ibac --timeout=120s
kubectl -n ibac wait --for=condition=Ready pod -l app=ibac-agent --timeout=120s
kubectl -n ibac wait --for=condition=Ready pod -l app=demo-observer --timeout=120s
kubectl -n ibac wait --for=condition=Ready pod -l app=finance-backend --timeout=120s
kubectl -n ibac wait --for=condition=Ready pod -l app=finance-agent --timeout=120s
kubectl -n ibac wait --for=condition=Ready pod -l app=sparc-reflector --timeout=120s

restart_host_sparc_worker

echo "Checking in-cluster reachability to SPARC reflector..."
kubectl -n ibac exec deploy/finance-backend -- sh -lc \
  'wget -qO- --timeout=15 http://sparc-reflector.ibac.svc.cluster.local:8090/healthz >/dev/null' || true

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
echo "  Finance Demo: http://localhost:30020"
echo "  Dashboard:    http://localhost:30070"
echo ""
echo "Run 'make demo-no-ibac', 'make demo-ibac', or 'make demo-finance' to execute the demos"
