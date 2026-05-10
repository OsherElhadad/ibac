"""Token counting — tiktoken o200k_base as closest approximation for gpt-oss-120b."""
from __future__ import annotations

import json
from typing import Any, Dict, List, Optional

import tiktoken

_ENC = tiktoken.get_encoding("o200k_base")


def _encode(s: str) -> int:
    if not s:
        return 0
    return len(_ENC.encode(s, disallowed_special=()))


def count_tokens(messages: List[Dict[str, Any]], tools: Optional[List[Dict[str, Any]]] = None) -> int:
    total = 0
    for m in messages or []:
        # Fixed per-message overhead for role/separators (approximation)
        total += 4
        for k, v in m.items():
            if v is None:
                continue
            if isinstance(v, str):
                total += _encode(v)
            else:
                total += _encode(json.dumps(v, separators=(",", ":"), ensure_ascii=False))
    for t in tools or []:
        total += _encode(json.dumps(t, separators=(",", ":"), ensure_ascii=False))
    return total
