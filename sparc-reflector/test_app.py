import importlib.util
import os
import pathlib
import unittest


MODULE_PATH = pathlib.Path(__file__).with_name("app.py")
SPEC = importlib.util.spec_from_file_location("sparc_reflector_app", MODULE_PATH)
APP = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(APP)


class NormalizeWatsonxEnvTest(unittest.TestCase):
    def setUp(self):
        self.original = dict(os.environ)

    def tearDown(self):
        os.environ.clear()
        os.environ.update(self.original)

    def test_normalize_prefers_existing_wx_values(self):
        os.environ["WX_API_KEY"] = "wx-key"
        os.environ["WX_PROJECT_ID"] = "wx-project"
        os.environ["WX_URL"] = "https://example.com"
        os.environ["WATSONX_API_KEY"] = "other-key"

        env = APP.normalize_watsonx_env()

        self.assertEqual(env["WX_API_KEY"], "wx-key")
        self.assertEqual(env["WX_PROJECT_ID"], "wx-project")
        self.assertEqual(env["WX_URL"], "https://example.com")
        self.assertEqual(env["LITELLM_LOCAL_MODEL_COST_MAP"], "True")

    def test_normalize_falls_back_to_watsonx_names(self):
        os.environ["WATSONX_API_KEY"] = "legacy-key"
        os.environ["WATSONX_PROJECT_ID"] = "legacy-project"

        env = APP.normalize_watsonx_env()

        self.assertEqual(env["WX_API_KEY"], "legacy-key")
        self.assertEqual(env["WX_PROJECT_ID"], "legacy-project")
        self.assertEqual(env["WX_URL"], "https://us-south.ml.cloud.ibm.com")
        self.assertEqual(env["LITELLM_LOCAL_MODEL_COST_MAP"], "True")


class TruthyParsingTest(unittest.TestCase):
    def test_truthy_values(self):
        self.assertTrue(APP.is_truthy("true"))
        self.assertTrue(APP.is_truthy(" YES "))
        self.assertTrue(APP.is_truthy("1"))

    def test_falsey_values(self):
        self.assertFalse(APP.is_truthy("false"))
        self.assertFalse(APP.is_truthy(""))
        self.assertFalse(APP.is_truthy("0"))


class ParseIntEnvTest(unittest.TestCase):
    def setUp(self):
        self.original = dict(os.environ)

    def tearDown(self):
        os.environ.clear()
        os.environ.update(self.original)

    def test_returns_default_for_missing_or_invalid_values(self):
        self.assertEqual(APP.parse_int_env("SPARC_RETRIES", 3), 3)
        os.environ["SPARC_RETRIES"] = "oops"
        self.assertEqual(APP.parse_int_env("SPARC_RETRIES", 3), 3)

    def test_parses_integer_values(self):
        os.environ["SPARC_MAX_PARALLEL"] = "4"
        self.assertEqual(APP.parse_int_env("SPARC_MAX_PARALLEL", 2), 4)


if __name__ == "__main__":
    unittest.main()
