"""Run Go tests against a temporary real Python vector store (no Redis/LLM needed).

Install vector_store_service/requirements-test.txt and run:
    python scripts/test_topk_integration.py --go /path/to/go
"""
import argparse
import os
from pathlib import Path
import socket
import subprocess
import sys
import threading
import time


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--go", default="go")
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    os.environ["VECTOR_DIM"] = "2"
    os.environ["VECTOR_INDEX_BACKEND"] = "faiss"
    os.environ["SIMILARITY_METRIC"] = "cosine"
    sys.path.insert(0, str(root / "vector_store_service"))
    import faiss
    import uvicorn
    from app.main import app

    faiss.omp_set_num_threads(1)
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
        server = uvicorn.Server(uvicorn.Config(app, log_level="error"))
        thread = threading.Thread(target=server.run, kwargs={"sockets": [sock]}, daemon=True)
        thread.start()
        try:
            deadline = time.monotonic() + 10
            while not server.started:
                if not thread.is_alive() or time.monotonic() >= deadline:
                    raise RuntimeError("test vector store did not start")
                time.sleep(0.05)
            env = dict(os.environ, TOPK_TEST_VECTORSTORE_URL=f"http://127.0.0.1:{port}")
            return subprocess.run([args.go, "test", "./...", "-count=1"], cwd=root, env=env).returncode
        finally:
            server.should_exit = True
            thread.join(timeout=10)


if __name__ == "__main__":
    sys.exit(main())
