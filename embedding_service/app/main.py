"""Embedding service (v1 — real model).

=====================================================================
BIG PICTURE — read this before anything else
=====================================================================
This service does exactly one job: turn a sentence into 384 numbers that
represent its MEANING. It's step 1 of the full cache loop:

  1. THIS SERVICE turns the question into 384 numbers ("a dot in space")
  2. vector store compares that dot against every dot it has stored -> finds nearest
  3. if close enough (within threshold) -> return that entry's id
  4. caller (orchestrator) uses that id to fetch the actual reply from Redis/memory
  5. if not close enough -> miss -> orchestrator calls the real LLM instead,
     then upserts the new vector (via this service) + reply for next time

This file never sees the vector store, never sees Redis, never sees the
LLM. It only ever does one translation: text in -> 384 numbers out.

Two sentences that MEAN similar things should produce two lists of 384
numbers that are close together (mathematically). Two sentences with very
different meanings should produce numbers that are far apart. That
closeness/farness is exactly what the vector store measures later -- this
service's only responsibility is producing numbers where that property
actually holds true, which the old hash-based stub could never do (it only
matched on shared literal words, not meaning).
"""

from fastapi import FastAPI
from pydantic import BaseModel
from sentence_transformers import SentenceTransformer

# DIM = how many numbers come out per sentence. Not a number we chose --
# it's a fixed property of the specific model below. A different model
# (e.g. one outputting 768 numbers) would need DIM changed to match.
# This MUST equal the vector store's DIM (see vector_store_service/app/main.py)
# -- both sides have to agree on vector length or nothing will work.
DIM = 384
MODEL_NAME = "all-MiniLM-L6-v2"

app = FastAPI(title="Embedding Service", version="1.0.0")

# Loaded ONCE, when the container starts up -- not on every request. Model
# loading is relatively slow (reading weights into memory), so doing it
# once at startup and reusing `_model` for every request is what makes
# each individual /embed call fast.
_model = SentenceTransformer(MODEL_NAME)


class EmbedRequest(BaseModel):
    text: str


class EmbedResponse(BaseModel):
    vector: list[float]
    dim: int


def embed_text(text: str) -> list[float]:
    """The actual text -> numbers conversion.

    normalize_embeddings=True scales the output so its length is exactly 1
    (a "unit vector"). This matters downstream: the vector store's search
    assumes inner product == cosine similarity, which is ONLY true when
    vectors are normalized like this. If this flag were off, the vector
    store's distance math would silently be wrong.
    """
    vec = _model.encode(text, normalize_embeddings=True)
    return vec.tolist()  # convert from the model's native array format to a plain Python list


@app.post("/embed", response_model=EmbedResponse)
def embed(req: EmbedRequest) -> EmbedResponse:
    # This is the ONLY endpoint that matters for the actual cache loop --
    # the orchestrator calls this first, on every single query, before it
    # ever talks to the vector store.
    return EmbedResponse(vector=embed_text(req.text), dim=DIM)


@app.get("/model-info")
def model_info() -> dict:
    # Not used in the core query loop -- just a way to ask "what model are
    # you actually running right now", useful for debugging/verification.
    return {"name": MODEL_NAME, "dim": DIM}


@app.get("/health")
def health() -> dict:
    return {"status": "ok"}