"""Stub subpackage to satisfy CE-Manager's ``from llm_client.llm import get_llm``
import pattern. Re-exports :func:`get_llm` from the parent stub.
"""
from __future__ import annotations

from .. import get_llm  # noqa: F401
