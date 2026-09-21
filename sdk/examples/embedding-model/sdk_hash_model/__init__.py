"""A minimal, deterministic EmbeddingModel plugin for the LLM Cache runner.

The runner image contains the platform's embedding service; this package is
copied into its ``app/plugins/`` directory, imported at startup, and its
``@register`` call makes the model selectable. Replace ``embed`` with a call to
your real model.

Contract (see docs/PLUGIN_CONTRACTS.md):
  * ``name``  - a stable identifier; two runs with the same name and dim are
                assumed to produce the same vector space
  * ``dim``   - the number of components in every vector, constant for the process
  * ``embed`` - return exactly ``dim`` finite floats, normalised to unit length
"""

import hashlib
import math
import os
import re

from app.models import EmbeddingModel, register  # provided by the runner image

_TOKEN = re.compile(r"[a-z0-9]+")


class HashModel(EmbeddingModel):
    """Hashes word tokens into ``dim`` buckets. A demo, not a semantic model."""

    def __init__(self, dim: int) -> None:
        if dim < 8:
            raise ValueError("CONFIG_DIM must be at least 8")
        self._dim = dim

    @property
    def name(self) -> str:
        return f"sdk-hash-d{self._dim}"

    @property
    def dim(self) -> int:
        return self._dim

    def embed(self, text: str) -> list[float]:
        vec = [0.0] * self._dim
        tokens = _TOKEN.findall(text.lower()) or ["<empty>"]
        for token in tokens:
            digest = hashlib.sha256(token.encode()).digest()
            bucket = int.from_bytes(digest[:4], "big") % self._dim
            sign = 1.0 if digest[4] & 1 else -1.0
            vec[bucket] += sign
        norm = math.sqrt(sum(x * x for x in vec))
        if norm == 0.0:
            vec[0], norm = 1.0, 1.0
        return [x / norm for x in vec]


@register("sdk-hash")  # must equal spec.python.backend in plugin.yaml
def _build() -> EmbeddingModel:
    return HashModel(int(os.getenv("CONFIG_DIM", "384")))
