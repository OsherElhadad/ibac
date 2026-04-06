import json
import pathlib
import sys
import types
import unittest
from unittest import mock


TOOLKIT_ROOT = pathlib.Path(__file__).resolve().parents[2] / "agent-lifecycle-toolkit"
if str(TOOLKIT_ROOT) not in sys.path:
    sys.path.insert(0, str(TOOLKIT_ROOT))

from altk.core.llm.providers.ibm_watsonx_ai.ibm_watsonx_ai import (  # noqa: E402
    WatsonxModelInferenceAdapter,
)


class _FakeHTTPResponse:
    def __init__(self, payload):
        self._payload = payload

    def read(self):
        return json.dumps(self._payload).encode("utf-8")

    def __enter__(self):
        return self

    def __exit__(self, exc_type, exc, tb):
        return False


class WatsonxAdapterTest(unittest.TestCase):
    def test_chat_uses_chat_endpoint_and_maps_chat_params(self):
        captured = {}

        def fake_urlopen(request, timeout=0):
            captured["url"] = request.full_url
            captured["timeout"] = timeout
            captured["body"] = json.loads(request.data.decode("utf-8"))
            return _FakeHTTPResponse(
                {"choices": [{"message": {"content": "{\"ok\": true}"}}]}
            )

        credentials = types.SimpleNamespace(
            token="token-123",
            api_key="",
            url="https://example.wx.ibm.com",
        )
        adapter = WatsonxModelInferenceAdapter(
            model_id="demo-model",
            credentials=credentials,
            project_id="project-1",
            timeout=33,
        )

        with mock.patch(
            "altk.core.llm.providers.ibm_watsonx_ai.ibm_watsonx_ai.urllib.request.urlopen",
            side_effect=fake_urlopen,
        ):
            result = adapter.chat(
                [
                    {"role": "system", "content": "Return JSON only."},
                    {"role": "user", "content": "Validate TX482."},
                ],
                params={
                    "max_tokens": 64,
                    "stop": ["DONE"],
                    "frequency_penalty": 1.2,
                },
            )

        self.assertEqual(
            captured["url"],
            "https://example.wx.ibm.com/ml/v1/text/chat?version=2023-05-29",
        )
        self.assertEqual(captured["timeout"], 33)
        self.assertEqual(captured["body"]["model_id"], "demo-model")
        self.assertEqual(captured["body"]["project_id"], "project-1")
        self.assertEqual(captured["body"]["messages"][0]["role"], "system")
        self.assertEqual(captured["body"]["messages"][1]["role"], "user")
        self.assertEqual(captured["body"]["max_tokens"], 64)
        self.assertEqual(captured["body"]["stop"], ["DONE"])
        self.assertEqual(captured["body"]["frequency_penalty"], 1.2)
        self.assertEqual(result["choices"][0]["message"]["content"], "{\"ok\": true}")

    def test_sets_demo_friendly_defaults_when_budget_is_missing(self):
        captured = {}

        def fake_urlopen(request, timeout=0):
            captured["body"] = json.loads(request.data.decode("utf-8"))
            return _FakeHTTPResponse({"results": [{"generated_text": "{\"ok\": true}"}]})

        credentials = types.SimpleNamespace(
            token="token-123",
            api_key="",
            url="https://example.wx.ibm.com",
        )
        adapter = WatsonxModelInferenceAdapter(
            model_id="demo-model",
            credentials=credentials,
            project_id="project-1",
        )

        with mock.patch(
            "altk.core.llm.providers.ibm_watsonx_ai.ibm_watsonx_ai.urllib.request.urlopen",
            side_effect=fake_urlopen,
        ):
            adapter.generate("hello world")

        self.assertEqual(captured["body"]["parameters"]["max_new_tokens"], 256)
        self.assertEqual(captured["body"]["parameters"]["decoding_method"], "greedy")


if __name__ == "__main__":
    unittest.main()
