# Model candidates — August 2026 sweep

Trigger: @sudoingX tweet ranking "every serious model" on one DGX Spark.
Every fact below was verified live against the Hugging Face API / linked
sources on 2026-08-03 (five parallel research passes). Tweet numbers are
kept only where corroborated; anything unverified is marked. Nothing in
this file is a measurement of ours — the console measures after install.

Already in the catalog: Qwen 3.6 35B-A3B, Qwen 3.6 27B, Laguna S 2.1 + DFlash.

## Ready to qualify (single Spark, vLLM path)

### 1. Qwen3.5-122B-A10B — strongest candidate
- Artifact: `RedHatAI/Qwen3.5-122B-A10B-NVFP4`, rev `49d19c10`, 76,484,732,971 bytes
  (nvfp4-pack-quantized, group 16, fp8 scales). Alternatives: Spark-specific
  `scottgl/Qwen3.5-122B-A10B-NVFP4-GB10` (79.75 GB) and `sjug/…-resharded` (76.5 GB).
- Base: `Qwen/Qwen3.5-122B-A10B` (Apache-2.0, LICENSE fetched). Note: the RedHatAI
  repo itself declares no licence file — inherits Apache-2.0 from base; record both.
- vLLM: native. The RedHatAI card ships the serve command, including MTP
  speculative decoding (`qwen3_next_mtp`, 2 tokens, flashinfer_cutlass MoE backend).
  MTP draft head confirmed in config (`mtp_num_hidden_layers: 1`).
- Fit: ~76 GB weights → ~50 GB headroom on 128 GB. FP8 (127 GB) and BF16 (250 GB) do not fit.
- Community speed, corroborated: 28.3 tok/s baseline → 51 tok/s with AutoRound
  INT4 + MTP-2 stack (ice-ice-bear.github.io 2026-05-07, verbatim; NVIDIA forum
  thread 365639 agrees). A "80+ tok/s with DFlash" thread exists (374328), content
  not independently re-read. Tweet's exact 28.2/35 pairing: unverified but in-family.
- Known vLLM MTP rough edges: issues #36498 (closed), #36031 — retest at qualification.
- Multimodal (image-text-to-text); text path first.

### 2. Nemotron-3-Nano-Omni-30B-A3B-Reasoning — easiest add
- Tweet said "31B-A3B": actual slug is 30B-A3B. Tweet's "264 tok/s speed king":
  NOT corroborated anywhere; measured community numbers are 56.94 tok/s median
  (Omni NVFP4, dev.classmethod.jp bench) and 74.75 tok/s for the non-Omni Nano
  W4A16. Do not print 264 anywhere.
- Artifact: `nvidia/Nemotron-3-Nano-Omni-30B-A3B-Reasoning-NVFP4`, rev `dc5f0b0b`,
  22,431,647,821 bytes (modelopt NVFP4). First-party, 1.5M downloads.
- vLLM: officially supported (≥ 0.20.0) with an explicit DGX Spark/aarch64 section
  on the card (`--gpu-memory-utilization 0.8 --max-model-len 131072`).
- Licence: NVIDIA Open Model Agreement (`license: other`) → recipe must set
  required_licence_acceptance and link the agreement.
- Omni caveat: text chat works on the LLM backbone; image/audio/video ingestion
  uses custom code (CRADIO v4-H + Parakeet encoders, trust_remote_code) and is
  not confirmed as a stock vLLM multimodal path. Qualify text-only first.

## Skip

### 3. Ornith-1.0-35B (DeepReinforce AI, Qwen3.5-MoE family)
the owner (2026-08-03): skip; unfamiliar name, no pull. Data kept for reference.
- Base artifact real: `deepreinforce-ai/Ornith-1.0-35B`, rev `5df2ed3f`,
  70,250,400,102 bytes BF16 safetensors; card documents vLLM ≥ 0.19.1.
- Blockers: (a) MIT tag but the LICENSE file 404s in the base repo — trust
  policy needs the licence text pinned; (b) the only ready NVFP4 vLLM quant is a
  community "uncensored/abliterated" build (AEON-7) — not shippable as a default;
  (c) lineage description inconsistent across sources.
