#!/usr/bin/env python3
import json
import os
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any


def normalize_watsonx_env() -> dict[str, str]:
    api_key = os.getenv("WX_API_KEY") or os.getenv("WATSONX_API_KEY") or ""
    project_id = os.getenv("WX_PROJECT_ID") or os.getenv("WATSONX_PROJECT_ID") or ""
    url = (
        os.getenv("WX_URL")
        or os.getenv("WATSONX_URL")
        or "https://us-south.ml.cloud.ibm.com"
    )

    os.environ["WX_API_KEY"] = api_key
    os.environ["WX_PROJECT_ID"] = project_id
    os.environ["WX_URL"] = url
    os.environ.setdefault("LITELLM_LOCAL_MODEL_COST_MAP", "True")
    return {
        "WX_API_KEY": api_key,
        "WX_PROJECT_ID": project_id,
        "WX_URL": url,
        "LITELLM_LOCAL_MODEL_COST_MAP": os.environ["LITELLM_LOCAL_MODEL_COST_MAP"],
    }


def is_truthy(value: str) -> bool:
    return value.strip().lower() in {"1", "true", "yes", "on"}


def parse_int_env(name: str, default: int) -> int:
    raw = os.getenv(name, "").strip()
    if not raw:
        return default
    try:
        return int(raw)
    except ValueError:
        return default


def emit_event(event: dict[str, Any]) -> None:
    observer_url = os.getenv("OBSERVER_URL", "").strip()
    session_id = event.get("session_id", "").strip()
    if not observer_url or not session_id:
        return

    payload = json.dumps(event).encode("utf-8")
    req = urllib.request.Request(
        observer_url.rstrip("/") + "/api/events",
        data=payload,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=2):
            return
    except Exception:
        return


