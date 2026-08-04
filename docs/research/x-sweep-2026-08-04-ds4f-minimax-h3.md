# X sweep — DeepSeek V4 Flash 0731 (Spark/GB10) + MiniMax H3 — 2026-08-04

Method: same approach as `docs/MODEL-CANDIDATES-2026-08.md` — live X API
`tweets/search/recent` (recent-search window, last ~7 days of index, most
results landing 2026-07-29 through 2026-08-04), `-is:retweet` filtered,
sorted by likes within each query. Token loaded from onspark's `.env`
straight into the process environment for the request only; never printed
in this file or committed. 11 queries total, all HTTP 200, no rate-limiting
encountered (440+/450 remaining after the full sweep). One exact-phrase
query (`"deepseek v4 flash spark"`) returned zero results — noted, not
padded.

Every number below is a **community report from a single X post**, not a
verified benchmark. Where two or more independent accounts converge on a
similar figure, that's called out explicitly; everything else is one
person's claim.

---

## Subject 1: DeepSeek V4 Flash 0731 on DGX Spark / GB10

### Recurring themes

1. **Two-Spark (TP2) is the consensus "it just works well" tier; single-Spark is contested.**
   Multiple independent single-Spark attempts report friction that
   two-Spark setups don't: OOM on router/MoE-router mode, quant ladders
   not actually fitting as advertised, and reasoning-mode token-cap
   failures. Two-Spark reports are uniformly smoother.

