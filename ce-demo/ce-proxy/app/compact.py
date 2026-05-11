"""Compaction strategies for the CE proxy:
  - run_masker:     role-scoped assistant-only rewrite (every assistant turn)
  - run_summarizer: CE-Manager's Summarizer collapses all assistant+tool
                    messages into ONE long summarized assistant message
  - run_truncator:  deterministic eviction of oldest assistant/tool pairs
Plus a per-session compacted-prefix cache so we don't re-compact work we
already compacted on a prior turn.
"""
from __future__ import annotations

import copy
import json
import logging
import re
import threading
import time
from dataclasses import dataclass, field
from typing import Any, Dict, List, Optional, Tuple

from ce_manager.components.summarizer import Summarizer
from ce_manager.config import SummarizerConfig

from .litellm_client import MaskerClient
from .settings import settings

log = logging.getLogger(__name__)


@dataclass
class MessageDiff:
    index: int
    role: str
    tool_call_id: str
    chars_before: int
    chars_after: int
    content_before: str
    content_after: str


# ---------------------------------------------------------------------------
# Per-session compacted-prefix cache
#
# The agent sends the full conversation on every turn. Without a cache, we
# re-run the masker on every threshold crossing even when the older, already
# compacted, portion hasn't changed. This cache stores the most recent
# compacted result per session plus how long the raw input was at that time;
# on the next call, we reconstruct the effective view as:
#
#     effective = cached_compacted + raw_incoming[cached_raw_len:]
#
# so the masker (and watsonx) never sees the uncompacted old history again.
# ---------------------------------------------------------------------------


@dataclass
class SessionState:
    compacted_messages: List[Dict[str, Any]] = field(default_factory=list)
    raw_len: int = 0
    compact_count: int = 0


class SessionCache:
    def __init__(self) -> None:
        self._sessions: Dict[str, SessionState] = {}
        self._lock = threading.Lock()

    def get(self, session_id: str) -> Optional[SessionState]:
        with self._lock:
            return self._sessions.get(session_id)

    def save(self, session_id: str, compacted: List[Dict[str, Any]], raw_len: int) -> None:
        with self._lock:
            s = self._sessions.get(session_id) or SessionState()
            s.compacted_messages = copy.deepcopy(compacted)
            s.raw_len = raw_len
            s.compact_count += 1
            self._sessions[session_id] = s

    def invalidate(self, session_id: str) -> None:
        with self._lock:
            self._sessions.pop(session_id, None)

    def clear(self) -> None:
        with self._lock:
            self._sessions.clear()


_session_cache = SessionCache()


def apply_cache(
    session_id: Optional[str],
    incoming: List[Dict[str, Any]],
) -> Tuple[List[Dict[str, Any]], Optional[SessionState], int]:
    """Rebuild the effective conversation using the cached compacted prefix."""
    if not session_id:
        return incoming, None, len(incoming)
    entry = _session_cache.get(session_id)
    if entry is None or not entry.compacted_messages:
        return incoming, None, len(incoming)

    n_total = len(incoming)
    if n_total < entry.raw_len:
        _session_cache.invalidate(session_id)
        return incoming, None, n_total

    new_tail = incoming[entry.raw_len:]
    effective = list(entry.compacted_messages) + list(new_tail)
    return effective, entry, len(new_tail)


def _content_str(m: Dict[str, Any]) -> str:
    v = m.get("content")
    if v is None:
        return ""
    if isinstance(v, str):
        return v
    return json.dumps(v, ensure_ascii=False)


def diff_messages(
    before: List[Dict[str, Any]],
    after: List[Dict[str, Any]],
    *,
    include_content: bool = True,
) -> Tuple[List[Dict[str, Any]], int, int]:
    """Return (per_message_diff, total_chars_before, total_chars_after)."""
    total_before = 0
    total_after = 0
    per_msg: List[Dict[str, Any]] = []
    common = min(len(before), len(after))

    for i in range(common):
        b_s = _content_str(before[i])
        a_s = _content_str(after[i])
        total_before += len(b_s)
        total_after += len(a_s)
        if b_s != a_s:
            entry = {
                "index": i,
                "role": after[i].get("role", ""),
                "tool_call_id": after[i].get("tool_call_id", "") or "",
                "chars_before": len(b_s),
                "chars_after": len(a_s),
            }
            if include_content:
                entry["content_before"] = b_s
                entry["content_after"] = a_s
            per_msg.append(entry)

    if len(before) != len(after):
        for i in range(common, len(before)):
            total_before += len(_content_str(before[i]))
        for i in range(common, len(after)):
            total_after += len(_content_str(after[i]))

    return per_msg, total_before, total_after


