# ADR and plan implementation audit, August 2026

This is a code-reading audit of ADRs 0001 through 0016 and every file in
`docs/plans/`. I traced concrete rules, checks, security boundaries and user
behaviours into the current Go, React and packaging code. Detailed sections
below contain only non-implemented, partial or drifted commitments. Rows with
zeroes mean that I found no such gap after tracing the applicable current
commitments; they are not a claim of hardware qualification.

I excluded work that its ADR explicitly calls future, including ADR 0016 and
the roadmap in plans 12 through 17, unless the document also says that the
behaviour already exists. I did not run Git or any test suite, as required.
Real GB10 behaviour, external release availability and live network endpoints
remain unverified. Where absence is material, the exact repository search is
recorded.

## Summary

| ADR | Title | Not implemented | Partial | Drifted |
| --- | --- | ---: | ---: | ---: |
| 0001 | Qwen 3.5 candidate pins | 0 | 0 | 0 |
| 0002 | Requested three-recipe candidate pack | 0 | 0 | 0 |
| 0003 | Transactional single-active-model switching | 0 | 0 | 1 |
| 0004 | Per-node memory and disk guardrails | 0 | 2 | 0 |
| 0005 | Automatic discovery and explicit placement | 0 | 0 | 1 |
| 0006 | Runtime exposure, IPC, cancellation and removal safety | 0 | 0 | 1 |
| 0007 | Stable authenticated endpoint and API keys | 0 | 0 | 1 |
| 0008 | Manager self-update design | 0 | 1 | 1 |
| 0009 | Signed recipe feed design | 0 | 1 | 1 |
| 0010 | Network setup with GB10 autodiscovery | 0 | 0 | 1 |
| 0011 | Multi-runtime support | 4 | 0 | 4 |
| 0012 | Curated model trust | 1 | 0 | 1 |
| 0013 | Delegated placement | 0 | 0 | 0 |
| 0014 | Console Spark adoption | 0 | 0 | 0 |
| 0015 | Roles on the stable endpoint | 0 | 0 | 0 |
| 0016 | Multi-node fleet control and placement | 0 | 0 | 0 |

## ADR 0003: Transactional single-active-model switching

### Two installs begun while idle bypass switch planning

**Classification: DRIFTED.**

**Promised.** "Starting an installed model while another model is active is a
switch, not an independent start", and the target must stop the previous model,
verify itself, then become the sole active model
(`docs/decisions/0003-transactional-single-active-model.md:7-18`). Plan 02 also
promises that different installs may download together but their final
load-into-memory phases serialize (`docs/plans/02-concurrent-installs.md:6-12`).

**Current state.** Each job records its previous model when its plan is created
(`internal/engine/engine.go:774-795`). If two install jobs both plan while no
model is active, both retain `previous == nil`. When the second job later takes
the runtime lock, `premiseHolds` immediately accepts that stale premise instead
of turning the job into a switch (`internal/engine/engine.go:250-263`). The code
comment explicitly says that two such installs are allowed to finish and each
activate (`internal/engine/engine.go:256-259`), and ADR 0015 repeats the same
exception (`docs/decisions/0015-roles-on-the-stable-endpoint.md:184-196`). The
database activation is exclusive only at the end
(`internal/engine/engine.go:1184-1195`); no stop of the newly active container
was added to the second job's plan.

**Why it matters.** Every current recipe declares host port 8000 (confirmed by
`rg -n 'default_host_port:' internal/recipe/recipes/*.yaml`), so the second start
can reach the runtime phase with the first container already holding the port. With
different-port runtimes it can leave two GPU-consuming containers running while
the store calls only one active. Either result breaks the promised transactional
switch at the exact boundary Plan 02 made concurrent.

## ADR 0004: Per-node memory and disk guardrails

### Worker-side distributed work is outside disk reservation and runtime admission

**Classification: PARTIAL.**

**Promised.** Disk is checked before mutation and throughout transfers, each
node must pass independently, and aggregate capacity can never substitute for a
node's own safety result (`docs/decisions/0004-per-node-resource-guardrails.md:9-19`).
Plan 02 added a reservation for every running install specifically so two jobs
cannot both pass against the same free bytes
(`docs/plans/02-concurrent-installs.md:14-32`).

