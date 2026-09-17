"""HTTP endpoint coverage for /upsert, /delete, /flush, /rebuild, /size, and
the VectorIndex / SimilarityMetric registries -- these were previously only
exercised indirectly (test_search.py covers /search directly and the FAISS
index class thoroughly, but not the other REST endpoints or the pluggable
backend/metric selection contract).
"""

import unittest
from unittest.mock import patch

from fastapi.testclient import TestClient

from app import index as index_module
from app import main
from app import metrics as metrics_module
from app.index import FaissVectorIndex
from app.metrics import CosineMetric


class EndpointCRUDTests(unittest.TestCase):
    def setUp(self):
        self.index = FaissVectorIndex(2, CosineMetric())
        self.patch = patch.object(main, "_index", self.index)
        self.patch.start()
        self.addCleanup(self.patch.stop)
        self.client = TestClient(main.app)
        self.addCleanup(self.client.close)

    def test_upsert_then_size(self):
        response = self.client.post("/upsert", json={"vector": [1, 0]})
        self.assertEqual(response.status_code, 200)
        entry_id = response.json()["id"]
        self.assertTrue(entry_id)

        response = self.client.get("/size")
        self.assertEqual(response.json(), {"size": 1})

        response = self.client.post("/search", json={"vector": [1, 0], "top_k": 1, "threshold": 0.01})
        self.assertEqual(response.json()["matches"], [{"id": entry_id, "distance": 0.0}])

    def test_upsert_rejects_wrong_dimension(self):
        response = self.client.post("/upsert", json={"vector": [1, 0, 0]})
        self.assertEqual(response.status_code, 400)

    def test_delete_existing_returns_200_and_removes_it(self):
        entry_id = self.client.post("/upsert", json={"vector": [1, 0]}).json()["id"]
        response = self.client.delete(f"/entries/{entry_id}")
        self.assertEqual(response.status_code, 200)
        self.assertEqual(self.client.get("/size").json(), {"size": 0})

    def test_delete_unknown_id_returns_404_not_silently_ok(self):
        # This is the exact bug class previously found in the Go client's
        # Delete(): a delete of something that isn't there must be
        # distinguishable from a successful delete, via the status code.
        response = self.client.delete("/entries/does-not-exist")
        self.assertEqual(response.status_code, 404)

    def test_delete_is_not_idempotent_as_200_twice(self):
        entry_id = self.client.post("/upsert", json={"vector": [1, 0]}).json()["id"]
        first = self.client.delete(f"/entries/{entry_id}")
        second = self.client.delete(f"/entries/{entry_id}")
        self.assertEqual(first.status_code, 200)
        self.assertEqual(second.status_code, 404)

    def test_flush_clears_the_index(self):
        self.client.post("/upsert", json={"vector": [1, 0]})
        self.client.post("/upsert", json={"vector": [0, 1]})
        self.assertEqual(self.client.get("/size").json(), {"size": 2})
        response = self.client.post("/flush", json={})
        self.assertEqual(response.status_code, 200)
        self.assertEqual(self.client.get("/size").json(), {"size": 0})

    def test_rebuild_replaces_contents_and_reports_count(self):
        self.client.post("/upsert", json={"vector": [9, 9]})  # must be gone after rebuild
        response = self.client.post(
            "/rebuild",
            json={"entries": [{"id": "a", "vector": [1, 0]}, {"id": "b", "vector": [0, 1]}]},
        )
        self.assertEqual(response.status_code, 200)
        self.assertEqual(response.json(), {"restored": 2})
        self.assertEqual(self.client.get("/size").json(), {"size": 2})
        response = self.client.post("/search", json={"vector": [1, 0], "top_k": 5, "threshold": 0.01})
        self.assertEqual([m["id"] for m in response.json()["matches"]], ["a"])

    def test_rebuild_on_empty_redis_state_yields_empty_index(self):
        self.client.post("/upsert", json={"vector": [1, 0]})
        response = self.client.post("/rebuild", json={"entries": []})
        self.assertEqual(response.status_code, 200)
        self.assertEqual(response.json(), {"restored": 0})
        self.assertEqual(self.client.get("/size").json(), {"size": 0})

    def test_rebuild_rejects_duplicate_ids_rather_than_corrupting_the_index(self):
        response = self.client.post(
            "/rebuild",
            json={"entries": [{"id": "a", "vector": [1, 0]}, {"id": "a", "vector": [0, 1]}]},
        )
        self.assertEqual(response.status_code, 400)

    def test_repeated_rebuild_calls_do_not_accumulate_entries(self):
        payload = {"entries": [{"id": "a", "vector": [1, 0]}, {"id": "b", "vector": [0, 1]}]}
        for _ in range(3):
            response = self.client.post("/rebuild", json=payload)
            self.assertEqual(response.json(), {"restored": 2})
        self.assertEqual(self.client.get("/size").json(), {"size": 2})

    def test_health(self):
        self.assertEqual(self.client.get("/health").json(), {"status": "ok"})


