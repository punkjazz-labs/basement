# Qualifying a redaction model

Date: 2026-08-13. Status: method agreed, pattern-only baseline measured 2026-08-13, three
candidate arms measured 2026-08-14 on the Mac Studio. Quality ranking exists; the winner
still needs confirmation on a Spark at the quantization it would actually ship with.

## Why this document exists

On 2026-08-13 the owner decided that document redaction uses a dedicated, pinned model
rather than whichever model happens to be serving. The reason is controlled, repeatable
quality: the same document should produce the same findings tomorrow, and a model swapped
in for an unrelated reason should not quietly change what a redacted document leaks. ADR
0022 records that decision and the code that encodes it, namely the `redactor` role coming
first in the model pass endpoint's resolution order, with the fallback to the serving model
as the visible interim state.

This document is how that model gets chosen. It fixes the metric, the candidate classes,
and the commands, before any number exists, so the choice is settled by measurement instead
of by whichever model was easiest to try first.

## The metric

**A gold literal still visible in the redacted output is a leak.** That is the number that
decides this. It is a product outcome, not span bookkeeping: `ScoreDocument` in
`internal/docredact/benchscore.go` takes `Document.Redacted()` and searches it for each
labeled literal, so it does not care which pass found what, or whether a literal was
covered by a longer finding from a different source. A redacted document either still
contains the person's name or it does not.

The supporting columns exist to explain the leak count, not to compete with it:

- **OVER-REDACTED**: enabled findings whose spans never touch a gold occurrence. A model
  that drives leaks to zero by redacting most of the document has not solved the problem,
  and this is where that shows up.
- **HALLUCINATED**: literals the model named that do not appear in the document verbatim,
  dropped by the engine and counted. A high count is a signal about the model's discipline
  even though none of it reaches the output.
- **CHUNKS FAILED** and **DOCS FAILED**: how often the model could not be parsed or could
  not answer in time. A candidate that scores well only when it answers is not the same as
  a candidate that answers.
- **AVG TIME/SCORED DOC**: wall time per document, averaged only over documents that were
  actually scored.

The per-category leak breakdown printed under each arm is where the real comparison lives.
The pattern-only arm is expected to leak every `person`, `org`, `address`, `job_title` and
`amount` literal in the corpus, because no detector looks for those. Those five categories
are the model's job, and a candidate is judged on them.

## The bar

The thresholds are set by the owner against the measured pattern-only baseline, once that
baseline exists. Inventing them now would be inventing facts. The shape of the bar is
agreed, though:

1. A large reduction in leaks across the five model categories, compared with pattern-only.
2. No meaningful increase in leaks in the pattern categories. The model pass must never
   make the deterministic result worse, and the append-order rule in ADR 0022 is what is
   supposed to guarantee it.
3. Over-redaction that stays reviewable. The owner checks every finding before export, so
   the question is whether the list is still worth reading, not whether it is perfect.
4. Zero degraded documents, and a chunks-failed count near zero. A model that needs the
   repair retry constantly is not a model this feature can pin.
5. Time per document the owner will actually wait for, measured on the hardware the model
   will run on, not on the gateway.

## Candidates to measure

No candidate has been chosen. These are the classes to put through the bench:

**Very small open instruct models, roughly 0.5B to 4B parameters.** This is the class the
decision points at, because the redaction model has to be affordable enough to sit
alongside whatever primary model the owner actually uses. The task helps here: the model is
not asked to reason, only to copy identifying substrings out of a short document into a JSON
array, which is close to the smallest useful thing an instruct model does. Structured
output support is worth noting per candidate but is not a requirement, since
`ErrStructuredUnsupported` downgrades the whole document to lenient parsing on the first
refusal.

**GLiNER-class NER models, as a possible future third pass.** Spec 12 argued against a
dedicated NER model up front and wrote its own exit clause: "if measurement shows recall is
poor, a NER pass becomes a third pass, not a replacement, and it gets its own spec." This
benchmark is that measurement. A token-classification model gives real offsets and is
fast, but it does not speak the OpenAI-compatible `/v1` the `Completer` seam is built on,
so it cannot be scored by `cmd/docredact-bench` as it stands and would need its own spec
and its own harness. It is listed here so that the exit clause has somewhere to point.

Whatever wins ships as a pinned recipe assigned to the `redactor` role. Whether it can run
co-resident with a primary model on one node is a separate qualification belonging to the
roles system, and is recorded there rather than in this document.

## How to run it

`cmd/docredact-bench` always runs the pattern-only arm first as the baseline, then one arm
per model id, all against the same corpus in the same process. Flags: `-corpus`,
`-base-url`, `-model`, `-api-key`, `-json`, `-timeout`. Run from the repository root, since
the default corpus path is relative.

**The baseline, no model involved:**

```
go run ./cmd/docredact-bench
```

