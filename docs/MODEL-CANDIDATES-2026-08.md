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

## Round 7 — 2026-08-28, owner link (GLM-5.3-Flash EXL3 dual-Spark)

Trigger: the owner sent `MiaAI-Lab/GLM-5.3-Flash-EXL3-2x-DGX-Sparks`. Every fact
below was verified live on 2026-08-28 (GitHub clone and API, GHCR and Docker Hub
registry manifests, HF API, licence files fetched verbatim). Research method as
in Rounds 2, 4 and 6. Nothing here is a measurement of ours.

Same publisher as the Qwen 3.6, Qwen 3.8 and Qwen3.8-Flash-Next launchers. This
is the publisher's second GLM-5.3-Flash launcher; the other one
(`MiaAI-Lab/GLM-5.3-Flash-NVFP4-Dual-DGX-Spark`, NVFP4 with a Ray TP=2 executor)
is a different artifact and a different method, and is not assessed here.

## Sources, with pins

- Launcher: `github.com/MiaAI-Lab/GLM-5.3-Flash-EXL3-2x-DGX-Sparks` at commit
  `bd7f55edff9e37b41e1d32e2cf37054fe66d1e58` (merge of PR #9, 2026-08-28
  20:34:54 +0300). Repository created 2026-08-27T16:22:58Z. Repository licence
  MIT. `git rev-list --count HEAD` = 49, so the 49-commits claim is exact.
- Runtime image: `ghcr.io/miaai-lab/glm-5.3-flash-2x-dgx-sparks:exl3` =
  `sha256:9bb1557a4234fce63d59599e44d10747eabd742beb337eebf9e7070be8a0fd58`.
- Base image: `vllm/vllm-openai:glm53-flash-arm64-cu130` =
  `sha256:905c02933be6021301db2dc284e24e3727467aa3a0f63b41d609885778a07bce`.
- Served checkpoint: `Mia-AiLab/GLM-5.3-Flash-EXL3-TR3-4bpw`, pinned by the
  launcher at revision `25a44fdbf16862a46b7cc9921142c6c81350af2f`. Mirror main
  today is `024db9f7e9871e8efdf21538ba55af7442be3cd5`.
- Upstream of that mirror: `brandonmusic/GLM-5.3-Flash-tr3-4bpw`, main
  `792d065e90b3b3a9315cc93b396322ccd169da29`.
- Base model: `zai-org/GLM-5.3-Flash`, sha `04c4e9e95c5da8862dced7e5056455116f83a7e0`.
- Drafter: `incoai/GLM-5.3-Flash-DFlash2`, sha `7d74cdd881ed7e32c31175984a67823127b66cfe`.
- ExLlamaV3 source pin: `turboderp-org/exllamav3` commit
  `c5d9c657966ffeeaa9353f0cc899f18629da4a13` (its own config calls it 0.0.43).

## 1. Patch delivery, the fact that decides the verdict

This is **not** the Round 4 case. Almost everything is baked into the published
image at build time. One patch is not, and that single omission is the blocker.

Baked at image build (`Dockerfile` at the pinned commit):

- The whole NoPE sparse-MLA rewrite, as an inline Python heredoc that edits
  vLLM in `dist-packages` (lines 91 to 318), followed by a separate assertion
  stage that fails the build if any edit did not land (lines 320 to 367).
- `overlay/exl3.py` copied over
  `vllm/model_executor/layers/quantization/exl3.py` (line 433).
- **ExLlamaV3 `c5d9c657` is compiled at image build, not on the Spark** (lines
  415 to 428): the source tarball is fetched, `patch_exl3_ext_aarch64.py` stubs
  the AVX CPU allreduce so it builds on aarch64, and `pip install` runs with
  `TORCH_CUDA_ARCH_LIST=12.1a`. The layer then asserts
  `hasattr(exllamav3_ext, 'exl3_moe')`. The published image's own config blob
  carries `TORCH_CUDA_ARCH_LIST=12.1a`, which confirms the compile happened in
  the image and not at first boot.
- Executed at build (lines 447 to 452): `patch_model_overrides.py`,
  `patch_dflash2.py`, `patch_glm_eagle3.py`, `patch_glm5_drafter_group.py`,
  `patch_suppress_stops_in_reasoning.py`, `patch_scheduler_decode_floor.py`.

Not executed at build: **`patch_glm_video_placeholders.py` is copied in at line
442 and never given a `RUN` line.** It is the only patch in that state.

Bind-mounted at `docker run` and executed by the launcher's inner script. The
exact head lines (`start.sh` 891 to 930):

