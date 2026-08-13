# Redactor model research, August 2026

Research date: 2026-08-13, three parallel web research passes (dedicated PII models,
small instruct LLMs, published NER-vs-LLM evidence). Every model fact was verified
against its Hugging Face page or primary source by the researching agent; items that
could not be verified are marked as such. This note is the synthesis; the question it
serves is defined in `redactor-model-qualification.md`: which dedicated model should
the `redactor` role pin.

## The shape of the answer

Three findings decide the direction before any measurement:

1. **Only small instruct LLMs fit the shipped serving contract.** The model pass
   speaks OpenAI chat completions through the manager's `/v1` (ADR 0022). Of every
   dedicated PII model surveyed, exactly one family is vLLM-native at all: OpenAI's
   `privacy-filter` token classifiers, and those serve on vLLM's pooling runner
   (`POST /pooling`), a different route the manager does not proxy. GLiNER-class span
   models are not in vLLM's model registry at all and need a sidecar process. A 3-4B
   instruct model needs zero new serving machinery.

2. **Dedicated encoder models crater on non-Latin scripts; LLM detectors do not.**
   An independent 32-benchmark evaluation (arXiv:2608.02616) measured
   `openai/privacy-filter` at F1 0.138 on Chinese, 0.038 on Farsi, and 0.027 on
   Cyrillic, versus 0.20 to 0.32 on Latin scripts, and explicitly falsified tokenizer
   fertility as the cause. The only per-locale LLM recall table (RECAP,
   arXiv:2510.07551) shows the opposite: GPT-4o's best locales are Chinese (recall
   0.603 zh_SG) and its worst are European (0.247 pt_PT). Our corpus spans thirteen
   locales including zh, ja, ru, ar; the encoder route fails exactly where we need
   coverage most.

3. **No dedicated model covers Russian.** `OpenMed/privacy-filter-multilingual-v2`
   (the only model whose 54-category label set covers organization, job title, and
   amount) declares sixteen languages without Russian and has zero published
   evaluations. GLiNER2-PII (arXiv:2605.09973, best span model, F1 0.471 on SPY)
   covers seven Latin-script languages only. Piiranha is licensed cc-by-nc-nd-4.0
   (non-commercial, no derivatives) and is unusable regardless of quality.

The published evidence also endorses the shipped architecture directly: the RECAP
hybrid study measured regex + context-aware LLM at weighted F1 0.657 against 0.558
for a zero-shot LLM alone and 0.360 for transformer NER baselines, and the REDACT
benchmark (arXiv:2606.19881) measured rule-based detection at 0.07 recall on
GDPR-high-sensitivity entities, confirming that patterns must never carry names.
RedactionBench (arXiv:2606.18782) found human annotators agree on only 47.7 percent
of contextual redaction decisions; the review-every-finding UI is load-bearing, not a
convenience. No published system, at any size, reaches "safe to ship unreviewed":
the best small models land between F1 0.47 and 0.58 on realistic benchmarks, and the
aggregated human baseline on RedactionBench is 0.77.

## Candidates to measure

All three are Apache-2.0 and serve as ordinary vLLM chat models.

| Candidate | Disk | Languages (claimed) | Key number | Risk |
|---|---|---|---|---|
| `Qwen/Qwen3.5-4B` | 9.32 GB BF16, 4.19 GB via community NVFP4 (`AxionML/Qwen3.5-4B-NVFP4`) | 201 | IFEval 89.8, highest of any small model surveyed | needs vLLM main/nightly (Gated DeltaNet); two open text-only issues; mamba-cache CUDA-graph tuning on GB10; community quant unvalidated |
| `mistralai/Ministral-3-3B-Instruct-2512` | 4.67 GB BF16, no quant needed | names 10 of our 11; Russian absent from the claimed list | Multilingual MMLU 0.652; card recommends temperature at or below 0.1 for production | Russian unproven; no published IFEval for the 3B |
| `Qwen/Qwen3-4B-Instruct-2507` | ~8 GB BF16 (unverified), many community quants | strong multilingual | MultiIF 69.0, the most on-target published number found (multilingual instruction following) | none notable; the control arm, vLLM-supported since 0.8.5 |

Ruled out: Gemma 4 E2B/E4B (the "effective parameters" do not reduce weight bytes:
10.25 and 16.00 GB actual), Qwen3.5-2B and 0.8B (IFEval cliff: 89.8 to 61.2 to 52.1
across 4B/2B/0.8B; paraphrasing instead of copying literals is our failure mode),
Nemotron-3-Nano-4B (English-only claim, non-Apache license), SmolLM3 (European
languages only), Phi-5 (14B, out of band).

## Measurement notes the literature dictates

- **A/B structured output against plain prompting.** ExtractBench (arXiv:2602.12247)
  found constrained decoding reduced accuracy on wide schemas. Our schema is a flat
  array of two string fields, the favorable case, but the comparison must be measured,
  not assumed. The engine's `Completer` seam already supports both paths.
- **Do not over-prompt against hallucination.** The "Safety Tax" result
  (arXiv:2601.02023) shows anti-hallucination prompting suppresses real spans. The
  engine verifies every claimed literal and discards inventions, so the prompt should
  bias toward recall and let verification do its job.
- **Schema field names carry instruction** (arXiv:2604.14862): name the field
  `exact_substring_from_document`, not `text`, when using json_schema mode.
- **GB10 note:** NVFP4 on GB10 currently runs weight-only (W4A16) in vLLM; it buys
  memory, not speed.

## Longer-term option recorded, not scheduled

Fine-tuning beats zero-shot on this task shape in every source that measured it:
roughly 500 labeled examples took a fine-tuned XLM-R past `privacy-filter` zero-shot
(arXiv:2608.02616), and binary hide-or-not labeling beats per-class labeling at small
sample sizes (F1 0.634 vs 0.360 at n=100), which suits a redactor: it needs "hide
this", not taxonomy. The labeled corpus in `internal/docredact/testdata/corpus/` is a
seed for exactly this. A LoRA fine-tune of whichever candidate wins the bake-off is
the likely second act once measured numbers exist.

## Recommended next step

Run the three candidates through `cmd/docredact-bench` against the 27-document
corpus: leak-based recall per category per locale, over-redaction, hallucinated-span
rate, both prompting modes. Quality numbers are hardware-independent and can be
measured through any OpenAI-compatible endpoint today; latency and co-residency
memory wait for a Spark. The corpus is the acceptance metric; public benchmark
scores documented here set expectations, not the decision.
