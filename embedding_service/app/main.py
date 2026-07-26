"""Embedding service (v1 — real model).

=====================================================================
BIG PICTURE — read this before anything else
=====================================================================
This service does exactly one job: turn a sentence into a list of numbers that
represent its MEANING. It's step 1 of the full cache loop:

  1. THIS SERVICE turns the question into numbers ("a dot in space")
  2. vector store compares that dot against every dot it has stored -> finds nearest
  3. if close enough (within threshold) -> return that entry's id
  4. caller (orchestrator) uses that id to fetch the actual reply from Redis/memory
  5. if not close enough -> miss -> orchestrator calls the real LLM instead,
     then upserts the new vector (via this service) + reply for next time

This file never sees the vector store, never sees Redis, never sees the LLM. It
only ever does one translation: text in -> numbers out.

WHICH model does that translation is a pluggable detail. This file depends only
on the EmbeddingModel contract (see models.py); the concrete model is chosen at
startup by create_from_env(). That is why nothing here hardcodes a model name
or a vector dimension anymore -- both come from whichever model is loaded
(selected via EMBEDDING_MODEL_BACKEND / EMBEDDING_MODEL).
"""

from fastapi import FastAPI
from pydantic import BaseModel

from .models import EmbeddingModel, create_from_env

app = FastAPI(title="Embedding Service", version="1.0.0")

# The pluggable seam is resolved ONCE at startup. The rest of this file talks to
# `_model` purely through the EmbeddingModel interface, so swapping the backend
# (EMBEDDING_BACKEND / EMBEDDING_MODEL) never touches the code below.
_model: EmbeddingModel = create_from_env()


class EmbedRequest(BaseModel):
    text: str


class EmbedResponse(BaseModel):
    vector: list[float]
    dim: int


@app.post("/embed", response_model=EmbedResponse)
def embed(req: EmbedRequest) -> EmbedResponse:
    # The ONLY endpoint that matters for the cache loop -- the orchestrator
    # calls this first, on every query, before it ever talks to the vector store.
    return EmbedResponse(vector=_model.embed(req.text), dim=_model.dim)


@app.get("/model-info")
def model_info() -> dict:
    # Not used in the core query loop -- just a way to ask "what model are you
    # actually running right now", useful for debugging/verification.
    return {"name": _model.name, "dim": _model.dim}


@app.get("/health")
def health() -> dict:
    return {"status": "ok"}