_masker_client: Optional[MaskerClient] = None


def _client() -> MaskerClient:
    global _masker_client
    if _masker_client is None:
        _masker_client = MaskerClient()
    return _masker_client


_VALID_ROLES = {"system", "user", "assistant", "tool", "developer"}


def _strip_harmony_markers(text: str) -> str:
    """Remove gpt-oss / Harmony format tokens from assistant content.

    When the model's reasoning_content leaks tokens like
    ``<|start|>assistant<|channel|>final<|message|>…<|end|>`` into
    plain content, watsonx/vllm re-parses them as structural on the
    NEXT request and errors with ``Unknown role: final``. Stripping
    these markers on the way out keeps round-trips clean.
    """
    if not isinstance(text, str) or "<|" not in text:
        return text
    # Remove every <|...|> token wholesale. The `[^>]*?` pattern tolerates
    # pipes inside (e.g. `<|im_start|>` style) and stops at the first `|>`.
    cleaned = re.sub(r"<\|[^>]*?\|>", "", text)
    # Also drop a trailing `assistantfinal`/`assistant` preamble left behind
    # when Harmony headers are stripped — the original text was
    # `<|start|>assistant<|channel|>final<|message|>TEXT<|end|>`, and after
    # stripping tokens we're left with `assistantfinalTEXT` which confuses
    # downstream consumers. Keep just TEXT when we recognize this pattern.
    cleaned = re.sub(r"^\s*assistant(?:final|analysis|commentary)?\s*", "", cleaned)
    return cleaned


def sanitize_messages(
    compacted: List[Dict[str, Any]],
    original: List[Dict[str, Any]],
) -> List[Dict[str, Any]]:
    """Repair any role/tool_call_id/content fields that are not watsonx-valid."""
    out: List[Dict[str, Any]] = []
    for i, m in enumerate(compacted):
        if not isinstance(m, dict):
            continue
        m = copy.deepcopy(m)
        role = m.get("role")
        if not isinstance(role, str) or role not in _VALID_ROLES:
            replacement = None
            if i < len(original) and isinstance(original[i].get("role"), str):
                cand = original[i]["role"]
                if cand in _VALID_ROLES:
                    replacement = cand
            if replacement is None:
                replacement = "user"
            log.warning("sanitize_messages: msg %d role=%r replaced with %r", i, role, replacement)
            m["role"] = replacement
        if m.get("role") == "tool" and not isinstance(m.get("tool_call_id"), str):
            if i < len(original) and isinstance(original[i].get("tool_call_id"), str):
                m["tool_call_id"] = original[i]["tool_call_id"]
            else:
                m["tool_call_id"] = f"tool-{i}"
        c = m.get("content")
        if c is not None and not isinstance(c, str):
            try:
                m["content"] = json.dumps(c, ensure_ascii=False)
            except Exception:  # noqa: BLE001
                m["content"] = str(c)
        # Strip Harmony-format tokens that would confuse vllm's parser
        # on a round-trip. Only applied to assistant content; tool/user
        # content doesn't hit the Harmony parser.
        if m.get("role") == "assistant" and isinstance(m.get("content"), str):
            m["content"] = _strip_harmony_markers(m["content"])
        out.append(m)
    return out


# ---------------------------------------------------------------------------
# Masker — role-scoped, assistant-only
#
# CE-Manager's compact_conversation bakes in a "preserve last N turns
# verbatim" rule (`_universal_preservation_rules`) that cannot be disabled
# via config. On a short demo conversation the recent assistant turns — the
# ones actually driving context growth — are exactly the ones we need to
# compress. So we do not use compact_conversation here. Instead we call the
# watsonx LLM directly with a focused prompt asking it to rewrite every
# assistant message's `content` field and leave tool_calls / tool
# responses / system / user messages untouched.
# ---------------------------------------------------------------------------


