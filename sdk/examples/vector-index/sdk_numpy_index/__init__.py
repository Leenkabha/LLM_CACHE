"""A minimal VectorIndex plugin (exact search with numpy) for the LLM Cache runner.

The runner image contains the platform's vector-store service; this package is
copied into its ``app/plugins/`` directory and its ``@register`` call makes the
index selectable. Replace the numpy scan with your own engine.

Contract (see docs/PLUGIN_CONTRACTS.md):
  * ``search`` returns up to ``top_k`` matches with distance <= threshold,
    best (lowest distance) first
  * ``upsert`` stores a vector and returns a new, stable, URL-safe id
  * ``delete`` returns False for an unknown id
  * ``rebuild`` REPLACES the contents with the given (id, vector) pairs
  * the injected metric turns a raw score into a distance (lower = more similar)
"""

import uuid

import faiss
import numpy as np

from app.index import SearchMatch, SearchResult, VectorIndex, register
from app.metrics import SimilarityMetric


class NumpyIndex(VectorIndex):
    def __init__(self, dim: int, metric: SimilarityMetric) -> None:
        self._dim = dim
        self._metric = metric
        self._ids: list[str] = []
        self._vecs = np.zeros((0, dim), dtype=np.float32)

    @property
    def dim(self) -> int:
        return self._dim

    def _check(self, vector, context: str = "") -> None:
        if len(vector) != self._dim:
            raise ValueError(f"vector{context} has dimension {len(vector)}, want {self._dim}")

    def _scores(self, query: np.ndarray) -> np.ndarray:
        if self._metric.faiss_metric_type == faiss.METRIC_INNER_PRODUCT:
            return self._vecs @ query
        diff = self._vecs - query
        return np.einsum("ij,ij->i", diff, diff)  # squared L2, like FAISS

    def search(self, vector, top_k, threshold):
        if top_k < 1:
            raise ValueError("top_k must be at least 1")
        self._check(vector)
        if not self._ids:
            return SearchResult()
        scores = self._scores(np.asarray(vector, dtype=np.float32))
        matches = []
        for i, score in enumerate(scores):
            distance = self._metric.to_distance(float(score))
            if distance <= threshold:
                matches.append(SearchMatch(self._ids[i], distance))
        matches.sort(key=lambda m: (m.distance, m.entry_id))
        return SearchResult(matches[:top_k])

    def upsert(self, vector) -> str:
        self._check(vector)
        entry_id = uuid.uuid4().hex
        self._ids.append(entry_id)
        self._vecs = np.vstack([self._vecs, np.asarray(vector, dtype=np.float32)])
        return entry_id

    def delete(self, entry_id: str) -> bool:
        if entry_id not in self._ids:
            return False
        i = self._ids.index(entry_id)
        del self._ids[i]
        self._vecs = np.delete(self._vecs, i, axis=0)
        return True

    def rebuild(self, entries) -> int:
        ids, vecs = [], []
        seen = set()
        for entry_id, vector in entries:
            self._check(vector, f" for entry {entry_id!r}")
            if entry_id in seen:
                raise ValueError(f"duplicate entry id {entry_id!r}")
            seen.add(entry_id)
            ids.append(entry_id)
            vecs.append(np.asarray(vector, dtype=np.float32))
        self._ids = ids
        self._vecs = np.vstack(vecs) if vecs else np.zeros((0, self._dim), dtype=np.float32)
        return len(ids)

    def size(self) -> int:
        return len(self._ids)

    def flush(self) -> None:
        self._ids = []
        self._vecs = np.zeros((0, self._dim), dtype=np.float32)


@register("sdk-numpy")  # must equal spec.python.backend in plugin.yaml
def _build(dim: int, metric: SimilarityMetric) -> VectorIndex:
    return NumpyIndex(dim, metric)
