# Test image for the Python services and plugin runners: the runtime
# dependencies of both services plus the test-only ones, without the heavy
# sentence-transformers model (its tests are opt-in).
#   docker build -t llmcache-pytest -f deploy/python-test.Dockerfile .
FROM python:3.11-slim
RUN pip install --no-cache-dir \
    fastapi==0.115.0 "uvicorn[standard]==0.30.6" pydantic==2.9.2 \
    faiss-cpu==1.8.0 numpy==1.26.4 httpx==0.27.2 packaging PyYAML
WORKDIR /repo