@dataclass
class _ProgMetrics:
    generation_time_s: float = 0.0


@dataclass
class _Metrics:
    """Minimal shim with the single field main.py reads via getattr()."""
    programmatic_compaction: Optional[_ProgMetrics] = None


# Per-tool-output character cap when sending content to the masker LLM.
# Set high enough to include the full body of every fixture in this
# demo (largest is ~43K chars), so the masker sees every row and can
# apply its relevance rules. The masker only processes messages past
# the cached prefix (see run_masker), so total masker input is bounded
# by one turn's worth of new content — not a concern for the 131K
# input window even at this cap.
_TOOL_TRUNCATE_CHARS = 60_000


_MASKER_SYSTEM_PROMPT = (
    "You compact the `targets` array inside an agent's "
    "conversation history so it fits the context window.\n"
    "\n"
    "DO NOT answer user_goal. DO NOT continue the agent's task. "
    "Your ONLY job is to return a JSON object mapping each idx to "
    "a shorter `content` string.\n"
    "\n"
    "## The ONLY masking rule\n"
    "\n"
    "For each target, KEEP every value the agent could need to "
    "(a) execute its NEXT pipeline steps, or (b) assemble its "
    "FINAL ANSWER to user_goal. Drop only content that cannot "
    "possibly serve (a) or (b) — filler, duplicate-in-spirit rows "
    "for clearly unrelated entities, chain-of-thought prose with "
    "no values. When in doubt, KEEP. Dropping a signal is a HARD "
    "FAILURE.\n"
    "\n"
    "## Input schema\n"
    "\n"
    "You receive a JSON object:\n"
    "  \"user_goal\": string — shows you what the agent is trying "
    "to do and what its final-answer format looks like. Use it "
    "ONLY to judge relevance. Do not answer it.\n"
    "  \"targets\": array of {\"idx\": int, \"role\": "
    "\"assistant\"|\"tool\", \"content\": string}.\n"
    "\n"
    "## Output schema (critical — the parser is strict)\n"
    "\n"
    "Your entire response is ONE JSON OBJECT. The first character "
    "MUST be `{`, the last MUST be `}`. Every input idx MUST "
    "appear as a key (string form of the integer). The value for "
    "each key is the compacted `content` as a STRING.\n"
    "\n"
    "  • For role='assistant' targets the value is a plain text "
    "    string (shortened prose).\n"
    "  • For role='tool' targets the value is a STRING that, when "
    "    passed to json.loads, yields a JSON object with the same "
    "    top-level keys as the input content. Inside that JSON, "
    "    retained entries are copied BYTE-FOR-BYTE from the input.\n"
    "\n"
    "No preamble. No 'We need to…' or 'Here is…' lead-in. No "
    "commentary. No code fences. No prose before or after the JSON.\n"
    "\n"
    "## Forbidden in output\n"
    "\n"
    "  • `...` or any ellipsis.\n"
    "  • placeholders like `[omitted]`, `[truncated]`, `[CE-MASKED]`, "
    "    notes about the content being shortened.\n"
    "  • empty strings as values.\n"
    "  • partial / truncated JSON for tool entries.\n"
    "  • naive first-N or last-N trimming — pick by relevance.\n"
    "  • paraphrased or invented values.\n"
    "  • answering the user_goal.\n"
    "\n"
    "## How to decide row-by-row (for arrays inside role='tool')\n"
    "\n"
    "For each row in a tool response's array, ask: 'Could this "
    "row's values end up in the final answer, or be needed by a "
    "follow-up tool call?' If YES or UNSURE → keep. If clearly "
    "NO → drop.\n"
    "\n"
    "Almost always keep rows that:\n"
    "  • mention any entity, IP, file, email, user, or "
    "    identifier named or implied in user_goal;\n"
    "  • share a flagged identifier (IP, filename, email, domain, "
    "    hash) with another row that stood out. Once an IP or "
    "    file is implicated, EVERY row touching it is signal — "
    "    follow-up questions will ask about OTHER users / files "
    "    tied to that identifier;\n"
    "  • stand out against peers (unusual IP, foreign country, "
    "    off-hours timestamp, unfamiliar user-agent, abnormal "
    "    bytes, external recipient, bypass/deny policy flag, "
    "    unusual result code);\n"
    "  • carry a value type the final answer requires;\n"
    "  • carry a value referenced later in the conversation.\n"
    "\n"
    "## Example 1 — tool response with a decoy + two signals + "
    "unrelated rows\n"
    "\n"
    "INPUT (what you receive):\n"
    "\n"
    "{\n"
    "  \"user_goal\": \"Investigate whether account X was "
    "compromised. Answer: HH:MM · IP · FILE · BYTES · TO.\",\n"
    "  \"targets\": [\n"
    "    {\"idx\": 3, \"role\": \"tool\", \"content\": "
    "\"{\\\"events\\\":[{\\\"ts\\\":\\\"09:12\\\","
    "\\\"account\\\":\\\"X\\\",\\\"ip\\\":\\\"10.0.0.1\\\","
    "\\\"action\\\":\\\"read\\\",\\\"bytes\\\":42},"
    "{\\\"ts\\\":\\\"09:27\\\",\\\"account\\\":\\\"X\\\","
    "\\\"ip\\\":\\\"203.0.113.9\\\",\\\"action\\\":\\\"exfil\\\","
    "\\\"bytes\\\":2441472,\\\"file\\\":\\\"q3.xlsx\\\","
    "\\\"to\\\":\\\"bad@evil.example\\\"},"
    "{\\\"ts\\\":\\\"09:45\\\",\\\"account\\\":\\\"Y\\\","
    "\\\"ip\\\":\\\"10.0.0.2\\\",\\\"action\\\":\\\"read\\\","
    "\\\"bytes\\\":88}],\\\"total\\\":3}\"}\n"
    "  ]\n"
    "}\n"
    "\n"
    "CORRECT OUTPUT (what you return):\n"
    "\n"
    "{\"3\": \"{\\\"events\\\":[{\\\"ts\\\":\\\"09:27\\\","
    "\\\"account\\\":\\\"X\\\",\\\"ip\\\":\\\"203.0.113.9\\\","
    "\\\"action\\\":\\\"exfil\\\",\\\"bytes\\\":2441472,"
    "\\\"file\\\":\\\"q3.xlsx\\\",\\\"to\\\":\\\"bad@evil.example"
    "\\\"}],\\\"total\\\":3}\"}\n"
    "\n"
    "Notes on the output:\n"
    "  • Wrapped as `{\"3\": \"...\"}` — one entry per input idx.\n"
    "  • The value is a STRING that json.loads parses into "
    "{\"events\":[...],\"total\":3} — a valid JSON object with "
    "the same top-level keys (`events`, `total`).\n"
    "  • Kept the exfil row — anomalous IP + abnormal bytes + "
    "external recipient + matches answer schema (TS/IP/FILE/"
    "BYTES/TO).\n"
    "  • Dropped the 09:12 row — routine read, small bytes, no "
    "anomaly, no value needed in answer.\n"
    "  • Dropped the 09:45 row — different account (Y), unrelated "
    "to user_goal, no shared identifier.\n"
    "  • Top-level keys `events` and `total` are preserved; "
    "`total:3` is copied byte-for-byte even though the kept array "
    "has only 1 row (so the original count is still visible).\n"
    "\n"
    "## Example 2 — two targets, one assistant + one tool\n"
    "\n"
    "INPUT:\n"
    "\n"
    "{\n"
    "  \"user_goal\": \"Which users logged in from the attacker "
    "IP 203.0.113.9 in the last 30 days?\",\n"
    "  \"targets\": [\n"
    "    {\"idx\": 5, \"role\": \"assistant\", \"content\": "
    "\"I will search logins by IP. The attacker IP is "
    "203.0.113.9 based on earlier findings. Let me think about "
    "what other analyses to run. Actually I'll just call the "
    "login-search tool now.\"},\n"
    "    {\"idx\": 6, \"role\": \"tool\", \"content\": "
    "\"{\\\"ip\\\":\\\"203.0.113.9\\\",\\\"logins\\\":"
    "[{\\\"user\\\":\\\"alice\\\",\\\"ts\\\":\\\"04:14\\\","
    "\\\"result\\\":\\\"success\\\"},"
    "{\\\"user\\\":\\\"bob\\\",\\\"ts\\\":\\\"07:51\\\","
    "\\\"result\\\":\\\"success\\\"},"
    "{\\\"user\\\":\\\"nobody\\\",\\\"ts\\\":\\\"12:01\\\","
    "\\\"result\\\":\\\"failure\\\"}],\\\"total\\\":3}\"}\n"
    "  ]\n"
    "}\n"
    "\n"
    "CORRECT OUTPUT:\n"
    "\n"
    "{\"5\": \"Attacker IP is 203.0.113.9. Calling "
    "login-search tool.\", \"6\": \"{\\\"ip\\\":\\\"203.0.113.9"
    "\\\",\\\"logins\\\":[{\\\"user\\\":\\\"alice\\\","
    "\\\"ts\\\":\\\"04:14\\\",\\\"result\\\":\\\"success\\\"},"
    "{\\\"user\\\":\\\"bob\\\",\\\"ts\\\":\\\"07:51\\\","
    "\\\"result\\\":\\\"success\\\"}],\\\"total\\\":3}\"}\n"
    "\n"
    "Notes:\n"
    "  • idx 5 (assistant): kept the concrete fact "
    "(attacker IP) and the decision (calling tool); dropped the "
    "filler 'let me think about what other analyses'.\n"
    "  • idx 6 (tool): value is a JSON STRING that json.loads "
    "yields a JSON OBJECT with top-level keys `ip`, `logins`, "
    "`total` — same as the input. Kept both successful logins "
    "(they match user_goal's filter). Dropped the failure row "
    "(not a successful login for the attacker IP). Top-level "
    "`total:3` is preserved byte-for-byte.\n"
)