**Current state.** Local engine jobs place other installs' reservations in
`Execution.ReservedBytes` (`internal/operations/operations.go:18-22`) and refresh
that value for each step (`internal/engine/engine.go:538-540`). A two-Spark
worker step instead constructs a fresh `Execution` with no `ReservedBytes` and
calls the local executor directly (`internal/httpapi/node.go:244-270`). It also
does not acquire the worker engine's runtime semaphore. The multispark plan
records the consequence plainly: a local install on the worker can race the
delegated one, and delegated steps take neither its runtime lock nor its disk
reservation (`docs/plans/multispark-v1.md:156-160`).

**Why it matters.** A person can start local work on the worker while the head
is installing a two-Spark model. The two paths can independently approve the
same disk or GPU capacity, defeating the safety mechanism precisely on the node
whose state the head cannot directly see.

### Advisory live-memory preflight recognizes a switch only when ports match

**Classification: PARTIAL.**

**Promised.** During a switch, the old manager-owned model stops before the
target's live-memory recheck, and failure after that point invokes rollback
(`docs/decisions/0004-per-node-resource-guardrails.md:15-17`). Plan 11 keeps that
rule for text-to-media switching even though ComfyUI uses port 8188 rather than
8000 (`docs/plans/11-media-generation.md:505-515`,
`docs/plans/11-media-generation.md:531-533`).

**Current state.** The HTTP preflight performs a live-memory check before it
will create the install job. It suppresses an expected active-model memory
failure only when `managedPortOwner` returns an owner
(`internal/httpapi/server.go:416-429`). That helper returns an owner only if the
active and target recipes use the same host port
(`internal/httpapi/server.go:486-500`). A failed advisory preflight prevents job
creation (`internal/httpapi/server.go:657-675`), so the engine never reaches the
promised stop, recheck and rollback sequence for a different-port target.

**Why it matters.** A text model can hold the memory that a media or other
different-port target needs. The console can reject the switch as unsafe before
the transactional path gets the chance to free and re-evaluate that memory.
The shipped catalog currently has no media recipe, so this path is code-verified
but runtime-unverified.

## ADR 0005: Automatic discovery and explicit placement

### Multi-node execution shipped before the ADR's admission prerequisites

**Classification: DRIFTED.**

**Promised.** The ADR says the current release does not claim network discovery
and multi-node recipes remain unavailable until authenticated membership,
leases, topology validation and failure tests exist
(`docs/decisions/0005-automatic-discovery-and-placement.md:40-45`).

**Current state.** The Models view treats a configured peer as additional
detected capacity and makes a two-Spark recipe fit
(`webui/ui/src/views/Models.tsx:142-152`). The worker API accepts and executes
distributed steps (`internal/httpapi/node.go:215-275`). The later multispark
plan deliberately shipped this experimental path while recording that it has
no peer-scoped identity, no real local admission control and no true lease
against another head (`docs/plans/multispark-v1.md:148-163`).

**Why it matters.** A user can now install a two-Spark recipe through a path the
accepted ADR said would remain unavailable until stronger safety and membership
boundaries existed. The experimental plan discloses the compromise, but ADR
0005 was never amended and still describes a safety gate the product no longer
enforces.

## ADR 0006: Runtime exposure, IPC, cancellation and removal safety

### Distributed containers re-enable host IPC

**Classification: DRIFTED.**

**Promised.** "Containers no longer run with `IpcMode: host`" and `/dev/shm` is
sized from the validated recipe field
(`docs/decisions/0006-review-remediation-runtime-and-lifecycle.md:28-32`).

**Current state.** A normal container gets loopback port bindings and an
explicit `ShmSize` (`internal/operations/docker.go:310-320`). The distributed
branch then sets both `NetworkMode: host` and `IpcMode: host`
(`internal/operations/docker.go:328-338`). The multispark plan requires that
configuration and calls it a narrow alternative to `--privileged`
(`docs/plans/multispark-v1.md:131-139`,
`docs/plans/multispark-v1.md:170-175`), but it does not amend ADR 0006.

**Why it matters.** A two-Spark model shares the host IPC namespace, so the
recipe's `shm_bytes` no longer defines an isolated `/dev/shm` boundary for that
container. This weakens isolation and increases the chance of interference with
host or other container IPC objects.

