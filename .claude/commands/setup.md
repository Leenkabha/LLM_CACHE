Read the README.md file in the current directory, then execute the setup steps described in it for a new team member joining the project.

Follow this sequence:

1. Read README.md and understand the full setup flow.

2. Check what is already installed by running version checks for: git, docker, docker compose, node, npm, claude (the claude-code CLI).
   - Report what is installed and what is missing.
   - If something critical is missing (git, docker), stop and tell the user exactly what to install with the command from the README, then wait.

3. Check if the repo is already cloned (i.e. we are already inside it). If yes, skip the clone step.

4. Check if .env exists. If not, copy .env.example to .env and tell the user.

5. Check if Docker is running by running `docker info`. If Docker is not running, tell the user to open Docker Desktop and wait for the engine to start, then stop.

6. Check if the containers are already running with `docker ps`. If all 4 services (orchestrator, embedding, vectorstore, redis) are already up, skip the build step and say so.

7. If containers are not running, run `docker compose up --build -d` to start everything in detached mode. Wait a few seconds, then verify all 4 containers are up with `docker ps`.

8. Run the 3 health checks:
   - curl -s localhost:8080/health
   - curl -s localhost:8001/health
   - curl -s localhost:8002/health
   Report the result of each. If any fails, show the docker logs for that container.

9. Run a test query to confirm the full flow works end-to-end:
   curl -s localhost:8080/query -H "Content-Type: application/json" -d '{"prompt":"What is virtual memory?"}'
   Confirm the response contains "cache_hit": false and "source": "llm".

10. Run the same query a second time and confirm "cache_hit": true and "source": "cache".

11. Print a final summary: what was already set up, what was installed/started, and confirm the project is ready.
