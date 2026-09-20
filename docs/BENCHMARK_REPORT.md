# Benchmark and correctness report

How we checked that the semantic cache works with a real LLM, what we
measured, and what it does and does not prove. Raw evidence for the run
described here is in [`benchmark-evidence/run-02/`](benchmark-evidence/run-02/).

## 1. Question we wanted to answer

1. Does the cache return **hits and misses correctly** (a hit only when the
   question really was answered before, never a wrong reused answer)?
2. Does it **save time and LLM calls** compared with calling the LLM every time?

## 2. Method

- **Real provider:** Gemini (`gemini-3.1-flash-lite`), no stub, no fallback.
- **Two arms, same requests in the same order:**
  - *no-cache baseline:* calls the same Go LLM adapter directly (no embedding,
    vector search, Redis or cache writes).
  - *cache:* the real application (MiniLM embeddings, FAISS, Redis), started
    with an empty cache.
- **Workload** (`scripts/benchmark/workload.json`): 12 short questions over 4
  topics (virtual memory, DNS, LRU eviction, TCP). Per topic: one *new*
  question, one *paraphrase* of it, and one exact *repeat*.
- **Settings:** similarity threshold 0.25 (cosine distance), top-k 1, LRU,
  capacity 1000. Two warm-up calls are excluded from timing.
- **Timing:** client-side elapsed time per request (includes HTTP overhead).
- **Isolation:** a dedicated compose project (`llm-cache-benchmark`, port 18080)
  so the benchmark never touches the demo or hosted stack.
- Run: `python run_benchmark.py --out ... --trials 1 --llm-pause 5 --allow-isolated-flush`.

### The rate-limit problem and the fix

The first run (`run-01`, not kept) failed at request 15 with HTTP 502. The
cause was Gemini's free-tier limit of **15 requests per minute**
(`GenerateRequestsPerMinutePerProjectPerModel-FreeTier`), not tokens. Calls
take about 2 s, so the runner exceeded 15 per minute. We added
`--llm-pause SECONDS` to `run_benchmark.py`: it waits after each real LLM call,
outside the timed section, and defaults to 0. The rerun used 5 s and completed.
Token use was tiny (about 400 tokens in the cache arm) because answers are one
sentence, so requests per minute is the limit that matters on the free tier.

## 3. Results (1 trial, 12 requests per arm)

| | No cache | Cache |
|---|---|---|
| LLM calls | 12 | 7 |
| Hits | 0 | 5 (41.7%) |
| Mean latency | 2,301 ms | 1,398 ms |
| Median latency | 2,203 ms | 1,437 ms |
| p95 latency | 3,584 ms | 3,160 ms |
| Avg hit / miss latency | - | 251 ms / 2,200 ms |

Total response time was 39% lower with the cache (1.65x), and 5 LLM calls were
avoided.

Per-request outcome in the cache arm:

| id | kind | result | distance to match |
|---|---|---|---|
| vm-1, dns-1, lru-1, tcp-1 | new | miss (correct) | - |
| vm-2 | paraphrase | **hit** | 0.023 |
| dns-2 | paraphrase | miss | 0.343 |
| lru-2 | paraphrase | miss | 0.397 |
| tcp-2 | paraphrase | miss | 0.460 |
| vm-3, dns-3, lru-3, tcp-3 | repeat | hit (correct) | about 0 |

The paraphrase distances for the misses were computed separately with the same
embedding model, because the API reports `distance = -1` on a miss.

## 4. Are the hits and misses correct?

Yes, at the configured threshold (0.25):

- All 4 new questions missed and all 4 repeats hit.
- The 3 paraphrase misses are correct behaviour: their distances (0.34-0.46)
  are above the 0.25 threshold.
- **No false hits.** Questions on different topics were at least 0.576 apart,
  well above the threshold. The one paraphrase hit returned the right answer.

Unit tests: `go vet ./...` and `go test ./...` pass for every Go package that
has tests (orchestrator, LLM, embedder, vector store, policy, persistence,
queue, config, CLI, plugin).

## 5. What this does not prove (limits to state out loud)

- One trial of 12 requests on a small authored workload: a demonstration, not
  a statistically solid benchmark or representative traffic.
- The threshold is **conservative**: it never reused a wrong answer but missed
  3 of 4 valid paraphrases. Raising it to about 0.5 would catch them, but the
  closest wrong pair is 0.576, so the margin is thin. Tune it on a separate,
  larger question set (`scripts/fetch_datasets.py`), not on these 12 prompts.
- Eviction was never triggered (0 evictions); it is covered by unit tests only.
- Sequential requests only: no concurrency or throughput measurement.
- Money saved is not estimated; that needs verified token prices.
- Cache hits still cost embedding and search time (about 250 ms here).

## 6. What we changed in the repo

- `scripts/benchmark/run_benchmark.py`: added `--llm-pause`; the value is
  recorded in `run.json` (`llm_pause_s`).
- `scripts/benchmark/README.md`: documents the flag and the rate-limit cause.
- `docs/benchmark-evidence/run-02/`: the raw evidence for this report.
- This report.

## 7. Talking points for the presentation

1. Problem: LLM calls are slow and cost money; many questions repeat.
2. Design: embed the question, search for a close cached one, return it on a
   hit, otherwise call the LLM and store the answer.
3. Test honestly: real provider, same requests with and without the cache.
4. Result: 5 of 12 requests answered from cache, about 250 ms instead of about
   2.2 s, and no wrong reuse.
5. Limits: conservative threshold, tiny workload, single trial, rate limits.
6. Next step: tune the threshold on a larger set and run more trials.
