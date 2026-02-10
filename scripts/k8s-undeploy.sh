#!/usr/bin/env bash
set -euo pipefail

echo "Deleting ibac namespace and all resources..."
kubectl delete namespace ibac --wait=true
echo "Done. Run 'make deploy' to redeploy."
