# Plugin platform security

This page states what the platform protects against, how, and - just as important - what
it does **not**. Read it before enabling `ENABLE_PLUGIN_INSTALLATION` anywhere that is not your laptop.

## Threat model

| Asset | Threat | Mitigation |
| --- | --- | --- |
| The orchestrator, embedding and vector-store processes | plugin code running inside them | plugin code runs **only** in separate processes/containers; no Go `plugin` package, no code loaded into a running service, no shell command built from a request |
| Plugin secrets | disclosure through storage, API, logs, image layers, build | AES-256-GCM at rest, write-only, scrubbed logs, runtime-only injection, no build arguments |
| The internal network / cloud metadata | SSRF through a hosted endpoint URL | https-only, public-address-only, dial-time address check, no redirects, metadata always blocked |
| The host | container escape or privilege | non-root, read-only rootfs, no capabilities, no privilege escalation, limits, private network, hardened flags that cannot be widened |
| Availability | a slow, huge or hung plugin | timeouts, size limits, process/CPU/memory limits, bounded queues, no result is trusted unvalidated |
| The cache's integrity | incompatible vectors, half-migrated state | explicit confirmations, write pause, verify-before-switch, atomic switch, automatic previous-plugin retention |
| The management interface | unauthenticated or brute-forced admin | mandatory `ADMIN_TOKEN`, constant-time compare, failed-auth rate limit, strict JSON, size limits, disabled by default |

## Authentication and disabling

* Every `/admin/...` route requires `Authorization: Bearer <ADMIN_TOKEN>`. If `ADMIN_TOKEN` is unset or
  empty - or `ENABLE_PLUGIN_INSTALLATION` is not `true` - the API answers `503` and reveals nothing. Startup
  **fails** if installation is enabled without an admin token or a valid `PLUGIN_SECRET_KEY`.
* Failed logins are limited to 10 per minute per address (`429` afterwards).
* Requests are strict JSON (unknown fields and trailing data are `400`), limited to 256 KiB.
* The web page keeps the token **only in memory** (never `localStorage`, `sessionStorage`, cookies or URLs);
  reloading signs you out. All API data is rendered with `textContent`/DOM nodes (no `innerHTML`), the
  page's CSP allows exactly its one inline script by hash, and secret inputs are write-only and cleared after use.

## Secrets

* `PLUGIN_SECRET_KEY` = base64 of 32 random bytes (`openssl rand -base64 32`). Each secret is sealed with
  AES-256-GCM using a fresh random nonce and bound to its plugin id and name (a blob cannot be swapped between
  records). Ciphertext, nonce and key id are stored **only** in the registry and are never returned by any API.
* Losing or changing the key makes stored secrets unreadable: the error says so
  (*"encrypted with a different PLUGIN_SECRET_KEY"*), affected plugins fail to restore, and the secrets must be
  re-entered. Back the key up separately from the Redis data.