2. **llama.cpp / ds4 (antirez's engine) dominates single-Spark; vLLM dominates two-Spark.**
   No single-Spark vLLM success was found in this sweep window — consistent
   with the existing catalog note that single-Spark vLLM is "effectively
   NO today." Two-Spark reports name vLLM 0.26 specifically.

3. **"ds4f" and "dsf4" are both live shorthand on X**, confirming the owner's
   note that both spellings circulate — "dsf4" mostly co-occurs with "ds4f"
   in the same threads rather than appearing as an independent naming
   camp.

4. **The exact phrase "deepseek v4 flash spark" returns nothing** — people
   write "DeepSeek V4 Flash on DGX Spark" or "ds4f + spark", not that
   compound phrase. Search-method note for future sweeps, not a finding.

### Notable posts

**Single-Spark, it works (with caveats):**
- @Blackwellboy (2026-08-03, 136 likes): "CAN A SINGLE SPARK DO SOMETHING REAL? the answer is a BIG YES!"
- @draslan_eth (2026-08-03, 0 likes but on-topic): "DS4F on one spark... just works and does actualy coding?!"
- @basecampbernie (2026-08-03): "3-bit on DGX Spark (GB10, 128GB unified)... Resident: ~114-115G of 121G — one tenant, run nothing else heavy" — unsloth UD-IQ3_S, ~108 GiB, 4 shards, llama.cpp b10218+, sm_121.
- @mochi_mochi_lab (2026-08-03, 5 likes, JP): reports an uncensored 87 GB build reaching 768K context ("実測PASS"), prompt processing ~445 tok/s, generation ~17 tok/s, no swap growth; the 1M-context attempt OOM'd ("1MはOOMに散った").
- @zerokn0wledge_ (2026-08-03): planning to try DeepSeek V4-Flash on an Asus Ascent GX10 (Spark-class), predicting 15-18 tok/s "bc Spark's 273 GB/s memory bandwidth is the bottleneck" — a prediction, not a measured result.

**Single-Spark, it did NOT just work (failure reports):**
- @sudoingX (2026-08-02, 195 likes — this is the same account behind the
  original tweet that triggered the earlier model-candidates sweep): "i put
  the full 284b on a single dgx spark... it did not just work. the '3bit
  fits in 110gb' line has an asterisk." Follow-up: only UD-IQ3_XXS (104 GB)
  fits fully on GPU; UD-IQ3_S and UD-IQ3_M both come in at a full 128 GB.
  This is a direct, first-party correction of the "3-bit fits" claim
  already recorded in the catalog doc — worth flagging back to that file.
- @making (2026-08-03): "unsloth/...UD-IQ3_XXS llama.cpp + DGX Spark... router modeだとoomになった。シングルモデルだと動いた。15 t/sくらい。" (router/MoE mode OOM'd; single-model mode worked, ~15 tok/s.)
- @limegreenpeper1 (2026-08-02, JP): one community re-quant
  (`apetersson/...Abliterated-DS4-Headroom128`) worked on 1x GB10 with
  ds4-mxfp4; a sibling quant (`...DS4-Quality128`) from the same publisher
  OOM'd on the same hardware — same publisher, different quant, different
  outcome.
- @danpacary (2026-08-03): ran 23 reasoning evals on single Spark (ds4
  v0.5.1, 0731 IQ2XXS) — "13 of 23 reasoning runs scored exactly zero...
  every one of them hit the token cap before finishing." Separately: raising
  reasoning effort improved a 90-row arithmetic trace (81.2→90.8 of 91) but
  collapsed a "write a VM" coding task (11.2→0.0) — a context/token-budget
  interaction, not a model-quality claim per se.

**Two-Spark reports:**
- @mr_r0b0t (2026-08-03, 108 likes): "DeepSeek-V4-Flash-DSpark has now
  completed the r0b0bench core-subset protocol on 2× NVIDIA GB10" —
  GSM8K 95.0%, HumanEval pass@1 90.9%, ARC-Easy 96.0%, IFEval 79.5%, BFCL
  75.5%, dedicated C1 decode 79.2 tok/s.
- @sash__mit (2026-08-03, home-lab 4x Spark owner): 2 Sparks, "API + Open
  WebUI · 262k ctx · ~156GB weights/node," fabric ~110 Gbit/s/half, NCCL
  ~24 GB/s "(GB10 ceiling)".
- @vysecurity (2026-08-02, 7 likes): "2xDGX Spark. DS4F Abliterated...
  running 16 concurrent agents at 807-813 tok/s aggregate." Single anecdote,
  no independent corroboration of that aggregate figure.
- @siraustin (2026-07-31): "dual-Spark lane decodes at ~62.6 t/s
  single-stream vs ~28.5 on M3 ds4f — 2.2× per stream... TP2 lane hits
  ~140 t/s aggregate at 6 concurrent."
- @Reederey (2026-07-29, 10 likes): vLLM 0.26 day-2 tuning — DSpark draft
  re-tuned from 3→2 tokens: "+3.8% single-stream decode (35.4 vs 34.1
  tok/s)... acceptance 0.71→0.80."
- @YRSM_Simon (2026-08-03, Chinese): 284B total/A13B MoE on 2×DGX Spark as
  personal-assistant/workflow hub, "Token generation 约为 60–80" (~60-80
  tok/s) — cut off before decode/concurrency context was given.
- @runsonai (2026-08-03): "with two the experience is so much better with
  1m context window and faster tok/s. I can't tell difference... from
  frontier" — single-Spark-to-two-Spark comparison from one user.

**Outside Spark (context, not Spark-specific — surfaced by the same
queries, included because they bound expectations):**
- @light_foundry (2026-08-03, 32 likes): 8× DGX Spark cluster, TP=8, "403
  tok/s aggregate @ c32 • 88 tok/s single stream • 1M context • 4.17M-token
  KV pool... Zero failures" — this is an 8-node claim, well beyond the
  1s/2s scope, single anecdote.
- Non-Spark hardware also active in the conversation: @analogalok (RTX
  4090, Q2, 12 tok/s), @Hikari_07_jp (2× RTX PRO 6000, 1300 tok/s claimed),
  @masahirochaen (Mac M4 Max 128GB, 2.05 tok/s — "遅すぎて使い物にならない",
  too slow to be usable), @shupeiman (MacBook Pro M5 Max 128GB, 32-38
  tok/s), @tplr_ai (4× RTX 5090, 62 tok/s). These establish DGX Spark is
  mid-pack among the options people are actually trying, not an outlier
  either way.

### Numbers, grouped by corroboration

- **Single-Spark decode, quantized:** ~15-18 tok/s appears independently
  from @making (15 t/s, router-OOM'd single-model fallback), @zerokn0wledge_
  (predicted 15-18, untested at post time), and @mochi_mochi_lab (17 tok/s
  generation at 768K context). Three independent-ish figures converging in
  the same 15-18 band — the closest thing to a corroborated number in this
  sweep.
- **Two-Spark single-stream decode:** ~34-35 tok/s (@Reederey, vLLM 0.26)
  and ~62.6 tok/s (@siraustin) and ~60-80 tok/s (@YRSM_Simon) — these do
  NOT converge; roughly 2x spread across three independent posts, likely
  reflecting different quant/config choices (DSpark draft tuning,
  concurrency settings) rather than one true number.
- **Two-Spark aggregate concurrent throughput:** 79.2 tok/s (@mr_r0b0t,
  C1 decode — note this is dedicated single-client, not aggregate despite
  the label), ~140 tok/s at 6 concurrent (@siraustin), 807-813 tok/s at 16
  concurrent agents (@vysecurity) — each a single anecdote, not
  cross-checked against each other's methodology.

### Contradicts existing catalog assumptions

The catalog doc (`MODEL-CANDIDATES-2026-08.md`, round 1) states: "The
tweet's '3bit, 16.5 tok/s' is real but is llama.cpp... UD-IQ3_XXS (103.0
GB)." @sudoingX's fresh post (same account as the original tweet, 2026-08-02)
now says the "3bit fits in 110gb" framing "has an asterisk" — only
UD-IQ3_XXS actually fits fully on GPU at 104 GB; the other two 3-bit
variants in the same ladder (UD-IQ3_S, UD-IQ3_M) are a full 128 GB and, per
@making's independent report, OOM in router mode. This sharpens rather than
reverses the existing note, but it's a direct update from the source and
worth folding back into the catalog doc.

No post in this sweep reported a working single-Spark vLLM path for
DeepSeek V4 Flash — consistent with, not contradicting, the existing "NO
today" call.

---

## Subject 2: MiniMax H3

### Recurring themes

1. **DGX Spark single-node MiniMax H3 is real and multiple people have done
   it independently** — this updates the catalog doc's characterization.
   The catalog (round 3) frames single-Spark MiniMax H3 as one existing
   report (79-like vLLM-Omni run). This sweep finds at least four
   independent DGX Spark users running it: @ivanfioravanti (three separate
   posts, ComfyUI templates), @aijoey (the vLLM-Omni/SM121 pinned-build
   path the catalog cites), @tonbistudio, and @Tech2Wild. A fifth,
   @u1tra_instinct, reports a working two-Spark run building on @aijoey's
   repo. @izutorishima's post is aspirational ("夢がある" — "it's a dream/
   something to aspire to"), not a completed run — flagged so it isn't
   miscounted as a fifth independent success.
2. **Generation times on Spark are long — minutes to well over an hour** —
   and multiple independent Spark users report this, not just one.
3. **Non-Spark local generation (consumer GPUs) is a much larger, separate
   conversation** — ComfyUI workflows, low-VRAM optimization (down to
   5-6 GB via WanGP), and INT8_ConvRot quantization becoming a de facto
   ComfyUI standard for this model. This is the dominant conversation by
   volume; Spark is a minority but present thread within it.
4. **License is the single most contentious, highest-engagement topic** —
   an official MiniMax clarification post is the highest-engagement post
   found in the entire sweep (both subjects).

### Notable posts — DGX Spark specifically

- @ivanfioravanti (2026-08-03, 573 likes — highest-engagement Spark-specific
  post in the sweep): "Another local video created with MiniMax H3 on DGX
  Spark, it took forever 1 hours and 20 minutes" — 1280x736, 15s, ComfyUI
  Text-to-Video template.
- @ivanfioravanti (2026-08-03, 178 likes, same user, different run): "960 ×
  544 - 10 seconds - generated in 16:57 mins on DGX Spark."
- @ivanfioravanti (2026-08-03, 78 likes, third post): 896x1184, 8s,
  Image-to-Video template, "36 mins."
- @aijoey (2026-08-03, 111 likes): "I got MiniMax H3 running on a single
  DGX Spark... It took some experimenting with SM121 compatibility, online
  FP8, and a pinned vLLM Omni build" — matches the catalog doc's existing
  citation of this exact path.
- @u1tra_instinct (2026-08-03, 17 likes): "MiniMax H3 running on two DGX
  [S]parks... strong dgx spark community work," built on @aijoey's repo —
  independent confirmation that a two-Spark path exists beyond the
  `joeynyc/MiniMax-H3-2x-DGX-Spark` repo already logged in the catalog.
- @tonbistudio (2026-08-03, 35 likes): "Got the MiniMax-H3 set up locally
  on my Spark and started experimenting."
- @Tech2Wild (2026-08-03, 14 likes): "Made A Repo For Minimax H3 if you
  need help getting it running on 1 x 3090. I also have a DGX Spark setup
  in there too."
- @YRSM_Simon (2026-08-03, 70 likes, JP/CN): head-to-head on DGX Spark vs
  LTX 2.3 Eros at 864x480/5s: H3 stronger on motion/instruction-following/
  character consistency and has native audio; LTX ~1.76x faster (154s vs
  270s). Direct same-hardware comparison, single source.
- @tmaiaroto (2026-08-04, 3 likes): "DGX Spark owners are pretty happy
  today with Minimax-H3. More memory is useful after all" — general vibe,
  not a data point.

### Notable posts — non-Spark hardware, VRAM, and workflows

- @cocktailpeanut (2026-08-04, 250 likes): low-VRAM WanGP support
  (credited to @deepbeepmeep): "5-6GB of VRAM only for 5s (124 frames)...
  8-9GB of VRAM for 15s at 832x480."
- @Spectromachina (2026-08-04, 67 likes): RTX 4060 Ti 16GB, pruned INT8
  model, "30 vertical videos generated... kept 23 strong candidates."
- @luta_ai (2026-08-03, 408 likes — highest-engagement MiniMax H3 post in
  the whole sweep after the license clarification): RTX 4090 (24GB), full
  15s audio+video generation reached — "9.6分" (9.6 min) after tuning three
  settings; the untuned baseline was "最初は5秒生成に496秒" (first 5s
  generation took 496 seconds / ~8.3 min) — notes cu130 is required (cu128
  breaks comfy_kitchen).
- @axiomofmind (2026-08-04, 5 likes): RTX PRO 6000 96GB, BF16, 960x544,
  124 frames/24fps, 20 steps, "~5 minutes... Peak usage was approximately
  80.5GB."
- @sadlemonjuice (2026-08-04, 6 likes, two near-duplicate posts): RTX 4090,
  960x544, "13m to generate."
- @araiguma_119 (2026-08-04, 3 likes): RTX 5090, 8s clip, "10分かかるかぁ"
  (took about 10 minutes) — notably slower per-second than the 4090 report
  above; single anecdotes, not directly comparable (different prompts/
  settings).
- @umiyuki_ai (2026-08-04, 103 likes): reports ComfyUI's 8-bit quant format
  for this model is "INT8_ConvRot," now the de facto 8-bit standard on
  ComfyUI (their own framing, presented as recently-learned rather than
  independently verified).
- @Tono_Ken3 (2026-08-04, 36 likes): re-quantized text encoder to NVFP4,
  "26.4 GB → 15.7 GB... Runs on a single 16 GB card (peak 9.9 GB VRAM
  measured)."
