"""Plugin-runner behaviour of the vector-store service.

Covers the versioned /v1 protocol, runner introspection, the in-process metric
conversion check, request serialisation, and drop-in discovery of an index and a
metric *package*. The SDK examples (numpy index, angular metric) are held to the
same behavioural contract as the built-in FAISS index.
"""

import math
import shutil
import sys
import tempfile
import unittest
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
from unittest.mock import patch

from fastapi.testclient import TestClient

from app import _plugins
from app import index as index_module
from app import main
from app import metrics as metrics_module
from app.index import FaissVectorIndex
from app.metrics import CosineMetric, EuclideanMetric

REPO = Path(__file__).resolve().parents[2]
SDK = REPO / "sdk" / "examples"


class V1ProtocolTests(unittest.TestCase):
    def setUp(self):
        self.index = FaissVectorIndex(2, CosineMetric())
        patcher = patch.object(main, "_index", self.index)
        patcher.start()
        self.addCleanup(patcher.stop)
        self.client = TestClient(main.app)
        self.addCleanup(self.client.close)

    def test_every_v1_route_works(self):
        c = self.client
        upsert = c.post("/v1/upsert", json={"vector": [1, 0]})
        self.assertEqual(upsert.status_code, 200)
        entry_id = upsert.json()["id"]
        self.assertEqual(c.get("/v1/size").json(), {"size": 1})
        found = c.post("/v1/search", json={"vector": [1, 0], "top_k": 3, "threshold": 0.1}).json()
        self.assertEqual([m["id"] for m in found["matches"]], [entry_id])
        self.assertEqual(c.post("/v1/rebuild", json={"entries": [{"id": "a", "vector": [0, 1]}]}).json(), {"restored": 1})
        self.assertEqual(c.delete("/v1/entries/a").status_code, 200)
        self.assertEqual(c.delete("/v1/entries/a").status_code, 404)
        self.assertEqual(c.post("/v1/flush").json(), {"status": "flushed"})

    def test_health_body_is_unchanged_and_info_reports_dimension(self):
        self.assertEqual(self.client.get("/health").json(), {"status": "ok"})
        info = self.client.get("/v1/info").json()
        self.assertEqual(info["metric"], main._metric.name)
        self.assertIsInstance(info["dim"], int)

    def test_bad_requests_are_client_errors(self):
        for path, body in [
            ("/v1/upsert", {"vector": [1, 2, 3]}),
            ("/v1/search", {"vector": [1, 0], "top_k": 0, "threshold": 1}),
            ("/v1/search", {"vector": [1, 2, 3], "top_k": 1, "threshold": 1}),
            ("/v1/rebuild", {"entries": [{"id": "x", "vector": [1, 2, 3]}]}),
        ]:
            self.assertEqual(self.client.post(path, json=body).status_code // 100, 4, path)

    def test_runner_info_and_metric_info(self):
        info = self.client.get("/v1/runner-info").json()
        self.assertEqual(info["kind"], "vector")
        self.assertIn("faiss", info["registered_backends"])
        self.assertIn("cosine", info["registered_metrics"])
        self.assertIn("euclidean", info["registered_metrics"])
        self.assertEqual(self.client.get("/v1/metric-info").json()["name"], main._metric.name)

    def test_metric_distance_converts_scores_in_process(self):
        with patch.object(main, "_metric", CosineMetric()):
            out = self.client.post("/v1/metric/distance", json={"scores": [1.0, 0.0, -1.0]}).json()
        self.assertEqual(out["distances"], [0.0, 1.0, 2.0])
        with patch.object(main, "_metric", EuclideanMetric()):
            out = self.client.post("/v1/metric/distance", json={"scores": [0.0, 4.0]}).json()
        self.assertEqual(out["distances"], [0.0, 2.0])
        self.assertEqual(self.client.post("/v1/metric/distance", json={"scores": [0.0] * 1001}).status_code, 422)

    def test_concurrent_writes_and_searches_do_not_corrupt_the_index(self):
        def work(worker):
            with TestClient(main.app) as c:
                for i in range(10):
                    v = [1.0, float(worker + i)]
                    self.assertEqual(c.post("/v1/upsert", json={"vector": v}).status_code, 200)
                    self.assertEqual(c.post("/v1/search", json={"vector": v, "top_k": 3, "threshold": 5}).status_code, 200)

        with ThreadPoolExecutor(8) as pool:
            list(pool.map(work, range(8)))
        self.assertEqual(self.client.get("/v1/size").json(), {"size": 80})


class PackageDiscoveryTests(unittest.TestCase):
    def setUp(self):
        from app import plugins

        self.tmp = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.tmp, True)
        plugins.__path__.append(str(self.tmp))
        self.addCleanup(plugins.__path__.remove, str(self.tmp))
        self.saved_idx = dict(index_module._REGISTRY)
        self.saved_met = dict(metrics_module._REGISTRY)
        self.addCleanup(self._restore)
        _plugins._loaded = False
        self.addCleanup(setattr, _plugins, "_loaded", True)

    def _restore(self):
        index_module._REGISTRY.clear()
        index_module._REGISTRY.update(self.saved_idx)
        metrics_module._REGISTRY.clear()
        metrics_module._REGISTRY.update(self.saved_met)
        for name in [n for n in sys.modules if n.startswith("app.plugins.")]:
            del sys.modules[name]

    def _install(self, example, package):
        shutil.copytree(SDK / example / package, self.tmp / package)

    def test_index_and_metric_packages_are_discovered(self):
        self._install("vector-index", "sdk_numpy_index")
        self._install("similarity-metric", "sdk_angular_metric")
        self.assertIn("sdk-numpy", index_module.registered())
        self.assertIn("sdk-angular", metrics_module.registered())

    def _contract(self, make):
        """The behavioural contract every VectorIndex must satisfy."""
        idx = make()
        self.assertEqual(idx.size(), 0)
        self.assertFalse(idx.search([1, 0], 1, 1.0).hit)
        a = idx.upsert([1.0, 0.0])
        b = idx.upsert([0.8, 0.6])
        c = idx.upsert([0.0, 1.0])
        self.assertEqual(len({a, b, c}), 3)
        self.assertEqual(idx.size(), 3)
        matches = idx.search([1.0, 0.0], 10, 1e9).matches
        self.assertEqual([m.entry_id for m in matches], [a, b, c])
        self.assertEqual(sorted(m.distance for m in matches), [m.distance for m in matches])
        self.assertEqual(len(idx.search([1.0, 0.0], 2, 1e9).matches), 2)
        exact = idx.search([1.0, 0.0], 1, 1e9).matches[0]
        threshold = idx.search([1.0, 0.0], 10, 1e9).matches[1].distance
        self.assertIn(b, [m.entry_id for m in idx.search([1.0, 0.0], 10, threshold).matches])
        self.assertNotIn(b, [m.entry_id for m in idx.search([1.0, 0.0], 10, threshold - 1e-3).matches])
        self.assertEqual(exact.entry_id, a)
        with self.assertRaises(ValueError):
            idx.upsert([1.0, 0.0, 0.0])
        with self.assertRaises(ValueError):
            idx.search([1.0], 1, 1.0)
        with self.assertRaises(ValueError):
            idx.search([1.0, 0.0], 0, 1.0)
        self.assertTrue(idx.delete(b))
        self.assertFalse(idx.delete(b))
        self.assertEqual(idx.size(), 2)
        self.assertEqual(idx.rebuild([("x", [1.0, 0.0]), ("y", [0.0, 1.0])]), 2)
        self.assertEqual({m.entry_id for m in idx.search([1.0, 0.0], 10, 1e9).matches}, {"x", "y"}, "rebuild must replace")
        with self.assertRaises(ValueError):
            idx.rebuild([("d", [1.0, 0.0]), ("d", [0.0, 1.0])])
        self.assertEqual(idx.rebuild([]), 0)
        self.assertEqual(idx.size(), 0)
        idx.upsert([1.0, 0.0])
        idx.flush()
        idx.flush()
        self.assertEqual(idx.size(), 0)

    def test_builtin_faiss_index_meets_the_contract(self):
        self._contract(lambda: FaissVectorIndex(2, CosineMetric()))

    def test_sdk_numpy_index_meets_the_same_contract(self):
        self._install("vector-index", "sdk_numpy_index")
        factory = index_module._REGISTRY  # populated by discovery
        index_module.registered()
        self._contract(lambda: factory["sdk-numpy"](2, CosineMetric()))
        self._contract(lambda: factory["sdk-numpy"](2, EuclideanMetric()))

    def test_sdk_angular_metric_meets_the_metric_contract(self):
        self._install("similarity-metric", "sdk_angular_metric")
        metric = metrics_module._REGISTRY["sdk-angular"] if "sdk-angular" in metrics_module.registered() else None
        metric = metric()
        self.assertEqual(metric.name, "sdk-angular")
        import faiss

        self.assertEqual(metric.faiss_metric_type, faiss.METRIC_INNER_PRODUCT)
        scores = [-1.0, -0.5, 0.0, 0.5, 0.9, 1.0]
        distances = [metric.to_distance(s) for s in scores]
        self.assertTrue(all(math.isfinite(d) for d in distances))
        self.assertEqual(distances, sorted(distances, reverse=True), "a better score must give a lower distance")
        self.assertAlmostEqual(metric.to_distance(1.0), 0.0)
        self.assertAlmostEqual(metric.to_distance(1.0000001), 0.0)  # clamps float noise
        # It must also work end to end inside a FAISS index.
        idx = FaissVectorIndex(2, metric)
        a = idx.upsert([1.0, 0.0])
        idx.upsert([0.0, 1.0])
        best = idx.search([1.0, 0.0], 1, 0.1)
        self.assertEqual([m.entry_id for m in best.matches], [a])
        self.assertAlmostEqual(best.matches[0].distance, 0.0, places=3)

    def test_a_broken_plugin_package_fails_loudly(self):
        pkg = self.tmp / "broken_index"
        pkg.mkdir()
        (pkg / "__init__.py").write_text("raise RuntimeError('plugin bug')\n")
        with self.assertRaises(RuntimeError):
            index_module.registered()


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
