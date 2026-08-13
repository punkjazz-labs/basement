# ADR 0022: Document redactor model pass

Date: 2026-08-13. Status: accepted. Implemented 2026-08-13.

## Context

ADR 0021 recorded the deterministic half of the document redactor: the pattern detectors,
overlap resolution, grouping, export, and the JSON surface in `internal/httpapi`. It
explicitly left spec step 3, the model pass, unbuilt.

This ADR records the choices made in landing that step: the `Completer` seam and the
chunking, prompting, parsing and verification the engine now owns; the
`POST /api/v1/docredact/sessions/{id}/modelpass` endpoint and how it picks a model; and
the labeled corpus and leak-based benchmark added so the choice of model can be measured
rather than argued about. Spec 12 is `docs/plans/12-doc-redactor.md`.

No benchmark numbers appear here. The method, and the empty results table it will fill,
live in `docs/research/redactor-model-qualification.md`.

## Decisions

**Redaction gets a dedicated, pinned model, not whatever happens to be serving.** This is
the owner's decision of 2026-08-13, and it is the one every other choice below arranges
itself around. Redaction quality has to be controlled and repeatable: the same document
should produce the same findings tomorrow, and a model swapped in for an unrelated reason
should not quietly change what a redacted document leaks. The endpoint encodes it in the
order `redactionModel` resolves a target: the `redactor` role first, then a role named in
the request body, then whichever model is active and ready. The dedicated model itself
ships later, as a pinned recipe assigned to the `redactor` role once it passes
qualification, so the fallback to the serving model is the interim state rather than the
design. That interim state is visible rather than silent: the response's
`model_pass.model` field carries the display name of the model that actually answered, so
the console can say which one it was. Whether the redaction model can sit co-resident with
a primary model on one node is a roles-system question and is qualified and recorded
there, not here.

**The engine talks to a `Completer`, not to HTTP.** `docredact.Completer` is one method:
system prompt and user prompt in, raw reply text out, plus a flag asking for structured
output. Everything that makes redaction correct lives above that line inside
`internal/docredact` -- chunking, the prompt, lenient parsing, verifying literals against
the document, counting, and overlap resolution. Everything about which model, at which
address, with which credential, lives below it in `internal/httpapi`, which builds a
`docredact.ModelClient` per pass. `ModelClient` ships in the same package because it is the
only production implementation anyone needs, but it is reached through the interface, so
the unit tests substitute canned replies and `cmd/docredact-bench` substitutes one client
per candidate model without either of them touching the engine.

**Never ask the model for offsets.** The prompt asks for exact substrings copied verbatim
from the document, each with a category, and nothing else. Every occurrence is then found
locally by exact string search, the same search `AddManual` uses. A literal that does not
appear in `Document.Text` verbatim is dropped and counted in `Hallucinated`, never
trusted, because a literal either is in the document or is not, while an offset from a
language model is neither reliable nor checkable. This is the whole reason a model finding
can be treated as a claim rather than a fact.

**The result counts the whole document, never a chunk.** `ModelPassResult` is a
per-document tally: a literal returned by three overlapping chunks is one literal. A `seen`
map deduplicates literals across chunks before any of them is verified, so the
`Hallucinated` and `Duplicates` figures are not inflated by the overlap that exists to
avoid losing a literal on a boundary.

**`Duplicates` deliberately covers two different fates.** A model literal that another
pass already covers (`literalKnown`, checked against existing findings and against queued
manual and model literals) counts as a duplicate, so re-running the pass, or a model
echoing what the owner already selected by hand, never double-counts. So does a literal
that was accepted and queued but then lost the overlap contest during `recompute` to a
longer finding from another source. That second case is neither an acceptance nor a
hallucination: the model was right about the text and something better already covers it,
which is the same thing the owner sees. `Accepted` is not incremented optimistically at
queue time; it is recounted from `Document.Findings` after the recompute, so it always
equals the number of model-sourced findings actually on screen.

**Model matches are appended last, so pattern and manual literals win an exact tie.**
`recompute` builds its candidate list as detector matches, then manual literals, then model
literals, and `ResolveOverlaps` sorts longest-first with a stable sort, breaking equal
lengths by original order. The effect is a rule worth stating plainly: a longer model
literal still beats a shorter pattern match, exactly as any two detectors settle it, but on
an identical span the deterministic source wins. A finding the owner can trace to a regular
expression or to their own selection is a better thing to show them than an identical
finding attributed to a guess.

**A misbehaving model is data, not an error.** The only error `ApplyModelPass` returns is
context cancellation. A transport failure, a non-200, a reply that cannot be parsed even
after the repair retry: each increments `ChunksFailed` and the loop moves to the next
chunk. `Degraded` is set only when `ChunksFailed == ChunksTotal`, meaning not one chunk
produced anything usable and the pattern findings stand alone. A pass that lost some chunks
and kept others is a partially useful pass, and reporting it as a failure would throw away
work the owner can see is good. On cancellation the document is left untouched: nothing has
been queued into `Document.model` and `recompute` never runs, so the findings on screen are
exactly what they were before the request.

**Five model categories, and an unknown name becomes a phrase rather than an error.** The
prompt names `person`, `org`, `address`, `job_title` and `amount`, matching the spec's own
list of what pass 2 is for. `ParseCategory` maps anything else, including an empty string,
to `CategoryPhrase` and reports that it did. This is the rule ADR 0021 already set for text
the owner selects by hand, applied to the model for the same reason: the literal is still
redacted, but naming a category the build does not know would be inventing a fact about the
text. A `[PHRASE_n]` pseudonym is the honest label for "this is sensitive and we are not
going to claim what kind".