def _parse_idx_map(raw: str) -> Dict[int, str]:
    """Parse the LLM's output into {int_idx: content}. Tolerates code
    fences, preamble prose ("We need to…"), trailing prose, string-
    int keys, and partial/truncated JSON — partial results beat
    sinking the whole compaction on one over-long entry."""
    s = raw.strip()
    if s.startswith("```"):
        s = re.sub(r"^```(?:json)?\s*", "", s)
        s = re.sub(r"\s*```\s*$", "", s)
    # Strip any preamble before the first '{' (LLM reasoning like
    # "We need to compact each target aggressively, keeping only…").
    first_brace = s.find("{")
    if first_brace > 0:
        s = s[first_brace:]
    # Strip any postamble after the last '}'.
    last_brace = s.rfind("}")
    if 0 <= last_brace < len(s) - 1:
        s = s[: last_brace + 1]
    try:
        obj = json.loads(s)
        if isinstance(obj, dict):
            return _coerce_idx_map(obj)
    except Exception:  # noqa: BLE001
        pass
    # Try to recover truncated JSON: find the last complete "idx":"..."
    # pair and close the object at that point.
    recovered = _recover_truncated_json(s)
    if recovered is not None:
        try:
            obj = json.loads(recovered)
            if isinstance(obj, dict):
                return _coerce_idx_map(obj)
        except Exception:  # noqa: BLE001
            pass
    out: Dict[int, str] = {}
    pair_re = re.compile(r'"(\d+)"\s*:\s*"((?:[^"\\]|\\.)*)"', flags=re.DOTALL)
    for match in pair_re.finditer(s):
        try:
            i = int(match.group(1))
        except Exception:  # noqa: BLE001
            continue
        try:
            out[i] = json.loads(f'"{match.group(2)}"')
        except Exception:  # noqa: BLE001
            out[i] = match.group(2)
    if not out:
        log.warning("_parse_idx_map: no parseable entries in masker output: %r", raw[:300])
    return out