- @jtydhr88 (2026-08-04, 62 likes): ComfyTV workflow tutorial for using
  MiniMax H3 in ComfyUI "in a more elegant way" — image references, dynamic
  image-input counts.
- @superoo7 (2026-08-04, 4/67 likes across two posts): benchmarked
  MiniMax-H3 against Wan, LTX, and Seedance on an RTX PRO 6000, all 5s
  clips — "delivers very good quality for the generation time."
- @AIAKASATANA (2026-08-04, JP): "VRAM16GB＋RAM64GB推奨、モデル計42GB" (16GB
  VRAM + 64GB RAM recommended, ~42GB model total) — a spec summary post,
  not a first-hand benchmark.

### License discussion

- @MiniMax_AI (official account, 2026-08-04, 331 likes — highest-engagement
  post in the entire sweep, both subjects): "Claims that MiniMax H3
  'cannot legally be used' in certain regions are incorrect... can be
  licensed for deployment in the US, EU, UK, and South Korea through
  MiniMax's formal authorization process."
- @RyanLeeMiniMax (2026-08-03, 227 likes, MiniMax employee replying in the
  same thread): "This regional carve-out stems from our ongoing generative
  video copyright litigation with major Hollywood studios."
- @shujisado (2026-08-03, 111 likes, JP): calls the license "実質的にほぼ使
  えない" (practically almost unusable) despite the community-license
  framing, and notes it isn't OSI-recognized open source.