**Resolution (2026-08-13).** Resolved in code. The distributed branch no longer
sets `IpcMode`, so a rank keeps its own IPC namespace with `/dev/shm` sized from
the recipe's `shm_bytes`; host networking, `/dev/infiniband`, `IPC_LOCK` and
unlimited `memlock` stay, since those are what RDMA actually argues for. Both
two-Spark recipes were version-bumped so existing containers are recreated
(drift detection does not compare `IpcMode` or `ShmSize`). Hardware verification
of the two-Spark path under isolated IPC is still pending: the fleet is
currently reserved for other work.

## ADR 0007: Stable authenticated endpoint and API keys

### Editorial speed figures still precede device measurements

**Classification: DRIFTED.**

**Promised.** Automatic benchmarking persists measured throughput and the
catalog shows it "instead of editorial speed claims"
(`docs/decisions/0007-stable-endpoint-api-keys.md:34-40`).

**Current state.** The engine does create an automatic benchmark after first
activation (`internal/engine/engine.go:1228-1240`). Before a measurement exists,
however, the console reads a hard-coded `REFERENCE_TPS` table
(`webui/ui/src/views/Models.tsx:20-30`) and renders those values as the row,
detail and install-dialog speed (`webui/ui/src/views/Models.tsx:390-433`,
`webui/ui/src/views/Models.tsx:494-500`,
`webui/ui/src/views/Models.tsx:830-841`). The figures are labelled "typical" and
the source comment points to the candidate research, but they remain editorial
figures in the primary catalog surface.

**Why it matters.** A first-time user sees a community-derived speed before the
Spark has measured anything, despite the ADR promising that catalog speed would
come from this device. The labels reduce the risk of confusion but do not match
the documented source-of-truth rule.

## ADR 0008: Manager self-update design

### The documented manual update path does not activate the replacement binary

**Classification: PARTIAL.**

**Promised.** Applying an update is currently an operator action: download the
release package and rerun the idempotent `install.sh`
(`docs/decisions/0008-manager-self-update-design.md:5-10`).

**Current state.** The packaged script verifies and overwrites the binary
(`packaging/install.sh:17-37`), then runs only `systemctl enable --now
basement.service` (`packaging/install.sh:69-70`). It never issues a service
restart, so an already running manager is not explicitly made to execute the
new bytes. The Go installer, by contrast, performs `enable --now` and an
explicit restart (`internal/setup/install.go:229-231`). The script also writes
a non-loopback drop-in when selected but has no branch that removes an existing
drop-in when loopback is selected (`packaging/install.sh:39-67`); the Go
installer has that convergence branch (`internal/setup/install.go:212-225`).

**Why it matters.** Following the only update procedure ADR 0008 says is
implemented can leave the old process running after the new file is installed,
and can preserve a previous network bind the operator intended to remove.

### The release check cache is one hour, not six

**Classification: DRIFTED.**

**Promised.** Both ADR 0007 and ADR 0008 specify a six-hour cache
(`docs/decisions/0007-stable-endpoint-api-keys.md:43-46`,
`docs/decisions/0008-manager-self-update-design.md:7-9`).

**Current state.** `updateCheck` reuses a result only while it is less than one
hour old (`internal/httpapi/server.go:1886-1897`), and the console polls that
endpoint hourly (`webui/ui/src/App.tsx:125-134`). Plan 14 also describes the
one-hour value as the current behaviour
(`docs/plans/14-release-notes.md:71-80`).

**Why it matters.** This is a low-severity drift, but it changes the promised
external-call cadence from at most four GitHub checks per day to as many as 24
per machine with an open console.

## ADR 0009: Signed recipe feed design

### The executed feed path is not operationally publishable

**Classification: PARTIAL.**

**Promised.** Executed Plan 04 says that after the work, recipes arrive over the
network signed, with the embedded catalog as fallback
(`docs/plans/04-remote-recipe-index.md:1-7`), and the manager fetches on startup
and every six hours (`docs/plans/04-remote-recipe-index.md:44-52`).

**Current state.** The fetch, verify, cache and fallback machinery exists
(`internal/recipefeed/fetch.go:115-175`,
`internal/recipefeed/fetch.go:202-258`). Its production URL is explicitly a
placeholder for a repository the code says does not exist
(`internal/recipefeed/fetch.go:26-30`). More decisively, the embedded public key
is a placeholder whose private half was discarded, and the file states that
nothing signed with it should be trusted
(`internal/recipe/signature.go:10-19`). Plan 15 therefore says nothing can be
published until a real key exists (`docs/plans/15-signed-recipe-feed.md:36-41`).

