"""Local stub for IBM-internal ``llm-client`` package.

CE-Manager's ``ce_manager/utils/__init__.py`` imports ``llm_client.llm`` at
package load time, which pulls the IBM-internal ``llm-client`` package over
SSH. This demo doesn't use that library — the ce-proxy provides its own
:class:`MaskerClient` that wraps LiteLLM directly — so we ship a tiny stub
that satisfies the import graph without any network dependency.

If anything actually tries to call :func:`get_llm` at runtime, we fail loudly
so the bug surfaces fast instead of silently returning None.
"""
from __future__ import annotations


def get_llm(*args, **kwargs):
    raise RuntimeError(
        "llm_client stub: the ce-proxy owns its own LLM client via "
        "ce_proxy.app.litellm_client.MaskerClient. If you hit this, CE-Manager "
        "is trying to call get_llm() — replace that call site with a direct "
        "LiteLLM invocation."
    )
