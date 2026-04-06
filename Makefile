.PHONY: help create-cluster delete-cluster deploy undeploy \
       demo-no-ibac demo-ibac demo-finance logs

.DEFAULT_GOAL := help

help:
	@echo "Usage: make <target>"
	@echo ""
	@echo "Kubernetes targets:"
	@echo "  create-cluster        Create kind cluster"
	@echo "  delete-cluster        Delete kind cluster"
	@echo "  deploy                Build images and deploy all resources"
	@echo "  undeploy              Delete all deployed resources (keeps cluster)"
	@echo "  demo-no-ibac          Run attack WITHOUT IBAC (exfiltration succeeds)"
	@echo "  demo-ibac             Run attack WITH IBAC (exfiltration blocked)"
	@echo "  demo-finance          Run finance demo with live SPARC + IBAC pipeline"
	@echo "  logs                  View logs from all pods"

# --- Kubernetes (kind) targets ---

create-cluster:
	./scripts/k8s-create-cluster.sh

delete-cluster:
	./scripts/k8s-cleanup.sh

deploy:
	./scripts/k8s-deploy.sh

undeploy:
	./scripts/k8s-undeploy.sh

demo-no-ibac:
	./scripts/k8s-demo-no-ibac.sh

demo-ibac:
	./scripts/k8s-demo-ibac.sh

demo-finance:
	./scripts/k8s-demo-finance.sh

logs:
	@echo "=== evil-server ===" && kubectl -n ibac logs -l app=evil-server --tail=50 || true
	@echo "=== email-server ===" && kubectl -n ibac logs -l app=email-server --tail=50 || true
	@echo "=== agent-no-ibac ===" && kubectl -n ibac logs -l app=agent-no-ibac --tail=50 || true
	@echo "=== ibac-agent (agent) ===" && kubectl -n ibac logs -l app=ibac-agent -c agent --tail=50 || true
	@echo "=== ibac-agent (envoy) ===" && kubectl -n ibac logs -l app=ibac-agent -c envoy --tail=50 || true
	@echo "=== ibac-agent (sidecar) ===" && kubectl -n ibac logs -l app=ibac-agent -c sidecar --tail=50 || true
	@echo "=== finance-backend ===" && kubectl -n ibac logs -l app=finance-backend --tail=50 || true
	@echo "=== finance-agent ===" && kubectl -n ibac logs -l app=finance-agent -c finance-agent --tail=50 || true
	@echo "=== finance-agent (envoy) ===" && kubectl -n ibac logs -l app=finance-agent -c envoy --tail=50 || true
	@echo "=== finance-agent (sidecar) ===" && kubectl -n ibac logs -l app=finance-agent -c sidecar --tail=50 || true
	@echo "=== sparc-reflector (pod) ===" && kubectl -n ibac logs -l app=sparc-reflector --tail=50 || true
	@echo "=== sparc-worker (host) ===" && tail -n 50 .runtime/sparc-worker.log || true
	@echo "=== demo-observer ===" && kubectl -n ibac logs -l app=demo-observer --tail=50 || true
