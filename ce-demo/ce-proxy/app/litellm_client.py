"""LiteLLM-backed clients for masker (generate) + outbound forward to watsonx.

The masker LLM client calls watsonx DIRECTLY (not through this proxy) to avoid
observability noise and re-gating of the masker's own codegen call.
"""
from __future__ import annotations

import logging
import os
import time
from typing import Any, Dict, List, Optional, Tuple

import httpx
import litellm

from .settings import settings

log = logging.getLogger(__name__)

# LiteLLM's watsonx provider reads WX_API_KEY / WX_PROJECT_ID / WX_URL.
# Populate both aliases so we don't care which the caller set.
for base in ("API_KEY", "PROJECT_ID", "URL"):
    wa = f"WATSONX_{base}"
    wx = f"WX_{base}"
    if os.environ.get(wa) and not os.environ.get(wx):
        os.environ[wx] = os.environ[wa]
    if os.environ.get(wx) and not os.environ.get(wa):
        os.environ[wa] = os.environ[wx]


# -----------------------------------------------------------------------------
# Persistent HTTPX connection pool for watsonx.
#
# Why: without this, LiteLLM constructs a fresh httpx.Client for every call,
# so every call pays (a) a full TCP 3-way handshake to the IBM Cloud edge,
# (b) a fresh TLS 1.3 handshake (no session resumption), and (c) potentially
# a new IAM-token exchange. On a demo where the agent chains 7-10 watsonx
# calls in sequence, that overhead adds up to many seconds.
#
# LiteLLM looks at `litellm.client_session` (sync) and `litellm.aclient_session`
# (async) as user-provided connection pools. Setting them here means:
#   - one long-lived TCP+TLS connection per watsonx host, kept-alive across calls
#   - TLS session tickets cached → resumption on reconnect
#   - glibc DNS cache honoured across calls
#
# This is NOT a timeout/retry patch — it's the standard way to make a Python
# HTTP client reuse connections instead of paying handshake cost per request.
# -----------------------------------------------------------------------------
_POOL_LIMITS = httpx.Limits(
    max_connections=32,
    max_keepalive_connections=16,
    keepalive_expiry=600.0,
)
litellm.client_session = httpx.Client(limits=_POOL_LIMITS, http2=False)
litellm.aclient_session = httpx.AsyncClient(limits=_POOL_LIMITS, http2=False)


# Cache: we ask litellm.completion_cost once; if it doesn't know the model
# (returns 0 / raises), we remember that and skip the call forever for this
# process. For watsonx gpt-oss-120b this saves tens of ms per LLM call.
_litellm_cost_known: Optional[bool] = None


def _extract_meta(resp: Any, elapsed_ms: int) -> Dict[str, Any]:
    """Pull token usage + cost + latency off a LiteLLM ModelResponse.

    Cost: first ask LiteLLM via completion_cost(). If it returns 0 / None /
    raises (common for non-mainstream providers like watsonx gpt-oss), fall
    back to env-var rates applied to resp.usage. After the first fallback,
    skip the LiteLLM call entirely — repeated misses are pure overhead.
    """
    global _litellm_cost_known
    prompt_tokens = 0
    completion_tokens = 0
    total_tokens = 0
    try:
        usage = getattr(resp, "usage", None)
        if usage is not None:
            prompt_tokens = int(getattr(usage, "prompt_tokens", 0) or 0)
            completion_tokens = int(getattr(usage, "completion_tokens", 0) or 0)
            total_tokens = int(getattr(usage, "total_tokens", 0) or 0)
    except Exception:  # noqa: BLE001
        pass

    cost_usd = 0.0
    price_source = "fallback"
    if _litellm_cost_known is not False:
        try:
            c = litellm.completion_cost(completion_response=resp)  # type: ignore[attr-defined]
            if isinstance(c, (int, float)) and c and c > 0:
                cost_usd = float(c)
                price_source = "litellm"
                _litellm_cost_known = True
            else:
                _litellm_cost_known = False
        except Exception:  # noqa: BLE001
            _litellm_cost_known = False
            cost_usd = 0.0

    if cost_usd <= 0.0 and (prompt_tokens or completion_tokens):
        cost_usd = (
            prompt_tokens / 1_000_000.0 * settings.price_input_per_1m_usd
            + completion_tokens / 1_000_000.0 * settings.price_output_per_1m_usd
        )
        price_source = "fallback"

    return {
        "prompt_tokens": prompt_tokens,
        "completion_tokens": completion_tokens,
        "total_tokens": total_tokens,
        "cost_usd": round(cost_usd, 6),
        "latency_ms": elapsed_ms,
        "price_source": price_source,
    }


