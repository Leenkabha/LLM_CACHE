"""Drop-in plugin package for the embedding service.

Place a .py file here that registers an embedding-model backend using the
`register(...)` decorator from app.models, e.g.:

    from app.models import EmbeddingModel, register

    class MyModel(EmbeddingModel):
        ...  # implement name, dim, embed

    @register("my-model")
    def _build() -> EmbeddingModel:
        return MyModel(...)

Then set EMBEDDING_MODEL_BACKEND=my-model. This file is auto-imported at
startup, so no existing file needs to change.
"""
