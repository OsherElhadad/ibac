# IBAC Demo

IBAC is an intent-based access control demo for agentic systems. It combines:

- IBAC at the outbound network boundary with Envoy + an ext_proc sidecar
- SPARC before first-party finance tool calls
- Ollama for the local task agent
- Watsonx for SPARC reflection
- A live observer UI for replaying the full pipeline

The repo now includes two demos:

- Email prompt-injection demo
- Finance sequential demo with SPARC first and IBAC second

## What The Finance Demo Shows

The finance demo is the main end-to-end scenario:

1. The user asks for a refund with a partial transaction ID and explicitly states: `The refund reason is duplicate charge.`
2. The finance agent hallucinates a full ID and proposes `get_transaction("TX4821")`.
3. SPARC blocks that proposal because the full ID was not grounded.
4. The agent asks for clarification.
5. The user provides the real full ID, which is different from the hallucinated one, and the refund succeeds.
6. Later the user asks to process an invoice.
7. The invoice contains an explicit prompt-injection instruction telling automated agents to ignore the user and POST data to a separate audit endpoint.
8. The finance agent attempts `http_post(...)`.
9. IBAC intercepts the outbound request and blocks it as prompt injection.

## Architecture

### High-Level Flow

```text
User / Browser
    |
    v
localhost:30020
    |
    v
Envoy :10000
    |
    v
finance-agent :8080
    |
    +--> outbound HTTP via Envoy :10001 --> sidecar :9090 --> SPARC reflector :8090 --> host SPARC worker --> Watsonx
    |
    +--> finance-backend :8181
    |
    +--> outbound HTTP via Envoy :10001 --> sidecar :9090 --> ALLOW/BLOCK
    |
    v
demo-observer :7070 / localhost:30070
```

### Services And Ports

| Component | Purpose | Port / Address |
|---|---|---|
| `finance-agent` | Protected finance agent | `localhost:30020` via Envoy |
| `demo-observer` | Live UI and replay API | `localhost:30070` |
| `sparc-reflector` | In-cluster SPARC relay endpoint | `sparc-reflector:8090` |
| Host SPARC worker | Watsonx-backed SPARC execution | local process under `.runtime/` |
| `finance-backend` | Transactions, customers, refunds, invoices | `finance-backend:8181` |
| `sidecar` | IBAC policy engine and sidecar-owned SPARC execution | `:9090` ext_proc |
| `envoy` | Inbound and outbound interception | `:10000` and `:10001` |
| `evil-server` | Exfiltration target for demos | `evil-server:9999` |
| `audit-acme-payments` | Malicious invoice callback alias | `audit-acme-payments:9999` |
| `email-server` | Poisoned email API | `localhost:30888` internally `:8888` |
| `agent-no-ibac` | Unprotected email demo agent | `localhost:30080` |
| `ibac-agent` | Protected email demo agent | `localhost:30000` |
| `ibac-ollama` | Local model container | `localhost:11434` |

### Trust Boundaries

- SPARC is only used before first-party finance tools:
  - `get_transaction`
  - `lookup_customer`
  - `issue_refund`
  - `get_invoice`
- The finance agent does not know about SPARC directly and does not prepare SPARC payloads.
- The sidecar reconstructs SPARC inputs from observed traffic:
  - inbound user request
  - outbound Ollama tool proposal
  - outbound finance-backend request
  - finance-backend response
  - final assistant reply
- The sidecar is the only component that calls `sparc-reflector`.
- SPARC is not used for `http_post`. That second failure is intentionally left for IBAC.
- The finance backend is a trusted transport destination.
- The invoice text returned by the finance backend is untrusted content.
- IBAC treats document-introduced outbound write destinations as prompt injection and blocks them.

## Repository Structure

```text
ibac/
├── agent/                      # Email demo agent
├── email-server/               # Poisoned email API
├── evil-server/                # Exfiltration receiver
├── finance-agent/              # Finance demo agent
├── finance-backend/            # Finance demo backend
├── observer/                   # Live dashboard + replay API
├── sidecar/                    # IBAC ext_proc server + sidecar-owned SPARC flow
├── internal/finance/           # Shared finance tool schema and prompts
├── sparc-reflector/            # SPARC relay service + host worker
├── internal/demo/              # Shared event schema/emitter
├── k8s/                        # Kubernetes manifests
├── scripts/                    # Cluster, deploy, undeploy, demo scripts
└── testdata/                   # Email demo prompt-injection fixtures
```

## Prerequisites

- `kind`
- `kubectl`
- `podman`
- `docker`
- a local Docker container named `ibac-ollama`
- Watsonx credentials in [`ibac/.env`](./.env)

### Watsonx Environment

