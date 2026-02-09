# IBAC - Intent-Based Access Control

IBAC uses an Envoy sidecar to intercept AI agent traffic and an LLM to validate that outbound actions align with the user's original intent. This prevents prompt injection attacks where a malicious file tricks an AI agent into exfiltrating sensitive data.

## Architecture

```
curl ──POST──> Envoy :10000 ──ext_proc──> Agent :8080 ──(ollama)──> tool calls
                (captures intent)                              |
                                                               v
               Envoy :10001 <── outbound HTTP ────── http_post tool
                (validates vs intent via LLM)
                  |
                  |── ALLOW ──> destination
                  └── BLOCK ──> 403 Forbidden
```

**Components:**
- **Agent** (:8080) - AI agent with `read_file` and `http_post` tools, powered by ollama (llama3.2:3b)
- **Sidecar** (:9090) - gRPC ext_proc server that captures inbound intent and validates outbound actions via LLM
- **Envoy** (:10000 inbound, :10001 outbound) - Transparent proxy with ext_proc filters
- **Evil-server** (:9999) - Mock exfiltration target for demo

## Prerequisites

- Go 1.23+
- [ollama](https://ollama.ai) with `llama3.2:3b` model pulled
- [func-e](https://func-e.io) for running Envoy locally (`brew install func-e`)

```bash
# Pull the required model
ollama pull llama3.2:3b

# Verify ollama is running
curl http://localhost:11434/v1/models
```

## Build

```bash
make build
```

This builds three binaries into `bin/`:
- `bin/agent`
- `bin/sidecar`
- `bin/evil-server`

## End-to-End Demo

### Demo 1: Normal Flow (benign file through IBAC)

Open 3 terminals:

```bash
# Terminal 1: Start the IBAC sidecar
make run-sidecar

# Terminal 2: Start Envoy
make envoy

# Terminal 3: Start the agent with IBAC proxy
make run-agent-ibac
```

Send a request through the IBAC pipeline:

```bash
curl -s -X POST \
  -H "Content-Type: application/json" \
  -H "X-Session-Id: demo-1" \
  -d '{"query":"Read the file report.txt and give me a summary"}' \
  http://localhost:10000/ | python3 -m json.tool
```

**Expected:** The agent reads the benign report and returns a summary. The sidecar logs show intent capture but no outbound validation (no external HTTP calls made).

### Demo 2: Attack WITHOUT IBAC (exfiltration succeeds)

This demonstrates the attack vector that IBAC protects against.

Open 2 terminals:

```bash
# Terminal 1: Start the agent in direct mode (no IBAC proxy)
make run-agent

# Terminal 2: Start the evil server
make run-evil-server
```

Send the malicious request directly to the agent (bypassing IBAC):

```bash
curl -s -X POST \
  -H "Content-Type: application/json" \
  -d '{"query":"Read the file report-malicious.txt and give me a summary"}' \
  http://localhost:8080/ | python3 -m json.tool
```

**Expected:** The agent reads the malicious file, gets tricked by the prompt injection into reading `/etc/passwd`, and POSTs data to the evil server. Check the evil-server terminal to see the exfiltrated data.

### Demo 3: Attack WITH IBAC (exfiltration blocked)

This demonstrates IBAC blocking the attack.

Open 4 terminals:

```bash
# Terminal 1: Start the IBAC sidecar
make run-sidecar

# Terminal 2: Start Envoy
make envoy

# Terminal 3: Start the agent with IBAC proxy
make run-agent-ibac

# Terminal 4: Start the evil server
make run-evil-server
```

Send the same malicious request, but through IBAC:

```bash
curl -s -X POST \
  -H "Content-Type: application/json" \
  -H "X-Session-Id: demo-3" \
  -d '{"query":"Read the file report-malicious.txt and give me a summary"}' \
  http://localhost:10000/ | python3 -m json.tool
```

**Expected:**
- The agent reads the malicious file and gets tricked into attempting exfiltration
- The IBAC sidecar intercepts the outbound POST to `localhost:9999/exfiltrate`
- The LLM compares the outbound action against the original intent ("Read report-malicious.txt and give me a summary")
- The sidecar returns **BLOCK** with a 403 Forbidden
- The evil-server receives **nothing**

Check the sidecar logs (Terminal 1) to see:
```
[IBAC] Captured intent for session demo-3: Read the file report-malicious.txt and give me a summary
[IBAC] outbound request: session=demo-3 method=POST authority=localhost:9999 path=/exfiltrate
[IBAC] Decision for session demo-3: BLOCK - POSTing sensitive data to external server is suspicious...
```

## Shortcut: Demo Scripts

Alternatively, use the provided scripts (after starting the required components):

```bash
./scripts/demo-normal.sh              # Demo 1
./scripts/demo-attack-no-ibac.sh      # Demo 2
./scripts/demo-attack-with-ibac.sh    # Demo 3
```

## How It Works

1. **Inbound intent capture**: When a user request arrives at Envoy (:10000), the Lua filter adds `x-ibac-direction: inbound`. The ext_proc sidecar extracts the `query` field and stores it keyed by `X-Session-Id`.

2. **Agent processing**: The agent receives the request, calls ollama with tool definitions, and executes tool calls (read_file, http_post) in a loop.

3. **Outbound validation**: When the agent makes an outbound HTTP request (via `IBAC_PROXY=http://localhost:10001`), Envoy's outbound listener routes it through ext_proc. The sidecar looks up the original intent for the session, asks the LLM to compare intent vs. action, and either allows or blocks with a 403.

4. **Fail-closed**: If the LLM is unavailable, the session ID is missing, or the response is unparseable, the sidecar defaults to **BLOCK**.

## Project Structure

```
ibac/
├── agent/main.go              # AI agent: HTTP server + ollama + tools
├── sidecar/main.go            # IBAC ext_proc: intent capture + LLM validation
├── evil-server/main.go        # Mock exfiltration target
├── envoy/envoy.yaml           # Envoy config: inbound + outbound listeners
├── testdata/
│   ├── report.txt             # Benign file
│   └── report-malicious.txt   # File with prompt injection payload
├── scripts/
│   ├── demo-normal.sh
│   ├── demo-attack-no-ibac.sh
│   └── demo-attack-with-ibac.sh
├── go.mod / go.sum
└── Makefile
```
