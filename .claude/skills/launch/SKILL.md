---
name: launch
description: Start the LLM Semantic Cache and open its web UI in one step. For people who just want to use the app — builds, starts and health-checks every service, then opens the browser. Use when the user asks to run, start, launch, or open the project or its web page.
---

# Launch the LLM Semantic Cache

This is the skill for people who want to *use* the app (contributors onboarding
to the codebase should use `/setup` instead). It runs the launcher script from
the repository root, which does what the README describes in one go:

1. checks Docker and curl are available and Docker is running
2. creates `.env` from `.env.example` if it does not exist
3. runs `docker compose up --build -d`
4. waits until all services are healthy (the embedding model takes a while to load)
5. opens the web UI at http://localhost:8080/

## Steps

1. Run the launcher. The first build can take several minutes, so give the
   command a long timeout (at least 10 minutes):

   ```bash
   bash scripts/launch.sh
   ```

   Options if the user asks for them:
   - `--smoke` also sends the same prompt twice and confirms miss, then hit
   - `--no-open` do not open the browser
   - `--timeout N` wait up to N seconds for health (default 240)

2. If it fails, read the error the script printed and explain it in plain terms:
   - Docker, compose or curl missing: tell the user what to install, pointing at
     the README "Prerequisites" section, then stop.
   - Docker not running: ask them to start Docker Desktop (or the docker
     service), wait for the engine to be ready, then re-run.
   - Health timeout: the script already prints container status and recent
     logs; summarize the likely cause. Do not retry blindly.
   - Port already in use (8080, 8001, 8002, 6379): name the port and let the
     user decide what to stop.

3. On success, tell the user the app is running, give the URL, say whether the
   browser was opened (if the script could not open one, ask them to open the
   URL themselves), and mention `docker compose down` to stop everything.

## Notes

- Never edit or delete an existing `.env`; the script only creates it when absent.
- Do not flush or otherwise change cached data unless asked.
- The default `LLM_MODE=stub` needs no API key and makes no network calls. Real
  providers need keys in `.env` (see docs/USER_GUIDE.md).