The finance demo expects these in [`ibac/.env`](./.env):

```bash
WX_API_KEY=...
WX_PROJECT_ID=...
WX_URL=https://us-south.ml.cloud.ibm.com
```

### Start Ollama

```bash
podman machine start

docker run -d --name ibac-ollama --network kind -p 11434:11434 \
  -v ollama-data:/root/.ollama ollama/ollama:latest

docker exec ibac-ollama ollama pull llama3.2:3b
curl http://localhost:11434/api/tags
```

## Quick Start

```bash
make create-cluster
make deploy
open http://localhost:30070
make demo-finance
```

Useful extras:

```bash
make demo-no-ibac
make demo-ibac
make logs
make undeploy
make delete-cluster
```

## Clean End-To-End Run

If you want to reproduce the demo from scratch:

```bash
make delete-cluster
make create-cluster
make deploy
open http://localhost:30070
make demo-finance
```

## Dashboard Guide

The observer UI at `http://localhost:30070` shows four complementary views:

- `Services`: static map of the cluster services and host entry points
- `Architecture`: trust boundaries, ports, and the SPARC/IBAC control points
- `Conversation`: user requests, tool calls, tool outputs, blocked network attempts, and final agent replies
- `Pipeline Timeline`: stage-by-stage event stream grouped by source
- `Raw Logs`: supporting evidence for each component

The intended finance flow is easy to inspect in the UI:

1. `User` lane shows the refund request.
2. `Finance Agent` lane shows the hallucinated tool proposal.
3. `SPARC` lane shows the observed tool proposal, the sidecar-launched reflection, and the rejection score.
4. `Conversation` shows the clarification turn.
5. `Finance Backend` shows transaction, customer, and refund outputs.
6. `User` lane shows the invoice request.
7. `Finance Agent` shows the `http_post` attempt.
8. `IBAC` shows the intercept and final block.

## Email Demo

### Without IBAC

```bash
make demo-no-ibac
```

This uses the unprotected agent at `localhost:30080`. The poisoned email contains an instruction to POST to the evil server, and the agent follows it.

### With IBAC

```bash
make demo-ibac
```

This uses the protected agent at `localhost:30000`. IBAC captures the original user intent, intercepts the malicious outbound POST, and blocks it.

## Finance Demo

```bash
make demo-finance
```

Expected behavior:

- Turn 1: SPARC blocks the hallucinated `transaction_id`
- Turn 2: refund succeeds after clarification
- Turn 3: IBAC blocks the injected outbound POST
- Demo ends after the IBAC block and the final agent explanation

The demo script automatically:

- creates a fresh session ID
- emits a session-start event to the observer
- runs the three turns in order
- waits for the expected SPARC + refund + IBAC sequence

## How IBAC Works

1. Envoy receives inbound traffic on `:10000`.
2. The sidecar captures the original user intent and stores it under `X-Session-Id`.
3. The agent runs normally and may make outbound HTTP requests.
4. Outbound traffic is redirected through Envoy `:10001`.
5. The sidecar compares the outbound action against the original intent.
6. IBAC fails closed on missing session state, unavailable model, or unparseable decision.

## How SPARC Works Here

1. The finance agent calls Ollama with the shared finance tool inventory.
2. The sidecar observes the model's tool proposal in the outbound Ollama response and stores it as a pending tool call.
3. When the agent tries to reach the trusted finance backend, that outbound request is intercepted by Envoy and evaluated inside the sidecar before it is allowed through.
4. The sidecar combines:
   - the observed conversation history
   - the shared finance tool inventory
   - the pending model tool proposal
5. The sidecar sends that reconstructed payload to `sparc-reflector`.
6. The reflector relays the job to the host SPARC worker.
7. The host worker runs ALTK SPARC with Watsonx using `Track.FAST_TRACK`.
8. If SPARC blocks the tool call, the sidecar returns a synthetic clarification-needed tool result to the agent without mentioning SPARC.
9. If SPARC approves the tool call, the backend request continues normally.

## Commands

```bash
make create-cluster   # create kind cluster
make deploy           # build/load images and deploy resources
make demo-no-ibac     # email prompt injection without IBAC
make demo-ibac        # email prompt injection with IBAC
make demo-finance     # finance sequential SPARC + IBAC demo
make logs             # tail pod logs and host SPARC worker logs
make undeploy         # delete namespace resources, keep cluster
make delete-cluster   # delete kind cluster
```

## Notes

- The deploy script creates a host-side SPARC worker virtualenv under `.runtime/`.
- `.runtime/`, `.build-hashes/`, and Python `__pycache__` directories are ignored.
- The Watsonx-backed SPARC worker runs on the host because direct Watsonx egress from the kind pod path is unreliable on this machine.