- Path if wanted: qualify BF16 (70 GB fits) or wait for/make a clean NVFP4.
- Speeds: tweet's 78 tok/s appears to be llama.cpp Q4 (source tweet fetch-blocked,
  HTTP 402). Independently fetched vLLM-path numbers: 35 tok/s (NVIDIA forum,
  NVFP4) and 37.5 tok/s (FP8, vLLM 0.23).

## Two-Spark candidates (the multispark thread)

### 4. DeepSeek V4 Flash 0731 — should be the multispark qualification target
- Single Spark + vLLM: effectively NO today.
  - The tweet's "3bit, 16.5 tok/s" is real but is llama.cpp: `unsloth/DeepSeek-V4-Flash-GGUF`
    UD-IQ3_XXS (103.0 GB), measured 16.56 tok/s (dev.classmethod.jp 2026-08-02).
  - EXL3 3-bit single-Spark repo exists (`0xSero/deepseek-v4-flash-0731-spark`,
    106.86 GB, REAP-pruned) but its own README says untested, no benchmark.
  - Patched community vLLM fork (vLLM-Moet-GB10) runs 2-bit at ~9.8 tok/s.
- Two Sparks + vLLM: YES. `nvidia/DeepSeek-V4-Flash-NVFP4`, rev `e3cd60e7`,
  168,305,308,121 bytes (LICENSE present, MIT base). Community dual-Spark recipes:
  `MiaAI-Lab/DeepSeek-v4-Flash-DSpark-2x-DGX-Spark` (315 stars — same publisher
  as our Qwen recipe) and `tonyd2wild/…-2x-DGX-Spark` (236 stars). NVIDIA forum
  reports 42–76 tok/s across 2× Spark.
- This is the website's flagship claim and the natural first `spark_count: 2` recipe.

### 5. Step 3.7 Flash (StepFun, 198B MoE VLM, Apache-2.0)
- Single Spark: GGUF/llama.cpp only (community: "llama.cpp is the only path",
  forum 371804). `unsloth/Step-3.7-Flash-GGUF` IQ4_XS 95.34 GB; ~24–30 tok/s
  short-context corroborated (StepFun's own 24 tok/s tg128 cited secondhand;
  flowtivity.ai measured 27). Tweet's ~25 tok/s: credible.
- Two Sparks + vLLM: `stepfun-ai/Step-3.7-Flash-NVFP4`, rev `4275532f`,
  129,252,403,665 bytes — needs the 2-node cluster; measured ~31–32 tok/s decode
  with MTP at 262k ctx (forum 373163). Second 2s candidate after DeepSeek.

## Round 2 — 2026-08-03, X API sweep + deep verification

X recent-search (live API, ~last 72h) plus NVIDIA forum and HF verification.

### Inkling-Small (Thinking Machines) — top new candidate, needs SGLang
- The community event of the window: sgl_project announced official support
  on 2x DGX Spark over ConnectX-7 (X, 2026-08-01, 83 likes). MiaAI Lab recipe
  thread: ~34 tok/s single stream with dspark drafts, ~79 tok/s at 6 concurrent
  (X, 2026-08-02, 121 likes). A vLLM path was also demonstrated by @Reederey:
  21.4 tok/s C1 / ~94 tok/s C8, full 1M KV pool on 2x Spark, vLLM 0.26 with
  two kernel bugs fixed (X, 2026-07-31/08-01).
- Artifact verified: `thinkingmachines/Inkling-Small-NVFP4`, rev `b6a99534`,
  170,764,923,366 bytes → strictly 2 Sparks. Apache-2.0, licence link fetched
  live. Base `thinkingmachines/Inkling-Small`: 276B total / 12B active prose
  (safetensors sum ~266B, discrepancy noted), 1,048,576 ctx confirmed in
  config, natively multimodal (text+image+audio in). Draft model
  `RadixArk/Inkling-Small-DSpark-Preview`, rev `fcc210dc`, 1,799,831,426 bytes.
- The repo the owner linked (`MiaAI-Lab/Inkling-Small-NVFP4-Dual-DGX-Sparks`) is
  a 6-star shell wrapper (created 2026-07-30) around
  `drowzeys/keys-1M-CTX-…-SGlang-SM121-optimized` and the image
  `ghcr.io/drowzeys/inkling-sglang-gb10:kvquant`. Its 1M-context claim is
  disputed: forum testers report BF16-only KV capping usable context at
  ~262-300K on 2x Spark. Qualify with skepticism; consider the vLLM path too.
