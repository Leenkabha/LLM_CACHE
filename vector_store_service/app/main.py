"""Vector-store service (v0 skeleton).

Stores embedding vectors and finds the nearest one under a cosine *distance*
threshold. v0 uses a simple in-memory brute-force search; week 6 replaces the
internals with a FAISS index behind this same REST contract.

Distance convention: cosine distance = 1 - cosine similarity, so 0.0 is
identical and a *lower* distance means more similar. A hit requires
distance <= threshold.
"""

import math
import uuid

from fastapi import FastAPI, HTTPException
from pydantic import BaseModel

app = FastAPI(title="Vector Store Service", version="0.1.0")

# id -> vector. Replace with a FAISS index + id map in week 6.
_store: dict[str, list[float]] = {}


class SearchRequest(BaseModel):
    vector: list[float]
    top_k: int = 1
    threshold: float


class SearchResponse(BaseModel):
    hit: bool
    id: str = ""
    distance: float = -1.0


class UpsertRequest(BaseModel):
    vector: list[float]


class UpsertResponse(BaseModel):
    id: str


def cosine_distance(a: list[float], b: list[float]) -> float:
    dot = sum(x * y for x, y in zip(a, b))
    na = math.sqrt(sum(x * x for x in a))
    nb = math.sqrt(sum(y * y for y in b))
    if na == 0 or nb == 0:
        return 1.0
    return 1.0 - dot / (na * nb)


@app.post("/search", response_model=SearchResponse)
def search(req: SearchRequest) -> SearchResponse:
    best_id, best_dist = "", float("inf")
    for entry_id, vec in _store.items():
        d = cosine_distance(req.vector, vec)
        if d < best_dist:
            best_id, best_dist = entry_id, d
    if best_id and best_dist <= req.threshold:
        return SearchResponse(hit=True, id=best_id, distance=best_dist)
    return SearchResponse(hit=False)


@app.post("/upsert", response_model=UpsertResponse)
def upsert(req: UpsertRequest) -> UpsertResponse:
    entry_id = uuid.uuid4().hex
    _store[entry_id] = req.vector
    return UpsertResponse(id=entry_id)


@app.delete("/entries/{entry_id}")
def delete(entry_id: str) -> dict:
    if entry_id not in _store:
        raise HTTPException(status_code=404, detail="not found")
    del _store[entry_id]
    return {"status": "deleted"}


@app.get("/size")
def size() -> dict:
    return {"size": len(_store)}


@app.post("/flush")
def flush() -> dict:
    _store.clear()
    return {"status": "flushed"}


@app.get("/health")
def health() -> dict:
    return {"status": "ok"}
