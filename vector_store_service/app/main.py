"""Vector-store service (v1 — FAISS).

=====================================================================
BIG PICTURE — read this before anything else
=====================================================================
This service answers exactly one question: "given a new vector, what's
the closest vector you've already seen?" It does NOT know about the
actual questions/replies (that's Redis/memory-store's job elsewhere).

The full loop this service is one piece of:
  1. Question comes in -> embedder turns it into 384 numbers ("a dot in space")
  2. THIS SERVICE compares that dot against every dot it has stored -> finds nearest
  3. If close enough (within threshold) -> return that entry's id
  4. Caller (orchestrator) uses that id to fetch the actual reply from Redis/memory
  5. If not close enough -> miss -> orchestrator calls the real LLM instead,
     then upserts the new vector (here) + reply (elsewhere) for next time

This file only ever deals in NUMBERS and IDS. It never sees a sentence,
never sees a reply. That separation is intentional.

Distance convention: cosine distance = 1 - cosine similarity, so 0.0 is
identical and a *lower* distance means more similar. A hit requires
distance <= threshold. (FAISS itself gives back SIMILARITY, not distance
-- we flip it ourselves, see search() below.)

Implementation notes:
- FAISS's IndexFlatIP does *inner product*, not cosine distance. Since the
  embedder normalizes vectors to unit length (see embedding_service),
  inner product on normalized vectors IS cosine similarity. We convert to
  our distance convention (1 - similarity) at the boundary so callers never
  see the difference.
- FAISS only supports integer ids internally (IndexIDMap), so we map our
  external uuid strings to sequential int64 ids and keep a dict both ways.
- FAISS has no built-in delete-by-id for a flat index, so deletion is done
  via `remove_ids`, which IndexIDMap supports directly.
"""

import uuid

import faiss
import numpy as np
from fastapi import FastAPI, HTTPException
from pydantic import BaseModel

# DIM = how many numbers are in each vector. Not a meaningful number on its
# own -- it's just a fixed property of the embedding model teammate A uses
# (all-MiniLM-L6-v2 always outputs exactly 384 numbers per sentence).
# Every vector in this whole system MUST have exactly this many numbers.
DIM = 384

app = FastAPI(title="Vector Store Service", version="1.0.0")

# ---------------------------------------------------------------------
# THE INDEX -- two layers stacked on top of each other
# ---------------------------------------------------------------------
# _base_index = the actual search engine. Does the real math. On its own
#   it only knows vectors by POSITION (1st added, 2nd added...) -- no
#   concept of "my own id" at all.
# _index = a wrapper around _base_index that lets us attach OUR OWN
#   integer id to each vector, so it stays labeled correctly even if
#   other vectors get deleted around it. Without this wrapper, deleting
#   one vector would shift everyone else's position, which would break
#   the pairing between "vector store's id" and "Redis's id" for every
#   entry that comes after the deleted one. THIS is the actual reason
#   IndexIDMap exists -- it's what keeps vector store and Redis in sync.
_base_index = faiss.IndexFlatIP(DIM)  # Flat = exact/brute-force search, IP = inner product
_index = faiss.IndexIDMap(_base_index)  # never talk to _base_index directly -- always go through _index

# ---------------------------------------------------------------------
# THE ID PHRASEBOOK -- translates between the outside world and FAISS
# ---------------------------------------------------------------------
# The orchestrator (and Redis) only ever speak in UUID strings, e.g.
# "a3f9c2e1...". FAISS only accepts plain integers as ids. Something has
# to translate between the two -- that's these two dicts, storing the
# exact same pairing in both directions:
#   _uuid_to_int["a3f9c2e1..."] = 0
#   _int_to_uuid[0] = "a3f9c2e1..."
# Used in upsert() (uuid -> int, to tell FAISS what to store under) and
# in search() (int -> uuid, to tell the caller which entry matched).
#
# IMPORTANT: _index stores ONLY (int id, vector) pairs. It never knows
# the uuid, and never knows the reply text. Those live entirely outside
# _index -- the uuid lives in these dicts, the reply text lives in
# Redis/memory-store, a completely separate service.
_next_int_id = 0
_uuid_to_int: dict[str, int] = {}
_int_to_uuid: dict[int, str] = {}


