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

## Hold

### 3. Ornith-1.0-35B (DeepReinforce AI, Qwen3.5-MoE family)
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

## Strategic question raised by the sweep

The tweet's "one box" world is largely llama.cpp: both of its big-model
single-Spark stories (DeepSeek 3-bit, StepFun IQ4) only exist there. Our
runtime is vLLM-only. Options: (a) stay vLLM-pure and own the dual-Spark
story for big models (consistent with the site's "two sparks" flagship);
(b) add a llama.cpp runtime kind to the recipe schema later. Decision is
the owner/the designer's; no work started.