class RegistryFailFastTests(unittest.TestCase):
    """VECTOR_INDEX_BACKEND / SIMILARITY_METRIC must fail fast on a typo,
    listing what's registered -- mirrors the Go plugin registry contract."""

    def test_unknown_vector_index_backend_raises_with_registered_names(self):
        import os

        old = os.environ.get("VECTOR_INDEX_BACKEND")
        os.environ["VECTOR_INDEX_BACKEND"] = "does-not-exist"
        try:
            with self.assertRaises(ValueError) as ctx:
                index_module.create_from_env(2, CosineMetric())
            self.assertIn("does-not-exist", str(ctx.exception))
            self.assertIn("faiss", str(ctx.exception))
        finally:
            if old is None:
                os.environ.pop("VECTOR_INDEX_BACKEND", None)
            else:
                os.environ["VECTOR_INDEX_BACKEND"] = old

    def test_unknown_similarity_metric_raises_with_registered_names(self):
        import os

        old = os.environ.get("SIMILARITY_METRIC")
        os.environ["SIMILARITY_METRIC"] = "does-not-exist"
        try:
            with self.assertRaises(ValueError) as ctx:
                metrics_module.create_from_env()
            self.assertIn("does-not-exist", str(ctx.exception))
            self.assertIn("cosine", str(ctx.exception))
            self.assertIn("euclidean", str(ctx.exception))
        finally:
            if old is None:
                os.environ.pop("SIMILARITY_METRIC", None)
            else:
                os.environ["SIMILARITY_METRIC"] = old

    def test_register_decorator_is_reusable_directly_by_a_drop_in_plugin(self):
        # This is the actual contract app/plugins/*.py relies on: register()
        # works standalone, the plugins/ directory is just an auto-import
        # convenience on top of it.
        calls = []

        @index_module.register("throwaway-for-test")
        def _factory(dim, metric):
            calls.append((dim, metric))
            return FaissVectorIndex(dim, metric)

        built = index_module._REGISTRY["throwaway-for-test"](3, CosineMetric())
        self.assertEqual(built.dim, 3)
        self.assertEqual(len(calls), 1)
        del index_module._REGISTRY["throwaway-for-test"]


class PluginAutoDiscoveryTests(unittest.TestCase):
    """The app/plugins/ auto-import mechanism itself (app/_plugins.py)."""

    def test_load_plugins_is_idempotent_and_does_not_double_import(self):
        from app import _plugins

        # Reset the module-level guard to force a real (re-)load path.
        _plugins._loaded = False
        _plugins.load_plugins()
        self.assertTrue(_plugins._loaded)
        # A second call must be a no-op, not re-import (and not raise).
        _plugins.load_plugins()

    def test_missing_plugins_package_does_not_crash_discovery(self):
        from app import _plugins

        _plugins._loaded = False
        with patch("builtins.__import__", side_effect=ImportError("no plugins package")):
            _plugins.load_plugins()  # must not raise
        self.assertTrue(_plugins._loaded)


if __name__ == "__main__":
    unittest.main()