**Why it matters.** No publisher can create an index that current production
binaries both accept and should trust. Because feed failures remain log-only
(`internal/recipefeed/fetch.go:178-186`), a user sees only the embedded catalog
and no indication that the promised delivery channel has never become live.
The external URL's present network status was not independently verified.

### The implemented feed and signature formats differ from the ADR

**Classification: DRIFTED.**

**Promised.** The ADR specifies a signed manifest of recipe pointers containing
`id`, `version`, `sha256` and `url`, followed by a per-recipe download and hash
check, with the detached signature in minisign format
(`docs/decisions/0009-signed-recipe-feed-design.md:18-25`).

**Current state.** `Index` carries complete recipe objects inline
(`internal/recipe/index.go:17-24`) and the verifier accepts that single signed
document directly (`internal/recipe/index.go:52-82`). The signature file is raw
base64 Ed25519 and is explicitly not byte-compatible with minisign
(`internal/recipe/signature.go:37-48`). Plan 15 records the same divergence and
says the ADR should be amended rather than the code rebuilt
(`docs/plans/15-signed-recipe-feed.md:146-169`); that amendment is not present.

**Why it matters.** A feed publisher implementing the accepted ADR will produce
documents and signatures this manager cannot consume. The implementation may
be a reasonable simplification, but the architecture record no longer defines
the actual interoperability or verification boundary.

## ADR 0010: Network setup with GB10 autodiscovery

### The Go and release-package installers no longer have behavioural parity

**Classification: DRIFTED.**

**Promised.** The setup engine mirrors `packaging/install.sh` step for step so
both paths stay behaviourally identical
(`docs/decisions/0010-network-setup-and-gb10-discovery.md:45-52`). The ADR notes
that only the unit bytes are mechanically enforced, but still requires every
install-step change in both places
(`docs/decisions/0010-network-setup-and-gb10-discovery.md:103-109`).

**Current state.** The Go path detects and configures a missing NVIDIA container
runtime (`internal/setup/install.go:169-182`), adopts the pre-rename unit, data
directory and service account (`internal/setup/install.go:185-202`,
`internal/setup/install.go:282-338`), and explicitly restarts basement
(`internal/setup/install.go:229-231`). The packaged script moves directly from a
Docker-group check to creating the new user, binary and unit
(`packaging/install.sh:13-37`) and finishes with `enable --now` only
(`packaging/install.sh:69-70`). Searches for `runonspark`, `nvidia-ctk` and
`systemctl restart` in `packaging/install.sh` returned no matches.

**Why it matters.** The friendlier setup path can repair a fresh OEM Docker
installation and preserve a pre-rename basement, while the release-package path
can report success without the NVIDIA runtime fix, orphan old state under the
legacy path, or leave an update running the old process.

## ADR 0011: Multi-runtime support

### The promised installable MiniMax/ComfyUI recipe and published image do not exist

**Classification: NOT IMPLEMENTED.**

**Promised.** Plan 11 says the shipped feature includes a pinned ComfyUI image
and then specifically requires
`internal/recipe/recipes/minimax-h3-comfyui-1s.yaml` with real registry and
manifest values (`docs/plans/11-media-generation.md:204-216`,
`docs/plans/11-media-generation.md:602-610`).

**Current state.** The catalog embeds only YAML files under
`internal/recipe/recipes` (`internal/recipe/catalog.go:8-31`). The search
`rg -n 'kind: comfyui|minimax-h3-comfyui' internal/recipe/recipes` returned no
matches. A ComfyUI Dockerfile exists, but its own README says GPU smoke testing
and registry publication are later steps that the build script does not perform
(`packaging/comfyui-image/README.md:67-72`).

**Why it matters.** The runtime adapter, generation API and React view are dead
ends for a normal user because no shipped catalog entry can be installed and
activated to reach them.

### The Generate empty state is replaced by a hidden tab

**Classification: DRIFTED.**

**Promised.** Generate remains a tab and, when no media model is installed,
shows one line naming what is missing plus a link to Models
(`docs/plans/11-media-generation.md:537-545`).

