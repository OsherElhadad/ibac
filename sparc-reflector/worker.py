#!/usr/bin/env python3
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

sys.path.insert(0, os.path.dirname(__file__))

from app import SERVICE, normalize_watsonx_env  # noqa: E402


def observer_request(
    method: str,
    path: str,
    payload: dict | None = None,
    timeout: int = 35,
) -> dict | None:
    observer_url = os.getenv("OBSERVER_URL", "").strip()
    if not observer_url:
        raise RuntimeError("missing OBSERVER_URL for host SPARC worker")

    body = None
    headers = {}
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


def claim_job(worker_id: str, wait_seconds: int) -> dict | None:
    query = urllib.parse.urlencode(
        {"worker_id": worker_id, "wait_seconds": str(wait_seconds)}
    )
    return observer_request(
        "GET",
        f"/api/sparc-jobs/claim?{query}",
        timeout=max(wait_seconds + 5, 10),
    )


def post_result(job_id: str, status: str, response: dict | None = None, error: str = "") -> None:
    observer_request(
        "POST",
        f"/api/sparc-jobs/{job_id}/result",
        {"status": status, "response": response or {}, "error": error},
        timeout=10,
    )


def main() -> None:
    normalize_watsonx_env()

    worker_id = os.getenv("SPARC_WORKER_ID", "host-sparc").strip() or "host-sparc"
    wait_seconds = int(os.getenv("SPARC_WORKER_WAIT_SECONDS", "25"))
    idle_sleep = float(os.getenv("SPARC_WORKER_IDLE_SLEEP_SECONDS", "1.0"))

    print(f"[sparc-worker] polling observer as {worker_id}", flush=True)
    while True:
        try:
            job = claim_job(worker_id, wait_seconds)
            if not job:
                time.sleep(idle_sleep)
                continue

            job_id = job["id"]
            request_payload = job.get("request")
            if not isinstance(request_payload, dict):
                post_result(job_id, "failed", error="invalid SPARC job payload")
                continue

            print(f"[sparc-worker] processing {job_id}", flush=True)
            try:
                response = SERVICE.reflect(request_payload)
            except Exception as exc:  # pragma: no cover - operational path
                error = str(exc)
                print(f"[sparc-worker] {job_id} failed: {error}", flush=True)
                post_result(job_id, "failed", error=error)
                continue

            post_result(job_id, "completed", response=response)
            print(f"[sparc-worker] {job_id} completed", flush=True)
        except KeyboardInterrupt:
            raise
        except Exception as exc:  # pragma: no cover - operational path
            print(f"[sparc-worker] error: {exc}", flush=True)
            time.sleep(2)


if __name__ == "__main__":
    main()