def _recover_truncated_json(s: str) -> Optional[str]:
    """When the masker response is cut mid-entry by max_tokens, keep
    every COMPLETE "<digits>": "<string>" pair and close the object.
    Returns a valid JSON object as a string, or None if nothing can
    be recovered."""
    pair_re = re.compile(r'"(\d+)"\s*:\s*"(?:[^"\\]|\\.)*"', flags=re.DOTALL)
    matches = list(pair_re.finditer(s))
    if not matches:
        return None
    body = ", ".join(m.group(0) for m in matches)
    return "{" + body + "}"


def _coerce_idx_map(obj: Dict[Any, Any]) -> Dict[int, str]:
    out: Dict[int, str] = {}
    for k, v in obj.items():
        try:
            i = int(str(k).strip())
        except Exception:  # noqa: BLE001
            continue
        if isinstance(v, str):
            out[i] = v
        else:
            out[i] = json.dumps(v, ensure_ascii=False)
    return out


def run_masker(
    messages: List[Dict[str, Any]],
    *,
    session_id: Optional[str] = None,
) -> Tuple[List[Dict[str, Any]], Any, Optional[str], bool, Optional[Dict[str, Any]]]:
    """Compact assistant + tool messages by dropping irrelevant spans.

    Stability rule: if this session already has a cached compacted prefix
    from a prior pass, those messages are treated as FROZEN — the LLM is
    never asked to re-mask them. Only the tail (messages appended since
    the last compaction) is sent to the LLM. This prevents the drift
    where each subsequent masker pass re-compresses already-compact
    content and either re-expands decoy rows or drops signal rows.

    Returns (new_messages, metrics, generated_code, used_fallback, generate_meta).
    """
    msgs = [copy.deepcopy(m) for m in messages]

    # How many leading messages are already compacted (from prior pass)?
    # Those are never re-masked.
    cached_prefix_len = 0
    if session_id:
        entry = _session_cache.get(session_id)
        if entry is not None:
            cached_prefix_len = len(entry.compacted_messages)

    # Build per-target payload. Only include messages past the cached
    # prefix — these are the new assistant + tool entries the LLM hasn't
    # seen before. Already-compacted entries (idx < cached_prefix_len)
    # stay byte-for-byte identical in the returned list.
    targets: List[Tuple[int, str, str]] = []
    for i, m in enumerate(msgs):
        if i < cached_prefix_len:
            continue
        role = m.get("role")
        if role not in ("assistant", "tool"):
            continue
        c = m.get("content")
        if not isinstance(c, str) or not c.strip():
            continue
        excerpt = c if len(c) <= _TOOL_TRUNCATE_CHARS else (
            c[:_TOOL_TRUNCATE_CHARS] + "\n...[truncated for masker input]"
        )
        targets.append((i, role, excerpt))

    if not targets:
        metrics = _Metrics(programmatic_compaction=_ProgMetrics(generation_time_s=0.0))
        meta = {
            "cost_usd": 0.0, "latency_ms": 0, "prompt_tokens": 0,
            "completion_tokens": 0, "total_tokens": 0, "price_source": "noop",
            "targets_rewritten": 0, "targets_requested": 0,
        }
        return msgs, metrics, None, False, meta

    # Find the user's most recent question — that's the agent's goal.
    # The masker uses it to decide what's relevant.
    user_goal = ""
    for m in reversed(msgs):
        if m.get("role") == "user":
            c = m.get("content")
            if isinstance(c, str) and c.strip():
                user_goal = c.strip()
                break

    user_prompt = json.dumps(
        {
            "user_goal": user_goal,
            "targets": [
                {"idx": i, "role": role, "content": c}
                for i, role, c in targets
            ],
        },
        ensure_ascii=False,
    )

    client = _client()
    t0 = time.monotonic()
    raw, meta = client.complete_messages(
        [
            {"role": "system", "content": _MASKER_SYSTEM_PROMPT},
            {"role": "user", "content": user_prompt},
        ],
        max_tokens=16384,
        temperature=0.0,
        label="masker.rewrite",
        reasoning_effort="low",
    )
    elapsed_s = time.monotonic() - t0
    client.last_generate_meta = meta

    mapping = _parse_idx_map(raw)
    rewritten = 0
    for i, new_content in mapping.items():
        if not (0 <= i < len(msgs)):
            continue
        m = msgs[i]
        if m.get("role") not in ("assistant", "tool"):
            continue
        if not isinstance(new_content, str) or not new_content.strip():
            continue
        m["content"] = new_content
        rewritten += 1

    if rewritten == 0:
        log.warning(
            "run_masker: 0 targets rewritten (targets=%d, raw_len=%d). raw head=%r",
            len(targets), len(raw), raw[:500],
        )

    msgs = sanitize_messages(msgs, messages)

    meta = dict(meta)
    meta["targets_rewritten"] = rewritten
    meta["targets_requested"] = len(targets)

    metrics = _Metrics(programmatic_compaction=_ProgMetrics(generation_time_s=elapsed_s))
    return msgs, metrics, None, False, meta


