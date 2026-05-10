.PHONY: help create-cluster delete-cluster deploy undeploy \
       demo-no-ibac demo-ibac demo-finance logs \
       deploy-ce demo-ce-off demo-ce-on demo-ce-summary demo-ce-truncate demo-ce \
       ce-fixtures ce-probe ui-test ui-test-install \
       ce-proxy-mac-install ce-proxy-mac-up ce-proxy-mac-down ce-proxy-mac-logs \
       demo-ce-mac-off demo-ce-mac-on demo-ce-mac-summary demo-ce-mac-truncate demo-ce-mac

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
	@echo "  deploy-ce             Build + deploy the CE-Manager demo (ce-proxy + ce-demo-agent)"
	@echo "  demo-ce-off           Run CE demo with CE_MODE=off (Q1 overflows mid-investigation)"
	@echo "  demo-ce-on            Run CE demo with CE_MODE=on (programmatic full-rewrite CE)"
	@echo "  demo-ce-summary       Run CE demo with CE_MODE=summary (LLM rewrites old turns as prose)"
	@echo "  demo-ce-truncate      Run CE demo with CE_MODE=truncate (deterministic eviction of old turns)"
	@echo "  demo-ce               Run all four CE demo scenarios back-to-back"
	@echo "  ce-fixtures           Regenerate tool fixtures for the CE demo"
	@echo "  ce-probe              Run the watsonx/gpt-oss-120b sanity probe"
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

# --- CE-Manager demo (moved to ibac/ce-demo/ — run `make -C ibac/ce-demo help`) ---
#
# The CE-Manager demo is self-contained under ibac/ce-demo/. These
# delegation targets keep the old `make deploy-ce` / `make demo-ce-*` /
# `make ce-proxy-mac-*` entrypoints working from the ibac/ root.

deploy-ce:           ; $(MAKE) -C ce-demo deploy
demo-ce-off:         ; $(MAKE) -C ce-demo demo-off
demo-ce-on:          ; $(MAKE) -C ce-demo demo-on
demo-ce-summary:     ; $(MAKE) -C ce-demo demo-summary
demo-ce-truncate:    ; $(MAKE) -C ce-demo demo-truncate
demo-ce:             ; $(MAKE) -C ce-demo demo-off demo-truncate demo-summary demo-on

ce-proxy-mac-install: ; $(MAKE) -C ce-demo proxy-mac-install
ce-proxy-mac-up:      ; $(MAKE) -C ce-demo proxy-mac-up
ce-proxy-mac-down:    ; $(MAKE) -C ce-demo proxy-mac-down
ce-proxy-mac-logs:    ; $(MAKE) -C ce-demo proxy-mac-logs

demo-ce-mac-off:      ; $(MAKE) -C ce-demo demo-mac-off
demo-ce-mac-on:       ; $(MAKE) -C ce-demo demo-mac-on
demo-ce-mac-summary:  ; $(MAKE) -C ce-demo demo-mac-summary
demo-ce-mac-truncate: ; $(MAKE) -C ce-demo demo-mac-truncate
demo-ce-mac:          ; $(MAKE) -C ce-demo demo-mac-off demo-mac-truncate demo-mac-summary demo-mac-on

ce-fixtures:
	python3 ce-demo/scripts/fixtures_gen.py

ce-probe:
	python3 ce-demo/scripts/ce_probe.py

ui-test-install:      ; $(MAKE) -C ce-demo ui-test-install
ui-test:              ; $(MAKE) -C ce-demo ui-test

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
