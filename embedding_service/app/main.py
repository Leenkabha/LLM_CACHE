"""Embedding service (v0 skeleton).

Exposes the REST contract the orchestrator depends on. The v0 implementation
returns a *deterministic, hash-based* pseudo-embedding so the whole system runs
with no model download or GPU. Week 5 swaps `embed_text` for a real
sentence-transformers model (all-MiniLM-L6-v2) without changing the API.
"""

import hashlib
import math

from fastapi import FastAPI
from pydantic import BaseModel

DIM = 384  # matches all-MiniLM-L6-v2 so the real model is a drop-in later
MODEL_NAME = "stub-hash-embedder"

app = FastAPI(title="Embedding Service", version="0.1.0")


class EmbedRequest(BaseModel):
    text: str


class EmbedResponse(BaseModel):
    vector: list[float]
    dim: int


def embed_text(text: str) -> list[float]:
    """Deterministic placeholder embedding.

    Tokens are hashed into buckets so that texts sharing words land near each
    other under cosine distance. This is NOT semantic, only enough structure to
    exercise the cache end-to-end. Replace with sentence-transformers.
    """
    vec = [0.0] * DIM
    for token in text.lower().split():
        h = int(hashlib.md5(token.encode()).hexdigest(), 16)
        vec[h % DIM] += 1.0
    norm = math.sqrt(sum(v * v for v in vec))
    if norm > 0:
        vec = [v / norm for v in vec]
    return vec


@app.post("/embed", response_model=EmbedResponse)
def embed(req: EmbedRequest) -> EmbedResponse:
    return EmbedResponse(vector=embed_text(req.text), dim=DIM)


@app.get("/model-info")
def model_info() -> dict:
    return {"name": MODEL_NAME, "dim": DIM}


@app.get("/health")
def health() -> dict:
    return {"status": "ok"}