def _call_and_meter(kwargs: Dict[str, Any], label: str) -> Tuple[Any, Dict[str, Any]]:
    """Run litellm.completion(**kwargs) once and return (resp, meta).

    No timeout, no retry, no patches — one call to LiteLLM.
    """
    t0 = time.monotonic()
    resp = litellm.completion(**kwargs)
    elapsed_ms = int((time.monotonic() - t0) * 1000)
    meta = _extract_meta(resp, elapsed_ms)
    log.info(
        "watsonx[%s]: %d ms · %d in / %d out tokens · $%.6f · src=%s",
        label, meta["latency_ms"], meta["prompt_tokens"], meta["completion_tokens"],
        meta["cost_usd"], meta["price_source"],
    )
    return resp, meta


class MaskerClient:
    """Masker LLM client — an object with .generate(prompt) -> str.

    Used by CE-Manager's programmatic compactor Stage 2. Calls watsonx
    directly via LiteLLM; does NOT go through the proxy.

    After each .generate / .summarize call the per-call meta (cost, tokens,
    latency) is stashed in self.last_generate_meta / self.last_summarize_meta
    so compact.py can emit it on the corresponding event without changing
    the str-returning signature the CE-Manager pipeline expects.
    """

    def __init__(self, model: Optional[str] = None) -> None:
        self.model = model or settings.watsonx_model
        self.last_generate_meta: Optional[Dict[str, Any]] = None
        self.last_summarize_meta: Optional[Dict[str, Any]] = None

    def generate(self, prompt: str) -> str:
        kwargs: Dict[str, Any] = {
            "model": self.model,
            "messages": [
                {
                    "role": "system",
                    "content": (
                        "You are a code-generation assistant. Respond with a single fenced "
                        "```python code block and nothing else. Keep any internal reasoning "
                        "brief so the code block fits in your output budget."
                    ),
                },
                {"role": "user", "content": prompt},
            ],
            "temperature": 0,
            "top_p": 1,
            "max_tokens": 8192,
            "api_key": settings.watsonx_api_key,
            "project_id": settings.watsonx_project_id,
            "api_base": settings.watsonx_url,
        }
        resp, meta = _call_and_meter(kwargs, "masker.generate")
        self.last_generate_meta = meta
        msg = resp.choices[0].message
        content = msg.content or ""
        reasoning = getattr(msg, "reasoning_content", "") or ""
        log.info(
            "MaskerClient.generate: content=%d chars, reasoning=%d chars, prompt=%d chars",
            len(content), len(reasoning), len(prompt),
        )
        if os.environ.get("CE_MASKER_DEBUG") == "1":
            log.info("MaskerClient.generate content preview: %r", content[:400])
            log.info("MaskerClient.generate reasoning preview: %r", reasoning[:400])
        # gpt-oss-120b Harmony: content may be empty; code may land in
        # reasoning_content. Concatenate so the CE-Manager code-extractor
        # can find the ```python block wherever it landed.
        if not content.strip() and reasoning.strip():
            return reasoning
        if content.strip() and reasoning.strip():
            return content + "\n\n" + reasoning
        return content

    def complete_messages(
        self,
        messages: List[Dict[str, Any]],
        *,
        max_tokens: int = 4096,
        temperature: float = 0.2,
        label: str = "ce-call",
        response_format: Optional[Dict[str, Any]] = None,
        reasoning_effort: Optional[str] = None,
    ) -> Tuple[str, Dict[str, Any]]:
        """Send a pre-built messages list to watsonx and return (text, meta).

        Used by compact.py to drive CE-Manager components (Summarizer,
        role-scoped masker) that already know what system+user prompt they
        want — unlike .generate() which prepends its own code-gen system
        prompt. Honours reasoning_content fallback the same way .generate()
        does.

        `reasoning_effort` — when set, asks gpt-oss to keep its reasoning
        channel short. "low" makes the model jump straight to the final
        output instead of burning tokens on "We need to compact…" prose.
        """
        kwargs: Dict[str, Any] = {
            "model": self.model,
            "messages": messages,
            "temperature": temperature,
            "top_p": 1,
            "max_tokens": max_tokens,
            "api_key": settings.watsonx_api_key,
            "project_id": settings.watsonx_project_id,
            "api_base": settings.watsonx_url,
        }
        if response_format is not None:
            kwargs["response_format"] = response_format
        if reasoning_effort is not None:
            kwargs["reasoning_effort"] = reasoning_effort
        resp, meta = _call_and_meter(kwargs, label)
        msg = resp.choices[0].message
        content = msg.content or ""
        reasoning = getattr(msg, "reasoning_content", "") or ""
        # For masker.rewrite we want ONLY the JSON content. The reasoning
        # channel is where "We need to compact…" preamble lives; never
        # fall back to it for callers that passed reasoning_effort="low"
        # (they've explicitly asked for content-only output).
        if reasoning_effort == "low":
            return content.strip(), meta
        if not content.strip() and reasoning.strip():
            return reasoning.strip(), meta
        return content.strip(), meta

    def summarize(self, prompt: str) -> str:
        """Run the summarizer LLM on a prompt and return the summary text.

        The prompt is built by compact.py::run_summarizer. This method just
        forwards it to watsonx with a summarize-friendly system prompt and
        records meta in self.last_summarize_meta.
        """
        kwargs: Dict[str, Any] = {
            "model": self.model,
            "messages": [
                {
                    "role": "system",
                    "content": (
                        "You are summarizing an agent-tool conversation so the agent "
                        "can continue with a reduced context window. Write a faithful "
                        "recap (400-800 words) as flowing prose. PRESERVE every specific "
                        "value VERBATIM — timestamps, HH:MM, IPs, ASNs, domains, "
                        "email addresses, filenames, byte counts, session IDs — do NOT "
                        "paraphrase them. Output only the summary prose; no headings, "
                        "no markdown, no preamble."
                    ),
                },
                {"role": "user", "content": prompt},
            ],
            "temperature": 0.2,
            "top_p": 1,
            "max_tokens": 2048,
            "api_key": settings.watsonx_api_key,
            "project_id": settings.watsonx_project_id,
            "api_base": settings.watsonx_url,
        }
        resp, meta = _call_and_meter(kwargs, "summarizer")
        self.last_summarize_meta = meta
        msg = resp.choices[0].message
        content = msg.content or ""
        reasoning = getattr(msg, "reasoning_content", "") or ""
        log.info(
            "MaskerClient.summarize: content=%d chars, reasoning=%d chars",
            len(content), len(reasoning),
        )
        if not content.strip() and reasoning.strip():
            return reasoning.strip()
        return content.strip()