```
docker run -d --name "$CONTAINER_HEAD" \
    --gpus all --network host --ipc=host --shm-size 32g --stop-timeout 60 \
    --device /dev/infiniband --cap-add IPC_LOCK \
    --ulimit memlock=-1 --ulimit stack=67108864 \
    -v "$HF_CACHE_DIR:/root/.cache/huggingface" \
    ...
    -v "$HEAD_SCRIPT:/start.sh:ro" \
    -v "$CHAT_TEMPLATE_HOST:$CHAT_TEMPLATE:ro" \
    -v "$VIDEO_PATCH_HOST:/opt/glm53/patch_glm_video_placeholders.py:ro" \
    -v "$STOP_PATCH_HOST:/opt/glm53/patch_suppress_stops_in_reasoning.py:ro" \
    -v "$SCHED_PATCH_HOST:/opt/glm53/patch_scheduler_decode_floor.py:ro" \
    ...
    --entrypoint bash "$IMAGE" /start.sh
```

The worker run (lines 868 to 888) is the same, with the three patch files and
the template `scp`-ed to `/tmp` first. The inner script then runs, before
`exec vllm serve`:

```
if [ -f /opt/glm53/patch_glm_video_placeholders.py ]; then
    python3 /opt/glm53/patch_glm_video_placeholders.py
fi
if [ -f /opt/glm53/patch_suppress_stops_in_reasoning.py ]; then ...
if [ -f /opt/glm53/patch_scheduler_decode_floor.py ]; then ...
```

Reading of the three mounts:

- `patch_suppress_stops_in_reasoning.py` and `patch_scheduler_decode_floor.py`
  are already applied in the image. Re-running them is belt and braces; the
  scheduler patch checks its own marker and prints "already present, skipping".
  Mounting them lets a user edit behaviour without a rebuild. Neither is needed
  for a correct serve from the image alone.
- `patch_glm_video_placeholders.py` **is** needed, and it is not only about
  video. It does two things. It aligns video timestamp blocks to the encoder
  `grid_t`, and it disables GB10 `persistent_topk` in
  `sparse_attn_indexer_kpool.py` so long-history decode uses
  `top_k_per_row_decode`. The second is a text-path decode fix. The README says
  the same thing in its own words.

Why this is fixable rather than fatal. Run as `__main__`, that script copies
itself to `dist-packages/glm53_video_patch.py` and writes
`dist-packages/glm53_video.pth` containing `import glm53_video_patch`, then
edits the kpool file on disk and clears the stale `.pyc`. A `.pth` file in
site-packages is executed by `site` at every interpreter start, so the import
hook survives into every later process. **The patch therefore persists if it is
run at build time.** Adding one `RUN python3
/opt/glm53/patch_glm_video_placeholders.py` line to the Dockerfile makes the
image self-contained and removes the need for all three mounts.

Why basement cannot use the published image unchanged. basement replaces the
entrypoint and launches `vllm serve` directly (`internal/operations/docker.go`,
`vllmArgs` builds `serve /model ...`). The launcher's inner script never runs,
so the video and `persistent_topk` patch would never be applied. That is Round
4 blocker 2 in kind. It is also the whole of the problem here, and it is the
Round 6 answer that clears it.

## 2. The image

- `:exl3`, `:latest` and `:20260828-dflash2` all resolve to the one digest
  `sha256:9bb1557a4234fce63d59599e44d10747eabd742beb337eebf9e7070be8a0fd58`.
- Single manifest (`application/vnd.docker.distribution.manifest.v2+json`), not
  a multi-arch index. 52 layers, **9,788,994,117 bytes compressed**.
- Config blob `sha256:ad0cdd86d1ddd15ee758f519d16da15ac237f7f0648a5c52fbc20f9554944263`:
  architecture **arm64**, os linux, created **2026-08-28T10:46:05+03:00**, 117
  history entries whose base layers date to 2025-08-19.
- Provenance labels are inherited only, none first-party:
  `ai.vllm.build.commit=unknown`, `ai.vllm.build.pipeline=local`,
  `ai.vllm.image.tag=local/vllm-openai:dev`,
  `org.opencontainers.image.source=https://github.com/vllm-project/vllm`,
  `org.opencontainers.image.revision=unknown`, `maintainer=NVIDIA CORPORATION`.
  No label names MiaAI-Lab, the launcher commit, or a licence. This is the same
  thinness ADR 0012 flagged as Round 4 blocker 3. Note the base image itself
  self-declares `pipeline=local`, so the chain is thin one level down as well.
- Does the in-repo Dockerfile plausibly reproduce it? Yes. The base is pinned by
  digest and that digest is live (the Docker Hub tag
  `glm53-flash-arm64-cu130` currently resolves to exactly `905c0293...`), every
  patch input is in-tree, and ExLlamaV3 is pinned by commit. Inputs are fully
  determined. It is not byte-reproducible, because pip and nvcc are not, which
  is the same position as the Round 6 image.

## 3. The checkpoint

Which repo does the launcher actually download? `download.sh` is a one-line
`exec` of `start.sh download`. `start.sh` sets
`MODEL=Mia-AiLab/GLM-5.3-Flash-EXL3-TR3-4bpw` and
`MODEL_REVISION=25a44fdbf16862a46b7cc9921142c6c81350af2f`, and
`hf_download_repo` passes `--revision` only for `MODEL`. So **it fetches the
Mia-AiLab mirror at `25a44fdb`.** `brandonmusic/GLM-5.3-Flash-tr3-4bpw` is used
only as `MODEL_FALLBACK`, and only when the mirror yields fewer than
`EXPECTED_SHARDS=120` safetensors.