# ---------------------------------------------------------------------------
# Summarizer — uses CE-Manager's built-in Summarizer + SUMMARIZER prompts.
#
# Collapses every assistant+tool message into a single long assistant
# message carrying the summary. System + user messages are preserved
# verbatim. The single summary message replaces the entire tool-call chain
# — since the tool messages are gone along with the assistant messages
# that anchored them, there are no dangling tool_call_ids.
# ---------------------------------------------------------------------------


class _SummarizerLLMAdapter:
    """Adapter between MaskerClient and CE-Manager's Summarizer class,
    which expects ``.generate(messages) -> str``.

    Forwards the library's messages verbatim — no augmentation. The
    prompt in use is CE-Manager's built-in ``highly_detailed`` level
    (see ce_manager.prompts.summarizer.SUMMARY_LEVELS_TO_PROMPT). Stashes
    the last call's cost/latency meta for propagation to events.
    """

    def __init__(self, client: MaskerClient) -> None:
        self._client = client
        self.last_meta: Optional[Dict[str, Any]] = None

    def generate(self, messages: List[Dict[str, Any]]) -> str:
        content, meta = self._client.complete_messages(
            messages,
            max_tokens=16384,
            temperature=0.2,
            label="summarizer",
        )
        self.last_meta = meta
        self._client.last_summarize_meta = meta
        return content