**Current state.** The app includes Generate in `TABS`, but filters it out unless
a ready media model is already active and redirects any open Generate view back
to Models when that condition becomes false (`webui/ui/src/App.tsx:227-240`).
The component is rendered only for an active recipe with media configuration
(`webui/ui/src/App.tsx:307-322`).

**Why it matters.** A user cannot discover the media feature, see what is
missing, or follow the promised link to installation. The entry point appears
only after the user has already completed the setup it was meant to explain.

### Image-to-video is declared but explicitly refused

**Classification: NOT IMPLEMENTED.**

**Promised.** The service schema includes both text-to-video and image-to-video
graphs (`docs/plans/11-media-generation.md:239-260`), and the Generate composer
includes an optional source image (`docs/plans/11-media-generation.md:546-550`).

**Current state.** Both embedded graphs exist, but the API hard-refuses
`image_to_video` because nothing stages a source image
(`internal/httpapi/generate.go:313-327`). The UI renders the mode disabled and
says it is not available (`webui/ui/src/views/Generate.tsx:303-310`,
`webui/ui/src/views/Generate.tsx:395-410`).

**Why it matters.** One of the two workflow modes represented in the recipe
contract and graph assets cannot be used from either the API or console.

### The promised generation time estimate is absent

**Classification: NOT IMPLEMENTED.**

**Promised.** After the first completed generation, the view should show an
estimate derived from that machine's measured seconds per frame, while showing
no invented number before then (`docs/plans/11-media-generation.md:551-561`).

**Current state.** The view shows live elapsed time and runtime step progress
(`webui/ui/src/views/Generate.tsx:24-73`) and the final measured duration after
completion (`webui/ui/src/views/Generate.tsx:161-190`). The search
`rg -n 'estimate|estimated|time estimate' webui/ui/src/views/Generate.tsx`
returned no matches, and there is no calculation from previous generation
durations.

**Why it matters.** Community timings in the plan range from minutes to more
than an hour. A user starting a clip gets no machine-specific expectation for
how long the current request will occupy the Spark.

### Generation storage is missing from Storage

**Classification: NOT IMPLEMENTED.**

**Promised.** The Storage view gains a Generations group with totals and both
single-generation and whole-group deletion
(`docs/plans/11-media-generation.md:442-449`), implemented as Phase F
(`docs/plans/11-media-generation.md:622-623`).

**Current state.** The storage response totals only the database, configs,
artifacts, caches and images (`internal/httpapi/server.go:1745-1769`). The search
`rg -n generation webui/ui/src/views/Storage.tsx` returned no matches.
Individual generation deletion exists in Generate, but there is no storage
accounting or group cleanup in Storage.

**Why it matters.** Generated video files can be large and persistent. The
product's disk-management surface neither counts nor offers bulk cleanup for
them, so managed totals understate basement-owned disk use.

### A second generation is queued instead of rejected with 409

**Classification: DRIFTED.**

**Promised.** With `concurrent_generations: 1`, a generation already running
causes `POST /api/v1/generate` to return 409
(`docs/plans/11-media-generation.md:426-433`).

**Current state.** The queue explicitly retains additional requests in FIFO
order (`internal/httpapi/generate.go:40-65`). Every valid submission is inserted,
enqueued and answered 202 with a queue position
(`internal/httpapi/generate.go:270-286`). The recipe type comment also says
extra requests are queued, never refused (`internal/recipe/types.go:248-253`).

**Why it matters.** An API client written to the documented conflict contract
can accidentally create long-running queued work rather than receive the
immediate refusal it uses for backpressure or user confirmation.

### The generation request and canvas rules changed without the plan changing

**Classification: DRIFTED.**

**Promised.** The API request contains `short_edge`, and resulting dimensions
are rounded to a multiple of 32 while out-of-range values are rejected
(`docs/plans/11-media-generation.md:415-433`).

**Current state.** The handler accepts explicit `width` and `height`, not
`short_edge` (`internal/httpapi/generate.go:224-232`), and rejects any dimension
that is not already a multiple of 32 rather than rounding it
(`internal/httpapi/generate.go:331-352`). The shipped UI is written to this new
width/height contract (`webui/ui/src/views/Generate.tsx:311-336`).

**Why it matters.** A client following the written API sends the wrong shape,
while a client relying on documented normalization gets a 400. The console and
server agree with each other, but the plan no longer documents their API.

### Automatic benchmarking is still text-only for a media activation