def observer_request(
    method: str,
    path: str,
    payload: dict[str, Any] | None = None,
    timeout: int = 10,
) -> dict[str, Any] | None:
    observer_url = os.getenv("SPARC_RELAY_OBSERVER_URL", "").strip() or os.getenv(
        "OBSERVER_URL", ""
    ).strip()
    if not observer_url:
        raise RuntimeError("missing observer URL for SPARC relay")

    body = None
    headers: dict[str, str] = {}
    if payload is not None:
        body = json.dumps(payload).encode("utf-8")
        headers["Content-Type"] = "application/json"

    req = urllib.request.Request(
        observer_url.rstrip("/") + path,
        data=body,
        headers=headers,
        method=method,
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read()
            if not raw:
                return None
            return json.loads(raw)
    except urllib.error.HTTPError as exc:
        if exc.code == 204:
            return None
        raise RuntimeError(exc.read().decode("utf-8", errors="replace") or str(exc)) from exc


class ReflectorService:
    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._reflector = None
        self._initialization_error = None

    def build_reflector(self):
        with self._lock:
            if self._reflector is not None:
                return self._reflector
            if self._initialization_error is not None:
                raise RuntimeError(self._initialization_error)

            env = normalize_watsonx_env()
            if not env["WX_API_KEY"] or not env["WX_PROJECT_ID"]:
                self._initialization_error = "missing Watsonx credentials"
                raise RuntimeError(self._initialization_error)

            from altk.core.llm import get_llm
            from altk.core.toolkit import ComponentConfig
            from altk.pre_tool.core import SPARCExecutionMode, Track
            from altk.pre_tool.sparc import SPARCReflectionComponent

            watsonx_client = get_llm("litellm.watsonx.output_val")
            model_id = os.getenv(
                "WX_MODEL_ID",
                "mistral-large-2512"
            )
            config = ComponentConfig(
                llm_client=watsonx_client(
                    model_name=model_id,
                    api_key=env["WX_API_KEY"],
                    project_id=env["WX_PROJECT_ID"],
                    api_base=env["WX_URL"],
                    timeout=parse_int_env("SPARC_LLM_TIMEOUT", 120),
                )
            )
            reflector = SPARCReflectionComponent(
                config=config,
                track=Track.FAST_TRACK,
                execution_mode=SPARCExecutionMode.ASYNC,
                include_raw_response=True,
                retries=parse_int_env("SPARC_RETRIES", 3),
                max_parallel=parse_int_env("SPARC_MAX_PARALLEL", 2),
            )

            if reflector._initialization_error:
                self._initialization_error = reflector._initialization_error
                raise RuntimeError(self._initialization_error)

            self._reflector = reflector
            return reflector

    def reflect(self, payload: dict[str, Any]) -> dict[str, Any]:
        reflector = self.build_reflector()

        from altk.core.toolkit import AgentPhase
        from altk.pre_tool.core import SPARCReflectionRunInput

        session_id = payload.get("session_id", "")
        tool_name = payload.get("tool_calls", [{}])[0].get("function", {}).get("name", "")
        emit_event(
            {
                "session_id": session_id,
                "source": "sparc",
                "stage": "reflection",
                "status": "started",
                "title": "SPARC reflection started",
                "summary": f"Evaluating {tool_name or 'tool call'} with FAST_TRACK.",
                "data": {"tool_name": tool_name, "track": "FAST_TRACK"},
                "raw_log": f"Reflecting on {tool_name or 'tool call'}",
            }
        )

        run_input = SPARCReflectionRunInput(
            messages=payload["messages"],
            tool_specs=payload["tool_specs"],
            tool_calls=payload["tool_calls"],
        )
        result = reflector.process(run_input, phase=AgentPhase.RUNTIME)
        reflection = result.output.reflection_result
        raw_pipeline = result.output.raw_pipeline_result or {}
        overall_avg_score = raw_pipeline.get("overall_avg_score")

        response = {
            "decision": reflection.decision,
            "issues": [
                {
                    "issue_type": issue.issue_type,
                    "metric_name": issue.metric_name,
                    "explanation": issue.explanation,
                    "correction": issue.correction,
                }
                for issue in reflection.issues
            ],
            "execution_time_ms": result.output.execution_time_ms,
            "overall_avg_score": overall_avg_score,
            "raw_pipeline_result": raw_pipeline,
        }

        emit_event(
            {
                "session_id": session_id,
                "source": "sparc",
                "stage": "reflection",
                "status": "success" if reflection.decision == "approve" else "blocked",
                "title": "SPARC reflection finished",
                "summary": f"SPARC returned {reflection.decision} for {tool_name or 'tool call'}.",
                "data": response,
                "raw_log": f"decision={reflection.decision} score={overall_avg_score}",
            }
        )
        return response


def relay_enabled() -> bool:
    return is_truthy(os.getenv("SPARC_RELAY_MODE", ""))


def relay_reflect(payload: dict[str, Any]) -> dict[str, Any]:
    timeout_seconds = parse_int_env("SPARC_RELAY_TIMEOUT", 300)
    poll_interval = max(parse_int_env("SPARC_RELAY_POLL_INTERVAL_MS", 500), 100) / 1000.0

    job = observer_request("POST", "/api/sparc-jobs", {"request": payload}, timeout=5)
    if not job or not job.get("id"):
        raise RuntimeError("failed to enqueue SPARC relay job")

    deadline = time.time() + timeout_seconds
    job_id = job["id"]
    while time.time() < deadline:
        current = observer_request("GET", f"/api/sparc-jobs/{job_id}", timeout=10)
        if not current:
            time.sleep(poll_interval)
            continue

        status = (current.get("status") or "").lower()
        if status == "completed":
            response = current.get("response")
            if isinstance(response, dict):
                return response
            raise RuntimeError("SPARC relay returned an invalid response payload")
        if status == "failed":
            raise RuntimeError(current.get("error") or "SPARC relay job failed")

        time.sleep(poll_interval)

    raise RuntimeError("timed out waiting for host SPARC worker")


SERVICE = ReflectorService()


class Handler(BaseHTTPRequestHandler):
    def _write_json(self, status: int, payload: dict[str, Any]) -> None:
        body = json.dumps(payload).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self) -> None:  # noqa: N802
        if self.path == "/healthz":
            relay_mode = relay_enabled()
            env = normalize_watsonx_env() if not relay_mode else {}
            self._write_json(
                200,
                {
                    "status": "ok",
                    "mode": "relay" if relay_mode else "local",
                    "watsonx_url": env.get("WX_URL", ""),
                    "has_api_key": bool(env.get("WX_API_KEY")),
                    "has_project_id": bool(env.get("WX_PROJECT_ID")),
                    "llm_provider": "litellm.watsonx.output_val",
                    "litellm_local_model_cost_map": env.get("LITELLM_LOCAL_MODEL_COST_MAP", ""),
                    "llm_timeout_seconds": parse_int_env("SPARC_LLM_TIMEOUT", 120),
                    "sparc_retries": parse_int_env("SPARC_RETRIES", 1),
                    "sparc_max_parallel": parse_int_env("SPARC_MAX_PARALLEL", 2),
                    "relay_timeout_seconds": parse_int_env("SPARC_RELAY_TIMEOUT", 300),
                },
            )
            return
        self._write_json(404, {"error": "not_found"})

    def do_POST(self) -> None:  # noqa: N802
        if self.path != "/reflect":
            self._write_json(404, {"error": "not_found"})
            return

        content_length = int(self.headers.get("Content-Length", "0"))
        try:
            payload = json.loads(self.rfile.read(content_length) or b"{}")
        except json.JSONDecodeError:
            self._write_json(400, {"error": "invalid_json"})
            return

        try:
            response = relay_reflect(payload) if relay_enabled() else SERVICE.reflect(payload)
        except Exception as exc:  # pragma: no cover - returned to caller for debugging
            emit_event(
                {
                    "session_id": payload.get("session_id", ""),
                    "source": "sparc",
                    "stage": "reflection",
                    "status": "blocked",
                    "title": "SPARC reflection failed",
                    "summary": str(exc),
                    "raw_log": str(exc),
                }
            )
            self._write_json(500, {"error": str(exc)})
            return

        self._write_json(200, response)

    def log_message(self, fmt: str, *args: Any) -> None:
        return


def main() -> None:
    normalize_watsonx_env()
    port = int(os.getenv("PORT", "8090"))
    server = ThreadingHTTPServer(("0.0.0.0", port), Handler)
    print(f"[sparc-reflector] listening on :{port}")
    server.serve_forever()


if __name__ == "__main__":
    main()
