"""Auto-discovery of drop-in plugin modules for the embedding service.

Any module placed in app/plugins/ is imported once at startup. A third-party
adapter that calls register(...) at import time therefore becomes available
without editing ANY existing file -- just drop a .py file into app/plugins/.
"""

from __future__ import annotations

import importlib
import pkgutil

_loaded = False


def load_plugins() -> None:
    """Import every module under app/plugins/ (idempotent)."""
    global _loaded
    if _loaded:
        return
    _loaded = True

    try:
        from . import plugins
    except ImportError:
        return  # no plugins package present -> nothing to load

    for module in pkgutil.iter_modules(plugins.__path__, plugins.__name__ + "."):
        importlib.import_module(module.name)
