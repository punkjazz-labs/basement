# ADR 0021: Document redactor engine and API choices

Date: 2026-08-12. Status: accepted. Implemented 2026-08-12.

## Context

Spec 12 (`docs/plans/12-doc-redactor.md`) describes a document redactor that finds
sensitive literals with plain pattern matching plus checksums, groups them by literal for
review, and exports a redacted copy and a mapping. The spec's build plan originally
called for a standalone binary the owner runs on their own laptop, with its own loopback
server, browser launch, and OS-keychain credential storage for a Spark base URL and API
key.

That direction changed mid-build: the redactor runs from basement itself, as part of the
manager and console, not as a separate binary. This ADR records the choices made in
landing the deterministic engine (pattern pass only; the model pass is spec step 3, not
built here) as package `internal/docredact`, plus the JSON surface added to
`internal/httpapi`, and drops the choices the old direction required that no longer apply.

Note for future readers: `internal/redact` is a different, older package that scrubs
tokens out of job receipts and log lines. `internal/docredact` does not use it and does
not extend it; the names are similar on purpose only in the sense that both mean
"remove sensitive text," not because one builds on the other.

## Decisions

**One package, not two.** The engine could have split pattern detection from
grouping/export the way an earlier draft did (`detect` and `redact` packages). It did
not: `internal/docredact` holds detectors, overlap resolution, grouping, and mapping
encoding together, because nothing outside this feature imports any piece of it
separately, and a single package is what "a new package `internal/docredact`" asked for.

**No server-side file writes.** The manager never writes a redacted copy or a mapping
file to its own disk. `Document.Redacted()` and `Document.MappingBytes()` return
in-memory bytes; `internal/httpapi/docredact.go` writes them straight into an HTTP
response as a `Content-Disposition: attachment` download. This follows directly from the
redactor running on the Spark rather than the owner's laptop: the document being
redacted lives in the owner's browser, not on this machine's filesystem, so there is
nothing here to read a path from and nothing here that should be left behind after a
session ends.

**Sessions are in-memory only, keyed by a random id.** Analyzing a document creates a
session (`docredactSession`) held in a map on `httpapi.Server`, guarded by its own mutex,
with no persistence. A manager restart loses every open session, which is the correct
failure mode for a tool whose output is an immediate download rather than a durable
record.

**Findings group by literal text alone, not by (category, literal).** In practice no two
detectors here ever produce different categories for the same exact literal (an email
string is never also a valid IBAN), so a compound key would add complexity without
changing behavior. If a literal is ever claimed by two categories, whichever occurrence
comes first in document order wins the category, deterministically.

**Overlap resolution: sort candidates longest-first, then greedily accept non-overlapping
spans.** This is the literal implementation of "the longer literal wins": processing
matches from longest to shortest and rejecting any candidate that intersects an
already-accepted span guarantees a longer match always beats every shorter match it
overlaps, regardless of which one starts first or which detector produced it.
`TestResolveOverlapsLongerLiteralWins` and `TestAnalyzeAppliesOverlapResolution` in
`internal/docredact/overlap_test.go` pin this, the second using a genuine cross-detector
overlap (an email address whose domain looks like an IPv4 address) rather than a
contrived one.

**The mapping's first line is a plain sentence, not JSON.** `Document.MappingBytes()`
writes `MappingWarning` followed by a newline, then the JSON payload. This means the
mapping is not parseable by a bare `json.Unmarshal` on the whole byte slice -- deliberately
so a human who opens the file cannot skim past the warning inside a JSON string field,
and so nothing accidentally treats it as safe-to-upload structured data. `ParseMapping`
skips the first line on the way back in. The HTTP handler serves it as
`text/plain; charset=utf-8` rather than `application/json` for the same reason: calling
it JSON would be a promise the bytes do not keep.

**US SSN detection matches only the hyphenated form, `AAA-GG-SSSS`.** A bare 9-digit run
is indistinguishable from any other 9-digit number in a document without separators, and
the false-positive rate of flagging every such number as a candidate SSN was judged worse
than missing unformatted ones. The SSA's structural invalid-range rules (area 000, 666,
or 900-999; group 00; serial 0000) still apply on top of the shape match.

**IBAN validation combines a country-length table with the mod-97 checksum.** A handful
of common countries (`ibanLength` in `iban.go`) get an exact length check in addition to
mod-97; a country not in that table still gets mod-97 and the generic 15-34 character
bound. This is an accuracy aid, not the locale list -- it can grow independently of which
national-identifier detectors exist.

**Locales default to IT and US**, per the spec's own naming, and are recorded as a
pending default rather than a considered final answer in `docredact.Locales` -- the spec
leaves "which locales ship first" as an open question for the owner.

## What was dropped from the original direction

The standalone binary, its loopback HTTP server, browser-launch-on-startup, the
config screen, OS-keychain or file-based credential storage, and the `/v1/models`
API-key probe do not exist in this design: basement already knows its own models and
already has a console session to authenticate through. An earlier, now-abandoned
prototype of the standalone shape lived briefly at
`~/Dropbox/Projects/redactor` outside this repository; it was never wired into anything
and is not part of this codebase.

## API surface

`internal/httpapi/docredact.go` adds, all under console session auth
(`withReadAuth` at the mux, `AuthorizeMutation` inline on every POST, matching the
`jobAction`/`modelAction` dispatch shape already used elsewhere in the package):

- `POST /api/v1/docredact/analyze` -- run the pattern pass over submitted text, start a
  session.
- `GET /api/v1/docredact/sessions/{id}/findings` -- list findings.
- `POST /api/v1/docredact/sessions/{id}/findings/{findingId}/toggle` -- enable or disable
  a finding.
- `GET /api/v1/docredact/sessions/{id}/preview` -- the document with enabled findings
  replaced.
- `GET /api/v1/docredact/sessions/{id}/export/redacted` -- download the redacted copy.
- `GET /api/v1/docredact/sessions/{id}/export/mapping` -- download the mapping.

There is no `path` field on analyze: the manager cannot read a file from the owner's own
machine, so the console reads the file client-side and sends its text.