def forward_to_watsonx(
    messages: List[Dict[str, Any]],
    tools: Optional[List[Dict[str, Any]]] = None,
    tool_choice: Optional[str] = None,
    temperature: Optional[float] = None,
    max_tokens: Optional[int] = None,
    stream: bool = False,
) -> Tuple[Dict[str, Any], Dict[str, Any]]:
    """Forward a chat-completion request to watsonx via LiteLLM.

    Returns (raw_openai_shaped_dict, meta) where meta has cost/tokens/latency.
    """
    kwargs: Dict[str, Any] = {
        "model": settings.watsonx_model,
        "messages": messages,
        "temperature": 0 if temperature is None else temperature,
        "top_p": 1,
        "api_key": settings.watsonx_api_key,
        "project_id": settings.watsonx_project_id,
        "api_base": settings.watsonx_url,
    }
    if tools:
        kwargs["tools"] = tools
    if tool_choice:
        kwargs["tool_choice"] = tool_choice
    if max_tokens is not None:
        kwargs["max_tokens"] = max_tokens

    resp, meta = _call_and_meter(kwargs, "forward")
    if hasattr(resp, "model_dump"):
        return resp.model_dump(), meta
    if hasattr(resp, "json"):
        return resp.json(), meta
    return dict(resp), meta  # best-effort