Live tree sums (HF API, recursive, exact):

| Repo | Files | Safetensors | Total bytes | GiB |
|---|---:|---:|---:|---:|
| `Mia-AiLab/GLM-5.3-Flash-EXL3-TR3-4bpw` | 144 | 120 | 175,715,854,761 | 163.65 |
| `brandonmusic/GLM-5.3-Flash-tr3-4bpw` | 559 | 120 | 175,789,306,517 | 163.72 |

The byte-identity claim in the README verifies. Restricted to safetensors, both
repos sum to **exactly 175,642,157,752 bytes**, with identical file names and
identical per-file sizes. The tree difference is metadata and the upstream's
extra `runtime-results/`, `src/`, `scripts/` and `docs/` trees, which
`start.sh` excludes by default through `HF_DOWNLOAD_EXCLUDE`. The README's
"~164 GiB" is consistent (163.58 GiB of safetensors). The KLD table's "176 GB"
row is the same number in decimal GB.

Revision drift worth recording: the launcher pins `25a44fdb`
(lastModified 2026-08-28T13:21:35Z) while the mirror main has already moved to
`024db9f7` (2026-08-28T14:25:14Z), about an hour later the same day. The
README describes the artifact by a third identifier, the upstream snapshot
`5ab363a8`, which also resolves. A recipe must pin one and say which.

Quantization format, from the checkpoint's own `quantization_config` (format
only): `quant_method` exl3, `bits` 4, `codebook` mcg, `head_bits` 16, `scope`
`glm53_routed_experts_only`, `non_routed_dtype_policy`
`official_source_native`, `version` 0.0.43. The same block carries
**`"serving_reader_qualified": false`**. The checkpoint declares itself not
qualified for serving readers. That is the Round 4 blocker 4 pattern and
belongs in any recipe comment.

Headroom against the KV pool claim. Two Sparks hold 256 GB unified, which is
238.4 GiB. At the recipe's `util 0.87` the budget is about 207.4 GiB. Weights
are 163.58 GiB, leaving about 43.8 GiB for KV, activations, the drafter and
runtime. The claimed 982,612-token pool at about 15.67 GiB plus the 2.18 GiB
drafter sits inside that with room. So the claim is arithmetically plausible at
cluster level. It does not reconcile at record level: 982,612 tokens times the
656 B packed record over the 11 full-attention layers is about 6.6 GiB, not
15.67 GiB. The kpool indexer also stores per-token compressed pool entries,
which is the likely remainder, but the launcher does not break the figure down
and I did not verify it. Carried as an open question, not as a fact.

## 4. The licence chain, all three layers

### (a) EXL3 weights: ShapleyMCG License 1.0

Fetched verbatim from the pinned revision. 29,918 bytes, and **byte-identical**
to the file on the brandonmusic upstream. Full title: "SHAPLEYMCG LICENSE,
Attribution-Required, Source-Available License with Named Exclusion, Version
1.0, August 2026". Copyright 2026 Brandon M. Music.

The name is unusual and the file is genuinely unusual. What it actually says:

- **Commercial use is permitted.** Section 2.1 grants a "worldwide,
  royalty-free, non-exclusive license to use, reproduce, study, modify, create
  Derivatives of, distribute, publicly display, publicly perform, and otherwise
  exploit the Work, for any purpose, including commercial purposes."
- **No territory exclusion. No revenue gate. No user-count gate.** Nothing
  resembling the MiniMax H3 excluded-territories clause that disqualified that
  model in Round 3.
- **Attribution is a condition of the grant, not a courtesy.** Section 3.1
  requires retaining the licence in full, the copyright notice and the
  Attribution Notice in every copy and every Derivative, "in a location
  reasonably likely to be seen by recipients". Section 3.2 requires "clear and
  prominent attribution", naming the Work, linking the canonical repository, in
  any model card, README or documentation, and "where the platform supports it,
  in the artifact's metadata (for example, `license`, `base_model`, or `tags`
  fields)". Section 3.3 requires keeping the provenance fields the Work writes
  into artifacts. Section 2.4 makes Sections 3 and 5 conditions on scope, so
  use outside them is unlicensed rather than merely a breach.
- **A named Excluded Party.** Sections 4 to 6 grant nothing to "the natural
  person who ... uses or has used the online persona **0xSero**", including
  `github.com/0xsero` and `huggingface.co/0xSero`, and follow that person
  through any renaming. Section 5.1 conditions your own grant on not
  distributing the Work or any Derivative to that party and not "knowingly
  permit[ting] the Excluded Party to obtain it through You". Section 5.2
  forbids publishing it through any channel that party controls.