**Quality numbers now, through a backend that already has the candidates.** `-model` is a
comma-separated list, so several candidates are scored against the same corpus in one
invocation, and `-base-url` must not include the `/v1` suffix, which `ModelClient` appends
itself. Through the LiteLLM gateway on the Mac Mini:

```
go run ./cmd/docredact-bench \
  -base-url http://192.168.10.129:4000 \
  -model <candidate-a>,<candidate-b> \
  -api-key <local-placeholder> \
  -json /tmp/redactor-bench-gateway.json
```

Or straight at the Ollama host on the Mac Studio, whose OpenAI-compatible endpoint is on
port 11434 by default:

```
go run ./cmd/docredact-bench \
  -base-url http://192.168.10.131:11434 \
  -model <candidate-a>,<candidate-b> \
  -json /tmp/redactor-bench-studio.json
```

Concrete model ids are correct here rather than the `profile/` aliases: the whole point is
to tell named models apart, which is exactly the benchmark and diagnosis carve-out the
routing defaults make for physical routes. Verify the live addresses and ports before
running; the machine table is a starting point, not an oracle.

**Latency and memory, on a Spark, once one is free.** The gateway and the Studio answer the
quality question, but not the one that decides whether this model can be pinned next to a
primary model on a GB10. Point the bench at the Spark's own runtime port for the served
model and raise `-timeout` if a candidate is slow enough to trip the default five minutes:

```
go run ./cmd/docredact-bench \
  -base-url http://<spark-host>:<port> \
  -model <served-model-id> \
  -timeout 10m \
  -json /tmp/redactor-bench-spark.json
```

Memory is not something the bench measures. Read it off the node while the arm is running.

## Results

| Arm | Gold | Leaked | Leak% | Over-redacted | Hallucinated | Chunks failed | Docs failed | Avg time/scored doc |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| pattern-only (baseline) | 242 | 142 | 58.7% | 0 | 0 | 0 | 0 | 1ms |
| redact-qwen35 | 242 | 37 | 15.3% | 2 | 0 | 9 | 0 | 1m14.848s |
| redact-ministral3 | 242 | 35 | 14.5% | 6 | 2 | 0 | 0 | 1.521s |
| redact-qwen3 | 242 | 6 | 2.5% | 32 | 5 | 0 | 0 | 2.701s |

The pattern-only row is from `go run ./cmd/docredact-bench` on 2026-08-13. The three model
arms were measured on 2026-08-14 against Ollama 0.32.3 on the Mac Studio
(`192.168.10.131:11434`), one arm at a time, from a laptop on the same LAN. Each arm name
is a derived Ollama model created for the run with `num_ctx 16384`, so no chunk could be
silently truncated by Ollama's default context window; the bases are q8_0 GGUF quants, the
near-lossless quantization, chosen so the comparison ranks the models rather than their
quantizers:

- `redact-qwen35` = `qwen3.5:4b-q8_0`
- `redact-ministral3` = `ministral-3:3b-instruct-2512-q8_0`
- `redact-qwen3` = `qwen3:4b-instruct-2507-q8_0`

**`redact-qwen3` (Qwen3-4B-Instruct-2507, the control arm) wins outright**: 6 leaks out of
242 against 35 and 37 for the other two, with zero failed chunks. Its 32 over-redactions
and 5 hallucinated spans mark it as the most aggressive of the three, which is the right
direction for a leak-first metric with a review-everything UI; the hallucinations never
reach output by construction. Its per-category table is the only one with `person` and
`address` at zero.

The other two arms lose for different reasons. `redact-qwen35` (Qwen3.5-4B) is a thinking
model: it spent 75 seconds per document against 2.7 for the winner, and 9 of its 27 chunks
failed outright, which by bar 4 disqualifies it regardless of quality — and since every
corpus document is a single chunk, those 9 failures mean 9 documents fell back to
pattern-only, which is where most of its 37 leaks come from. `redact-ministral3`
(Ministral-3-3B) answered everything quickly but leaked 53.8% of `org` literals, the worst
model-category cell measured on any arm.

Public benchmark expectations from the research note were inverted by measurement:
Qwen3.5-4B's IFEval 89.8 did not survive contact with a thinking-mode extraction task, and
the arm with the least impressive paper numbers won. The corpus was the acceptance metric,
exactly as intended.

Leaks in the five model categories, the only categories where any arm leaked at all (every
pattern category was 0 leaks on every arm, so bar 2, never making the deterministic result
worse, held everywhere):

| Category | Gold | redact-qwen35 | redact-ministral3 | redact-qwen3 |
| --- | --- | --- | --- | --- |
| person | 38 | 10 | 3 | 0 |
| address | 30 | 7 | 8 | 0 |
| job_title | 28 | 7 | 7 | 3 |
| org | 26 | 8 | 14 | 2 |
| amount | 20 | 5 | 3 | 1 |

What this run does not settle: latency and memory on a GB10, co-residency next to a
primary model, and behaviour at NVFP4 (W4A16), the quantization a pinned Spark recipe
would actually use. Those are the Spark measurements this document already describes, to
be run when a machine is free. The quality ranking on q8_0 GGUF picks the candidate to
carry into that confirmation; it does not by itself pin the recipe.

