# CE-Manager Demo — Runbook

Two paths, same demo. Pick ONE.

- **Local dev** (what I actually tested end-to-end): three processes on localhost, ~5 min to first run.
- **Kubernetes** (kind): `make deploy-ce` + `make demo-ce-on`. Needs SSH agent forwarding for `llm-client`.

## Prerequisites (both paths)

- `ibac/.env` with:
  ```
  WATSONX_API_KEY=...
  WATSONX_PROJECT_ID=...
  WATSONX_URL=https://us-south.ml.cloud.ibm.com
  ```
- CE-Manager checked out at the workspace root (sibling of `ibac/`): `../CE-Manager`.
- Python 3.11+ with venv support, Go 1.23+.

---

## Path A — Local dev (tested, recommended for first run)

From `ibac/`.

### 1. Set up the Python venv with CE-Manager + proxy deps

```bash
python3 -m venv /tmp/ce-venv
source /tmp/ce-venv/bin/activate
pip install -U pip
pip install fastapi 'uvicorn[standard]' litellm tiktoken httpx pydantic python-dotenv numpy
# CE-Manager from source, --no-deps so llm-ce-proxy is skipped:
pip install --no-deps ../CE-Manager
deactivate
```

CE-Manager still eagerly imports `llm_client` at load time. For local dev,
stub it (real one is installed via SSH inside Docker):

```bash
SITE=$(/tmp/ce-venv/bin/python -c "import sysconfig; print(sysconfig.get_paths()['purelib'])")
mkdir -p "$SITE/llm_client/llm"
cat > "$SITE/llm_client/__init__.py" <<'PY'
def get_llm(*a, **k):
    raise RuntimeError("llm_client stub — ce-proxy provides its own MaskerClient")
PY
cat > "$SITE/llm_client/llm/__init__.py" <<'PY'
from .. import get_llm  # noqa: F401
PY
cat > "$SITE/llm_client/llm/types.py" <<'PY'
class GenerationArgs:
    def __init__(self, *a, **k): pass
PY
```

### 2. Sanity-check watsonx connectivity (optional but catches credential bugs)

```bash
/tmp/ce-venv/bin/python scripts/ce_probe.py
```

Expected: "NATIVE tool_calls work" + "All checks passed".

### 3. Generate fixtures (idempotent, already committed to repo)

```bash
python3 scripts/fixtures_gen.py
```

Produces 8 JSON fixtures in `ce-demo-agent/fixtures/`, each sized to ~18–24K o200k tokens.

### 4. Start the three services

**Terminal 1 — observer** (Go):

```bash
go build -o /tmp/observer ./observer/
/tmp/observer
# listens on :7070 — UI at http://localhost:7070
```

**Terminal 2 — ce-proxy** (Python):

```bash
set -a; . ./.env; set +a
for b in API_KEY PROJECT_ID URL; do
  wa="WATSONX_$b"; wx="WX_$b"
  eval "[ -n \"\$$wa\" ] && export $wx=\"\$$wa\""
done
export PYTHONPATH=$PWD/ce-proxy
export CE_MODE=on                 # flip between off/on for the two scenarios
export CE_MAX_TOKENS=131072
export CE_THRESHOLD_FRAC=0.50     # NOT 0.75 — see "gotchas" below
export CE_EMIT_DIFF_CONTENT=true
export CE_KEEP_LAST_N_TURNS=2
export CE_AGGRESSIVENESS=0.8
export CE_MAX_OUTPUT_CHARS=120000
export OBSERVER_URL=http://localhost:7070
export WATSONX_MODEL=watsonx/openai/gpt-oss-120b
/tmp/ce-venv/bin/uvicorn app.main:app --host 127.0.0.1 --port 9100
```

**Terminal 3 — ce-demo-agent** (Go):

```bash
go build -o /tmp/ce-demo-agent ./ce-demo-agent/
LLM_URL=http://127.0.0.1:9100 \
LLM_MODEL=openai/gpt-oss-120b \
OBSERVER_URL=http://127.0.0.1:7070 \
  /tmp/ce-demo-agent
# listens on :8080
```

