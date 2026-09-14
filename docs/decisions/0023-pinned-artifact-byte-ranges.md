# ADR 0023: Pinned artifact byte ranges

Date: 2026-09-14. Status: accepted.

## Context

Some qualified runtime variants need a small, immutable byte region from a
large Hugging Face artifact. Downloading the containing file just to extract
that region defeats the disk and transfer reservations that recipe artifacts
are meant to make explicit.

## Decision

An `ArtifactFile` may declare a `range` with an upstream `source`, byte
`offset`, exact `source_bytes`, and SHA-256 of the destination slice. `name`
remains the managed destination and `expected_bytes` remains the slice size.
The repository and revision already pin the source snapshot.

Validation requires safe paths, positive bounded sizes, a nonnegative offset,
an exact lowercase SHA-256, and a destination distinct from the source. That
last rule matters because artifact directories are shared by repository and
revision: a slice must never overwrite a retained complete upstream file.

The downloader resolves the source in the pinned revision manifest and checks
its exact full size before requesting the precise byte interval. It accepts
only HTTP 206 with an exact `Content-Range`, writes no bytes beyond the pinned
slice, and verifies the slice SHA-256 before promoting it from `.part`.
Completion markers record the range coordinates and digest as well as the
destination. A changed slice therefore cannot reuse an earlier marker. A
short `.part` resumes with the corresponding offset; a full-size bad-hash
`.part` is discarded before retry because it cannot be extended safely.

## Consequences

Recipes can reserve and fetch only the bytes they mount while preserving the
existing whole-file path for ordinary artifacts. This is not a general remote
file API: ranges remain bound to a signed recipe, immutable repository
revision, exact source length, and output digest. Servers that ignore or
misstate a range fail closed.