- Section 7.1 terminates automatically on any breach of Section 3 or Section 5.
  A first, non-wilful Section 3 breach is curable within 30 days. A Section 5
  breach is not curable.
- Section 10.1 sets governing law as Kentucky, venue Eastern District of
  Kentucky. Section 10.5 states plainly that the licence is not OSI approved and
  that the Work "should not be described as open source without qualification".
- Schedule A is an eleven-instance record of alleged unattributed reuse of the
  licensor's prior calibration corpus and TR3 encoder bundle.

Basement-relevant note: the Excluded Party is the publisher of
`0xSero/deepseek-v4-flash-0731-spark`, the single-Spark DeepSeek EXL3 checkpoint
the pack already tracked in Round 4. The pack has referenced that publisher
before.

### (b) Base model: genuinely MIT

`zai-org/GLM-5.3-Flash` declares `license: mit` and ships a LICENSE file. Text
fetched verbatim: standard MIT, "Copyright (c) 2026 Z.AI Co., Ltd".

The brief asked whether a quant imposing a more restrictive licence than an MIT
base deserves a suspicious note. It does deserve the check, and the check
passes. The licence scopes itself explicitly. Section 1.1: "The Work does not
include, and this License neither licenses nor restricts, third-party
components on which the Work builds or with which it interoperates, including
the EXL3 format and exllamav3, REAP, and any base model, each of which remains
under its own license." Section 1.2 then makes any checkpoint "produced in
whole or in part by running the Work" a Derivative. So the claim is over the
calibration corpus, recipe, schema and encoder, not over Z.AI's weights. That
is a coherent position rather than a licence grab. The practical effect stands
either way: the artifact as published carries ShapleyMCG terms, and the mirror
card declares `license: other` with a `shapleymcg` tag.

### (c) DFlash2 drafter: CC BY-NC-ND 4.0

- Lives in a **separate HF repo**, `incoai/GLM-5.3-Flash-DFlash2`, not inside
  the image. 4 files, **2,342,175,855 bytes** (2.18 GiB), card front matter
  `license: cc-by-nc-nd-4.0`, 74 likes, created 2026-08-27.
- **It is fetched by default.** `SPEC_METHOD` defaults to `dflash`, and
  `start.sh` downloads and rsyncs the drafter on that path.
- **`SPEC_METHOD=mtp` avoids it entirely.** `download_dflash()` returns on its
  first line unless `SPEC_METHOD=dflash`, and the MTP branch emits
  `--speculative-config {"method":"mtp","num_speculative_tokens":2}` with no
  model path.
- **MTP needs no extra artifact.** The base config carries
  `num_nextn_predict_layers: 1`, so the draft layer ships inside the
  checkpoint, exactly as in the Round 6 NEXTN case.
- Non-commercial and no-derivatives means the default serving path is
  research and evaluation only. **If the pack ships this model, the MTP path is
  the recommendation.**
- What MTP costs, in the launcher's own numbers: MTP k=2 baseline about **24.6
  tok/s** against DFlash2 structured **61.7** and prose **26.9**. So MTP gives
  up most of the gain on structured and code work, and very little on prose.

## 5. Model facts for the catalog line

**Correction to the brief's summary: the 193B figure is not supported by any
primary source and must not be printed.** The model card states: "With **320B
total parameters and just 18B active parameters**, it outperforms GLM-5.2
across benchmarks and real-world workloads at one-tenth the price, while
approaching Claude Opus 4.8 on coding and agentic benchmarks." The launcher's
own `start.sh` agrees, calling it "a 320B MoE" in its health-wait message.

- Maker: `zai-org` is Z.AI (Zhipu). The LICENSE copyright reads "Z.AI Co., Ltd".
- Architecture, read from the base `config.json` at the pinned sha:
  `Glm5NextForConditionalGeneration`, text tower `glm5_next_text`, **45 layers**,
  hidden size 4096, **288 routed experts**, **8 experts per token**, **1 shared
  expert**, `moe_intermediate_size` 2048, `first_k_dense_replace` 3,
  `scoring_func` sigmoid, `topk_method` noaux_tc.
- Hybrid attention, from `linear_attn_config`: **34 KDA linear-attention layers
  and 11 full-attention layers** at indices 3, 7, 11, 15, 19, 23, 27, 31, 35,
  39, 43. The card calls this the GLM series' first hybrid sparse-plus-linear
  architecture.
- **NoPE MLA is verified in the config itself, not inferred from the README**:
  `mla_use_nope: true`, `qk_rope_head_dim: 0`, `kv_lora_rank: 512`,
  `q_lora_rank: 1536`, `v_head_dim: 256`. That is exactly the geometry the
  overlay zero-pads into the 576-wide GLM_NSA record.
- Sparse indexer: `index_topk` 2048, `index_kpool` 4,
  `index_kpool_always_select_tail` true, `index_n_heads` 32, `index_head_dim`
  128. These are the values the Dockerfile's analysis reasons about.