Per-category leak breakdown for the pattern-only baseline, copied from the bench output on
2026-08-13:

| Category | Gold | Leaked | Leak% |
| --- | --- | --- | --- |
| address | 30 | 30 | 100.0% |
| amount | 20 | 20 | 100.0% |
| br_cpf | 1 | 0 | 0.0% |
| card | 3 | 0 | 0.0% |
| cn_resident_id | 1 | 0 | 0.0% |
| de_steuer_id | 2 | 0 | 0.0% |
| dob | 15 | 0 | 0.0% |
| email | 26 | 0 | 0.0% |
| es_dni | 2 | 0 | 0.0% |
| fr_nir | 2 | 0 | 0.0% |
| iban | 10 | 0 | 0.0% |
| ipv4 | 2 | 0 | 0.0% |
| ipv6 | 2 | 0 | 0.0% |
| it_codice_fiscale | 4 | 0 | 0.0% |
| job_title | 28 | 28 | 100.0% |
| jp_my_number | 1 | 0 | 0.0% |
| nl_bsn | 1 | 0 | 0.0% |
| org | 26 | 26 | 100.0% |
| person | 38 | 38 | 100.0% |
| phone | 24 | 0 | 0.0% |
| pt_nif | 1 | 0 | 0.0% |
| uk_nino | 1 | 0 | 0.0% |
| us_ssn | 2 | 0 | 0.0% |

Every leak is in one of the five model categories (`address`, `amount`, `job_title`, `org`,
`person`), exactly as expected: no detector looks for those, so the pattern-only arm leaks
every one of them, and every pattern category, national-identifier or otherwise, leaks
none. Add a row per candidate model arm as it is measured, and keep the arm name exactly as
it was passed to `-model` so a result can be traced back to the model that produced it.

## What the corpus does and does not measure

`internal/docredact/testdata/corpus` holds 27 synthetic documents across 13 locales,
labeled with every sensitive literal they contain across both the pattern categories and
the five model categories: 6 IT, 7 US, 3 DE, 2 FR, and 1 each of ES, PT, NL, UK, BR, CN,
JP, RU and AR. Two documents (`13-us-tech-memo-no-pii.json` and
`27-de-negative-changelog.json`) carry no gold literal at all, so a model that redacts on
reflex has somewhere to be caught. They were written for this benchmark; no real person's
data is in the repository.

Most locales' documents carry gold literals in all five model categories, so once a model
arm is measured, recall on `person`, `org`, `address`, `job_title` and `amount` is
comparable per language, not just pooled across the whole corpus, by filtering scored
documents on their `locale` field. Four locales are the exception, each missing gold in one
or two of the five: CN has no `amount` or `job_title` gold, ES has no `org` gold, and JP and
NL each have no `amount` gold. `person`, `address`, `email` and `phone` are the categories
every locale actually covers; `dob` is covered by every locale except BR, whose single
document (`22-br-invoice.json`) has no `dob` gold. A per-language `amount`, `org`, or
`job_title` comparison involving CN, ES, JP or NL, or a `dob` comparison involving BR, should
be read as missing that cell rather than as a true zero, until the corpus grows a document
supplying it. RU and AR have no
national-identifier detector: `docredact.Registry()` runs no RU or AR-specific detector, so
their two documents carry gold literals only in the universal pattern categories (`dob`,
`email`, `phone`) and the five model categories, all five of which they do cover. A leak in
an RU or AR document is either a model-category miss or a universal-pattern miss; it is
never a missed national identifier, because none exists to miss.

Known limits, which a reader of the results table should hold in mind:

- **They are synthetic.** They measure the shape of the problem, not the mess of real
  correspondence. A candidate that wins here has earned a trial on the owner's own
  documents, not a conclusion.
- **They are short.** Every document is well under the 6000-byte chunk size, so each one is
  a single chunk and a single model round. Chunking and overlap are covered by unit tests
  in `internal/docredact/chunk_test.go`, but nothing in this corpus exercises them, and
  nothing here says how a candidate behaves on a long document.
- **Thirteen locales in the corpus, eleven in `docredact.Locales`.** The corpus covers IT,
  US, FR, ES, PT, DE, NL, UK, BR, CN, JP, RU and AR. `docredact.Locales`, which the owner
  expanded from the IT/US default on 2026-08-13, lists only the eleven with a
  national-identifier detector behind them (IT, US, FR, ES, PT, DE, NL, UK, BR, CN, JP); RU
  and AR are corpus-only, by design, to measure the five model categories and the universal
  patterns in those languages without a new identifier format to qualify.
- **The gold labels are a judgement.** They encode what the owner would want redacted in
  these documents. A candidate that flags something reasonable but unlabeled is scored as
  over-redaction, which is the right default for a leak-first metric and still worth
  reading the actual findings for before dismissing a model.