* Secrets are decrypted only when a plugin instance starts (or a hosted endpoint is called), held in memory for
  that call, and injected as environment variables through a `0600` env file removed immediately (never on the
  Docker client's command line). They are **never** passed to a build, baked into an image, or written to a log.
  Every log line, error message and audit detail is scrubbed of known secret values and credential-looking text
  (bearer tokens, `key=value`, URL credentials, common API-key shapes).
* Manifests cannot contain secret values; credential-looking strings are rejected.
* A running container's environment is visible to anyone who can `docker inspect` it - i.e. to the Docker
  administrator. Plaintext secrets exist there for as long as the container runs.

## Hosted endpoints (SSRF)

By default an endpoint URL must be `https`, contain no credentials, query or fragment, and resolve to a public
address. The **address actually dialled** is checked after DNS resolution (so a name that resolves, or is rebound,
to a private address fails), redirects are never followed, proxy environment variables are ignored, and responses
are read through a size limit. Loopback, private (RFC 1918, ULA), link-local, CGNAT, multicast and unspecified
ranges are refused, and cloud-metadata addresses/names (`169.254.169.254`, `metadata.google.internal`, ...) are
refused **even in development mode**.

`ALLOW_INSECURE_PLUGIN_ENDPOINTS=true` additionally allows http, loopback, private addresses and Compose service
names. It is for local development only; the production Compose file pins it to `false`.

## Isolation of plugin containers

Started by the controller with these settings, all fixed in code (`controller.RunArgs`); a manifest or API
request cannot add a privileged flag, a mount, a device, a capability or a command:

`--read-only`, `--tmpfs /tmp` (size-limited, `noexec,nosuid,nodev`), `--user 10001:10001`, `--cap-drop ALL`,
`--security-opt no-new-privileges`, `--pids-limit`, `--memory` (= swap limit), `--cpus`, `--init`, bounded
restarts, bounded logs, a private per-plugin network, the image referenced by **digest**, and secrets from an env
file. Native math libraries are told to use no more threads than the CPU limit (FAISS otherwise exhausts the
process limit).

* **Network**: each plugin gets its own network. When the controller runs in a container (production) that
  network is `--internal`, so the plugin has **no route out**, and the orchestrator never joins it: it reaches the
  plugin only through the controller's authenticated proxy, which allows only `/v1/...` and `/health`, strips the
  orchestrator's credential, adds the plugin's own per-instance token, and caps request and response sizes.
  Internet egress exists only if the operator sets `PLUGIN_ALLOW_EGRESS=true` *and* the manifest asks for it.
  There is no per-host allow-list.
* **Publish mode** (`PLUGIN_CONTROLLER_PUBLISH=true`, controller on the host) publishes each plugin on
  `127.0.0.1` and does **not** restrict egress. It exists for local development and tests.
* Images are size-limited (`PLUGIN_MAX_IMAGE_BYTES`, default 2 GiB). Sources are size- and file-count-limited
  (`PLUGIN_MAX_SOURCE_BYTES`, default 100 MiB), symlinks are refused, and paths in the manifest are resolved
  without following links. Repositories are fetched as **one revision, shallow, without submodules or history, with hooks
  disabled and symlinks checked out as plain files**; only `https://github.com/<owner>/<repo>` is accepted
  (local paths only when the controller is told to for development).
* **Digest pinning**: a pulled image is recorded and started by its registry digest; a locally built image by its
  ID. Later pushes to a tag cannot change what runs.
* **Provenance** (repository builds): source URL, exact commit, a hash over every source file, the manifest hash,
  builder, time, size, stored on the record and shown in the UI.
* **Scanning**: `PLUGIN_SCANNER=trivy` scans every image and fails the install at the configured severities.
  Without a scanner the report says `skipped` (never `passed`), and `PLUGIN_REQUIRE_SCAN=true` blocks the install.
  The controller image does not bundle trivy.

## The Docker socket

**Access to a raw Docker socket is effectively root on that host.** Anyone who can create a container can mount
the host filesystem or run a privileged container. Therefore:

* The orchestrator and the public web service **never** receive the socket or `DOCKER_HOST`.
* Image and repository installation is **disabled by default**; it needs `PLUGIN_CONTROLLER_URL`, a running
  controller, and an operator decision to enable the `plugins` Compose profile.
* Only the controller reaches the daemon, and only through `tecnativa/docker-socket-proxy`, which allows
  containers, images, networks and build and denies exec, volumes, secrets, swarm, services, plugins and
  system. **This narrows the API surface but does not remove the risk**: container creation remains powerful, so a
  compromised controller can still create a privileged container. The controller is small, authenticated, internal-only
  and constructs container flags itself, but treat it as a privileged component.
* Prefer, in order: a dedicated build/plugin host that holds nothing else; rootless Docker or Podman for the
  controller's daemon; the Kubernetes controller implementation (not included). Do not run the plugin controller on
  the host that runs your other production data.

## What this platform does not protect against

* **A malicious plugin's content.** An LLM plugin can return misleading answers; a persistence plugin can return wrong
  data; contract tests check behaviour, not honesty. Review what you install.
* **Build-time code execution.** A repository's Dockerfile or `pip install` runs in the Docker daemon's builder. This
  release does **not** use rootless BuildKit, and build steps have network access. Only build sources you trust.
* **Denial of service by a hosted endpoint you added** beyond the timeouts and size limits above.
* **Compromise of Redis or of `PLUGIN_SECRET_KEY`** - together they expose all plugin secrets.
* **Multi-user administration.** There is a single `ADMIN_TOKEN`; the audit log's actor is always `admin`.
* **Supply-chain attacks on base images** (`python:3.11-slim`, your `FROM`): pin digests in your own Dockerfile and scan.
