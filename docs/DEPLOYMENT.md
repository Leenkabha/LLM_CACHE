# Hosting the LLM Semantic Cache behind a public link

This puts the cache on a cloud VM so visitors only need a browser and a URL.
Only a Caddy reverse proxy is exposed (ports 80/443). The orchestrator,
embedding service, vector store and Redis stay on the private Docker network.

## What you need

- A Linux VM with Docker and the Compose plugin (2 vCPU / 4 GB RAM or more; the
  embedding model needs memory to load).
- A Gemini API key.
- Optional: a domain name pointing at the VM, for automatic HTTPS.
- Inbound ports 80 (and 443 for HTTPS) open in the VM's firewall/security group.

## Deploy

```bash
git clone https://github.com/Leenkabha/LLM_CACHE.git
cd LLM_CACHE
cp production.env.example production.env
# edit production.env: GEMINI_API_KEY, ADMIN_TOKEN (openssl rand -hex 24), SITE_ADDRESS
docker compose -f docker-compose.prod.yml --env-file production.env up --build -d
```

Startup refuses to proceed if `GEMINI_API_KEY` or `ADMIN_TOKEN` is missing. The
first start is slow while the embedding model downloads and loads.

Set `SITE_ADDRESS` to your domain (for example `cache.example.com`) for HTTPS;
leave it as `:80` to serve plain HTTP on the VM's IP.

## Prove it works

Run the verifier against the public URL, from any machine:

```bash
bash scripts/verify_hosted.sh https://cache.example.com --expect-locked
```

It checks that:

1. `/health` reports every dependency ok.
2. The web UI is served at `/`.
3. A unique question returns a non-empty answer that is not the stub placeholder
   (a real Gemini reply) and is a cache miss.
4. Asking the same question again is a cache hit.
5. `POST /flush` and `POST /policy` without the admin token are rejected (401).

It exits non-zero if anything fails. Then open the URL in a browser and ask a
question yourself.

## What protects the public link

| Protection | Setting | Default in prod |
|------------|---------|-----------------|
| `/flush` and `/policy` need `Authorization: Bearer <ADMIN_TOKEN>` | `ADMIN_TOKEN` | required |
| Per-visitor limit on `/query` (protects your Gemini quota) | `RATE_LIMIT_PER_MIN` | 30 |
| No stub fallback: a Gemini failure returns an error, never fake text | `LLM_FALLBACK_MODE` | empty |
| Internal services not reachable from outside | compose networking | only Caddy is published |

Admin example:

```bash
curl -X POST https://cache.example.com/flush -H "Authorization: Bearer $ADMIN_TOKEN"
```

`/stats` and `/health` remain public and reveal only counters and dependency
status, never prompts or replies. Visitors share one cache: a question asked by
one person can be served to another if it is similar enough. Do not use this
setup for private or sensitive prompts.

## Watch your Gemini quota

Every real Gemini answer writes a line to the orchestrator log:

```
gemini usage: model=... prompt_tokens=12 output_tokens=85 thinking_tokens=140 total_tokens=237 | today: requests=7 tokens=1520 | requests_left=13/20 tokens_left=unlimited | resets_in=9h12m0s
```

Gemini does not report remaining quota, so `requests_left` is counted here
against the limit you set: `GEMINI_DAILY_REQUEST_LIMIT` (free tier is about 20
per model per day) and optionally `GEMINI_DAILY_TOKEN_BUDGET`. `LOW` is added
when 20% or less is left. The day resets at midnight Pacific, like Google's
quota. If Google refuses with a daily-quota 429, a `gemini quota EXHAUSTED`
line is logged and the real limit is learned from the error. Counts are in
memory, so they restart from zero when the orchestrator restarts.

```bash
docker compose -f docker-compose.prod.yml --env-file production.env logs orchestrator | grep "gemini usage\|gemini quota"
```

## Operate

```bash
docker compose -f docker-compose.prod.yml --env-file production.env ps
docker compose -f docker-compose.prod.yml --env-file production.env logs -f orchestrator
tail -f logs/orchestrator.log
docker compose -f docker-compose.prod.yml --env-file production.env down   # stop
```

Redis data lives in the `redis_data` volume, so the cache survives restarts.

## Optional: the plugin platform

Everything above is unchanged and the plugin platform is **off**. To let administrators add plugins from the web UI:

1. Set in `production.env`: `ENABLE_PLUGIN_INSTALLATION=true`, and `PLUGIN_SECRET_KEY` (`openssl rand -base64 32`). `ADMIN_TOKEN`
   is already required. Back the key up separately from the Redis volume: without it stored plugin secrets cannot be read.
2. Hosted-endpoint plugins now work: open `https://<your host>/plugins`. Endpoints must be https and public (the production
   Compose file pins `ALLOW_INSECURE_PLUGIN_ENDPOINTS=false`).
3. Image and GitHub-repository plugins additionally need the controller: set `PLUGIN_CONTROLLER_URL=http://plugin-controller:8090`
   and `PLUGIN_CONTROLLER_TOKEN` (at least 16 characters) and start with the `plugins` profile:

   ```bash
   docker compose -f docker-compose.prod.yml --env-file production.env --profile plugins up --build -d
   ```

   **Read [PLUGIN_SECURITY.md](PLUGIN_SECURITY.md#the-docker-socket) first.** The controller needs a Docker daemon, which is
   effectively root on that host. Prefer a dedicated build/plugin host or rootless Docker. Caddy still publishes only ports 80/443;
   the controller and the socket proxy are not published.
4. Enable vulnerability scanning if you can (`PLUGIN_SCANNER=trivy` with a trivy binary in the controller image, and
   `PLUGIN_REQUIRE_SCAN=true`); without a scanner the scan is recorded as `skipped`.

`/admin/*` and `/plugins` sit behind the same Caddy proxy and the same `ADMIN_TOKEN` protection; the admin API additionally
rate-limits failed logins. Operate and troubleshoot with [PLUGIN_OPERATIONS.md](PLUGIN_OPERATIONS.md).
