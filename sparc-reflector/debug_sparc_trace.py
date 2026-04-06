#!/usr/bin/env python3
import asyncio
import json
import os
import time

from altk.core.llm import get_llm
from altk.core.toolkit import AgentPhase, ComponentConfig
from altk.pre_tool.core import SPARCExecutionMode, SPARCReflectionRunInput, Track
from altk.pre_tool.sparc import SPARCReflectionComponent


def build_client():
    client_cls = get_llm("litellm.watsonx.output_val")
    return client_cls(
        model_name=os.getenv(
            "WX_MODEL_ID",
            "meta-llama/llama-4-maverick-17b-128e-instruct-fp8",
        ),
        api_key=os.environ["WX_API_KEY"],
        project_id=os.environ["WX_PROJECT_ID"],
        api_base=os.environ["WX_URL"],
        timeout=int(os.getenv("SPARC_LLM_TIMEOUT", "120")),
        include_schema_in_system_prompt=True,
    )


def build_run_input():
    return SPARCReflectionRunInput(
        messages=[
            {
                "role": "user",
                "content": "Refund transaction TX482 because it was a duplicate charge.",
            },
            {"role": "assistant", "content": "I will process the refund."},
        ],
        tool_specs=[
            {
                "type": "function",
                "function": {
                    "name": "get_transaction",
                    "description": "Fetch transaction details by transaction id.",
                    "parameters": {
                        "type": "object",
                        "properties": {
                            "transaction_id": {
                                "type": "string",
                                "description": "Exact transaction identifier.",
                            }
                        },
                        "required": ["transaction_id"],
                    },
                },
            }
        ],
        tool_calls=[
            {
                "id": "call_1",
                "type": "function",
                "function": {
                    "name": "get_transaction",
                    "arguments": json.dumps({"transaction_id": "TX4821"}),
                },
            }
        ],
    )


def main():
    client = build_client()
    original = client.generate_async
    call_counter = {"value": 0}

    async def traced_generate_async(*args, **kwargs):
        call_counter["value"] += 1
        idx = call_counter["value"]
        prompt = kwargs.get("prompt", args[0] if args else None)
        summary = ""
        if isinstance(prompt, list) and prompt:
            summary = str(prompt[-1].get("content", ""))[:160].replace("\n", " ")
        elif isinstance(prompt, str):
            summary = prompt[:160].replace("\n", " ")
        start = time.time()
        print(f"llm_call_start idx={idx} summary={summary}", flush=True)
        try:
            result = await original(*args, **kwargs)
        except Exception as exc:
            print(
                f"llm_call_error idx={idx} elapsed_s={time.time() - start:.2f} error={exc}",
                flush=True,
            )
            raise
        print(f"llm_call_done idx={idx} elapsed_s={time.time() - start:.2f}", flush=True)
        return result

    client.generate_async = traced_generate_async

    reflector = SPARCReflectionComponent(
        config=ComponentConfig(llm_client=client),
        track=Track.FAST_TRACK,
        execution_mode=SPARCExecutionMode.ASYNC,
        include_raw_response=True,
        retries=1,
        max_parallel=2,
    )
    print(f"init_error={reflector._initialization_error}", flush=True)

    run_input = build_run_input()
    start = time.time()
    result = reflector.process(run_input, phase=AgentPhase.RUNTIME)
    print(f"total_elapsed_s={time.time() - start:.2f}", flush=True)
    print(result.output.reflection_result.model_dump_json(indent=2), flush=True)
    raw_pipeline_result = result.output.raw_pipeline_result or {}
    print(json.dumps(raw_pipeline_result, indent=2)[:6000], flush=True)


if __name__ == "__main__":
    main()
