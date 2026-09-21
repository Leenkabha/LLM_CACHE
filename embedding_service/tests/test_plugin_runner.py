"""Plugin-runner behaviour of the embedding service.

Covers the versioned /v1 protocol, runner introspection, and drop-in discovery of
a plugin *package* (a directory), which is exactly how the plugin platform's
runner image installs a developer's EmbeddingModel. The SDK example is loaded
from sdk/examples/embedding-model and held to the contract.
"""

import importlib
import math
import os
import shutil
import sys
import tempfile
import textwrap
import unittest
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

os.environ.setdefault("EMBEDDING_MODEL_BACKEND", "test-fake")

from app import _plugins, models  # noqa: E402


class _Fake(models.EmbeddingModel):
    name = "fake-model"
    dim = 4

    def embed(self, text):
        vec = [0.0] * 4
        vec[(len(text) % 4)] = 1.0
        return vec


if "test-fake" not in models._REGISTRY:
    models.register("test-fake")(lambda: _Fake())

from fastapi.testclient import TestClient  # noqa: E402

from app import main  # noqa: E402

REPO = Path(__file__).resolve().parents[2]
SDK_EXAMPLE = REPO / "sdk" / "examples" / "embedding-model" / "sdk_hash_model"


class V1ProtocolTests(unittest.TestCase):
    def setUp(self):
        self.client = TestClient(main.app)
        self.addCleanup(self.client.close)

    def test_v1_embed_matches_unversioned_and_reports_model(self):
        v1 = self.client.post("/v1/embed", json={"text": "hello"}).json()
        legacy = self.client.post("/embed", json={"text": "hello"}).json()
        self.assertEqual(v1, legacy)
        self.assertEqual(v1["dim"], len(v1["vector"]))
        self.assertTrue(v1["model"])

    def test_v1_model_info_matches_legacy(self):
        self.assertEqual(
            self.client.get("/v1/model-info").json(), self.client.get("/model-info").json()
        )

    def test_v1_embed_rejects_a_non_string_text(self):
        self.assertEqual(self.client.post("/v1/embed", json={"text": 42}).status_code, 422)

    def test_runner_info_lists_registered_backends_and_selection(self):
        info = self.client.get("/v1/runner-info").json()
        self.assertEqual(info["kind"], "embedding")
        self.assertEqual(info["backend"], os.environ["EMBEDDING_MODEL_BACKEND"])
        self.assertIn(info["backend"], info["registered_backends"])

    def test_health_body_is_unchanged(self):
        self.assertEqual(self.client.get("/health").json(), {"status": "ok"})

    def test_concurrent_embeds_are_consistent(self):
        def call(_):
            with TestClient(main.app) as c:
                return c.post("/v1/embed", json={"text": "same"}).json()["vector"]

        with ThreadPoolExecutor(8) as pool:
            results = list(pool.map(call, range(16)))
        self.assertTrue(all(r == results[0] for r in results))


class PackageDiscoveryTests(unittest.TestCase):
    """A developer package copied into app/plugins/ is discovered on startup."""

    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.tmp, True)
        from app import plugins

        self.plugins = plugins
        plugins.__path__.append(str(self.tmp))
        self.addCleanup(plugins.__path__.remove, str(self.tmp))
        self.saved = dict(models._REGISTRY)
        self.addCleanup(self._restore)
        _plugins._loaded = False
        self.addCleanup(setattr, _plugins, "_loaded", True)

    def _restore(self):
        models._REGISTRY.clear()
        models._REGISTRY.update(self.saved)
        for name in [n for n in sys.modules if n.startswith("app.plugins.")]:
            del sys.modules[name]

    def test_a_plugin_package_registers_itself(self):
        pkg = self.tmp / "my_pkg_model"
        pkg.mkdir()
        (pkg / "__init__.py").write_text(
            textwrap.dedent(
                """
                from app.models import EmbeddingModel, register

                class M(EmbeddingModel):
                    name = "pkg-model"
                    dim = 2
                    def embed(self, text):
                        return [1.0, 0.0]

                @register("pkg-model")
                def _b():
                    return M()
                """
            )
        )
        self.assertIn("pkg-model", models.registered())

    def test_selecting_an_unregistered_backend_fails_with_the_list(self):
        old = os.environ["EMBEDDING_MODEL_BACKEND"]
        os.environ["EMBEDDING_MODEL_BACKEND"] = "nope"
        self.addCleanup(os.environ.__setitem__, "EMBEDDING_MODEL_BACKEND", old)
        with self.assertRaises(ValueError) as ctx:
            models.create_from_env()
        self.assertIn("nope", str(ctx.exception))

    def test_a_broken_plugin_package_fails_loudly_at_startup(self):
        pkg = self.tmp / "broken_pkg"
        pkg.mkdir()
        (pkg / "__init__.py").write_text("raise RuntimeError('plugin bug')\n")
        with self.assertRaises(RuntimeError):
            models.registered()

    def test_the_sdk_example_meets_the_embedding_contract(self):
        shutil.copytree(SDK_EXAMPLE, self.tmp / "sdk_hash_model")
        self.assertIn("sdk-hash", models.registered())
        os.environ["CONFIG_DIM"] = "64"
        self.addCleanup(os.environ.pop, "CONFIG_DIM", None)
        model = models._REGISTRY["sdk-hash"]()
        self.assertEqual(model.dim, 64)
        self.assertTrue(model.name)
        texts = ["hello", "What is virtual memory?", "héllo 你好", "a", ""]
        first = [model.embed(t) for t in texts]
        for text, vec in zip(texts, first):
            self.assertEqual(len(vec), model.dim, text)
            self.assertTrue(all(math.isfinite(x) for x in vec), text)
            self.assertAlmostEqual(math.sqrt(sum(x * x for x in vec)), 1.0, places=6)
        self.assertEqual(first, [model.embed(t) for t in texts], "embedding must be deterministic")
        self.assertNotEqual(first[0], first[1])
        with ThreadPoolExecutor(8) as pool:
            same = list(pool.map(model.embed, ["concurrent"] * 32))
        self.assertTrue(all(v == same[0] for v in same))


class PluginAuthTests(unittest.TestCase):
    """PLUGIN_AUTH_TOKEN is enforced when set and invisible when not."""

    def _app(self, token):
        from fastapi import FastAPI

        from app.plugin_auth import install_plugin_auth

        app = FastAPI()

        @app.get("/health")
        def health():
            return {"status": "ok"}

        @app.get("/v1/secret")
        def secret():
            return {"ok": True}

        return app, install_plugin_auth(app, token)

    def test_unset_token_changes_nothing(self):
        app, installed = self._app("")
        self.assertFalse(installed)
        with TestClient(app) as c:
            self.assertEqual(c.get("/v1/secret").status_code, 200)

    def test_token_is_required_except_for_health(self):
        app, installed = self._app("s3cret-token")
        self.assertTrue(installed)
        with TestClient(app) as c:
            self.assertEqual(c.get("/health").status_code, 200)
            self.assertEqual(c.get("/v1/secret").status_code, 401)
            self.assertEqual(c.get("/v1/secret", headers={"Authorization": "Bearer wrong"}).status_code, 401)
            self.assertEqual(c.get("/v1/secret", headers={"Authorization": "s3cret-token"}).status_code, 401)
            self.assertEqual(c.get("/v1/secret", headers={"Authorization": "Bearer s3cret-token"}).status_code, 200)


if __name__ == "__main__":
    unittest.main()