**Chunking is 6000 bytes with 400 bytes of overlap, and the overlap is what carries the
guarantee.** A literal that lands on a naive boundary still appears whole inside the shared
overlap of at least one chunk. Boundary snapping, which walks back up to 200 bytes looking
for a newline or space, is tidiness on top of that guarantee and not part of it, which is
why it is allowed to fail and find nothing. `ChunkText` panics when `size <= overlap`
rather than looping forever, because that is a programmer error and not an input error, and
it falls back to the unsnapped boundary in the one case where snapping would leave the
window unable to advance.

**Structured output is preferred, and refused once means refused for the document.**
`ModelClient` sends `response_format` with a JSON schema matching `ModelItem`. A backend
that rejects it answers with a 4xx, so a 4xx body mentioning `response_format`, or any 400
received while structured output was requested, is reported as `ErrStructuredUnsupported`
rather than as a failed chunk. `ApplyModelPass` retries that same chunk unstructured and
then sets `structured` false for every later chunk: a backend that rejected the schema once
will reject it again, and paying a round trip per chunk to rediscover that is waste the
owner watches. This is the spec's stated order of preference, resolved at run time against
the backend actually answering instead of being decided in advance per runtime.

**One repair retry, and it contains the previous reply.** When a reply has no parseable
array, the model is asked again with the failed reply included in the prompt, always
unstructured. Handing the model its own broken output gives it something concrete to
correct rather than a blind second attempt at the same task. A second unparseable reply
ends the chunk as failed; there is no third try.

**Parsing is lenient by design, and "no array" is not "found nothing".** `ExtractModelItems`
tries the whole trimmed reply first, then hunts for the first balanced top-level array,
tracking JSON string state and escapes so a literal containing a bracket cannot close the
array early. That bracket walk is also what makes an object wrapper such as
`{"items":[...]}` parse without a special case. A reply with no array anywhere returns
`ErrNoModelItems`, which is a different fact from a reply that parses into an empty array:
the first means the reply could not be read, the second means the model looked and found
nothing, and only the first is worth a repair retry.

**The endpoint never activates a model.** Unlike a `/v1` request naming a role, which will
wait for a switch, `redactionModel` accepts only a candidate that is serving right now. A
document open in a browser tab is not worth stopping somebody else's running model for.
When nothing suitable is serving, or the only thing serving is a media-generation runtime
that does not answer on `/v1` at all (`answersOnV1`), the endpoint returns 503 with a
sentence that says what did not happen and what is still true: the pattern findings are
unchanged.

**Model resolution happens inside the serving gate's `tryAdmit` closure.** All three
candidates in the resolution order are resolved under the gate's lock, so the model that is
chosen is the model the hold is taken on and no switch can slip between the two. The hold
is released when the pass finishes. Store reads inside that closure use
`context.Background()` rather than the request context, matching `admitToServingModel`: a
client that has already gone away should be discovered by the response write failing, never
by leaving the gate held on a dead context.

**A degraded pass still answers 200.** The response carries the current findings and the
`model_pass` tally, including `degraded`, rather than an HTTP error. The pass is a request
to improve a document that already has findings in it; the only outcome that deserves a
refusal is the one where the pass could not start at all.

**Quality gets a labeled corpus and a leak-based score, in the repository.** `testdata/corpus`
holds 13 synthetic documents, IT and US, in shapes the redactor is actually pointed at:
contracts, invoices, referral and demand letters, an email thread, meeting minutes, memos.
One of them contains no sensitive literal at all, so over-redaction has somewhere to show
up. Each document's gold list covers both the pattern categories and the five model
categories, which means the pattern-only arm is expected to leak every model-category
literal, and that is the baseline every candidate model is measured against. The documents
are written for this benchmark; no real person's data is in the repository.

**The metric is the leak, not the span.** `ScoreDocument` takes `Document.Redacted()` and
asks, for each gold literal, whether it is still visible in the output. That is the product
outcome the owner cares about, and it is indifferent to how the passes got there: a literal
covered by a longer finding from another source is not a leak, even though no finding
carries its exact text. Over-redaction is counted from the other side, as an enabled
finding whose spans never touch any gold occurrence. `cmd/docredact-bench` always runs the
pattern-only arm first as the baseline, adds one arm per model id given to `-model`, and
excludes any document whose pass failed outright from every total including the time
average, so `AVG TIME/SCORED DOC` is never dragged upward by a document that never
finished.

## API surface

`internal/httpapi/docredact.go` adds one route to the set ADR 0021 listed, under the same
console session auth and the same inline `AuthorizeMutation` on POST:

- `POST /api/v1/docredact/sessions/{id}/modelpass` -- run the model pass over the session's
  document and fold what it verifies into the existing findings. The body is optional and
  carries one field, `model`; a `role/`-prefixed value is resolved as a role, and a
  concrete model id is answered by whichever model is serving, exactly as `/v1` resolves
  one. The response is `{findings, model_pass}`, where `model_pass` is
  `docredact.ModelPassResult` plus the display name of the model that answered. 503 when no
  text model is serving.

`cmd/docredact-bench` is a developer command, not part of the API. It reads the corpus from
disk, runs against any OpenAI-compatible base URL given to `-base-url`, and writes an
aligned text table plus optional JSON. It is never invoked by the manager.