def _strip_summary_tags(s: str) -> str:
    """Remove the library's <summary>...</summary> wrapper if present."""
    s = s.strip()
    m = re.search(r"<summary>([\s\S]*?)</summary>", s, flags=re.IGNORECASE)
    if m:
        return m.group(1).strip()
    # Some outputs only have an opening or closing tag.
    s = re.sub(r"^\s*<summary>\s*", "", s, flags=re.IGNORECASE)
    s = re.sub(r"\s*</summary>\s*$", "", s, flags=re.IGNORECASE)
    return s.strip()


def run_summarizer(
    messages: List[Dict[str, Any]],
    *,
    session_id: Optional[str] = None,
) -> Tuple[List[Dict[str, Any]], Optional[str], bool, Optional[Dict[str, Any]]]:
    """Replace every assistant + tool message with ONE summarized assistant
    message. System and user messages pass through verbatim.

    Uses CE-Manager's ``Summarizer`` class with
    ``summary_level="highly_detailed"`` (longest level in
    ``SUMMARY_LEVELS_TO_PROMPT``) so the recap is rich enough to carry all
    the values the agent needs to finish its answer.
    """
    system_msgs = [m for m in messages if m.get("role") == "system"]
    user_msgs   = [m for m in messages if m.get("role") == "user"]
    traj        = [m for m in messages if m.get("role") in ("assistant", "tool")]

    if not traj:
        return messages, None, False, None

    adapter = _SummarizerLLMAdapter(_client())
    cfg = SummarizerConfig(
        enabled=True,
        summary_level="regular",
        include_tool_calls=True,
        sub_summarize_without_tool_calls=False,
        model=None,  # adapter ignores this — it uses settings.watsonx_model
    )
    raw_summary = Summarizer(cfg, adapter).summarize_trajectory(traj)
    summary_text = _strip_summary_tags(raw_summary)
    if not summary_text:
        raise RuntimeError("summarizer returned empty summary — refusing to emit a naive fallback")

    marker = (
        f"[CE-SUMMARY] The agent previously processed {len(traj)} messages "
        f"(assistant + tool). Those are replaced with this recap:\n\n{summary_text}"
    )

    new_messages: List[Dict[str, Any]] = []
    new_messages.extend(copy.deepcopy(m) for m in system_msgs)
    new_messages.extend(copy.deepcopy(m) for m in user_msgs)
    new_messages.append({"role": "assistant", "content": marker})
    new_messages = sanitize_messages(new_messages, messages)

    meta = dict(adapter.last_meta or {})
    meta["summary_chars"] = len(summary_text)
    meta["source_messages"] = len(traj)

    return new_messages, summary_text, False, meta


