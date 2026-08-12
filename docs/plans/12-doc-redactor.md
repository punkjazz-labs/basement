# Spec 12: doc redactor, a safe copy for cloud upload

**Amended 2026-08-13, owner decision: the redactor runs from basement directly.** It is
a console tab, not a standalone app. The engine lives in this repository as
`internal/docredact`, the console API serves it from `internal/httpapi` behind the same
session auth as every other tab, and the page ships in the console bundle. There is no
configuration screen: basement already knows its models, and the model pass uses them
the way Generate does. The document travels from the owner's browser to the manager
over the console session, so the privacy line becomes "read in your browser and sent
only to your Spark". Export is two browser downloads (redacted copy, mapping); nothing
is written server side. The standalone-repo framing below is kept for the engine,
detection, review, export, and test requirements, which are unchanged; ignore its
packaging, loopback server, keychain, and API-key sections.

Note for anyone searching this repository: `internal/redact` here is unrelated. It scrubs
tokens out of job receipts and log lines. Do not reuse the name in the new repository and
do not extend it for document work.

## Problem

People paste contracts, medical letters, HR files, and client documents into cloud AI
because that is where the good models are. The document is often 95 percent unremarkable
and 5 percent the reason it must not leave the building. Today the only options are do
not use the tool, or use it and hope.

basement already runs a capable model on hardware the owner controls. The gap is a piece
of software that takes a document, finds what is sensitive with a model that never leaves
the machine, lets a human check the list, and writes out a copy that is safe to upload.

## User-visible outcome

Drop a file in. A list of findings appears, grouped by the literal text found, each with
a category, how it was found, and how many times it appears. Every finding has a toggle,
on by default. A preview shows the document with the enabled findings replaced. Export
writes a redacted copy next to the original and, separately, a mapping file that stays
local. Nothing was sent anywhere except the owner's own Spark.

## Where it runs

