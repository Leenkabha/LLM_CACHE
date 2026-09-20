# Real-provider cache benchmark

Prepared tooling, **not benchmark results**. No measured savings exist until a real run completes. Do not put example numbers or simulated screenshots in the presentation.

## Method

- Baseline invokes the same Go LLM adapter as the project, without embedding, vector search, Redis or cache writes.
- Cache arm runs the actual application with MiniLM embeddings, FAISS and Redis. Both arms share provider, model and retry code. Fallback is disabled.
- Default workload: 12 authored requests over 4 topics (new questions, paraphrases, exact repeats). This is a demonstration workload, not downloaded instructor traces or representative production traffic.
- Two trials, alternating which arm runs first. Each cache trial starts empty.
- Two warm-up LLM calls are excluded from timed samples but count toward paid usage.
- Client elapsed time includes HTTP and response decoding. Report mean, median, p95, sum of response times, hit rate and successful logical LLM completions. Counts do not measure provider retries or token billing.
- Background-write settlement waits are recorded separately and excluded from response latency. This is a controlled sequential test, not a throughput benchmark.
- Review returned answers and matched IDs for incorrect reuse before claiming useful improvement.
- Do not infer money saved from hit rate; token usage and verified prices would be required.

## Run

1. Start Docker Desktop and ensure the Linux engine is running.
2. Change to `scripts/benchmark`. The Compose file builds this checkout into a dedicated `llm-cache-benchmark` project, separate from the normal application.
3. Set `LLM_MODE`, the corresponding API key and an available model in your terminal environment. Never place keys in evidence or chat.
4. From this directory: `docker compose -f compose.yml up --build -d`.
5. Wait until `http://127.0.0.1:18080/health` reports healthy dependencies.
6. Run `python run_benchmark.py --out ../../data/benchmark-results/run-01 --allow-isolated-flush`.
7. Preserve `requests.jsonl`, `requests.csv`, `run.json`, `evidence.html` and `compose.yml`. The run records the Git revision when Git is available. Review any saved container logs for credentials before sharing.
8. Screenshot the actual UI or evidence view. Label measured output as such, not as application UI. Plot only complete recorded runs and identify workload size and run ID.
9. Install plotting dependencies with `python -m pip install -r requirements.txt`, then run `python plot_results.py ../../data/benchmark-results/run-01`. The plotter refuses incomplete runs.

Free-tier providers enforce a requests-per-minute limit (Gemini `gemini-3.1-flash-lite`: 15/min); a run without pacing failed with HTTP 502 for this reason. Add `--llm-pause 5` to wait 5 seconds after each real LLM call. The pause is outside the timed section and recorded as `llm_pause_s` in `run.json`. See `docs/BENCHMARK_REPORT.md` for the measured run.

The runner flushes the dedicated benchmark cache at port 18080. Never point it at shared or production services.

The baseline lives in `cmd/benchmark-baseline` and uses the existing `llm.Backend` registry. The runner currently accepts the built-in real providers `gemini` and `openai`. A custom provider requires an explicit evidence-classification change in the runner; changing it does not require changing the cache implementation. Neither fallback nor artificial delays are enabled for real-provider measurements.

The latest `/stats` fields (average hit/miss latency, best-match distance and evictions) are saved after each cache trial. Graphs use client elapsed time, which also includes enqueueing and HTTP overhead, rather than treating the server's shorter timing interval as end-to-end latency.

The bundled workload stays below capacity. A larger workload that triggers eviction needs a different cache-settlement check: the current runner waits for size to grow after a miss, and will stop rather than publish misleading results if it cannot confirm the write.

Run `python selftest.py` for metric calculation checks; these use synthetic unit-test values, never performance evidence. `check_gemini.ps1` reports only whether a key exists in the calling environment or an orchestrator container. It never prints the key.

## Instructor traces

Provide a JSON file with `description` and a `queries` array; each query needs unique `id`, `prompt`, `kind` and `group`. Preserve source dataset IDs and sampling method. Pass `--workload FILE`. Avoid tuning thresholds on the reported evaluation queries. The instructor downloader retains question text and embeddings, not reference answers.

## Presentation

After measurement, add method, latency/completion graphs and a genuine run screenshot. Keep the talk within 20 minutes by shortening existing sections. Record limitations and answer-quality review in notes. Report the observed result, including slowdowns: cache misses add overhead.
