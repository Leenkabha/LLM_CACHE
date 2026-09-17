import math
import unittest
from unittest.mock import patch

import faiss
import numpy as np
from fastapi.testclient import TestClient

from app.index import FaissVectorIndex
from app.metrics import CosineMetric, EuclideanMetric
from app import main

faiss.omp_set_num_threads(1)


class SearchTests(unittest.TestCase):
    def test_filter_order_limit_both_metrics(self):
        for metric in (CosineMetric(), EuclideanMetric()):
            index = FaissVectorIndex(2, metric)
            if isinstance(metric, CosineMetric):
                rows = [("outside", [0, 1]), ("boundary", [0.75, math.sqrt(1 - 0.75**2)]),
                        ("best", [1, 0]), ("middle", [0.875, math.sqrt(1 - 0.875**2)])]
                query, threshold, expected = [1, 0], 0.25, [0, 0.125, 0.25]
            else:
                rows = [("outside", [2, 0]), ("boundary", [1, 0]),
                        ("best", [0, 0]), ("middle", [0.5, 0])]
                query, threshold, expected = [0, 0], 1, [0, 0.5, 1]
            index.rebuild(rows)
            for k in (1, 2, 3, 10):
                with self.subTest(metric=metric.name, k=k):
                    result = index.search(query, k, threshold)
                    self.assertEqual([m.entry_id for m in result.matches], ["best", "middle", "boundary"][:k])
                    self.assertEqual([m.distance for m in result.matches], expected[:k])
                    self.assertTrue(result.hit)
                    self.assertEqual(result.entry_id, "best")
            self.assertFalse(index.search(query, 10, -1).hit)
            self.assertEqual(len(index.search(query, 10, 0).matches), 1)

    def test_matches_full_scan_reference(self):
        rng = np.random.default_rng(7)
        for metric in (CosineMetric(), EuclideanMetric()):
            vectors = rng.normal(size=(24, 4)).astype("float32")
            query = rng.normal(size=4).astype("float32")
            if isinstance(metric, CosineMetric):
                vectors /= np.linalg.norm(vectors, axis=1, keepdims=True)
                query /= np.linalg.norm(query)
                distances = 1 - vectors @ query
            else:
                distances = np.linalg.norm(vectors - query, axis=1)
            index = FaissVectorIndex(4, metric)
            index.rebuild([(str(i), v.tolist()) for i, v in enumerate(vectors)])
            for threshold in (0.1, 0.7, 1.3, 4):
                expected = sorted((float(d), str(i)) for i, d in enumerate(distances) if d <= threshold)
                for k in (1, 5, 30):
                    with self.subTest(metric=metric.name, threshold=threshold, k=k):
                        result = index.search(query.tolist(), k, threshold)
                        self.assertEqual([m.entry_id for m in result.matches], [i for _, i in expected[:k]])
                        for match, (distance, _) in zip(result.matches, expected[:k]):
                            self.assertAlmostEqual(match.distance, distance, places=5)

    def test_empty_invalid_and_deleted(self):
        index = FaissVectorIndex(2, CosineMetric())
        self.assertEqual(index.search([1, 0], 3, 0.25).matches, [])
        for k in (0, -1):
            with self.assertRaises(ValueError):
                index.search([1, 0], k, 0.25)
        with self.assertRaises(ValueError):
            index.search([1], 3, 0.25)
        first = index.upsert([1, 0])
        second = index.upsert([1, 0])
        tied = index.search([1, 0], 3, 0)
        self.assertEqual({m.entry_id for m in tied.matches}, {first, second})
        self.assertEqual([m.distance for m in tied.matches], [0, 0])
        index.delete(first)
        self.assertEqual([m.entry_id for m in index.search([1, 0], 3, 0).matches], [second])


class EndpointTests(unittest.TestCase):
    def setUp(self):
        self.index = FaissVectorIndex(2, EuclideanMetric())
        self.index.rebuild([("far", [2, 0]), ("boundary", [1, 0]), ("best", [0, 0])])
        self.patch = patch.object(main, "_index", self.index)
        self.patch.start()
        self.addCleanup(self.patch.stop)
        self.client = TestClient(main.app)
        self.addCleanup(self.client.close)

    def test_response_and_default(self):
        response = self.client.post("/search", json={"vector": [0, 0], "top_k": 10, "threshold": 1})
        self.assertEqual(response.status_code, 200)
        self.assertEqual(response.json(), {"hit": True, "id": "best", "distance": 0,
                         "matches": [{"id": "best", "distance": 0}, {"id": "boundary", "distance": 1}]})
        response = self.client.post("/search", json={"vector": [0, 0], "threshold": 1})
        self.assertEqual(len(response.json()["matches"]), 1)

    def test_miss_and_validation(self):
        response = self.client.post("/search", json={"vector": [9, 9], "top_k": 3, "threshold": 0.25})
        self.assertEqual(response.json(), {"hit": False, "id": "", "distance": -1, "matches": []})
        for k in (0, -1, 1.5, "bad"):
            response = self.client.post("/search", json={"vector": [0, 0], "top_k": k, "threshold": 1})
            self.assertEqual(response.status_code, 422)
        response = self.client.post("/search", json={"vector": [0], "top_k": 3, "threshold": 1})
        self.assertEqual(response.status_code, 400)


if __name__ == "__main__":
    unittest.main()