One Go binary, no GUI framework, serving a local page in the owner's browser. This is
the pattern this project already chose and shipped for setup (`internal/setupweb` in this
repository, spec 08's "browser-served wizard inside a double-clickable .app"), so it
cross-compiles from one machine, needs no new dependency family, and can wear the console
design system directly.

The binary runs on the owner's laptop, not on the Spark, because that is where the
documents already are. It binds `127.0.0.1` on a random free port and opens a browser at
it. Nothing else can reach it.

Configuration is one screen: the Spark's base URL and an API key generated on that
Spark's Connect tab, plus a model or role name. Store them in the OS keychain where one
is available and in a `0600` file otherwise; never in the export directory and never in
the redacted output.

## Detection

Two independent passes, and every finding says which one produced it. The user interface
never presents a model guess and a deterministic match as the same kind of fact.

**Pass 1, patterns.** Plain Go regular expressions plus checksums where a checksum exists:
email addresses, phone numbers, IBANs, credit card numbers validated with Luhn, IPv4 and
IPv6 addresses, dates of birth in common formats, and national identifiers for the
locales the owner picks. These are cheap, exhaustive, and never wrong about their own
category. They run first so the model pass has less to be responsible for.

**Pass 2, the model, through basement `/v1`.** Names, organisations, addresses, job
titles, amounts tied to a person, and anything else that identifies a person in context.

The instruction that makes this work: **do not ask the model for offsets.** Ask it for
the exact literal substrings to redact, each with a category, and locate every occurrence
locally with an exact string search. Offsets from a language model are unreliable and
unverifiable; a literal either appears in the document or it does not, and one that does
not is dropped and counted as a hallucinated span. That count is shown, because a model
that invents spans is a fact the owner should be able to see.

Request shape, in order of preference, and the executor investigates which the pinned
runtime actually supports before choosing:

1. `response_format` with a JSON schema (vLLM's structured output). If the pinned recipe's
   runtime accepts it through the manager's proxy, use it, and the parser becomes trivial.
2. A prompt that demands a JSON array, parsed leniently, with one repair retry.

Either way the parser must survive garbage without losing the pattern pass: a model that
returns nothing usable degrades the app to pattern-only, says so on screen, and still
exports.

Long documents are chunked with an overlap of a few hundred characters, findings are
deduplicated by literal, and occurrence counting happens once over the whole document
after the passes finish, so a literal found in one chunk is redacted everywhere.

**Why not a dedicated NER model.** A local NER model (a small token-classification model)
would be faster and would give real offsets. It is also a second model to ship, pin, and
qualify, on a machine that already has a good general model loaded and idle. Start with
the model that is already there. If measurement shows recall is poor, a NER pass becomes
a third pass, not a replacement, and it gets its own spec.

## Review and export

- Findings group by literal, not by occurrence. Toggling a group toggles every
  occurrence; the count is shown.
- Each finding shows its source: `pattern` or `model`. Model findings are sorted after
  pattern findings within a category.
- Replacement is a stable pseudonym per literal, `[PERSON_1]`, `[ORG_2]`, `[AMOUNT_3]`,
  not a black bar. A cloud model can still reason about a document where the same person
  is consistently `[PERSON_1]`, and it cannot reason about one where every name is
  `[REDACTED]`. The category prefix comes from the finding.
- The user can add a finding by selecting text in the preview, and can edit a
  replacement. Both are recorded as `manual`.
- Export writes two files: `<name>.redacted.<ext>` and `<name>.mapping.json`. The mapping
  is the pseudonym-to-literal table. It is written next to the original with mode `0600`
  and a one-line header inside it saying it must not be uploaded anywhere. The redacted
  copy never contains the mapping.

**File formats for v1: `.txt` and `.md` in, the same format out.** Plain text in, plain
text out, no layout to preserve and no hidden structures to leak.

`.pdf` and `.docx` are input-only in v1, and they export as markdown, never as a rewritten
PDF or docx. This is a deliberate refusal rather than a missing feature. A redacted PDF
that still carries the original text under a black rectangle is the single most common
way redaction fails in the real world, and a docx carries revision history, comments, and
document properties that no text pass touches. The app must not produce a file that looks
like the original and quietly is not safe. The screen says: `basement read this PDF and
wrote a redacted text copy. The original PDF is unchanged and is still sensitive.`

Text extraction from PDF needs a library; pick one pure-Go option, pin it, and record why
in the report. If none is acceptable, PDF input waits for its own spec and v1 ships with
text and markdown only. That is a fine outcome.

## The privacy claim, exactly

What the app may say:

> Your document is read on this computer and sent only to your Spark at
> `<base URL>`. It is not sent to any other service.

What it must also say, in the same visual weight, next to the export button:

> Check every finding before you upload. basement finds what it can and cannot promise it
> found everything.

What it must never say: that the redacted copy is anonymous, that it is safe, that it is
compliant with any regulation, or that no data leaves the machine (it leaves this machine
for the Spark, which is a different machine, over the owner's own network).

If the Spark is reached over Tailscale the traffic crosses the tailnet, and the
configuration screen says which address is in use, so the claim on screen is always about
the address actually configured.

## Build plan

Each step is a reviewable chunk and leaves something runnable.

1. **Skeleton.** Go binary, loopback server, embedded static page, config screen,
   `/v1/models` probe against the configured Spark that proves the key works before
   anything else is attempted. Ship this alone and use it.
2. **Pattern pass** with its own test corpus. No model involved. Findings list renders,
   toggles work, export writes text and mapping. This is already a useful product.
3. **Model pass**: chunking, the literal-only contract, structured output if available,
   leniently parsed otherwise, hallucinated-span counting, degradation to pattern-only.
4. **Review affordances**: manual add, replacement editing, preview.
5. **PDF and docx input**, with the refusal copy above, only if step 3's investigation
   found an acceptable pure-Go extractor.
6. **Packaging**: reuse this repository's `scripts/release.sh` shape, macOS signing
   through the same identity and notary profile `packaging/sign-macos-release.sh` uses.

## Test strategy

The seam is the Spark. Everything above the HTTP client is testable without hardware.

- Pattern pass: a fixture corpus with known positives and known near-misses (a number
  that fails Luhn, a date that is not a birth date). Assert both the finds and the passes.
- Model pass: an `httptest` server standing in for `/v1`, returning canned completions.
  Cases: clean JSON, JSON in a code fence, truncated JSON, a literal that does not appear
  in the document (dropped and counted), a literal appearing five times (five
  occurrences, one finding), overlapping literals where one contains the other (the
  longer wins, and there is a test that pins which).
- Export: the redacted output contains no enabled literal, anywhere, byte for byte. This
  is the assertion the whole product rests on; write it first and make it exhaustive over
  the corpus.
- Mapping: round trip, and a test that the mapping file is never written into the same
  bytes as the redacted copy.
- No test may require a Spark. One optional integration test behind a build tag can hit a
  real endpoint when an environment variable names one.

## Open questions (owner)

- **Which Spark model, and by which name?** Roles exist now, so the app can address
  `role/fast` and let the owner decide. That is the right default, but it means the
  answer changes when the owner reassigns the role. Should the app pin a model id
  instead, and say so?
- **Restoring the answer.** The mapping file makes the reverse trip possible: paste the
  cloud model's reply back in and get real names restored locally. That is arguably the
  better half of the product. Is it v1, or the first thing after v1?
- **Locales.** National identifier patterns are locale specific. Which ones ship first?
- **Where the redacted copy goes.** Next to the original is the simple answer and it puts
  a sensitive-adjacent file in a shared folder. An explicit output directory is safer and
  is one more thing to configure.
