"""Observer event emission.

All emit() calls are fire-and-forget: the payload is pushed onto a bounded
in-process queue and posted to the observer by a single daemon thread. The
hot request path never blocks on network I/O — it calls put_nowait(), which
is O(1) and lock-free in CPython.

Rationale: the pre-refactor version held a module-level threading.Lock around
a synchronous httpx.Client.post() with timeout=2s. main.py emits ~20 events
per watsonx call; if the observer was slow or GC-paused, each emit could
burn up to 2s while holding the lock, adding ~40s of padding to one watsonx
request. That's the "sometimes fast, sometimes slow" variance the user saw.

The queue is bounded at 1024 payloads; on overflow we log and drop. No
retries, no backoff — observability is best-effort.
"""
from __future__ import annotations

import logging
import queue
import threading
from datetime import datetime, timezone
from typing import Any, Dict, Optional

import httpx

from .settings import settings

log = logging.getLogger(__name__)

_QUEUE_CAPACITY = 1024
_POST_TIMEOUT_S = 2.0

_queue: "queue.Queue[Dict[str, Any]]" = queue.Queue(maxsize=_QUEUE_CAPACITY)
_worker_started = False
_worker_lock = threading.Lock()


def _worker() -> None:
    """Background daemon that drains _queue and POSTs each payload to the
    observer. Runs for the lifetime of the process. Never raises."""
    client = httpx.Client(timeout=_POST_TIMEOUT_S)
    while True:
        payload = _queue.get()
        url = payload.pop("__url", "")
        try:
            r = client.post(url, json=payload)
            if r.status_code >= 300:
                log.warning(
                    "observer returned %s for %s/%s",
                    r.status_code, payload.get("stage"), payload.get("status"),
                )
        except Exception as e:  # noqa: BLE001
            log.warning(
                "failed to emit event %s/%s: %s",
                payload.get("stage"), payload.get("status"), e,
            )
        finally:
            _queue.task_done()


def _ensure_worker() -> None:
    global _worker_started
    if _worker_started:
        return
    with _worker_lock:
        if _worker_started:
            return
        threading.Thread(target=_worker, name="ce-proxy-emit", daemon=True).start()
        _worker_started = True


def emit(
    session_id: str,
    stage: str,
    status: str,
    title: str,
    summary: str,
    data: Optional[Dict[str, Any]] = None,
    raw_log: str = "",
    source: str = "ce-manager",
) -> None:
    """Best-effort enqueue for observer emission. Never blocks on network I/O.
    Never raises. Drops the payload if the queue is at capacity."""
    if not session_id or not settings.observer_url:
        return
    _ensure_worker()
    payload = {
        "session_id": session_id,
        "source": source,
        "stage": stage,
        "status": status,
        "title": title,
        "summary": summary,
        "data": data or {},
        "raw_log": raw_log,
        "timestamp": datetime.now(timezone.utc).isoformat(),
        "__url": settings.observer_url + "/api/events",
    }
    try:
        _queue.put_nowait(payload)
    except queue.Full:
        log.warning(
            "observer event queue full; dropping %s/%s (size=%d)",
            stage, status, _queue.qsize(),
        )