**Classification: DRIFTED.**

**Promised.** ADR 0011 says benchmark and the rest of the lifecycle are already
runtime-neutral (`docs/decisions/0011-multi-runtime-support.md:38-47`). Plan 11
says token throughput must not be invented for media
(`docs/plans/11-media-generation.md:398-403`).

**Current state.** The HTTP action correctly refuses a manual media benchmark
(`internal/httpapi/server.go:638-649`). The engine nonetheless calls
`autoBenchmark` after every successful install or start without checking the
runtime kind (`internal/engine/engine.go:1170-1196`). That function creates a
normal benchmark job (`internal/engine/engine.go:1228-1240`), whose plan is
`wait_http` followed by text `measure_throughput`
(`internal/engine/engine.go:751-756`).

**Why it matters.** Once a media recipe becomes installable, every first
activation can create a doomed text-throughput job in Activity immediately
after a successful media install, making a working model look partially broken.

## ADR 0012: Curated model trust

### Signed delivery is claimed as current but has no production trust root

**Classification: NOT IMPLEMENTED.**

**Promised.** Every recipe reaching a user has been "signed for delivery", and
the ADR says this is already what the manager does, not new work
(`docs/decisions/0012-curated-model-trust.md:24-40`).

**Current state.** The recipe-feed key is explicitly a placeholder with no
retained private key (`internal/recipe/signature.go:10-19`) and its configured
repository URL is also explicitly a placeholder
(`internal/recipefeed/fetch.go:26-30`). Embedded recipes therefore reach users
inside manager releases instead. The release workflow creates SHA-256 files
(`.github/workflows/release.yml:23-35`) but contains no Ed25519 or minisign
signature step; the search for `minisign|ed25519|signature` in that workflow
returned no matches.

**Why it matters.** Pinning and local validation are real, but the stated
tamper-evident delivery link is absent on both available paths. A user cannot
verify the trust chain that ADR 0012 says is already complete.

### The first-run hero calls a candidate recipe verified

**Classification: DRIFTED.**

**Promised.** A recipe is installed and run on basement's own Spark hardware
before its label changes from candidate to verified
(`docs/decisions/0012-curated-model-trust.md:26-35`). The README says every
current recipe is still a candidate and the console label always tells that
truth (`README.md:101-109`, `README.md:128-131`).

**Current state.** The recommended hero is the first curated recipe
(`webui/ui/src/catalog.ts:23-29`), whose recipe says `trust:
basement-candidate` and `verification: candidate`
(`internal/recipe/recipes/qwen36-35b-a3b-nvfp4-1s.yaml:1-10`). The hero
nevertheless prints "Verified and pinned for a single Spark" unconditionally
(`webui/ui/src/views/Models.tsx:585-627`). The API type carries the trust field,
but Models does not use it; `rg -n trust webui/ui/src/views/Models.tsx` returned
no matches.

**Why it matters.** This is the most direct trust failure in the console. A new
user is told that hardware verification happened even though the recipe and
repository documentation both say it has not.

## Highest value gaps

1. **Candidate shown as verified (ADR 0012).** It makes the product's strongest
   trust claim false on the first screen a new user sees.
2. **Concurrent idle installs bypass transactional switching (ADR 0003).** A
   normal concurrent-download workflow can reach port conflict, memory
   overcommit or an extra live container at activation time.
3. **Worker work bypasses local reservations and the runtime lock (ADR 0004).**
   Local and delegated installs can approve and consume the same worker
   resources.
4. **Manual updates do not restart the running service (ADR 0008).** The only
   documented current update path can install new bytes without actually
   running them.
5. **Distributed containers use host IPC (ADR 0006).** The two-Spark path
   reverses an explicit isolation remediation and makes recipe `shm_bytes` cease
   to be an isolated boundary. Resolved in code on 2026-08-13 (see the finding
   above); hardware verification is pending.
6. **The signed recipe channel has no usable production key or endpoint (ADRs
   0009 and 0012).** Users cannot receive or verify the recipe delivery path the
   trust story says exists.
7. **No installable media recipe or published image exists (ADR 0011).** A large
   amount of runtime, API and UI implementation is unreachable through the
   shipped product.
8. **Generation files are absent from Storage (ADR 0011).** When media becomes
   reachable, large basement-owned files will be omitted from managed disk
   totals and bulk cleanup.
