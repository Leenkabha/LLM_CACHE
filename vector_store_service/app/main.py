"""Vector-store service (v1 — FAISS).

=====================================================================
BIG PICTURE — read this before anything else
=====================================================================
This service answers exactly one question: "given a new vector, what's the
closest vector you've already seen?" It does NOT know about the actual
questions/replies (that's the persistence store's job elsewhere).

The full loop this service is one piece of:
  1. Question comes in -> embedder turns it into numbers ("a dot in space")
  2. THIS SERVICE compares that dot against every dot it has stored -> finds nearest
  3. If close enough (within threshold) -> return that entry's id
  4. Caller (orchestrator) uses that id to fetch the actual reply from Redis/memory
  5. If not close enough -> miss -> orchestrator calls the real LLM instead,
     then upserts the new vector (here) + reply (elsewhere) for next time

This file is now a thin HTTP layer. The two pluggable decisions --
  * WHICH search engine stores the vectors (VectorIndex, see index.py), and
  * HOW similarity is measured (SimilarityMetric, see metrics.py)
are resolved once at startup and used only through their interfaces, so
swapping either one (VECTOR_INDEX_BACKEND / SIMILARITY_METRIC) never touches the
request handlers below.

Distance convention: lower distance == more similar, and a hit requires
distance <= threshold. The exact formula belongs to the active metric.
"""

import os

from fastapi import FastAPI, HTTPException
from pydantic import BaseModel

from .index import VectorIndex, create_from_env as create_index
from .metrics import SimilarityMetric, create_from_env as create_metric

# DIM = required length of every vector. It is NOT chosen here -- it must equal
# the embedding model's output size (all-MiniLM-L6-v2 -> 384). It is read from
# the environment so a different embedding model just needs a matching value.
DIM = int(os.getenv("VECTOR_DIM", "384"))

app = FastAPI(title="Vector Store Service", version="1.0.0")

# Resolve the pluggable seams ONCE at startup. Everything below talks to the
# index purely through the VectorIndex interface.
_metric: SimilarityMetric = create_metric()
_index: VectorIndex = create_index(DIM, _metric)


class SearchRequest(BaseModel):
    """Shape FastAPI expects for incoming POST /search bodies.

    threshold is NOT decided here -- the orchestrator reads SIMILARITY_THRESHOLD
    from its own config and sends it on every request. This service just
    compares the metric's distance against whatever number it's given.
    """

    vector: list[float]
    top_k: int = 1
    threshold: float


class SearchResponse(BaseModel):
    hit: bool
    id: str = ""
    distance: float = -1.0  # -1.0 = "not applicable" placeholder on a miss


class UpsertRequest(BaseModel):
    vector: list[float]


class UpsertResponse(BaseModel):
    id: str


class RebuildEntry(BaseModel):
    id: str
    vector: list[float]


class RebuildRequest(BaseModel):
    entries: list[RebuildEntry]


class RebuildResponse(BaseModel):
    restored: int


@app.post("/search", response_model=SearchResponse)
def search(req: SearchRequest) -> SearchResponse:
    try:
        result = _index.search(req.vector, req.top_k, req.threshold)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc))
    return SearchResponse(hit=result.hit, id=result.entry_id, distance=result.distance)


@app.post("/upsert", response_model=UpsertResponse)
def upsert(req: UpsertRequest) -> UpsertResponse:
    try:
        entry_id = _index.upsert(req.vector)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc))
    return UpsertResponse(id=entry_id)


@app.post("/rebuild", response_model=RebuildResponse)
def rebuild(req: RebuildRequest) -> RebuildResponse:
    """Restore the RAM-only index from durable Redis cache entries.

    Redis owns the durable cache entries (public UUID + vector). When this
    service restarts, the index starts empty, so the orchestrator calls this
    once at startup with everything Redis still has.
    """
    try:
        restored = _index.rebuild([(entry.id, entry.vector) for entry in req.entries])
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc))
    return RebuildResponse(restored=restored)


@app.delete("/entries/{entry_id}")
def delete(entry_id: str) -> dict:
    if not _index.delete(entry_id):
        raise HTTPException(status_code=404, detail="not found")
    return {"status": "deleted"}


@app.get("/size")
def size() -> dict:
    return {"size": _index.size()}


@app.post("/flush")
def flush() -> dict:
    _index.flush()
    return {"status": "flushed"}


@app.get("/health")
def health() -> dict:
    return {"status": "ok"}