class SearchRequest(BaseModel):
    """Shape FastAPI expects for incoming POST /search bodies.

    threshold: float means "a decimal number" -- the cutoff distance for
    "close enough to count as a match". This value is NOT decided here --
    the orchestrator reads SIMILARITY_THRESHOLD from its own config
    (docker-compose.yml, default 0.25) and sends it on every request.
    This service just compares against whatever number it's given.
    """
    vector: list[float]
    top_k: int = 1  # how many top matches to return; we only ever use 1
    threshold: float


class SearchResponse(BaseModel):
    hit: bool
    id: str = ""
    distance: float = -1.0  # -1.0 = placeholder meaning "not applicable" on a miss


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


def _to_np(vec: list[float]) -> np.ndarray:
    """FAISS refuses plain Python lists -- it insists on numpy arrays.
    This just repackages the same numbers into the format FAISS demands."""
    return np.array([vec], dtype="float32")


# response_model=SearchResponse -> FastAPI double-checks OUR OWN return
# value matches this shape before sending it out, same way it checks
# incoming requests match SearchRequest. Catches bugs like accidentally
# returning the wrong field name before they ever reach the orchestrator.
@app.post("/search", response_model=SearchResponse)
def search(req: SearchRequest) -> SearchResponse:
    # Nothing stored yet -> nothing to compare against -> immediate miss.
    if _index.ntotal == 0:
        return SearchResponse(hit=False)

    query = _to_np(req.vector)

    # THE ACTUAL SEARCH: FAISS compares the query against every stored
    # vector (this is what "Flat" means -- brute force, but done in fast
    # optimized code instead of a Python loop) and returns the closest
    # top_k matches. Because FAISS supports searching multiple query
    # vectors in one call, results always come back as a 2D structure --
    # one row per query. We only ever send ONE query vector, so there's
    # always exactly one row: similarities[0], ids[0].
    similarities, ids = _index.search(query, min(req.top_k, _index.ntotal))

    # top_k defaults to 1, so we only ever want the single best result:
    # similarities[0][0] = "first (only) query's first (only) result".
    best_sim = similarities[0][0]
    best_int_id = ids[0][0]

    # FAISS's own convention for "found genuinely nothing" is id == -1.
    if best_int_id == -1:
        return SearchResponse(hit=False)

    # FAISS gives back SIMILARITY (higher = closer). Our API contract
    # promises DISTANCE (lower = closer). This line is the one place
    # that conversion happens: distance = 1 - similarity. Works because
    # embeddings are normalized, so inner product == cosine similarity.
    distance = 1.0 - float(best_sim)

    # The actual hit/miss decision: is the best match close enough?
    # threshold comes from the orchestrator's config, not decided here.
    if distance <= req.threshold:
        # Translate FAISS's internal int id back into the public uuid
        # before handing it back -- the orchestrator never sees our
        # internal integers, only uuids.
        return SearchResponse(
            hit=True, id=_int_to_uuid[best_int_id], distance=distance
        )
    # FAISS found *something*, but it wasn't close enough to count.
    return SearchResponse(hit=False)


@app.post("/upsert", response_model=UpsertResponse)
def upsert(req: UpsertRequest) -> UpsertResponse:
    # global = "I'm changing the shared variable outside this function,
    # not creating a throwaway local copy." Needed because we reassign
    # _next_int_id below.
    global _next_int_id

    entry_id = uuid.uuid4().hex  # the PUBLIC id -- what the orchestrator will use later
    int_id = _next_int_id        # the PRIVATE id -- what FAISS will use internally
    _next_int_id += 1            # next upsert gets a fresh, never-reused integer

    # Fill in both directions of the phrasebook NOW, before FAISS even
    # sees the vector -- so search() can always translate back correctly
    # afterwards.
    _uuid_to_int[entry_id] = int_id
    _int_to_uuid[int_id] = entry_id

    vec = _to_np(req.vector)
    ids = np.array([int_id], dtype="int64")  # FAISS wants ids as a numpy int64 array too, even just one

    # THE ACTUAL STORAGE MOMENT: from here on, this vector genuinely
    # lives inside _index, in this container's RAM, and could be matched
    # by a future search(). Nothing is written to disk -- if this
    # container restarts, this line's effect is completely gone.
    _index.add_with_ids(vec, ids)

    # Only the public uuid goes back to the caller -- they never need to
    # know our internal integer scheme exists at all.
    return UpsertResponse(id=entry_id)