- `mhc: true`, `hc_mult` 4, `hc_sinkhorn_iters` 20. The card names it
  Manifold-Constrained Hyper-Connections.
- **Context: `max_position_embeddings` = 1,048,576. Native is 1M.** The
  launcher's `--max-model-len 900000` is therefore a **reduction below native
  for memory reasons, not an extension**. The README states "Native 1M still
  does not allocate". The Round 6 lesson applies here in the opposite
  direction from Round 6.
- **Multimodal surface: image and video IN, text OUT.** `vision_config` is
  present (depth 24, image size 448, patch size 14, out hidden size 4096) and
  the top level declares `image_token_id`, `video_token_id`, and the image and
  video start and end token ids. The launcher serves
  `--limit-mm-per-prompt {"image":4,"video":1}`. The card calls it "the first
  natively multimodal model in the GLM-5 series". There is no media output.
- Thinking: the card documents a `reasoning_effort` parameter taking `low`,
  `high` or `max`, defaulting to `max`, and `clear_thinking` defaulting to
  false. The launcher's template defaults thinking on and documents
  `chat_template_kwargs: {"enable_thinking": false}` as the way off.
- Tool use: the launcher runs `--tool-call-parser glm47
  --enable-auto-tool-choice --reasoning-parser glm45`.
- Official release format (format only): the base `config.json` carries a
  `quantization_config` with `"fmt": "e4m3"` and dynamic activation scaling,
  plus a `modules_to_not_convert` list. Its published tree is 328,337,455,672
  bytes over 62 shards.
- Publisher's own training claim: a 30T-token multimodal pre-training corpus.
  Not ours, not verified.

## 6. The two-node method, against what basement does today

Launcher method: rank 0 on the head with the API, rank 1 on the worker with
`--headless`. Both ranks get `--tensor-parallel-size 2 --nnodes 2 --node-rank N
--master-addr $HEAD_IP --master-port 29521 --distributed-executor-backend mp`.
No Ray, no torchrun. NCCL over CX7 with `NCCL_SOCKET_IFNAME` and
`GLOO_SOCKET_IFNAME` set to `enp1s0f1np1` on the head and `enp1s0f0np0` on the
worker, `NCCL_IB_HCA` `rocep1s0f1` and `rocep1s0f0`, `NCCL_IB_GID_INDEX=3`,
`NCCL_NET=IB`, `NCCL_NET_PLUGIN=none`, `NCCL_IB_DISABLE=0`,
`NCCL_IB_ROCE_VERSION_NUM=2`, NVLS and CUMEM off, `VLLM_HOST_IP` per rank. API
on port 8888 on the head. `USE_HOST_NCCL=0` keeps the image's own NCCL; the
launcher warns that LD_PRELOADing the host 2.30.7 build makes DeepEP assert a
duplicate NCCL. Passwordless SSH from head to worker is required, and the head
drives the worker over `ssh`, `scp` and `rsync`.

**Does basement support two-node vLLM at all? Yes, and it emits these exact
flags already.** `internal/operations/docker.go` has `vllmDistributedArgs`,
whose comment reads: "the two-node launch flags the community DGX Spark recipe
uses: plain vllm serve with --nnodes/--node-rank/--master-addr/--master-port,
the multiprocessing executor, and --headless on the rank that serves no HTTP.
There is no ray and no torchrun." `RankBindsHostPort` already knows a vLLM
worker binds no host port. `serveEndpointArgs` binds the distributed head on
127.0.0.1 so the authenticated proxy stays the only path (ADR 0007). The
in-pack two-Spark vLLM recipe is `deepseek-v4-flash-0731-2s`;
`inkling-small-nvfp4-2s` and `qwen38-flash-next-nvfp4-2s` are the SGLang
two-node pattern.

NCCL wiring is also already solved. `Topology.Interconnect` carries `kind`,
`master_port` and `shared_environment` / `head_environment` /
`worker_environment` maps, and `fabric.go` detects the live cable per rank at
deploy time and overrides `NCCL_SOCKET_IFNAME` and `NCCL_IB_HCA`. The
launcher's fixed interface names become fallbacks, which is exactly what
`qwen38-flash-next-nvfp4-2s` already documents.

What basement does not need at all: the SSH and SCP orchestration, the
`rsync` of the HF cache to the worker, and the `docker save | ssh docker load`
image shipping. basement stages artifacts and images per node through its own
fleet path.

One policy conflict, and it is not a schema gap. The launcher runs
`--ipc=host --shm-size 32g`. ADR 0006 decision 3 removed `IpcMode: host`, and
its 2026-08-13 amendment records that the distributed-container branch briefly
reintroduced it, that it was removed again, and that both two-Spark recipes
were version-bumped "so ranks share the fabric through host networking and RDMA
only". basement already emits `CapAdd: IPC_LOCK` and the memlock ulimits. So
this recipe must run on `shm_bytes` like its two-Spark siblings, and whether
that is sufficient is a qualification question.

## 7. Basement schema gap list, flag by flag