### 5. Run the scenario (Terminal 4)

```bash
# Reset CE-Manager state between runs
curl -s -X POST http://127.0.0.1:9100/admin/reset

# Q1 + Q2 against a fresh session
SESS="demo-$(date +%s)"
curl -sS --max-time 900 -X POST http://127.0.0.1:8080/ \
  -H "Content-Type: application/json" -H "X-Session-Id: $SESS" \
  -d @testdata/ce-demo-queries.json  # won't work as-is; see below
```

Use the script for both queries:

```bash
# Produces two curls for Q1 and Q2 using testdata/ce-demo-queries.json
python3 - <<PY
import json, os, subprocess, time
q = json.load(open("testdata/ce-demo-queries.json"))
sid = f"demo-{int(time.time())}"
print(f"Session: {sid}")
print(f"Observer: http://localhost:7070  (click '{sid}')")
for label, text in (("Q1", q["q1"]), ("Q2", q["q2"])):
    print(f"\n=== {label} ===")
    out = subprocess.check_output(["curl", "-sS", "--max-time", "900",
        "-X", "POST", "http://127.0.0.1:8080/",
        "-H", "Content-Type: application/json",
        "-H", f"X-Session-Id: {sid}",
        "-d", json.dumps({"query": text})])
    print(json.loads(out).get("response", "")[:400])
PY
```

Open `http://localhost:7070`, click the session in the left sidebar, and
watch the CE Manager lane fill with `threshold_crossed` → `mask_started` →
`mask_codegen` → `context_diff` → `mask_applied` cards.

### 6. Compare scenarios

Re-run Step 5 after toggling `CE_MODE`:

```bash
# In Terminal 2, Ctrl-C the proxy, change CE_MODE to off, restart.
# Or, for a zero-downtime toggle via env-reload, kill -HUP doesn't work with
# uvicorn — you need a restart.
```

With `CE_MODE=off`, Q2 will overflow (HTTP 413, `agent_blocked` card in red).
With `CE_MODE=on`, Q2 succeeds with a real answer about checkout health.

---

## Path B — Kubernetes (kind)

Assumes the base IBAC demo is already deployed (`make create-cluster` + `make deploy`).
The CE demo only adds two new Deployments and a Secret; it reuses the existing
`demo-observer` pod.

```bash
# 1. Ensure SSH agent has your github.ibm.com key (needed to install llm-client)
eval "$(ssh-agent)"
ssh-add ~/.ssh/id_ed25519_ibm  # or whichever key has github.ibm.com access

# 2. Build + deploy the two new images
make deploy-ce

# 3. Run the off scenario (expect HTTP 413 / agent_blocked on Q2)
make demo-ce-off

# 4. Run the on scenario (expect masker + both answers)
make demo-ce-on

# Or both back-to-back:
make demo-ce
```

Dashboard: `http://localhost:30070`. Agent: `http://localhost:30021`.

---

## Gotchas / why the defaults are what they are

- **`CE_THRESHOLD_FRAC=0.50`, not 0.75.** The masker sends the entire current
  conversation + a ~3K-token prompt template to watsonx, so its own prompt
  needs to fit under the 131K window. At 75% threshold the masker's prompt
  overflows. At 50% it has comfortable headroom.
- **Validator relaxation.** CE-Manager rejects `[CE-OMITTED]` markers inside
  JSON-like tool content without a reversible handle. The demo relaxes that
  guard at proxy startup (see `ce-proxy/app/compact.py`). Set
  `CE_STRICT_VALIDATION=1` to restore.
- **Deterministic fallback.** If `compact_conversation` raises or returns a
  no-op, the proxy drops all but the last 2 tool-role messages to a marker
  string. Emits a `mask_fallback` warn event so it's visible in the UI.
- **gpt-oss-120b Harmony channels.** gpt-oss puts its internal reasoning in
  `reasoning_content`; final answer lands in `content`. The agent and the
  `MaskerClient` both fall back to `reasoning_content` when `content` is empty.
- **`max_tokens=8192` on the masker call.** Lower values cut off the model
  mid-reasoning, producing empty `content`. 8192 is the sweet spot.