# ---------------------------------------------------------------------------
# Deterministic truncation baseline (CE_MODE=truncate)
# ---------------------------------------------------------------------------


def _count_tokens_msgs(messages: List[Dict[str, Any]]) -> int:
    from .tokens import count_tokens  # type: ignore
    return count_tokens(messages, None)


_MAX_PAIRS_PER_CALL = 2


def run_truncator(
    messages: List[Dict[str, Any]],
    *,
    session_id: Optional[str] = None,
) -> Tuple[List[Dict[str, Any]], int, bool]:
    """Drop up to the oldest 2 assistant-with-tool_calls + their matching
    tool-response messages."""
    keep_last_n = max(1, settings.ce_keep_last_n)

    if not messages:
        return messages, 0, False

    system_idx = 0 if messages[0].get("role") == "system" else -1
    tail_start = max(system_idx + 1, len(messages) - keep_last_n)

    drop_idx: set[int] = set()
    removed_pairs = 0

    for i in range(system_idx + 1, tail_start):
        if removed_pairs >= _MAX_PAIRS_PER_CALL:
            break
        m = messages[i]
        if m.get("role") != "assistant":
            continue
        tool_calls = m.get("tool_calls") or []
        ids = {tc.get("id") for tc in tool_calls if isinstance(tc, dict) and tc.get("id")}
        if not ids:
            continue
        drop_idx.add(i)
        for j in range(i + 1, len(messages)):
            mj = messages[j]
            if mj.get("role") == "tool" and mj.get("tool_call_id") in ids:
                drop_idx.add(j)
                ids.discard(mj.get("tool_call_id"))
                if not ids:
                    break
        removed_pairs += 1

    new_messages = [copy.deepcopy(m) for k, m in enumerate(messages) if k not in drop_idx]

    if removed_pairs > 0:
        pair_word = "pair" if removed_pairs == 1 else "pairs"
        marker = {
            "role": "system",
            "content": (
                f"[CE-TRUNCATED] Dropped the oldest {removed_pairs} "
                f"assistant/tool_call {pair_word} ({len(drop_idx)} message(s)) "
                f"to fit the context window. Those tool outputs are no longer "
                f"available."
            ),
        }
        insert_at = 1 if system_idx == 0 else 0
        new_messages.insert(insert_at, marker)

    return new_messages, removed_pairs, False


def reset_state() -> Dict[str, Any]:
    """Clear our per-session compacted-prefix cache plus any CE-Manager
    singletons that might still be hanging around from legacy paths."""
    cleared = {
        "cache_cleared": False,
        "reversibility_cleared": False,
        "backoff_cleared": False,
        "session_cache_cleared": False,
    }
    try:
        from ce_manager.components.programmatic_context_engineering.pipeline import (
            clear_programmatic_noop_backoff,
            clear_reversibility_store,
            get_cache,
        )
        try:
            get_cache().clear()  # type: ignore[attr-defined]
            cleared["cache_cleared"] = True
        except Exception:  # noqa: BLE001
            pass
        try:
            clear_reversibility_store(None)
            cleared["reversibility_cleared"] = True
        except Exception:  # noqa: BLE001
            pass
        try:
            clear_programmatic_noop_backoff(None)
            cleared["backoff_cleared"] = True
        except Exception:  # noqa: BLE001
            pass
    except Exception as e:  # noqa: BLE001
        log.warning("reset_state: import failure %s", e)
    try:
        _session_cache.clear()
        cleared["session_cache_cleared"] = True
    except Exception:  # noqa: BLE001
        pass
    return cleared


def save_compacted(session_id: str, compacted: List[Dict[str, Any]], raw_len: int) -> int:
    """Persist the latest compaction for a session. Returns new compact_count."""
    _session_cache.save(session_id, compacted, raw_len)
    entry = _session_cache.get(session_id)
    return entry.compact_count if entry else 0
