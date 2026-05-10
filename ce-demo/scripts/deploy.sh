#!/usr/bin/env bash
# Deploy the CE-Manager demo components (ce-observer, ce-proxy, ce-demo-agent)
# into an existing kind cluster.
#
# No SSH keys required: the IBM-internal `llm-client` dep is stubbed into
# the ce-proxy image via committed source at ce-proxy/stubs/llm_client/.
# CE-Manager source is fetched via HTTPS if absent.
set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-ibac-demo}"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"                  # ibac/ce-demo/
WORKSPACE_ROOT="$(cd "$ROOT_DIR/../.." && pwd)"           # ibac-ce/ (contains CE-Manager/)

echo "=== Deploying CE-Manager demo ==="

kubectl config use-context "kind-${CLUSTER_NAME}" &>/dev/null || {
  echo "ERROR: kubectl context 'kind-${CLUSTER_NAME}' not found. Run make create-cluster first."
  exit 1
}

if [ ! -f "$ROOT_DIR/.env" ]; then
  echo "ERROR: missing $ROOT_DIR/.env with WATSONX_* credentials."
  echo "Copy .env.example to .env and fill in your watsonx credentials."
  exit 1
fi

set -a
. "$ROOT_DIR/.env"
set +a

WATSONX_API_KEY="${WATSONX_API_KEY:-${WX_API_KEY:-}}"
WATSONX_PROJECT_ID="${WATSONX_PROJECT_ID:-${WX_PROJECT_ID:-}}"
WATSONX_URL="${WATSONX_URL:-${WX_URL:-https://us-south.ml.cloud.ibm.com}}"

if [ -z "$WATSONX_API_KEY" ] || [ -z "$WATSONX_PROJECT_ID" ]; then
  echo "ERROR: WATSONX_API_KEY / WATSONX_PROJECT_ID not set in $ROOT_DIR/.env"
  exit 1
fi

# --- Ensure CE-Manager source is available (HTTPS clone if absent) ---
"$SCRIPT_DIR/fetch-ce-manager.sh"

kubectl get namespace ibac >/dev/null 2>&1 || kubectl create namespace ibac

# Watsonx secret for ce-proxy
kubectl -n ibac create secret generic ce-watsonx \
  --from-literal=WATSONX_API_KEY="$WATSONX_API_KEY" \
  --from-literal=WATSONX_PROJECT_ID="$WATSONX_PROJECT_ID" \
  --from-literal=WATSONX_URL="$WATSONX_URL" \
  --dry-run=client -o yaml | kubectl apply -f -

# --- Build the three images. None of these need SSH. ---
IBAC_DIR="$(cd "$ROOT_DIR/.." && pwd)"                  # ibac/ (parent of ce-demo/)

echo "Building ce-observer image..."
podman build -t localhost/ibac-ce-observer:latest \
  -f "$ROOT_DIR/dockerfiles/Dockerfile.ce-observer" "$IBAC_DIR"

echo "Building ce-demo-agent image..."
podman build -t localhost/ibac-ce-demo-agent:latest \
  -f "$ROOT_DIR/dockerfiles/Dockerfile.ce-demo-agent" "$IBAC_DIR"

echo "Building ce-proxy image (committed llm_client stub, no SSH required)..."
podman build \
  -t localhost/ibac-ce-proxy:latest \
  -f "$ROOT_DIR/dockerfiles/Dockerfile.ce-proxy" "$WORKSPACE_ROOT"

# Load into the kind node
for img in \
    localhost/ibac-ce-observer:latest \
    localhost/ibac-ce-demo-agent:latest \
    localhost/ibac-ce-proxy:latest \
; do
  tmp_tar="$(mktemp "/tmp/ibac-image-XXXXXX.tar")"
  echo "Loading $img into kind cluster..."
  podman save -o "$tmp_tar" "$img"
  kind load image-archive "$tmp_tar" --name "$CLUSTER_NAME"
  rm -f "$tmp_tar"
done

echo "Applying CE demo manifests..."
kubectl apply -f "$ROOT_DIR/k8s/ce-observer.yaml"
kubectl apply -f "$ROOT_DIR/k8s/ce-demo.yaml"

echo "Restarting pods to pick up fresh images..."
kubectl -n ibac delete pod -l app=ce-observer    --ignore-not-found
kubectl -n ibac delete pod -l app=ce-proxy       --ignore-not-found
kubectl -n ibac delete pod -l app=ce-demo-agent  --ignore-not-found

echo "Waiting for pods to be ready..."
kubectl -n ibac wait --for=condition=Ready pod -l app=ce-observer   --timeout=120s
kubectl -n ibac wait --for=condition=Ready pod -l app=ce-proxy      --timeout=120s
kubectl -n ibac wait --for=condition=Ready pod -l app=ce-demo-agent --timeout=120s

echo ""
echo "=== CE Demo deploy complete ==="
echo "  CE Observer UI: http://localhost:30071"
echo "  CE Demo Agent:  http://localhost:30021"
echo ""
echo "Next:"
echo "  make -C ibac/ce-demo demo-off   # scenario without CE (Q2 overflows)"
echo "  make -C ibac/ce-demo demo-on    # scenario with CE (masker fires)"
