#!/usr/bin/env python3
"""V0 probe: confirm watsonx + LiteLLM + openai/gpt-oss-120b works end-to-end.

Usage:
    pip install litellm tiktoken python-dotenv
    python scripts/ce_probe.py

Reads .env from the repo root (ibac/.env) for WATSONX_API_KEY, WATSONX_PROJECT_ID,
WATSONX_URL. Runs three checks:

    1. Plain chat completion.
    2. Chat completion with tools[] — confirm tool_calls come back natively.
    3. Token counting sanity: compare tiktoken o200k_base count against watsonx's
       reported usage.prompt_tokens for the same input.

Exit 0 on full success. Prints a recommendation for tool-call path (native vs
text-fallback) based on check 2.
"""
from __future__ import annotations

import json
import os
import sys
from pathlib import Path

try:
    from dotenv import load_dotenv
except ImportError:
    load_dotenv = None

try:
    import litellm
except ImportError:
    print("ERROR: litellm not installed. Run: pip install litellm tiktoken python-dotenv")
    sys.exit(2)

try:
    import tiktoken
except ImportError:
    print("ERROR: tiktoken not installed. Run: pip install tiktoken")
    sys.exit(2)


ROOT = Path(__file__).resolve().parents[1]
ENV_PATH = ROOT / ".env"

if load_dotenv is not None and ENV_PATH.exists():
    load_dotenv(ENV_PATH)

for key in ("WATSONX_API_KEY", "WATSONX_PROJECT_ID", "WATSONX_URL"):
    if not os.environ.get(key):
        alias = key.replace("WATSONX_", "WX_")
        if os.environ.get(alias):
            os.environ[key] = os.environ[alias]

missing = [k for k in ("WATSONX_API_KEY", "WATSONX_PROJECT_ID", "WATSONX_URL") if not os.environ.get(k)]
if missing:
    print(f"ERROR: missing env vars: {missing}")
    print(f"Looked at {ENV_PATH}")
    sys.exit(2)

MODEL = os.environ.get("CE_PROBE_MODEL", "watsonx/openai/gpt-oss-120b")


def encode(messages):
    enc = tiktoken.get_encoding("o200k_base")
    total = 0
    for m in messages:
        for k, v in m.items():
            if isinstance(v, str):
                total += len(enc.encode(v))
            else:
                total += len(enc.encode(json.dumps(v, separators=(",", ":"))))
    return total


def check1_plain():
    print("\n[check 1] plain chat completion")
    resp = litellm.completion(
        model=MODEL,
        messages=[{"role": "user", "content": "Say 'ok' in one word."}],
        temperature=0,
        max_tokens=8,
    )
    content = resp.choices[0].message.content
    usage = getattr(resp, "usage", None)
    print(f"  content: {content!r}")
    print(f"  usage:   {usage}")
    return True


def check2_tools():
    print("\n[check 2] chat completion with tools[]")
    tools = [
        {
            "type": "function",
            "function": {
                "name": "get_service_logs",
                "description": "Fetch recent log entries for a service.",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "service": {"type": "string", "description": "service name"},
                        "minutes": {"type": "integer", "description": "lookback window"},
                    },
                    "required": ["service"],
                },
            },
        }
    ]
    resp = litellm.completion(
        model=MODEL,
        messages=[
            {
                "role": "system",
                "content": (
                    "You are an SRE assistant. Use tools when relevant. "
                    "If you call a tool, you may either return a proper tool_calls array OR emit a line "
                    "like TOOL_CALL: {\"name\":\"...\",\"arguments\":{...}}."
                ),
            },
            {"role": "user", "content": "Please fetch the last 10 minutes of logs for the 'payments' service."},
        ],
        tools=tools,
        tool_choice="auto",
        temperature=0,
        max_tokens=200,
    )
    msg = resp.choices[0].message
    native_tool_calls = getattr(msg, "tool_calls", None)
    content = msg.content or ""
    print(f"  content:         {content!r}")
    print(f"  tool_calls:      {native_tool_calls}")

    native_ok = bool(native_tool_calls) and any(
        tc.function.name == "get_service_logs" for tc in native_tool_calls
    )
    text_ok = "TOOL_CALL" in content and "get_service_logs" in content

    if native_ok:
        print("  -> NATIVE tool_calls work. Agent can rely on them.")
    elif text_ok:
        print("  -> NATIVE tool_calls missing BUT TOOL_CALL: text fallback present. "
              "Agent must parse TOOL_CALL: lines.")
    else:
        print("  -> WARNING: neither native tool_calls nor TOOL_CALL: text emitted. "
              "Review the system prompt.")
    return native_ok or text_ok


def check3_tokens():
    print("\n[check 3] token count sanity")
    messages = [
        {"role": "user", "content": "Count the words in this sentence and return just the integer."},
    ]
    local = encode(messages)
    resp = litellm.completion(
        model=MODEL,
        messages=messages,
        temperature=0,
        max_tokens=8,
    )
    remote = getattr(resp, "usage", None)
    remote_prompt = getattr(remote, "prompt_tokens", None) if remote else None
    print(f"  local  (tiktoken o200k_base): {local}")
    print(f"  remote (watsonx usage):       {remote_prompt}")
    if remote_prompt:
        drift = abs(local - remote_prompt) / remote_prompt
        print(f"  drift: {drift*100:.1f}%")
        if drift > 0.2:
            print("  WARNING: drift > 20%. Consider a different encoder or "
                  "a calibration factor in the proxy.")
    return True


def main():
    print(f"Model: {MODEL}")
    print(f"Watsonx URL: {os.environ.get('WATSONX_URL')}")
    ok1 = check1_plain()
    ok2 = check2_tools()
    ok3 = check3_tokens()
    if ok1 and ok2 and ok3:
        print("\nAll checks passed.")
        sys.exit(0)
    print("\nOne or more checks failed.")
    sys.exit(1)


if __name__ == "__main__":
    main()
