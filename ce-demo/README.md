# CE-Manager Live Demo

Context Engineering live demo against IBM watsonx `gpt-oss-120b`. Shows
four compaction strategies — programmatic masker, LLM summarizer,
deterministic truncator, and no-compaction baseline — on a
tool-calling agent running a noisy 6-tool security investigation
pipeline.

## What you get

Three services:

| Service         | Role                                                                 | Port  |
|-----------------|----------------------------------------------------------------------|-------|
| `ce-proxy/`     | FastAPI proxy that runs CE-Manager's masker/summarizer/truncator     | 9100  |
| `ce-demo-agent/`| Go tool-calling agent running a 6-tool investigation pipeline        | 30021 |
| `ce-observer/`  | Go + static web observer (events, token/cost/latency charts, pieces) | 30071 |

The demo drives Q1 (an incident investigation that produces a compact
8-fact answer) and Q2 (a follow-up lookup), with the context deliberately
engineered to overflow `gpt-oss-120b`'s 131K window unless a compaction
strategy kicks in.

## Prerequisites

- macOS (Linux works — swap `rdctl` for your kind provider)
- Docker Desktop or Rancher Desktop with kind
- Python 3.11+
- Go 1.23+
- Node 20+ (only if you want to run Playwright UI smoke tests)
- IBM watsonx API credentials

## First-time install

```bash
cp .env.example .env           # paste WATSONX_API_KEY / PROJECT_ID / URL
make proxy-mac-install         # venv + CE-Manager fetch + requirements
```

## Mac-hosted fast path (recommended)

The ce-proxy runs on the Mac; the agent and observer run in kind. This
sidesteps the Rancher Desktop VM NAT, which is the single biggest source
of flaky watsonx latency.

```bash
make create-cluster            # kind cluster (minimal, no Ollama)
make deploy                    # build + load images, apply k8s manifests
make proxy-mac-up              # start ce-proxy on :9100

make demo-mac-off              # baseline: no CE, Q2 overflows
make demo-mac-on               # CE masker: Q1 + Q2 both answer correctly
make demo-mac-summary          # CE-Manager's built-in Summarizer
make demo-mac-truncate         # deterministic eviction baseline

# Tear down
make proxy-mac-down
make delete-cluster
```

Open the observer at <http://localhost:30071>.

## All-in-cluster path

If your VM network is healthy, you can skip the Mac-side proxy:

```bash
make create-cluster
make deploy
make demo-off demo-on demo-summary demo-truncate
```

## How the masker works

`ce-proxy/app/compact.py` implements `run_masker`. On each threshold
crossing (default 65 % of 131K input budget), the proxy calls the LLM
with:

1. The user's latest question (so the masker knows the goal).
2. A JSON array of `{idx, role, content}` targets — every assistant
   and tool message that has NOT yet been compacted on a prior pass
   (the cached prefix is frozen, preventing drift).

The LLM returns a JSON object mapping each idx to shorter content, with
these hard rules:

- For `role='tool'`, output must be a complete parseable JSON string
  with the same top-level keys; each kept row is copied byte-for-byte.
- For `role='assistant'`, output is plain text.
- No `"..."`, placeholders, empty strings, or truncated JSON.
- Rows matching the user goal's filter conditions are all kept (not
  just one).
- Naive first-N / last-N trimming is forbidden.

See `ce-proxy/app/compact.py` for the full prompt and verification
rules.

## File layout

```
ibac/ce-demo/
  Makefile                      # every target in this demo
  README.md                     # you are here
  .env.example                  # WATSONX creds template
  kind-config.yaml              # minimal kind cluster
  ce-proxy/                     # Python FastAPI proxy + CE logic
  ce-demo-agent/                # Go tool-calling agent
  ce-observer/                  # Go + static-web observer
  dockerfiles/                  # one Dockerfile per service
  k8s/                          # kind manifests (namespace: ibac)
  scripts/                      # create-cluster / deploy / demo / proxy
  testdata/                     # ce-demo-queries.json
```

## Troubleshooting

- **`ERROR: missing .env`** — run `cp .env.example .env` and paste your
  credentials.
- **`Mac venv missing`** — run `make proxy-mac-install`.
- **`no Mac IP is reachable from the kind pod`** — ensure your VPN is
  connected (kind pods can only reach you via the VPN tunnel IP). On
  flaky VM networks, run `rdctl shutdown && rdctl start` and retry.
- **`Unknown role: final` from watsonx** — a Harmony-format artefact;
  the proxy's `sanitize_messages` strips these tokens; if you still
  see it, check that `ce-proxy-mac-down && ce-proxy-mac-up` loads the
  current `compact.py`.
