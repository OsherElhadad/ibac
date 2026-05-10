"""Stub types to satisfy CE-Manager's ``from llm_client.llm.types import
GenerationArgs`` import. The dataclass surface is not exercised at runtime by
the programmatic masker path used in this demo.
"""
from __future__ import annotations


class GenerationArgs:
    """Placeholder that accepts any kwargs so CE-Manager's default
    configuration paths don't raise during import."""

    def __init__(self, *args, **kwargs):
        for k, v in kwargs.items():
            setattr(self, k, v)
