# CE-Manager Live Demo — standalone entrypoint.
#
# This Makefile has NO dependency on the parent ibac/ SPARC / finance-agent /
# Ollama setup. It drives three components:
#   - ce-proxy/       Python FastAPI proxy that runs CE-Manager's masker /
#                     summarizer / truncator against watsonx traffic.
#   - ce-demo-agent/  Go tool-calling agent running a 6-tool security
#                     investigation pipeline.
#   - ce-observer/    Go + static HTML/JS/CSS server showing per-call token,
#                     cost, and latency breakdown for watsonx + CE calls.
#
# Two ways to run:
#   (a) Mac-hosted fast path  — ce-proxy runs on the Mac, agent+observer
#       run in kind. Bypasses the Rancher Desktop VM NAT. Recommended.
#   (b) All-in-cluster path   — everything inside kind. Requires the VM
#       network to be healthy.
#
# Both paths use the SAME ce-watsonx secret / .env for credentials.

SHELL := /usr/bin/env bash

# ----- Cluster / in-cluster targets ------------------------------------------

.PHONY: create-cluster
create-cluster: ## Create a fresh kind cluster for the demo
	./scripts/create-cluster.sh

.PHONY: delete-cluster
delete-cluster: ## Delete the kind cluster
	kind delete cluster --name ibac-demo

.PHONY: deploy
deploy: ## Build + load images and apply k8s manifests
	./scripts/deploy.sh

.PHONY: demo-off demo-on demo-summary demo-truncate
demo-off:      ; ./scripts/demo.sh off       ## Run demo with CE_MODE=off      (baseline, Q2 overflows)
demo-on:       ; ./scripts/demo.sh on        ## Run demo with CE_MODE=on       (programmatic masker)
demo-summary:  ; ./scripts/demo.sh summary   ## Run demo with CE_MODE=summary  (LLM summarizer)
demo-truncate: ; ./scripts/demo.sh truncate  ## Run demo with CE_MODE=truncate (deterministic eviction)

# ----- Mac-hosted ce-proxy path (recommended for flaky VM networks) ----------

.PHONY: proxy-mac-install
proxy-mac-install: ## One-time: create Mac venv + fetch CE-Manager + install deps
	./scripts/install-proxy-venv.sh

.PHONY: proxy-mac-up
proxy-mac-up: ## Start Mac-hosted ce-proxy on :9100
	./scripts/run-proxy-mac.sh up

.PHONY: proxy-mac-down
proxy-mac-down: ## Stop Mac-hosted ce-proxy
	./scripts/run-proxy-mac.sh down

.PHONY: proxy-mac-status
proxy-mac-status: ## Check Mac-hosted ce-proxy status
	./scripts/run-proxy-mac.sh status

.PHONY: proxy-mac-logs
proxy-mac-logs: ## Tail Mac-hosted ce-proxy logs
	./scripts/run-proxy-mac.sh logs

.PHONY: demo-mac-off demo-mac-on demo-mac-summary demo-mac-truncate
demo-mac-off:      ; ./scripts/demo-mac.sh off       ## Mac proxy, CE_MODE=off
demo-mac-on:       ; ./scripts/demo-mac.sh on        ## Mac proxy, CE_MODE=on       (masker)
demo-mac-summary:  ; ./scripts/demo-mac.sh summary   ## Mac proxy, CE_MODE=summary  (summarizer)
demo-mac-truncate: ; ./scripts/demo-mac.sh truncate  ## Mac proxy, CE_MODE=truncate (truncator)

# ----- Observability ---------------------------------------------------------

.PHONY: logs
logs: ## Tail in-cluster ce-proxy logs
	kubectl -n ibac logs -f deploy/ce-proxy

.PHONY: open-observer
open-observer: ## Open observer UI in the default browser
	@open http://localhost:30071 2>/dev/null || xdg-open http://localhost:30071 2>/dev/null || echo "Open http://localhost:30071 manually"

# ----- Playwright UI smoke tests --------------------------------------------

.PHONY: ui-test-install
ui-test-install: ## Install Playwright + Chromium for UI smoke tests
	cd ce-observer && npm ci && npx playwright install --with-deps chromium

.PHONY: ui-test
ui-test: ## Run Playwright UI smoke tests
	cd ce-observer && npx playwright test

# ----- Housekeeping ----------------------------------------------------------

.PHONY: clean
clean: ## Reset proxy state + remove Mac venv + logs
	-curl -sS -X POST http://localhost:9100/admin/reset
	-rm -rf ../../.runtime/ce-proxy-venv ../../.runtime/ce-proxy-mac.log ../../.runtime/ce-proxy-mac.pid

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "CE-Manager Live Demo — standalone Makefile\n\nUsage: make <target>\n\nTargets:\n"} /^[a-zA-Z_-]+:.*?##/ { printf "  %-22s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

.DEFAULT_GOAL := help
