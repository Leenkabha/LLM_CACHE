"""Tests for the embedding service's HTTP layer and pluggable-model registry.

These tests never load the real sentence-transformers model (heavy: requires
torch + a model download). Instead they register a lightweight fake model
under EMBEDDING_MODEL_BACKEND *before* importing app.main, so main.py's
module-level `_model = create_from_env()` resolves to the fake. This mirrors
the registry contract every real adapter (including the shipped
SentenceTransformerModel) must satisfy: name, dim, and a normalized embed().

A separate, explicitly-opt-in test at the bottom exercises the real
SentenceTransformerModel end to end.
"""

import os
import unittest

os.environ.setdefault("EMBEDDING_MODEL_BACKEND", "test-fake")

from app import models  # noqa: E402


class FakeModel(models.EmbeddingModel):
    """Deterministic stand-in: returns a fixed-length unit vector."""

    def __init__(self) -> None:
        self._dim = 4

    @property
    def name(self) -> str:
        return "fake-model"

    @property
    def dim(self) -> int:
        return self._dim

    def embed(self, text: str) -> list[float]:
        # Deterministic, text-dependent, and pre-normalized to unit length so
        # it obeys the same contract the real adapter documents.
        seed = (len(text) % 4) or 1
        vec = [0.0] * self._dim
        vec[seed - 1] = 1.0
        return vec


@models.register("test-fake")
def _build_fake() -> models.EmbeddingModel:
    return FakeModel()


from fastapi.testclient import TestClient  # noqa: E402

from app import main  # noqa: E402


class EmbeddingModelRegistryTests(unittest.TestCase):
    def test_create_from_env_builds_registered_backend(self):
        model = models.create_from_env()
        self.assertEqual(model.name, "fake-model")
        self.assertEqual(model.dim, 4)

    def test_unknown_backend_fails_fast_with_registered_names_listed(self):
        os.environ["EMBEDDING_MODEL_BACKEND"] = "does-not-exist"
        try:
            with self.assertRaises(ValueError) as ctx:
                models.create_from_env()
            message = str(ctx.exception)
            self.assertIn("does-not-exist", message)
            self.assertIn("test-fake", message)
        finally:
            os.environ["EMBEDDING_MODEL_BACKEND"] = "test-fake"

    def test_register_is_a_plain_decorator_not_tied_to_plugin_directory(self):
        # The plugins/ auto-import mechanism is a convenience layer on top of
        # register(); register() itself must work standalone, since that is
        # what both built-in adapters and drop-in plugins ultimately call.
        calls = []

        @models.register("throwaway-for-test")
        def _build():
            calls.append(1)
            return FakeModel()

        models._REGISTRY["throwaway-for-test"]()
        self.assertEqual(calls, [1])
        del models._REGISTRY["throwaway-for-test"]


class EmbeddingEndpointTests(unittest.TestCase):
    def setUp(self):
        self.client = TestClient(main.app)
        self.addCleanup(self.client.close)

    def test_embed_returns_vector_and_dim_from_active_model(self):
        response = self.client.post("/embed", json={"text": "hello"})
        self.assertEqual(response.status_code, 200)
        body = response.json()
        self.assertEqual(body["dim"], 4)
        self.assertEqual(len(body["vector"]), 4)
        # "hello" has length 5 -> seed = 5 % 4 = 1 -> index 0 is 1.0
        self.assertEqual(body["vector"], [1.0, 0.0, 0.0, 0.0])

    def test_embed_is_deterministic_for_the_same_text(self):
        first = self.client.post("/embed", json={"text": "same prompt"}).json()
        second = self.client.post("/embed", json={"text": "same prompt"}).json()
        self.assertEqual(first, second)

    def test_embed_rejects_missing_text_field(self):
        response = self.client.post("/embed", json={})
        self.assertEqual(response.status_code, 422)

    def test_embed_rejects_malformed_json(self):
        response = self.client.post(
            "/embed", content="not json", headers={"Content-Type": "application/json"}
        )
        self.assertEqual(response.status_code, 422)

    def test_model_info_reports_active_model(self):
        response = self.client.get("/model-info")
        self.assertEqual(response.status_code, 200)
        self.assertEqual(response.json(), {"name": "fake-model", "dim": 4})

    def test_health(self):
        response = self.client.get("/health")
        self.assertEqual(response.status_code, 200)
        self.assertEqual(response.json(), {"status": "ok"})


@unittest.skipUnless(
    os.environ.get("RUN_SLOW_MODEL_TESTS") == "1",
    "set RUN_SLOW_MODEL_TESTS=1 to exercise the real sentence-transformers model "
    "(downloads ~90MB of weights and requires the sentence-transformers/torch deps)",
)
class RealSentenceTransformerModelTests(unittest.TestCase):
    def test_real_model_produces_unit_length_vectors_of_the_documented_dimension(self):
        model = models.SentenceTransformerModel("all-MiniLM-L6-v2")
        self.assertEqual(model.dim, 384)
        vec = model.embed("What is virtual memory?")
        self.assertEqual(len(vec), 384)
        norm = sum(x * x for x in vec) ** 0.5
        self.assertAlmostEqual(norm, 1.0, places=4)

    def test_real_model_gives_similar_prompts_a_small_distance(self):
        model = models.SentenceTransformerModel("all-MiniLM-L6-v2")
        a = model.embed("Explain virtual memory")
        b = model.embed("How does virtual memory work?")
        c = model.embed("What is the weather today?")
        dot_ab = sum(x * y for x, y in zip(a, b))
        dot_ac = sum(x * y for x, y in zip(a, c))
        # Paraphrases of the same question must score closer (higher cosine
        # similarity / lower distance) than an unrelated question.
        self.assertGreater(dot_ab, dot_ac)


if __name__ == "__main__":
    unittest.main()
