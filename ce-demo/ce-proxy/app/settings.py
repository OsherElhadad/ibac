"""Environment-backed settings for the CE-Proxy."""
from __future__ import annotations

import os


def _env_bool(name: str, default: bool) -> bool:
    v = os.environ.get(name)
    if v is None:
        return default
    return v.strip().lower() in ("1", "true", "yes", "on")


class Settings:
    def __init__(self) -> None:
        self.ce_mode: str = os.environ.get("CE_MODE", "off").strip().lower()
        self.ce_max_tokens: int = int(os.environ.get("CE_MAX_TOKENS", "131072"))
        self.ce_threshold_frac: float = float(os.environ.get("CE_THRESHOLD_FRAC", "0.65"))
        self.ce_emit_diff_content: bool = _env_bool("CE_EMIT_DIFF_CONTENT", True)
        self.ce_keep_last_n: int = int(os.environ.get("CE_KEEP_LAST_N_TURNS", "2"))
        # Summary mode keeps more turns verbatim so the recap is less
        # destructive — the overall compactor is still cheap, but the
        # last few turns that matter most to the next answer stay intact.
        self.ce_summary_keep_last_n: int = int(os.environ.get("CE_SUMMARY_KEEP_LAST_N", "4"))
        self.ce_max_output_chars: int = int(os.environ.get("CE_MAX_OUTPUT_CHARS", "120000"))
        self.ce_aggressiveness: float = float(os.environ.get("CE_AGGRESSIVENESS", "0.8"))
        # Use `hybrid` mode: distill() gets KEEP/MASK/SUMMARIZE/OMIT ops.
        # OMIT lets it replace a whole stale tool output with
        # "[CE-OMITTED: N chars]" which is what CE-Manager's validator
        # accepts as a legitimate drop (my _validate_tool_json_facts patch
        # whitelists `[CE-OMITTED:` outputs).
        #
        # `masking_only` is too strict for this demo: its rule "every output
        # string must be a substring of the corresponding input string"
        # forbids the [CE-OMITTED:] marker entirely, and CE-Manager's
        # _collect_action_facts invariant rejects substring outputs that
        # silently drop decoy rows. The two rules combine to make real
        # compaction impossible in masking_only; hybrid is the right mode
        # for "drop old tool outputs that already had their signal
        # extracted" behaviour.
        self.ce_prompt_mode: str = os.environ.get("CE_PROMPT_MODE", "hybrid").strip()
        self.ce_objective: str = os.environ.get(
            "CE_OBJECTIVE",
            # Hybrid-mode objective. CE-Manager gives the LLM four ops:
            # KEEP / MASK (substring) / SUMMARIZE ([CE-SUMMARY] ...) / OMIT
            # ([CE-OMITTED: N chars]). We want the masker to OMIT stale tool
            # outputs aggressively — that's how we get 80-90% per-pass
            # reduction on decoy-heavy JSON without hitting CE-Manager's
            # fact-preservation invariant.
            "Compact aggressively — target >=80% total-char reduction per pass. "
            "For each tool_result message whose signal value has ALREADY been "
            "echoed by a later assistant message, use OMIT: set its content to "
            "'[CE-OMITTED: N chars]' where N is the original char length. Never "
            "try to filter-and-keep rows of a JSON tool output — that triggers "
            "'lost action-critical facts' in the validator and every attempt "
            "will fail. Keep the most recent user_turn and the most recent "
            "assistant message verbatim. For tool_results whose signal has NOT "
            "been echoed yet, also KEEP verbatim.",
        )

        # Whether to emit a full proxy_request_snapshot event with the outbound
        # messages on every forward. Enables the "open full conversation"
        # viewer in the observer UI; costs a few MB of session data.
        self.ce_emit_outbound_snapshots: bool = _env_bool("CE_EMIT_OUTBOUND_SNAPSHOTS", True)

        # Pricing for cost attribution. We first try litellm.completion_cost();
        # if that returns 0 / None / raises, we fall back to these env rates.
        # Defaults match IBM's published watsonx gpt-oss-120b rate ($/1M tok).
        self.price_input_per_1m_usd: float = float(os.environ.get("WATSONX_PRICE_INPUT_PER_1M_USD", "0.16"))
        self.price_output_per_1m_usd: float = float(os.environ.get("WATSONX_PRICE_OUTPUT_PER_1M_USD", "0.72"))

        self.watsonx_api_key: str = os.environ.get("WATSONX_API_KEY") or os.environ.get("WX_API_KEY", "")
        self.watsonx_project_id: str = os.environ.get("WATSONX_PROJECT_ID") or os.environ.get("WX_PROJECT_ID", "")
        self.watsonx_url: str = (
            os.environ.get("WATSONX_URL")
            or os.environ.get("WX_URL")
            or "https://us-south.ml.cloud.ibm.com"
        )
        # LiteLLM-prefixed model id the proxy uses on the outbound side
        self.watsonx_model: str = os.environ.get("WATSONX_MODEL", "watsonx/openai/gpt-oss-120b")
        # Bare model id we accept on the inbound side (agents set this in their request)
        self.accepted_model: str = os.environ.get("ACCEPTED_MODEL", "openai/gpt-oss-120b")

        self.observer_url: str = os.environ.get("OBSERVER_URL", "").rstrip("/")

        self.port: int = int(os.environ.get("PORT", "9100"))

    @property
    def ce_threshold_tokens(self) -> int:
        return int(self.ce_max_tokens * self.ce_threshold_frac)

    def mode_on(self) -> bool:
        # True when CE is active at all (any of the three CE variants).
        return self.ce_mode in ("on", "summary", "truncate")

    def mode_masker(self) -> bool:
        # CE is on AND the programmatic masker/rewriter is selected.
        return self.ce_mode == "on"

    def mode_summary(self) -> bool:
        # CE is on AND the summarization rewrite is selected instead.
        return self.ce_mode == "summary"

    def mode_truncate(self) -> bool:
        # CE is on AND the deterministic message-eviction baseline is selected.
        # This mode drops old assistant+tool turn pairs at threshold, without
        # any LLM call — deliberately cheap and deliberately lossy.
        return self.ce_mode == "truncate"

    def compaction_method(self) -> str:
        # Human-readable method label used in events + UI.
        if self.ce_mode == "on":       return "programmatic"
        if self.ce_mode == "summary":  return "summary"
        if self.ce_mode == "truncate": return "truncate"
        return "off"


settings = Settings()