- vLLM's official Inkling recipe page does NOT list GB10 at all. On Spark,
  SGLang is the documented path today.

### ds4 / DwarfStar (antirez) — the single-Spark DeepSeek story
- Salvatore Sanfilippo's native C inference engine, ~20k GitHub stars in days,
  purpose-built for DeepSeek V4 Flash/Pro. Metal/CUDA/ROCm, GGUF mixed quants,
  DFlash speculative decoding, OpenAI-compatible serve.
- On one Spark: ~27 tok/s with DSpark drafts (@WescheNex1q measured 1x ds4
  27.19 vs 2x vLLM NVFP4 67.58 on the same prompt); 14.7h/971-request soak
  test clean (@MichaelGannotti); SparkBench quality 92.0 for the 1-Spark ds4
  GGUF stack vs 87.6 for 2-Spark NVFP4 (@WescheNex1q) — quant mix beat NVFP4.
- Implication: the best single-Spark DeepSeek experience is currently outside
  both vLLM and stock llama.cpp. Watch item for a future runtime kind.

### Watch: Qwen 3.8 (announced 2026-08-03, weights not yet out)
- Qwen3.8-Max announced (2.4T MoE); open weights stated "next week", and
  Qwen3.8-27B open weights announced alongside. The Spark community is
  already planning around the 27B ("DGX Spark's best partner alongside
  DeepSeek"). Revisit within days; likely replaces/joins Qwen 3.6 27B in
  the catalog when weights land.

### Other real traction in the window
- MiniMax H3: text-to-video 33B + Qwen3-VL encoder running on ONE Spark
  (pinned vLLM Omni build, SM121 workarounds). Different product surface
  (video), notable for the movement narrative.
- Gemma 4 31B: won a 29-case 30-stack comparison on a GX10 (@KI_Vater);
  another user daily-drives it multimodal on one Spark.
- GLM 5.2: active comparisons vs DeepSeek V4 Flash; `nvidia/GLM-5.2-NVFP4`
  exists; dual-Spark llama.cpp RPC experiment ran IQ1_S at ~8 tok/s (toy).
- Kimi K3: HF trending #1 but needs 8-24 GB10 nodes; not our market segment.
- DeepSeek V4 Flash 0731 confirmed newest DeepSeek (HF author listing checked
  live 2026-08-03; V4-Pro remains April preview). Param count reported as
  284B in most sources, 304B in one; unreconciled.

## Runtime direction (decided)

the owner (2026-08-03): basement must run models independently of runtime,
including SGLang. Design and phasing recorded in
`docs/decisions/0011-multi-runtime-support.md`. The evidence above is the
rationale: Inkling needs SGLang, single-Spark DeepSeek lives in ds4/llama.cpp
territory, Qwen family is vLLM. No single runtime covers the frontier.

## Round 3 — 2026-08-03 evening, MiniMax H3 deep-check (owner request)

### MiniMax H3 — the community moment, and why we cannot catalog it
- Traction confirmed by live X recent-search (36 posts/72h): 304-like ComfyUI
  T2V post (1280x736/15s in 1h20m on one Spark), 250-like text-rendering demo,
  79-like single-Spark vLLM-Omni run (SM121 + online FP8 + pinned build),
  52-like dual-Spark run. The dual-Spark repo is
  `joeynyc/MiniMax-H3-2x-DGX-Spark` (7 stars, single commit): Ulysses sequence
  parallel over RoCEv2, 154.9s -> ~65s for the same clip, batch-size-one,
  patched vLLM-Omni hook, self-described not production-validated.
- Artifact: `MiniMaxAI/MiniMax-H3`, 33B dense, video+stereo-audio out
  (T2VA/FL2VA/Ref2VA), up to 2K/15s/24fps.
- LICENCE (fetched verbatim 2026-08-03): "'Excluded Territories' means the
  European Union, the United Kingdom, the Republic of Korea and the United
  States of America." Use is licensed only OUTSIDE those territories, plus a
  $20M revenue gate. This alone disqualifies catalog inclusion: our users are
  overwhelmingly in the excluded territories.
- Product-surface mismatch, independent of licence: output is a video file
  produced over minutes to hours via ComfyUI or a patched vLLM-Omni fork.
  Nothing in our serve/verify/benchmark/playground surface applies. Adding it
  is not a recipe; it is a second product (job queue, file outputs, video UI).
- Decision: do NOT add a recipe (neither 1s nor 2s). Ride the moment
  editorially instead. If video-on-Spark becomes a product goal, start from a
  model whose licence permits US/EU use, and design the video surface first.

## Round 4 — 2026-08-23, owner links (Qwen3.8, single-Spark DS4F, Obliterated)

Trigger: the owner sent three links to add. Every fact verified live on
2026-08-23 (HF API, GitHub API, registry manifests); research method as in
Round 2.

### 1. Qwen3.8-27B on SGLang — ADDED as `qwen38-27b-nvfp4-1s`
- Base: `Qwen/Qwen3.8-27B` (Aug 2026, Apache-2.0), 27.8B dense, the pack's
  first hybrid attention model: 48 linear-attention (Gated DeltaNet) plus 16
  full-attention layers, in-checkpoint MTP head, native VLM, thinking on by
  default, native context 262144.
- Checkpoint: `RadixArk/Qwen3.8-27B-NVFP4` rev `319f741c`, 21,945,295,265
  bytes, NVFP4 (MLP, group 16) + FP8 (attention path) + FP8 KV, Apache-2.0
  with LICENSE in tree.
- Launcher: `MiaAI-Lab/Qwen3.8-27B-SGLang-DGX-Spark` @ `c90d8c34` (same
  publisher as the Qwen 3.6 recipes). Image `lmsysorg/sglang:qwen38-27b`
  (nightly-dev-20260814-c4271c3f, model-specific build), pinned by index
  digest `febfb971`; the live registry digest equals the launcher's own
  documented pin.
- Schema widened for the hybrid architecture (sglang fields: trust remote
  code, chunked prefill, prefill CUDA graph toggle, four mamba cache knobs,
  EAGLE steps/top-k, sampling defaults). Allowlist policy unchanged: values
  a maintainer has seen a runtime accept.
- Modes: the recipe carries the launcher's default EAGLE/MTP mode (34.5
  tok/s code, ~24 chat/essays, launcher's own measurements). DSPARK (51.5
  tok/s code, separate trained drafter `RadixArk/Qwen3.8-27B-DSpark`) is a
  possible later version bump; it needs a DSPARK enum, a drafter artifact
  role, block-size and draft-quantization fields, none qualified here yet.
  DFlash2 (66.6 tok/s chat) is excluded outright: its image is built
  locally by the user, and a pack that pins image bytes cannot ship it
  (ADR 0012).