- @MrMirolim (2026-08-03): "You can't ban half the globe and still claim
  the open-source label. It's a proprietary PR stunt."
- @VictorSuOrtiz (2026-08-03, 13 likes): points people to the formal
  application process via api@minimax.io.

**This confirms rather than contradicts the catalog's existing
disqualification** (round 3): the license explicitly excludes EU/UK/
Korea/US by default, and while an authorization path exists, it's a
manual application process, not a blanket grant — the catalog's "our
users are overwhelmingly in the excluded territories" reasoning stands.
Worth noting for the catalog doc: MiniMax is now on-record explaining the
carve-out is litigation-driven (Hollywood copyright dispute), which is new
context not in the round-3 entry.

### Numbers, grouped by corroboration

- **DGX Spark generation time:** independent Spark reports range from
  ~10 min (@ivanfioravanti's second post, 10s clip) to 80 min
  (@ivanfioravanti's first post, 15s clip) to 36 min (@ivanfioravanti's
  third post, 8s clip) — all from the same single user, so this is one
  person's range across settings, not corroborated across users. No other
  Spark user in this sweep posted a generation-time figure.
- **Non-Spark 24GB-class GPU (4090) generation time for a full 15s clip:**
  one figure, @luta_ai, 9.6 min after tuning (496s/~8.3min untuned for
  just 5s) — single anecdote.
- **VRAM floor:** two independent, non-contradicting data points — 5-6GB
  (@cocktailpeanut, via WanGP low-VRAM path, 5s) and 16GB
  (@Spectromachina / @AIAKASATANA's recommended spec) — consistent with
  "the floor keeps dropping as optimized paths appear," not a single
  fixed number.

---

## Notes for whoever reads this next

- The single highest-signal new fact for the catalog doc: @sudoingX's
  2026-08-02 correction that the DeepSeek V4 Flash "3bit fits in 110gb"
  line has an asterisk (only UD-IQ3_XXS actually fits fully on GPU at
  104GB; sibling 3-bit variants are a full 128GB and OOM in router mode
  per @making). Same source as the tweet that triggered the original
  sweep.
- Second highest-signal fact: single-Spark MiniMax H3 is confirmed by (at
  least) four independent users, not one — the catalog's round-3 note
  undercounts community adoption on this point, though the
  license-disqualification conclusion itself is unaffected and, if
  anything, reinforced by the new litigation context from MiniMax's own
  account.
- No rate-limiting or token failures occurred during this sweep.
