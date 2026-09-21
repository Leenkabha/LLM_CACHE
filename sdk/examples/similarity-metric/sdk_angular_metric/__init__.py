"""A minimal SimilarityMetric plugin (angular distance) for the LLM Cache runner.

The metric runs INSIDE the vector-store runner: the FAISS index computes raw
scores in native code and calls ``to_distance`` once per candidate, in-process.
Nothing crosses the network per score.

Contract (see docs/PLUGIN_CONTRACTS.md):
  * ``faiss_metric_type`` - the FAISS score the index should compute. Only
    ``faiss.METRIC_INNER_PRODUCT`` and ``faiss.METRIC_L2`` are supported.
  * ``to_distance``       - convert that raw score to a distance where LOWER IS
    MORE SIMILAR; a cache hit needs distance <= threshold. Identical vectors
    should map to the smallest distance.

Angular distance = arccos(cosine similarity) / pi, in [0, 1]. On unit vectors
the inner product IS the cosine similarity. Because its scale differs from the
default cosine distance (1 - similarity), the administrator must confirm a
similarity threshold when activating this metric.
"""

import math

import faiss

from app.metrics import SimilarityMetric, register


class AngularMetric(SimilarityMetric):
    name = "sdk-angular"
    faiss_metric_type = faiss.METRIC_INNER_PRODUCT

    def to_distance(self, faiss_score: float) -> float:
        clamped = max(-1.0, min(1.0, float(faiss_score)))
        return math.acos(clamped) / math.pi


@register("sdk-angular")  # must equal spec.python.backend in plugin.yaml
def _build() -> SimilarityMetric:
    return AngularMetric()
