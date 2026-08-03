# RunOnSpark Manager — Product Requirements and Technical Specification

Status: approved planning baseline  
Repository: `punkjazz-labs/basement`  
Companion catalogue: `punkjazz-labs/onspark` / [runonspark.ai](https://runonspark.ai)  
Initial scope: one NVIDIA DGX Spark, three curated models, vLLM  

## 1. Product definition

RunOnSpark Manager is a local-first management application for NVIDIA DGX Spark. It lets a person select a compatible model, install it, start it, verify it, and obtain a working OpenAI-compatible endpoint without assembling Docker and vLLM commands manually.

The public RunOnSpark website remains the discovery and recommendation layer. RunOnSpark Manager is the execution and lifecycle layer.

The product promise is:

> After a one-time manager installation, deploy a verified model on a DGX Spark with one deliberate action and receive a tested API endpoint.

“One click” does not include physical machine setup, connecting a Spark to a network, accepting a model licence, or initially installing the manager. It begins after the manager is installed and the Spark is reachable.

## 2. Product decisions

These decisions are part of the initial specification and should not be reopened without new evidence.

1. The manager lives in this separate repository. The static catalogue stays in `onspark`.
2. The first release supports one DGX Spark only.
3. The first release uses vLLM only.
4. The first release supports exactly three model families:
   - Qwen 3.6 35B-A3B
   - Qwen 3.6 27B
   - Laguna S 2.1
5. The manager may keep several models downloaded, but only one model is active by default on a single Spark.
6. The manager runs on the Spark and serves a local browser UI. A native desktop shell is not required.
7. The cloud website never receives Spark credentials, Hugging Face tokens, Docker access, or command execution access.
8. Installations are driven by versioned, typed recipes. Catalogue prose and existing `run.command` strings are not executable input.
9. Initial recipes ship inside the manager release. Remote recipe updates and community recipes are deferred until signing and trust controls exist.
10. Runtime images and model revisions must be pinned before a recipe can be marked verified. Floating tags such as `latest` and `nightly` are not acceptable in a released recipe.
11. The first useful unit is one complete Qwen 35B installation path, not a broad collection of incomplete screens.
12. No schedule or developer-time estimate is part of this specification. Work is ordered by dependency and verified outcome.

## 3. Initial users and jobs

### Primary user

A DGX Spark owner who wants to run a strong local model but does not want to reason about CUDA compatibility, container tags, model variants, vLLM flags, ports, persistent caches, process supervision, or health checks.

### Primary jobs

- See whether the Spark is ready to host a supported model.
- Understand required disk space and authentication before starting.
- Install a recommended model without using a terminal.
- See honest progress during large downloads and container preparation.
- Resume safely after a failed download, browser close, daemon restart, or machine reboot.
- Start and stop an installed model.
- Switch from one installed model to another.
- Copy a working OpenAI-compatible base URL and model identifier.
- Run a basic inference smoke test.
- Inspect useful logs when something fails.
- Remove a model and understand how much storage will be reclaimed.

## 4. Initial model recipes

The family name shown in the UI and the exact artifact installed are distinct fields. The initial release uses one verified artifact per family.

### 4.1 Qwen 3.6 35B-A3B

- Display name: `Qwen 3.6 35B-A3B (Unsloth)`
- Artifact: `unsloth/Qwen3.6-35B-A3B-NVFP4` (community quantization; supersedes the originally planned `nvidia/Qwen3.6-35B-A3B-NVFP4`, see ADR 0001/0002)
- Runtime: MiaAI Lab GB10 vLLM image `ghcr.io/miaai-lab/mia-vllm-gb10-linear-b12x`, pinned by digest
- Topology: one DGX Spark, tensor parallel size 1
- Quantization: NVFP4
- Initial role: first vertical slice and default recommendation
- Upstream reference: <https://huggingface.co/unsloth/Qwen3.6-35B-A3B-NVFP4>
- Important launch characteristics:
  - OpenAI-compatible server on port 8000
  - Qwen reasoning and tool-call parsers
  - MTP speculative decoding
  - DGX Spark-specific memory and attention settings

### 4.2 Qwen 3.6 27B

- Display name: `Qwen 3.6 27B`
- Artifact: `nvidia/Qwen3.6-27B-NVFP4`
- Runtime: vLLM
- Topology: one DGX Spark, tensor parallel size 1
- Quantization: NVFP4
- Upstream reference: <https://huggingface.co/nvidia/Qwen3.6-27B-NVFP4>

Recipes are not restricted to official first-party artifacts. Community-published artifacts (Unsloth, PrismaSCOUT, and similar) are acceptable recipe sources on equal footing, provided every artifact and image is pinned, passes the same validator, displays its true publisher and provenance in the UI, and carries a trust label that reflects its verification state rather than its publisher. Trust is earned through DGX qualification evidence, not through who published the weights.

### 4.3 Laguna S 2.1

- Display name: `Laguna S 2.1`
- Primary artifact: `poolside/Laguna-S-2.1-NVFP4`
- Drafter artifact: `poolside/Laguna-S-2.1-DFlash-NVFP4`
- Runtime: vLLM 0.25.0 or later, pinned to a verified image digest
- Topology: one DGX Spark, tensor parallel size 1
- Quantization: NVFP4 with DFlash speculative decoding
- Upstream reference: <https://huggingface.co/poolside/Laguna-S-2.1-NVFP4>

Laguna is implemented third because its installation contains two model artifacts and additional compilation/runtime requirements. The UI must show combined storage requirements rather than only the primary artifact size.

## 5. User experience

### 5.1 First launch

1. The manager starts as a system service on the Spark.
2. The user opens the manager URL in a browser on the same network.
3. The manager creates or loads a local admin identity.
4. The user completes local pairing/authentication.
5. The manager runs hardware and software inventory.
6. The home screen reports either `Ready` or a precise blocking condition.

### 5.2 Home screen

The home screen must show:

- Spark hostname and DGX Spark identity
- Manager version
- DGX OS, architecture, Docker and NVIDIA runtime status
- Available and total storage
- Current active model, if any
- Installed models
- Supported models available to install
- Active or recently completed jobs

### 5.3 Model card

Each model card must show:

- Human-readable family and artifact names
- Publisher and trust level
- Runtime and quantization
- Download/storage estimate
- Authentication requirement
- Licence link and acceptance requirement
- Installation status
- Primary action: `Install`, `Start`, `Stop`, `Switch to`, or `Remove`

Do not display unsupported recipes as if they are installable. An unavailable model may be shown only with the concrete incompatibility reason.

### 5.4 Installation flow

1. User selects `Install`.
2. Manager performs preflight without mutating the machine.
3. UI presents exact artifact, storage, port, licence, and any required token.
4. User confirms once.
5. Manager creates a persistent job and starts execution.
6. UI streams typed progress and useful logs.
7. Manager starts the runtime container only after artifacts and configuration are complete.
8. Manager waits for readiness and runs an inference smoke test.
9. UI reports `Ready` only after both health and inference verification pass.
10. UI exposes the base URL, model identifier, and copy buttons.

### 5.5 Switching models

When another model is active:

1. Installing a second model may proceed without stopping the first if resources permit the download.
2. Starting or switching must explain that the active model will stop.
3. The existing container stops cleanly.
4. The selected model starts and passes its health check.
5. If the selected model fails to start, the manager attempts to restore the previously active model and reports the outcome honestly.

### 5.6 Removal

Removal must distinguish:

- Stop service only
- Remove runtime container and generated configuration
- Remove model artifacts and reclaim storage
- Remove shared caches used by other installed models

Shared data must never be deleted silently. The UI must show the calculated reclaimable storage before destructive confirmation.

## 6. System architecture

```text
Browser on local network
          |
          | authenticated HTTP/WebSocket or SSE
          v
RunOnSpark Manager agent on DGX Spark
  |- embedded web UI
  |- local API
  |- inventory service
  |- recipe validator
  |- persistent job engine
  |- Docker/runtime adapter
  |- model artifact manager
  |- health and inference verifier
  |- local state database
          |
          v
Docker + NVIDIA Container Runtime
          |
          v
Pinned vLLM container + persistent model cache
          |
          v
OpenAI-compatible API on a declared local port
```

### 6.1 Agent

Preferred implementation: Go, producing a single Linux ARM64 binary.

Responsibilities:

- Serve the embedded UI and local API.
- Discover DGX Spark hardware and runtime prerequisites.
- Validate recipes before any mutation.
- Execute typed, allowlisted recipe operations.
- Persist jobs and step receipts.
- Interact with Docker through a structured API where practical.
- Manage model caches, configuration, ports, containers, and health checks.
- Recover incomplete jobs after restart.
- Redact secrets from logs and responses.

The agent should run as a dedicated service identity. Any access to Docker or privileged machine operations must be explicit and minimized. Docker-group access is effectively privileged and should be treated accordingly.

### 6.2 UI

Preferred implementation: React and TypeScript, compiled to static assets and embedded into the agent binary for releases.

The interface is a management console, not a marketing site. It should be calm, compact, and operationally explicit. It must work on desktop and tablet widths. Installation status should remain understandable without reading raw logs.

### 6.3 Local state

Preferred initial storage: SQLite.

Persist at least:

- Manager schema version
- Local installation identity and authentication state
- Machine inventory snapshots
- Accepted model licences and timestamps
- Installed recipe ID and version
- Artifact revisions and checksums
- Container image digest
- Generated configuration locations
- Job and step states
- Step receipts and redacted errors
- Current active model

Database writes must be transactional. A process crash must not produce a false `Ready` state.

## 7. Recipe system

### 7.1 Principles

- Recipes are declarative, versioned, validated, and immutable after release.
- A changed recipe receives a new recipe version.
- Released recipes use exact image digests and model revisions.
- Steps use a closed set of typed operations.
- Arbitrary remote shell scripts are not valid recipe steps.
- Every mutating step defines its completion test and compensation/removal behavior.
- Execution is idempotent: rerunning a completed step is safe.
- Secrets are referenced symbolically and injected only at execution time.
- Logs store secret names, never secret values.

### 7.2 Minimum recipe shape

```yaml
schema_version: 1
id: qwen36-35b-a3b-nvfp4-1s
version: 1
display_name: Qwen 3.6 35B-A3B
trust: runonspark-verified
topology:
  spark_count: 1
runtime:
  kind: vllm
  image: vllm/vllm-openai
  digest: sha256:REQUIRED_BEFORE_VERIFICATION
artifacts:
  - role: primary
    repository: nvidia/Qwen3.6-35B-A3B-NVFP4
    revision: REQUIRED_BEFORE_VERIFICATION
    expected_bytes: REQUIRED_BEFORE_VERIFICATION
requirements:
  architecture: aarch64
  docker: true
  nvidia_container_runtime: true
  secrets:
    - HF_TOKEN
service:
  internal_port: 8000
  default_host_port: 8000
operations:
  - type: verify_disk
  - type: pull_image
  - type: download_artifact
  - type: create_container
  - type: start_container
  - type: wait_http
  - type: verify_openai_inference
uninstall:
  - type: stop_container
  - type: remove_container
  - type: remove_artifact_if_unshared
```

The real schema must reject unknown fields where possible and must separate user-configurable values from recipe-author-controlled values.

### 7.3 Allowed operation categories

Initial operation categories:

- `verify_architecture`
- `verify_dgx_spark`
- `verify_disk`
- `verify_port`
- `verify_docker`
- `verify_nvidia_runtime`
- `pull_image`
- `download_artifact`
- `write_generated_config`
- `create_container`
- `start_container`
- `stop_container`
- `remove_container`
- `wait_http`
- `verify_openai_inference`
- `remove_artifact_if_unshared`

An escape-hatch `run_shell` operation must not be introduced for convenience. If a model requires a new kind of operation, implement and test that operation explicitly.

### 7.4 Recorded direction: automated recipe discovery

A future ingestion agent may scan public sources (X/Twitter announcements, Hugging Face releases, community benchmark threads) on a schedule and draft candidate recipes for newly published Spark-capable models automatically. This is a recorded product direction, not scheduled work. Any such agent produces *draft* recipes only: drafts must pass the same strict validator, pin exact digests and revisions, carry a `candidate` trust label, and require explicit human approval before entering the embedded recipe set. Decision 9 (no remote recipe execution until signing and trust controls exist) is unaffected — discovery automates authoring, never installation.

## 8. Job model and recovery

### 8.1 Installation states

```text
not_installed
  -> preflighting
  -> awaiting_confirmation
  -> downloading_runtime
  -> downloading_models
  -> configuring
  -> starting
  -> verifying_health
  -> verifying_inference
  -> ready
```

Any active state may transition to `failed`, `cancelled`, or `interrupted`. A retry resumes from the first incomplete or invalidated step.

### 8.2 Requirements

- A job is written to persistent storage before its first mutation.
- Every step records start time, completion time, inputs excluding secrets, result, and a resolvable receipt.
- Completed downloads are content-verified before being reused.
- Partial downloads use a temporary identity and are never presented as installed artifacts.
- Browser disconnection does not cancel a job.
- Agent restart reloads and reconciles incomplete jobs.
- Machine reboot while a model is ready restores the declared service policy without inventing readiness before health checks pass.
- Cancellation stops at a safe boundary and retains resumable downloads unless the user explicitly removes them.

## 9. Preflight requirements

Before installation, verify and report:

- Linux ARM64 architecture
- DGX Spark-compatible hardware identity
- DGX OS and kernel information
- Docker daemon reachability
- NVIDIA Container Runtime availability
- GPU visibility from a minimal container check
- Writable manager data directory
- Available storage against recipe requirement plus safety margin
- Host port availability
- Access to required model repositories
- Required licence acceptance
- Required secret presence without exposing its value
- Existing conflicting containers and installations

A failed preflight must not pull images, download models, alter Docker state, or write outside the manager's own job record.

## 10. Local API

The exact URL structure may evolve, but the first implementation needs these capabilities:

- `GET /api/v1/system`
- `GET /api/v1/preflight`
- `GET /api/v1/recipes`
- `GET /api/v1/models`
- `POST /api/v1/models/{recipe_id}/install`
- `POST /api/v1/models/{recipe_id}/start`
- `POST /api/v1/models/{recipe_id}/stop`
- `DELETE /api/v1/models/{recipe_id}`
- `GET /api/v1/jobs`
- `GET /api/v1/jobs/{job_id}`
- `POST /api/v1/jobs/{job_id}/cancel`
- `GET /api/v1/jobs/{job_id}/events`
- `POST /api/v1/models/{recipe_id}/smoke-test`
- `POST /api/v1/models/{recipe_id}/benchmark`
- `GET/POST /api/v1/keys`, `DELETE /api/v1/keys/{id}`
- `GET /api/v1/telemetry`
- `GET /api/v1/storage`
- `GET /api/v1/update`
- `/v1/*` — the stable OpenAI-compatible endpoint, reverse-proxied to the active model and authenticated by API key or console session (ADR 0007)

Mutating endpoints require authentication and CSRF/origin protection. Job creation endpoints must accept an idempotency key so duplicate clicks do not start duplicate installations.

## 11. Security and trust boundary

### 11.1 Non-negotiable rules

- No Spark, SSH, Docker, Hugging Face, or future NGC credential is sent to runonspark.ai.
- Secrets are never committed, included in recipes, returned by APIs, or written into job logs.
- The UI never accepts arbitrary shell commands.
- Recipes cannot introduce arbitrary environment variables, mounts, host networking, privileged containers, or host paths outside explicit schema policy.
- Container images are pinned by digest.
- Model artifacts are pinned by immutable revision and verified metadata/checksum where supported.
- The manager binds to a controlled interface and requires local pairing/authentication.
- Cross-origin requests are denied by default.
- Destructive removal requires explicit confirmation and an exact target/reclaim summary.
- Updating the manager is separate from updating models or recipes.
- A recipe cannot silently self-update.

### 11.2 Initial threat model

Defend against:

- A malicious or compromised recipe source
- A substituted container tag
- A model repository changing underneath an installation
- Command or argument injection through model metadata
- Browser cross-site request forgery against the local agent
- Secret exposure in environment inspection, logs, errors, or diagnostics
- Duplicate clicks creating competing jobs
- Path traversal in generated configuration or caches
- Removal of data shared by another installed recipe
- A crash producing stale or false state

## 12. Packaging and installation

The release artifact should be installable on DGX Spark's ARM64 Linux environment and should contain:

- Agent binary
- Embedded web assets
- Embedded verified recipes
- systemd unit
- Dedicated data and configuration directories
- Uninstaller that preserves downloaded models unless explicitly told otherwise

Preferred distribution is a versioned, checksummed ARM64 package from GitHub Releases. A convenience bootstrap command may download and verify that package, but it must not execute an unverified moving target.

The manager installation is the only unavoidable bootstrap step. After that, supported model deployment is performed through the UI.

## 13. Relationship with runonspark.ai

The existing catalogue already contains editorial model families, artifacts, topology, runtime, source evidence, and human-run commands in `../onspark/src/data/products.json` and `../onspark/src/domain/product-schema.ts`.

Those records are source material, not executable recipes.

Integration comes after the local manager proves one complete recipe. At that point:

- Catalogue product pages may display `Install with RunOnSpark Manager`.
- The link identifies a recipe ID, not a shell command.
- The local manager resolves the ID against its own trusted recipe set.
- If the manager is absent or unreachable, the website shows manager installation instructions.
- The public catalogue may describe models the installed manager does not yet support.
- The UI must never claim support merely because a catalogue entry exists.

Avoid a Git submodule between the repositories. Share a versioned generated catalogue/recipe contract only after its ownership and release flow are clear.

## 14. Observability and diagnostics

The user should be able to export a redacted diagnostic bundle containing:

- Manager version
- Recipe ID and version
- Machine inventory relevant to compatibility
- Docker and NVIDIA runtime versions
- Container image digest
- Model artifact revisions
- Job step results
- Health-check output
- Redacted recent logs

Diagnostics must exclude tokens, authorization headers, private keys, full environment dumps, and unrelated host files.

## 15. Acceptance criteria

### 15.1 First vertical slice: Qwen 35B

The first vertical slice is complete only when all of the following are demonstrated on a DGX Spark:

- Manager starts through systemd and the UI loads from another device on the local network.
- Inventory identifies the Spark and reports Docker/NVIDIA readiness.
- Qwen 35B preflight reports requirements without mutating runtime state.
- One confirmation creates exactly one installation job.
- The recipe uses a pinned vLLM image digest and pinned model revision.
- Progress survives a browser refresh.
- An interrupted download or agent restart can resume without corrupting state.
- The runtime starts without a manual terminal command.
- HTTP readiness passes.
- A real OpenAI-compatible inference request returns a non-empty model response.
- The UI exposes the tested endpoint and exact served model ID.
- Stop and start work from the UI.
- Removal deletes only the selected model's owned state and accurately reports retained shared data.
- No secret appears in API responses, UI logs, service logs, diagnostic exports, or repository files.

### 15.2 Three-model initial release

The initial release is complete only when:

- All three exact recipes install from a clean supported Spark state.
- Each recipe can start, pass health verification, and answer an inference smoke test.
- Several recipes can remain downloaded while only one is active.
- Switching models is transactional and restores or clearly reports the previous state on failure.
- Laguna accounts for and verifies both primary and drafter artifacts.
- Reboot recovery reconciles actual container health before showing `Ready`.
- Insufficient disk, missing token, occupied port, Docker unavailable, registry failure, and model download failure all produce actionable errors.
- Released recipes contain no floating image tags or mutable model revisions.

### 15.3 Outcomes that do not count

- A UI that only copies commands to the clipboard is not one-click installation.
- Running an existing third-party `start.sh` behind a button is not a trusted recipe engine.
- A container reaching `running` without health and inference verification is not `Ready`.
- A download progress animation disconnected from persisted byte/job state is not progress reporting.
- A successful installation that cannot resume after interruption is not complete.
- Support for a model family without pinning its exact artifact and runtime is not verified support.
- A cloud dashboard that stores local infrastructure credentials violates the architecture.

## 16. Implementation order

Work in thin, end-to-end units and preserve a runnable state after each unit.

1. **Repository foundation**
   - Establish Go module, application entry point, configuration directories, structured logging, and a minimal local HTTP server.
   - Add the smallest browser page proving the embedded UI path.
   - Follow `AGENTS.md` and `docs/PROJECT-AUTONOMY.md`; package manifests and test infrastructure are protected paths and require the repo's documented grant before modification.

2. **Read-only Spark inventory**
   - Implement typed inventory data and `GET /api/v1/system`.
   - Show hostname, architecture, storage, Docker, NVIDIA runtime, and GPU visibility.
   - Do not begin Docker mutations in this unit.

3. **Recipe schema v1**
   - Define strict validation and fixtures for one Qwen 35B recipe.
   - Reject floating image references, missing revisions, unknown operations, unsafe paths, and undeclared secrets.

4. **Persistent jobs**
   - Add SQLite migrations and the job/step state machine.
   - Demonstrate idempotent job creation, restart recovery, and event streaming with non-destructive fake operations.

5. **Docker and artifact operations**
   - Implement the allowlisted operations needed by Qwen 35B.
   - Record receipts and redact secrets at operation boundaries.

6. **Qwen 35B vertical slice**
   - Pin and verify the exact runtime image and model revision.
   - Complete preflight, install, start, readiness, inference, stop, restart, and removal.
   - Validate on a real DGX Spark.

7. **Operational UI**
   - Replace the minimal page with system, model, install-progress, endpoint, logs, and removal views.
   - Keep job semantics in the agent API rather than the browser.

8. **Qwen 27B recipe**
   - Reuse the same vLLM adapter and prove the recipe has no hidden family-specific shell dependency.

9. **Laguna recipe**
   - Add multiple artifacts, the DFlash drafter, required build environment, combined disk accounting, and verification.

10. **Packaging and clean-machine verification**
    - Produce the ARM64 release package and systemd installation.
    - Exercise clean install, manager update, reboot, interruption, low storage, and uninstall behavior.

11. **Catalogue integration**
    - Add recipe-aware install links to `runonspark.ai` only after the manager can safely resolve and execute them.

12. **Multi-Spark design**
    - Begin as a separate specification after the one-Spark acceptance criteria pass.

## 17. Explicit non-goals for the initial release

- Two- or four-Spark orchestration
- Kubernetes
- Arbitrary Docker Compose imports
- Arbitrary shell execution
- A general-purpose terminal
- Cloud-hosted remote management
- User accounts hosted by RunOnSpark
- Community recipe submission
- Model training or fine-tuning
- Benchmark leaderboards
- A full chat application
- Windows or macOS agent support
- Runtimes other than vLLM
- Automatic conversion or quantization of models
- Automatic mutation of host drivers, CUDA, DGX OS, or firmware

## 18. Open implementation questions

These require evidence during implementation and should be resolved in decision records:

- Exact tested vLLM container digest shared by the first two recipes, or whether they require separate digests
- Exact immutable Hugging Face revisions and verified expected download sizes
- Best supported download mechanism for resumable Hugging Face artifacts without leaking tokens
- Whether the service should bind only to LAN/Tailscale interfaces or default to localhost until pairing changes it (partially resolved: the model endpoint now follows the manager's listen interface, ADR 0006)
- Initial local authentication mechanism and recovery flow
- Safest Docker permission model compatible with a simple DGX Spark installation
- Default data directory and policy for Hugging Face cache interoperability
- Whether port 8000 remains the stable model endpoint or the manager provides a stable reverse-proxy endpoint while model containers use internal ports
- How manager updates are signed and rolled back

Do not resolve these by silently choosing convenience over security or recoverability. Record the chosen behavior and its verification evidence.

## 19. Source material

- NVIDIA DGX Spark documentation: <https://docs.nvidia.com/dgx/dgx-spark/>
- NVIDIA DGX Spark system overview: <https://docs.nvidia.com/dgx/dgx-spark/system-overview.html>
- NVIDIA Container Runtime on DGX Spark: <https://docs.nvidia.com/dgx/dgx-spark/nvidia-container-runtime-for-docker.html>
- NVIDIA DGX Spark playbooks: <https://github.com/NVIDIA/dgx-spark-playbooks>
- Qwen 3.6 35B-A3B NVFP4: <https://huggingface.co/nvidia/Qwen3.6-35B-A3B-NVFP4>
- Qwen 3.6 27B NVFP4: <https://huggingface.co/nvidia/Qwen3.6-27B-NVFP4>
- Laguna S 2.1 NVFP4: <https://huggingface.co/poolside/Laguna-S-2.1-NVFP4>
- Existing RunOnSpark product data: `../onspark/src/data/products.json`
- Existing RunOnSpark product schema: `../onspark/src/domain/product-schema.ts`

## 20. Starting instruction for the next Codex session

Read `AGENTS.md`, `docs/PROJECT-AUTONOMY.md`, and this PRD completely. Inspect the empty repository and current branch before acting. Treat this PRD as the product baseline. Begin with the smallest repository-foundation unit that can be verified locally; do not jump directly to executing upstream model commands. Keep protected-path gates intact, work on a feature branch, and distinguish local code proof from validation on an actual DGX Spark.