Already expressible in `VLLMConfig` or the engine: `--served-model-name`,
`--host`, `--port`, `--tensor-parallel-size`, `--nnodes`, `--node-rank`,
`--master-addr`, `--master-port`, `--distributed-executor-backend mp`,
`--headless`, `--gpu-memory-utilization` (string, so 0.87 arrives unrounded),
`--max-model-len`, `--max-num-seqs`, `--max-num-batched-tokens`,
`--kv-cache-dtype`, `--enable-prefix-caching`, `--tool-call-parser`,
`--reasoning-parser`, `--enable-auto-tool-choice`, and the `--speculative-config`
members `method`, `num_speculative_tokens`, `model` (via an artifact role) and
`draft_sample_method`.

Missing, exact:

| Launcher flag or setting | Basement status |
|---|---|
| `--quantization exl3` | **MISSING.** `VLLMConfig` has no `Quantization` field at all. `SGLangConfig` has one; the vLLM path does not. Required, and the README forbids `marlin`. |
| `--speculative-config` member `kv_cache_dtype: "auto"` | **MISSING.** Required on the DFlash2 path: the dense drafter cannot use the target's `fp8_ds_mla`. |
| `--speculative-config` member `rejection_sample_method: "standard"` | **MISSING.** |
| `--speculative-config` member `draft_tensor_parallel_size: 1` | **MISSING.** Keeps the 2.18 GiB drafter on rank 0, off CX7 per draft step. |
| `--cudagraph-capture-sizes 1 2 4 8 16 24 32` | **MISSING.** basement has `MaxCUDAGraphCaptureSize`, a single integer ceiling, not an explicit list. The launcher's list differs between the DFlash2 and MTP paths. |
| `--limit-mm-per-prompt {"image":4,"video":1}` | **PARTIAL.** `MultimodalImageLimit` marshals `{"image":N}` only. No video limit. |
| `--skip-mm-profiling` | **MISSING.** The launcher calls it required, because a max-size image and video dummy profile OOMs this UMA. |
| `--chat-template /opt/glm53/chat_template.jinja` | **MISSING as used.** `ChatTemplateFile` resolves to `artifactMountPath("primary") + "/" + file`, a file inside the checkpoint. The launcher's template is an image-resident file, and the README states the checkpoint's own jinja is language-only. An image-path template is not expressible. |
| `--no-enable-flashinfer-autotune` | **PARTIAL.** `FlashInferAutotune` only adds the positive flag. The negative form cannot be expressed. May be redundant, since the Dockerfile also patches autotune out of `kernel_warmup.py`. |
| EXL3 quant marker for catalog and UI | **MISSING.** No `exl3` value exists on the vLLM path. |
| Overlay env (`GLM53_SUPPRESS_STOPS_IN_REASONING`, `GLM53_MIXED_PREFILL_CHUNK`, `EXL3_FUSED_MOE`, `TORCH_CUDA_ARCH_LIST`, `PYTORCH_CUDA_ALLOC_CONF`, `VLLM_EXECUTE_MODEL_TIMEOUT_SECONDS`) | **PRESENT, needs wiring check.** `Interconnect` carries three env maps and `Runtime` carries an `Environment` map. The mechanism exists; confirm it reaches a non-distributed-specific path. |
| Drafter pin as a roled artifact with its own licence | **PRESENT.** `SpeculativeModelRole` plus `artifacts[].role` already exist, and `laguna-s-2-1-nvfp4-dflash-1s` is the in-pack drafter precedent. Artifact-level `licence`, `licence_url`, `licence_repository` and `licence_revision` all exist. |
| `required_licence_acceptance` | **PRESENT.** |

Not needed for this configuration: `--enforce-eager` (the recipe runs
`ENFORCE_EAGER=0`) and `--language-model-only` (`LANGUAGE_MODEL_ONLY=0`).

## 8. Traction and recency

- 80 stars, 1 fork, 1 watcher, 2 open issues. Created 2026-08-27T16:22:58Z,
  last push 2026-08-28T17:34:56Z. The whole project is about 25 hours old.
- 49 commits, matching the claim exactly.
- Branches: `main`, `ablit`, and three `fix/` branches. The optional ABLIT
  refusal edit (`ABLIT=1`, an o_proj weight edit) was deliberately moved off
  `main` onto its own branch, and a commit exists specifically to stop the
  local ablit directory shipping. Good hygiene, and relevant to the Round 4
  curation note about abliterated derivatives.