@app.post("/rebuild", response_model=RebuildResponse)
def rebuild(req: RebuildRequest) -> RebuildResponse:
    """Restore the RAM-only FAISS index from durable Redis cache entries.

    Redis owns the durable cache entries, including their public UUID and
    vector. When this service restarts, FAISS starts empty, so the orchestrator
    calls this endpoint once at startup with everything Redis still has.
    """
    global _index, _base_index, _next_int_id

    _base_index = faiss.IndexFlatIP(DIM)
    _index = faiss.IndexIDMap(_base_index)
    _uuid_to_int.clear()
    _int_to_uuid.clear()
    _next_int_id = 0

    if not req.entries:
        return RebuildResponse(restored=0)

    vectors: list[np.ndarray] = []
    int_ids: list[int] = []

    for entry in req.entries:
        if len(entry.vector) != DIM:
            raise HTTPException(
                status_code=400,
                detail=f"vector for entry {entry.id!r} has dimension {len(entry.vector)}, want {DIM}",
            )
        if entry.id in _uuid_to_int:
            raise HTTPException(
                status_code=400,
                detail=f"duplicate entry id {entry.id!r}",
            )

        int_id = _next_int_id
        _next_int_id += 1
        _uuid_to_int[entry.id] = int_id
        _int_to_uuid[int_id] = entry.id
        vectors.append(np.array(entry.vector, dtype="float32"))
        int_ids.append(int_id)

    matrix = np.array(vectors, dtype="float32")
    ids = np.array(int_ids, dtype="int64")
    _index.add_with_ids(matrix, ids)

    return RebuildResponse(restored=len(req.entries))


@app.delete("/entries/{entry_id}")
def delete(entry_id: str) -> dict:
    # entry_id comes from the URL itself (path parameter), not a JSON
    # body -- standard REST convention for "which specific thing".
    if entry_id not in _uuid_to_int:
        raise HTTPException(status_code=404, detail="not found")

    # .pop() removes AND returns the value in one step. Do this for BOTH
    # directions of the phrasebook -- forgetting one leaves a dangling,
    # stale entry in the other dict forever.
    int_id = _uuid_to_int.pop(entry_id)
    _int_to_uuid.pop(int_id)

    # Only works because we're using IndexIDMap -- a plain flat index
    # has no concept of "remove this one specific vector", only "clear
    # everything". np.array([int_id], dtype="int64") is just repackaging
    # our one number into the exact numpy shape FAISS's C++ code demands
    # -- remove_ids() always expects an array, even for a single id.
    #
    # NOTE: this is exactly the operation eviction calls -- and exactly
    # why the id-pairing between vector store and Redis has to stay perfectly
    # in sync (see IndexIDMap comment above).
    _index.remove_ids(np.array([int_id], dtype="int64"))

    return {"status": "deleted"}


@app.get("/size")
def size() -> dict:
    return {"size": _index.ntotal}


@app.post("/flush")
def flush() -> dict:
    global _index, _base_index, _next_int_id
    # Rather than carefully clearing FAISS's internal state piece by
    # piece, just throw the whole index away and build a fresh empty one
    # -- simpler and safer, no risk of leftover data hiding inside FAISS.
    _base_index = faiss.IndexFlatIP(DIM)
    _index = faiss.IndexIDMap(_base_index)
    _uuid_to_int.clear()
    _int_to_uuid.clear()
    _next_int_id = 0  # ids start fresh from 0 again after a flush
    return {"status": "flushed"}


@app.get("/health")
def health() -> dict:
    return {"status": "ok"}
