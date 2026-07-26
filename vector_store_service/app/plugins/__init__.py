"""Drop-in plugin package for the vector-store service.

Place a .py file here that registers a vector-index backend and/or a similarity
metric using the `register(...)` decorators from app.index / app.metrics, e.g.:

    from app.index import VectorIndex, register as register_index
    from app.metrics import SimilarityMetric, register as register_metric

    @register_index("my-db")
    def _build_index(dim, metric) -> VectorIndex:
        return MyDBIndex(dim, metric)

Then set VECTOR_INDEX_BACKEND=my-db (and/or SIMILARITY_METRIC=...). This file is
auto-imported at startup, so no existing file needs to change.
"""