- Contributors: MiaAI-Lab 47, `chriswritescode-dev` 1, `rwl4` 1.
- **Others besides the author do report running it.** Ten issues and PRs in
  about 25 hours, from six accounts that are not the publisher: `rwl4` (PR #1,
  thinking-off chat template fix, merged), `Charlie-Louis` (#5),
  `Acermax` (#6), `arthur-drozdov` (#7), `rdamron` (#8), `d3vilbug` (#10).
- **Three of them cite the exact GHCR digest I resolved**, which independently
  confirms the pin. Acermax (#6) reports on 2x GB10 at TP=2 over CX7 and RoCE,
  quoting recipe commit `4676496`, GHCR manifest `sha256:9bb1557a...`, image id
  `sha256:ad0cdd86...`, vLLM `0.1.dev20051+g487ecf187`, and target
  `brandonmusic/GLM-5.3-Flash-tr3-4bpw @ 5ab363a8`. d3vilbug (#10) reports
  production concurrent load at commit `f3043c9` with the same digest.
  rdamron (#8) reached the image-ship step on his own pair.
- Open issues that bear on serving quality: **#10 (open), tool calls sometimes
  emitted with blank or missing required arguments under concurrent load**, and
  explicitly not reproducible by replaying the same request alone. **#7 (open),
  prefix-cache hits collapse on long-running agentic workloads.** #6 was closed
  by the mixed-prefill scheduler fix (`GLM53_MIXED_PREFILL_CHUNK=skip`). #8 was
  closed by PR #9, which is today's HEAD.
- X: the publisher account is `x.com/MiaAI_lab`. A public post announcing the
  initial release describes "256k context, 500k+ kv" with full video and image
  input, which is an **earlier** configuration than today's 900k and 982,612.
  I did not fetch the post directly, because X blocks unauthenticated fetch and
  the brief forbids credentials. Treat that wording as search-surface only.
- NVIDIA Developer Forums thread 381429 covers GLM-5.3-Flash on 2x DGX Spark
  but measures a **different artifact**: tonyd615's LibertAIDAI NVFP4 quant at
  182 GiB, 21.8 tok/s decode with the native MTP head, 262,144 context, a 507K
  KV pool, and a TP4 bonus figure of 35.7 tok/s. RandomLlama reports 46.9 at C1
  on a vLLM path and 29.4 at C1 on SGLang with bf16 KV. **None of these
  measure the EXL3 4bpw stack, so none of them corroborate the launcher's
  numbers.**

## 9. Performance numbers, attribution, and which package they belong to

Every number in this section is **the launcher's own measurement on its own 2x
GB10 kit**, and every one is **single-source**. By the Round 6 corroboration
bar they stay out of the console's reference numbers and out of any UI surface.

The config package they belong to, at commit `bd7f55ed`: EXL3 4bpw weights,
DFlash2 k=7, TP=2 over CX7, `--max-model-len 900000`, util 0.87,
`--max-num-seqs 4`, `--max-num-batched-tokens 1024`, fp8 KV, CUDA graphs on,
fused EXL3 MoE, temperature 0, thinking off, 400 tokens, warm and empty KV.

sparkDash Decode bench, Structured (count 1 to 200) and Code prompts:

| Concurrency | TTFT | Stream tok/s | Aggregate tok/s |
|---|---:|---:|---:|
| x1 | 719 ms | 62.9 | 62.9 |
| x2 | 6.62 s | 51.7 | 103.3 |
| x4 | 6.30 s | 37.1 | 146.5 |

Lab `tests/bench_decode.py`, median of 5 runs of 400 tokens at C1: Structured
**61.7 tok/s** at **0.918** accept and 6.43 accepted per step; Prose (hash-map
explanation) **26.9 tok/s** at **0.332** accept and 2.33 per step. Long context
and mixed at roughly 60k to 100k KV: 24 to 27. MTP k=2 baseline about 24.6.

Per-position accept, structured: 0.98, 0.98, 0.94, 0.94, 0.91, 0.83, 0.83.
Prose: 0.75, 0.58, 0.41, 0.28, 0.16, 0.09, 0.06. Pinning
`attention_backend=TRITON_ATTN` dropped structured to about 29 tok/s at 0.31
accept.

KLD panel: **not the launcher's own measurement.** It is credited to `malaiwah`
in discussion #1 on the brandonmusic repository, five cold runs, 25 sealed
windows, 51,175 positions, KLD(teacher || model). EXL3 4bpw **0.024555** at
176 GB against official FP8 on the same stack **0.024629** at 328 GB, so
4bpw matches that FP8 row at about **54 percent** of the bytes. TR3 K6 6bpw is
0.013723 at 254 GB and NVFP4 is 0.060535. Second-hand: I read the launcher
README quoting malaiwah, not the discussion itself.

Two cautions before any of this is repeated:

1. The x2 and x4 TTFT figures (6.62 s and 6.30 s) are an order of magnitude
   above the x1 719 ms, and the README does not explain the jump. Given issue
   #6, mixed prefill and decode interaction is the likely cause. Do not present
   these as a clean concurrency story.
2. A third party contradicts the concurrency picture. Acermax (#6) measured the
   active lane collapsing from about 51 to 55 tok/s down to **5.00 tok/s** when
   a cold 100K prefill is admitted beside a decoding 100K request, with zero
   prefix-cache hits and zero preemptions. The launcher's own long-context band
   is 24 to 27. Concurrency behaviour under long context is not settled, even
   after the scheduler fix that closed the issue.

Two internal disagreements at one commit, recorded per the Round 6 lesson:

- The README's recipe table states a **982,612**-token KV pool. The README's
  own prefix-caching section states "This boot's pool was **926,373** tokens
  (CUDA-graph memory profiling; recipe table above is 982,612)". Both describe
  the same configuration.
- `start.sh` pins the checkpoint at revision `25a44fdb`; the README describes
  the artifact as the upstream `5ab363a8` snapshot; the mirror's main has moved
  to `024db9f7`. The safetensors are byte-identical across mirror and upstream,
  so this is a naming and durability question, not a bytes question.

## Verdict

**ADDABLE VIA FIRST-PARTY IMAGE**, on the Round 6 pattern. Not addable on the
launcher's published image as it stands.

Reasons, in order of weight:

1. The blocking mechanism is one missing Dockerfile line.
   `patch_glm_video_placeholders.py` is the only patch with no build-time
   execution, and it carries a text-path decode fix (GB10 `persistent_topk`)
   as well as the video alignment. It persists by writing a `.pth` into
   site-packages, so `RUN python3 /opt/glm53/patch_glm_video_placeholders.py`
   at build reproduces it exactly. Everything else is already baked, including
   the ExLlamaV3 `c5d9c657` compile, which happens in the image and not on the
   Spark.
2. basement cannot use the stock image, because it replaces the entrypoint and
   execs `vllm serve`, so the launcher's inner script and its three runtime
   patch invocations never run.
3. The published image carries no first-party provenance labels, and its base
   self-declares `pipeline=local`. A basement CI image fixes provenance at the
   same time, exactly as `packaging/qwen38-flash-next-image` did in Round 6.
4. The distribution method needs nothing new. basement already emits the
   launcher's exact two-node vLLM flags, and already resolves the CX7 fabric
   per rank.
5. The schema needs a small, bounded set of additions, led by `--quantization`
   on the vLLM path.

Blockers to clear, exact:

- **B1.** First-party CI image: base pinned at `sha256:905c0293...`, ExLlamaV3
  pinned at `c5d9c657`, the video and `persistent_topk` patch baked with a
  `RUN` line, published with self-declared provenance labels.
- **B2.** `VLLMConfig.Quantization` field, to emit `--quantization exl3`.
- **B3.** Speculative-config members `kv_cache_dtype`,
  `rejection_sample_method` and `draft_tensor_parallel_size`, needed only if the
  DFlash2 path ships.
- **B4.** Explicit `--cudagraph-capture-sizes` list.
- **B5.** Video limit in `--limit-mm-per-prompt`, and `--skip-mm-profiling`.
- **B6.** An image-resident chat template path.
- **B7.** Licence surface: ShapleyMCG attribution display in the recipe, the
  card and the console, plus an owner decision on the drafter.
- **B8.** Hardware qualification, which must cover the two open correctness
  issues (#10 blank tool arguments under load, #7 prefix-cache collapse) and
  the concurrency cliff from #6, and must run without host IPC.

Recommended shape if it ships: **the MTP path as the default**, because it is
commercially clean, needs no extra artifact and drafts from the in-checkpoint
layer. DFlash2 becomes an explicit opt-in, or is excluded. State plainly that
MTP costs the structured and code speed (about 24.6 against about 61.7 tok/s,
launcher's own numbers) and costs little on prose (24.6 against 26.9).

## Open questions

1. **The Section 5 distribution condition.** Does shipping a recipe that points
   at these weights, inside a public product, satisfy "must not knowingly
   permit the Excluded Party to obtain it through You"? basement does not host
   the bytes, but it does distribute the pointer and the method, and it cannot
   control who installs a recipe. Owner's call, and possibly counsel's. Note
   the Excluded Party published a checkpoint the pack already tracked in Round 4.
2. **The KV pool arithmetic.** 982,612 tokens at "~15.67 GiB" does not
   reconcile with the 656 B record over 11 full-attention layers (about 6.6
   GiB). The kpool indexer's per-token compressed entries are the likely
   remainder, but the launcher does not break it down.
3. **Which pool number is real**, 982,612 or 926,373, at one commit.
4. **Which revision to pin**: `25a44fdb` (what `start.sh` fetches), `024db9f7`
   (mirror main), or the upstream `5ab363a8` the README names.
5. **`serving_reader_qualified: false`** in the checkpoint's own
   `quantization_config`. What does the publisher mean, and does a later
   revision retract it?
6. **Is `--no-enable-flashinfer-autotune` load-bearing**, or redundant beside
   the Dockerfile's own edit to `kernel_warmup.py`?
7. **Does `--ipc=host` matter here?** The launcher sets it, ADR 0006 forbids it,
   and the existing two-Spark recipes run without it. Qualification decides.
8. **Project age.** The whole stack is about 25 hours old, with two open
   correctness issues and an upstream that rewrote its weight-mirror target
   twice in that window. Round 5 recorded what upstream drift costs. A pin
   taken today may not survive the week.