### 2. DeepSeek V4 Flash on ONE Spark (MiaAI-Lab One-DGX-Spark) — TRACKED, NOT ADDABLE YET
- What it is: `0xSero/deepseek-v4-flash-0731-spark` (EXL3 3.0bpw on a
  REAP-pruned K216 checkpoint, 216 of 256 experts, 106,862,968,575 bytes =
  99.5 GiB, fits one Spark), served by an NGC vLLM 26.02-based sparkinfer
  image (`ghcr.io/0xsero/deepseek-v4-flash-0731-spark-sparkinfer`, single
  tag, digest matches the launcher's pin). Launcher-measured: 44 to 47
  tok/s decode, 384k context, 439,622-token KV pool, exact needle recall at
  370k tokens. Roughly 3x our 3-bit GGUF single-Spark recipe.
- Why there is no recipe today, each blocker named:
  1. The working deployment bind-mounts six patch files over the pinned
     image: NaN-prefill fixes for the native 432-byte NVFP4 KV records, a
     DSpark draft KV-write fix, two b12x kernel backports. basement has no
     extra-mount primitive, deliberately (pinned image bytes only). Without
     the patches the default KV layout NaNs on any prompt of 7 or more
     tokens; the padded fallback layout avoids that but shrinks the KV pool
     to ~181k tokens and still hits blocker 2.
  2. The image's first-boot entrypoint coalesces the TP4 rank-sliced EXL3
     shards into a TP1 layout and builds the DSpark draft. basement
     replaces the entrypoint and launches vllm directly, so neither step
     ever runs. Serving the raw rank-sliced shards is not a configuration,
     it is a broken install.
  3. The image self-declares no provenance labels (no source, revision, or
     licence label; only inherited NVIDIA base labels). The Anemll image
     behind the two-Spark recipe self-declares all three. ADR 0012's
     primary-source bar is thinner here.
  4. The checkpoint's own card says end-to-end generation "has not yet
     passed" the publisher's validation; the launcher repo's stress tests
     are the only end-to-end evidence.
- Unblock paths: upstream publishes an image with the patches baked in plus
  a pre-coalesced TP1 checkpoint (their README says a newer kernel build is
  not published "yet"), or basement grows a verified extra-mount /
  first-boot-operation primitive. The second is an ADR-sized engine
  decision, owner's call, not a recipe detail.
- Meanwhile the pack keeps both existing DS4F recipes (two-Spark official
  weights, one-Spark 3-bit GGUF).

### 3. OBLITERATUS/Qwen3.8-27B-OBLITERATED — ADDED as `qwen38-27b-obliterated-q8-0-1s`
- Abliterated derivative of Qwen3.8-27B (weight-space ablation, refusal
  behaviour removed), by the OBLITERATUS org, card credits "Pliny the
  Prompter". Apache-2.0 with a verbatim LICENSE file at the pinned
  revision. Card's own quality claim: MMLU 82.33% vs 84.46% stock, its
  measurement, not ours.
- Path: Q8_0 GGUF (29,047,084,320 bytes) on the llama.cpp image the pack
  already pins. Architecture support verified by commit ancestry: qwen35
  merged in llama.cpp b7990 (2026-02-10), the pinned b10257 image builds
  2267 commits after the merge.
- Repo anomaly worth knowing: two overlapping bf16 safetensors shard sets
  (an orphaned 18-way split beside the live 29-file split); a
  whole-snapshot download would pull ~215 GB for a 29 GB serve. The recipe
  pins two files by name and size.
- No NVFP4 of the abliterated weights exists on HF (searched 2026-08-23).
- Curation note: the Ornith skip above ruled an abliterated community quant
  "not shippable as a default" where it would silently stand in for a clean
  model. This entry is different in kind: the owner asked for this model
  itself, it ships under its own name, and the recipe states what it is in
  plain words. Explicit, labeled, candidate.

## Round 5 — 2026-08-23, upstream drift the watcher parked

Both entries come from the first supervised feed-watch run. Both source
pins stay where validation happened; the rulings live in
docs/feed-acknowledged.yaml.

### Qwen3.6-27B DFlash (tracked, future recipe version)

- The MiaAI-Lab launcher repo abandoned the method our qwen36-27b-nvfp4-1s
  recipe validated (vLLM v0.24.0, MTP speculation, marlin MoE) and moved to
  DFlash speculative decoding with a separate drafter checkpoint
  (z-lab/Qwen3.6-27B-DFlash, 10 speculative tokens), flashinfer_b12x MoE,
  flash_attn attention, bfloat16 KV, gpu-mem 0.84. Then the repo itself
  moved: github.com/MiaAI-Lab/Qwen3.6-27B-NVFP4-DFlash-DGX-Spark.
- Not adoptable as published: the launcher runs an unpinned
  vllm/vllm-openai:nightly-aarch64 image with --privileged. basement pins
  images by digest and refuses privileged containers.
- Becomes a recipe version when: a pinned image digest serves it, the
  drafter checkpoint has a verified licence, and the DFlash gain is
  measured here (the laguna-s recipe is the in-pack pattern for a
  drafter-carrying recipe).

### DeepSeek V4 Flash DSpark, hardened stack (tracked, future recipe version)

- Upstream rewrote its history (our pinned commit 914c35b and today's main
  f104c39 share no ancestor; the pin still resolves) and grew a hardened
  stack: new kernel patches, a Dockerfile, CI, an AUDIT.md, changed
  launcher scripts.
- Our deepseek-v4-flash-0731-2s recipe implements the method validated at
  the pin; the model artifacts did not change (the 2s snapshot bump to
  7872f01 was metadata-only and shipped as version 4).
- Becomes a recipe version when: the rewritten tree's method is re-read
  end to end and the bind-mounted-patch objections recorded in Round 4 for
  the single-Spark variant are re-evaluated against the hardened stack.

## Round 6 — 2026-08-26, owner link (Qwen3.8-Flash-Next dual-Spark)

Trigger: the owner sent MiaAI-Lab/Qwen3.8-Flash-Next-Dual-DGX-Sparks.
Every fact verified live on 2026-08-26 (HF API, GitHub API, Docker Hub
registry manifests). The whole stack landed that same day: the base image
rebuilt 12:02 UTC, the checkpoint updated 12:28 UTC, the launcher
published 17:19-17:38 UTC.

### Qwen3.8-Flash-Next — ADDED as `qwen38-flash-next-nvfp4-2s` (first-party image)

- Model: `Qwen/Qwen3.8-Flash-Next`, a Qwen4-architecture preview
  (`qwen4_exp`): 176B MoE with about 6B active (launcher figure; the quant
  card rounds to ~180B), 48 layers alternating Gated DeltaNet and QSA
  sparse full attention, 512 routed experts with top-10 routing plus a
  shared expert, PLE n-gram embedding tables, in-checkpoint MTP draft
  layer, multimodal input (text, image, video), native context 262144,
  thinking on by default.
- Licence: "Qwen Community License 1.0" (LICENSE fetched verbatim
  2026-08-26). Permissive; attribution display required above 100M MAU or
  USD 20M monthly revenue; Model-as-a-Service and "AI Work Assistant"
  businesses need a separate licence for commercial use. No territory
  exclusions. Recipe sets required_licence_acceptance.
- Checkpoint: `RadixArk/Qwen3.8-Flash-Next-NVFP4`, sha `7b719225` on
  2026-08-26, 135,253,624,416 weight bytes per its own
  qualification-notes.md (NVFP4 W4A4 routed experts, FP8 PLE tables, BF16
  dense; 206 shards, 420 files). The card says "private candidate
  release" and defers licence to the base model. Publisher-measured
  quality in-tree: GSM8K 97.27 (reference band 97.12-97.50), AIME26
  pass@1 98.75. Not our measurements.
- Runtime: strictly two Sparks (135 GB does not fit one), SGLang TP=2
  over the ConnectX-7 link, NEXTN speculation (3 steps, topk 1, 4 draft
  tokens) over the in-checkpoint MTP head, nothing extra to download.
- The SM121 blocker and its resolution: the stock
  `lmsysorg/sglang:qwen38flashnext` image (index digest `12d3392b`,
  RadixArk overlay build with self-declared provenance labels) fails on
  GB10 because QSA resolves to flash-attn-4 CuTe kernels that fail MLIR
  compilation on SM121; the launcher's KERNEL_PATCH builds a local
  Triton-fallback derivative, which ADR 0012 cannot ship. Resolution:
  `packaging/qwen38-flash-next-image/` reproduces that build as a
  CI-published first-party image (the comfyui-image pattern), pinning the
  base by digest and extracting the launcher's fallback byte-identically
  (sha `195cebac`) at launcher commit `dccb035c`. The image also installs
  `nvidia-nccl-cu13==2.30.7`: the launcher preloads host-staged NCCL
  2.30.7 (its GLM-5.2 runbook pins >= 2.30.7 for CUDA graphs + TP on
  GB10 + CX7) and basement cannot mount host libraries, so the pinned
  wheel delivers the same library version by another path. Qualification
  decides whether the combination holds.
- Launcher-measured speeds on 2x GB10, TP=2, NEXTN (single source, its
  benchmark commit 74cb0fa8 of 2026-08-26; kept out of the console's
  reference numbers by the corroboration bar): 64.4 tok/s single stream
  at TTFT 117 ms, 116.8 tok/s aggregate over two streams, 114.1 over
  four.
- GB10 warning carried into the recipe comments: never probe this model
  with --load-format dummy; the fp16 copy of the ~26 GB/rank PLE table
  overcommits unified memory and hard-freezes the node (launcher observed
  it twice, full reboots).
