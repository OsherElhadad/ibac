"""FastAPI proxy: OpenAI-compatible /v1/chat/completions with CE-Manager compaction.

Supports three CE modes (plus off):
  - CE_MODE=on        — programmatic rewrite (CE-Manager full_rewrite)
  - CE_MODE=summary   — LLM summarizer replaces older messages with prose
  - CE_MODE=truncate  — deterministic eviction of old assistant+tool pairs
  - CE_MODE=off       — pass-through
"""
from __future__ import annotations

import asyncio
import logging
import time
import uuid
from typing import Any, Dict, List, Optional

from fastapi import FastAPI, Header, HTTPException, Request
from fastapi.responses import JSONResponse

from . import events
from .compact import (
    _strip_harmony_markers,
    apply_cache,
    diff_messages,
    reset_state,
    run_masker,
    run_summarizer,
    run_truncator,
    save_compacted,
)
from .litellm_client import forward_to_watsonx
from .settings import settings
from .tokens import count_tokens

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s %(message)s")
log = logging.getLogger("ce-proxy")

app = FastAPI(title="ce-proxy", version="0.1.0")


@app.get("/healthz")
def healthz() -> Dict[str, Any]:
    return {
        "status": "ok",
        "mode": settings.ce_mode,
        "model": settings.watsonx_model,
        "threshold_tokens": settings.ce_threshold_tokens,
        "max_tokens": settings.ce_max_tokens,
    }


@app.post("/admin/reset")
def admin_reset() -> Dict[str, Any]:
    cleared = reset_state()
    events.emit("admin", "proxy_reset", "info", "CE-Manager state reset",
                "Cleared CE caches and reversibility stores.", data=cleared)
    return cleared


def _resolve_session_id(request: Request) -> str:
    sid = request.headers.get("X-Session-Id") or request.headers.get("x-session-id")
    return sid or f"ce-{uuid.uuid4().hex[:12]}"


@app.post("/v1/chat/completions")
async def chat_completions(
    request: Request,
    x_session_id: Optional[str] = Header(default=None, alias="X-Session-Id"),
) -> JSONResponse:
    try:
        payload = await request.json()
    except Exception as e:  # noqa: BLE001
        raise HTTPException(status_code=400, detail=f"invalid json: {e}") from e

    session_id = x_session_id or _resolve_session_id(request)
    raw_messages: List[Dict[str, Any]] = payload.get("messages", []) or []
    tools: Optional[List[Dict[str, Any]]] = payload.get("tools") or None
    tool_choice = payload.get("tool_choice")
    temperature = payload.get("temperature")
    max_tokens = payload.get("max_tokens")

    raw_tokens = count_tokens(raw_messages, tools)
    method = settings.compaction_method()  # "programmatic" | "summary" | "truncate" | "off"

    # ----- Stateful rebuild: if we have a compacted prefix cached for this
    # session, splice it in place of the older raw messages. Only the new
    # tail (turns appended since the last compaction) is carried verbatim.
    # --------------------------------------------------------------------
    messages, cache_entry, new_turns = apply_cache(session_id, raw_messages)
    used_cache = cache_entry is not None
    if settings.mode_on() and used_cache:
        events.emit(
            session_id, "session_cache_hit", "info",
            f"Reusing compacted prefix (+{new_turns} new turns)",
            f"cached compacted prefix={len(cache_entry.compacted_messages)} msgs (compact_count={cache_entry.compact_count}); appended {new_turns} new turns",
            data={
                "cached_prefix_messages": len(cache_entry.compacted_messages),
                "raw_input_messages": len(raw_messages),
                "new_turns": new_turns,
                "compact_count": cache_entry.compact_count,
                "compacted_messages_full": list(cache_entry.compacted_messages),
                "mode": settings.ce_mode,
                "compaction_method": method,
            },
        )

    tokens_in = count_tokens(messages, tools)
    utilization_pct = round(100.0 * tokens_in / settings.ce_max_tokens, 2)
    raw_utilization = round(100.0 * raw_tokens / settings.ce_max_tokens, 2)

    events.emit(
        session_id, "proxy_request", "info",
        f"Chat completion received ({tokens_in} effective, {raw_tokens} raw)",
        f"Mode={settings.ce_mode} model={settings.watsonx_model} cache_hit={used_cache}",
        data={
            "tokens_in": tokens_in,
            "raw_tokens": raw_tokens,
            "raw_messages_count": len(raw_messages),
            "effective_messages_count": len(messages),
            "new_turns": new_turns,
            "cache_hit": used_cache,
            "model": settings.watsonx_model,
            "mode": settings.ce_mode,
            "compaction_method": method,
        },
    )
    events.emit(
        session_id, "token_count", "info",
        f"Context {utilization_pct}% of {settings.ce_max_tokens}" + (
            f"  (raw would have been {raw_utilization}%)" if used_cache else ""
        ),
        f"{tokens_in}/{settings.ce_max_tokens} tokens effective, threshold={settings.ce_threshold_tokens}",
        data={
            "tokens_in": tokens_in,
            "raw_tokens": raw_tokens,
            "max_tokens": settings.ce_max_tokens,
            "threshold_tokens": settings.ce_threshold_tokens,
            "utilization_pct": utilization_pct,
            "raw_utilization_pct": raw_utilization,
            "cache_hit": used_cache,
            "compaction_method": method,
        },
    )

    # CE active + over threshold → run the selected compactor.
    if settings.mode_on() and tokens_in > settings.ce_threshold_tokens:
        events.emit(
            session_id, "threshold_crossed", "warn",
            f"Context above {settings.ce_threshold_frac*100:.0f}% threshold",
            f"{utilization_pct}% ≥ {settings.ce_threshold_frac*100:.0f}% · method={method}",
            data={
                "utilization_pct": utilization_pct,
                "threshold_pct": settings.ce_threshold_frac * 100,
                "compaction_method": method,
            },
        )
        started_label = {
            "programmatic": "Programmatic rewriter started",
            "summary":      "Summarizer started",
            "truncate":     "Deterministic truncator started",
        }.get(method, "Compactor started")
        started_body = {
            "programmatic": settings.ce_objective,
            "summary":      "Paraphrase the older conversation turns into a short recap.",
            "truncate":     f"Drop oldest assistant+tool pairs until under {int(settings.ce_threshold_frac*90)}% of max.",
        }.get(method, "")
        events.emit(
            session_id, "mask_started", "started",
            started_label, started_body,
            data={
                "tokens_before": tokens_in,
                "objective": settings.ce_objective,
                "keep_last_n_turns": settings.ce_keep_last_n,
                "compaction_method": method,
            },
        )

        t_start = time.monotonic()
        used_fallback = False
        generated_code: Optional[str] = None
        summary_text: Optional[str] = None
        metrics: Any = None
        generate_meta: Optional[Dict[str, Any]] = None
        summarize_meta: Optional[Dict[str, Any]] = None
        removed_pairs: int = 0
        new_messages = messages
        compaction_failed = False
        try:
            # Run the (blocking) compactor in a worker thread so the event loop
            # stays free to answer liveness probes — a 100K-token watsonx call
            # can easily take 30-60s and liveness timeouts would SIGKILL us.
            if method == "summary":
                new_messages, summary_text, used_fallback, summarize_meta = await asyncio.to_thread(
                    run_summarizer, messages, session_id=session_id,
                )
            elif method == "truncate":
                new_messages, removed_pairs, used_fallback = await asyncio.to_thread(
                    run_truncator, messages, session_id=session_id,
                )
            else:  # programmatic
                new_messages, metrics, generated_code, used_fallback, generate_meta = await asyncio.to_thread(
                    run_masker, messages, session_id=session_id,
                )
        except Exception as e:  # noqa: BLE001
            log.exception("%s failed", method)
            events.emit(
                session_id, "mask_applied", "error",
                f"{method} failed — forwarding original messages",
                str(e),
                data={"error": str(e), "compaction_method": method},
            )
            new_messages = messages
            compaction_failed = True

        if used_fallback:
            events.emit(
                session_id, "mask_fallback", "warn",
                "Used deterministic fallback",
                f"{method} could not compact; dropped old tool outputs to a marker.",
                data={"fallback": "deterministic", "compaction_method": method},
            )

        if generated_code:
            preview = generated_code[:500]
            data = {
                "code_preview": preview,
                "code_full_len": len(generated_code),
                "generation_time_s": getattr(
                    getattr(metrics, "programmatic_compaction", None),
                    "generation_time_s", 0.0,
                ) if metrics else 0.0,
                "compaction_method": method,
            }
            if generate_meta:
                data["generate_meta"] = generate_meta
            events.emit(
                session_id, "mask_codegen", "info",
                "LLM-generated distill() function",
                f"code: {len(generated_code)} chars",
                data=data,
            )

        if summary_text:
            data = {
                "summary_text": summary_text,
                "summary_chars": len(summary_text),
                "compaction_method": method,
            }
            if summarize_meta:
                data["summarize_meta"] = summarize_meta
            events.emit(
                session_id, "summary_generated", "info",
                f"Summarizer wrote a {len(summary_text)}-char recap",
                "Older messages were replaced with a single natural-language summary.",
                data=data,
            )

        if method == "truncate":
            events.emit(
                session_id, "truncation_applied", "info",
                f"Evicted {removed_pairs} assistant/tool pair(s)",
                f"Dropped the oldest {removed_pairs} assistant+tool message pair(s) to get under the threshold.",
                data={
                    "removed_pairs": removed_pairs,
                    "kept_last_n": settings.ce_keep_last_n,
                    "kept_system": any(m.get("role") == "system" for m in new_messages[:2]),
                    "compaction_method": method,
                },
            )

        if compaction_failed:
            # The compactor raised — the error event has already been emitted
            # above. Don't pile on a misleading "context_diff/0 messages
            # changed" + "mask_applied/success 0% reduction" pair on top of
            # it; those events would hide the real failure in the UI. Fall
            # through to the watsonx forward with the original messages.
            pass
        else:
            # Per-message diff.
            per_msg, before_chars, after_chars = diff_messages(
                messages, new_messages, include_content=settings.ce_emit_diff_content,
            )
            events.emit(
                session_id, "context_diff", "info",
                f"{method.capitalize()} changed {len(per_msg)} messages",
                f"chars {before_chars} → {after_chars}",
                data={
                    "per_message": per_msg,
                    "overall_chars_before": before_chars,
                    "overall_chars_after": after_chars,
                    "messages_changed": len(per_msg),
                    "messages_before": len(messages),
                    "messages_after": len(new_messages),
                    "compaction_method": method,
                },
            )

            tokens_after = count_tokens(new_messages, tools)
            reduction_pct = round(100.0 * (1 - tokens_after / tokens_in), 2) if tokens_in else 0.0
            total_time_s = time.monotonic() - t_start

            # Aggregate CE-call cost for this compaction (sum of generate/summarize meta).
            ce_cost_usd = 0.0
            ce_latency_ms = 0
            for m in (generate_meta, summarize_meta):
                if not m:
                    continue
                ce_cost_usd += float(m.get("cost_usd") or 0.0)
                ce_latency_ms += int(m.get("latency_ms") or 0)

            events.emit(
                session_id, "mask_applied", "success",
                f"Context reduced {reduction_pct}% ({tokens_in} → {tokens_after} tokens)",
                f"{len(per_msg)} messages changed in {total_time_s:.2f}s · method={method}",
                data={
                    "tokens_before": tokens_in,
                    "tokens_after": tokens_after,
                    "reduction_pct": reduction_pct,
                    "total_time_s": round(total_time_s, 3),
                    "total_latency_ms": int(total_time_s * 1000),
                    "messages_changed": len(per_msg),
                    "validator_passed": metrics is not None,
                    "compaction_method": method,
                    "compacted_messages_full": list(new_messages),
                    "ce_cost_usd": round(ce_cost_usd, 6),
                    "ce_latency_ms": ce_latency_ms,
                    "generate_meta": generate_meta,
                    "summarize_meta": summarize_meta,
                    "removed_pairs": removed_pairs if method == "truncate" else None,
                },
            )

            compact_count = save_compacted(session_id, new_messages, raw_len=len(raw_messages))
            events.emit(
                session_id, "session_cache_save", "info",
                f"Saved compacted prefix for session (compact_count={compact_count})",
                f"compacted_messages={len(new_messages)} from raw_len={len(raw_messages)} · method={method}",
                data={
                    "compacted_messages": len(new_messages),
                    "raw_len": len(raw_messages),
                    "compact_count": compact_count,
                    "compaction_method": method,
                },
            )

            messages = new_messages
            tokens_in = tokens_after
    elif settings.mode_on():
        events.emit(
            session_id, "mask_skipped", "info",
            f"Below threshold — {method} compactor not run",
            f"{utilization_pct}% < {settings.ce_threshold_frac*100:.0f}%",
            data={"reason": "below_threshold", "utilization_pct": utilization_pct, "compaction_method": method},
        )
    else:
        events.emit(
            session_id, "proxy_passthrough", "info",
            "Pass-through (CE_MODE=off)",
            f"{tokens_in} tokens",
            data={"tokens_in": tokens_in, "mode": "off", "compaction_method": "off"},
        )

    # Hard overflow check: even compacted, if we're over the window, fail loud.
    if tokens_in > settings.ce_max_tokens:
        overflow_by = tokens_in - settings.ce_max_tokens
        events.emit(
            session_id, "context_overflow", "error",
            f"Context overflow by {overflow_by} tokens",
            f"{tokens_in} > {settings.ce_max_tokens}",
            data={
                "tokens_in": tokens_in,
                "max_tokens": settings.ce_max_tokens,
                "overflow_by": overflow_by,
                "compaction_method": method,
            },
        )
        return JSONResponse(
            status_code=413,
            content={
                "error": {
                    "type": "context_overflow",
                    "message": f"context window exceeded: {tokens_in} > {settings.ce_max_tokens}",
                    "tokens_in": tokens_in,
                    "max_tokens": settings.ce_max_tokens,
                }
            },
        )

    # Snapshot the outbound messages — lets the UI show "what the LLM actually
    # saw" at every turn, independent of compactions. Gated because each snap
    # clones the full message array (can be ~500KB per call near the limit).
    if settings.ce_emit_outbound_snapshots:
        events.emit(
            session_id, "proxy_request_snapshot", "info",
            f"Outbound context snapshot ({tokens_in} tokens, {len(messages)} msgs)",
            "Exact messages being sent to watsonx on this call.",
            data={
                "outbound_messages": list(messages),
                "tokens_in": tokens_in,
                "messages_count": len(messages),
                "compaction_method": method,
            },
        )

    # Strip Harmony-format tokens from any assistant content before forwarding.
    # These leak from gpt-oss reasoning_content as <|start|>…<|channel|>final<|message|>…
    # and cause vllm to error "Unknown role: final" on the NEXT request.
    for m in messages:
        if isinstance(m, dict) and m.get("role") == "assistant" and isinstance(m.get("content"), str):
            m["content"] = _strip_harmony_markers(m["content"])

    # Forward to watsonx.
    events.emit(
        session_id, "watsonx_call", "started",
        f"Forwarding to watsonx ({settings.watsonx_model})",
        f"{tokens_in} tokens, {len(messages)} messages",
        data={"model": settings.watsonx_model, "tokens_in": tokens_in, "compaction_method": method},
    )
    try:
        # Run in a worker thread for the same reason as the compactor above:
        # a long synchronous LiteLLM call would block /healthz and get us
        # SIGKILLed by the liveness probe.
        resp, call_meta = await asyncio.to_thread(
            forward_to_watsonx,
            messages, tools, tool_choice, temperature, max_tokens,
        )
    except Exception as e:  # noqa: BLE001
        log.exception("watsonx forward failed")
        events.emit(
            session_id, "watsonx_call", "error",
            "Watsonx call failed",
            str(e),
            data={"error": str(e), "model": settings.watsonx_model, "compaction_method": method},
        )
        return JSONResponse(status_code=502, content={"error": {"type": "watsonx_error", "message": str(e)}})

    usage = resp.get("usage") if isinstance(resp, dict) else None
    events.emit(
        session_id, "watsonx_call", "success",
        f"Watsonx returned ({call_meta['latency_ms']}ms, ${call_meta['cost_usd']:.6f})",
        f"usage={usage}",
        data={
            "model": settings.watsonx_model,
            "usage": usage,
            "latency_ms": call_meta["latency_ms"],
            "cost_usd": call_meta["cost_usd"],
            "prompt_tokens": call_meta["prompt_tokens"],
            "completion_tokens": call_meta["completion_tokens"],
            "total_tokens": call_meta["total_tokens"],
            "price_source": call_meta["price_source"],
            "compaction_method": method,
        },
    )

    # LLM reply metadata — useful confusion signal on large contexts.
    try:
        choice = (resp.get("choices") or [{}])[0] if isinstance(resp, dict) else {}
        msg = choice.get("message") or {}
        content = msg.get("content") or ""
        reasoning = msg.get("reasoning_content") or ""
        tc_count = len(msg.get("tool_calls") or [])
        finish_reason = choice.get("finish_reason")
        events.emit(
            session_id, "llm_reply", "info",
            f"LLM reply · content={len(content)} chars · reasoning={len(reasoning)} chars · tool_calls={tc_count}",
            f"finish_reason={finish_reason}",
            data={
                "content_chars": len(content),
                "reasoning_chars": len(reasoning),
                "tool_calls_count": tc_count,
                "finish_reason": finish_reason,
                "content_preview": content[:400],
                "compaction_method": method,
            },
        )
    except Exception:  # noqa: BLE001
        pass

    return JSONResponse(status_code=200, content=resp)
