# Spec 04: remote recipe index

Branch `spec/04-remote-recipe-index`. This is the strategic unlock: recipes ship today
embedded in the binary, so publishing or fixing a recipe requires a manager release.
After this spec, recipes arrive over the network, signed, with the embedded set as the
permanent offline fallback. Popularity/discovery pipelines later publish into this same
index.

This spec is security-critical. Where it says MUST, deviation is rejection.

## Format

An index file, JSON, hosted at a single HTTPS URL (constant in code; use
`https://raw.githubusercontent.com/punkjazz-labs/runonspark-recipes/main/index.json`
as the placeholder constant, clearly named so it can change once, and note in the
report that the repo does not exist yet).

```json
{
  "schema_version": 1,
  "generated_at": "2026-08-01T00:00:00Z",
  "recipes": [ { ...full recipe object, same schema as embedded yaml... } ]
}
```

Alongside it, a detached signature `index.json.minisig` (minisign / ed25519).

## Verification chain (MUST)

1. The minisign public key is a `const` in the binary. Generation of the keypair is out
   of scope; put a placeholder key constant and a `make sign-index` target that signs
   with a local secret key path taken from an env var. The private key MUST never
   appear in the repo, in code, or in CI config.
2. Fetch index + signature; verify signature over the exact bytes BEFORE parsing JSON.
3. Parse, then validate every recipe with the existing recipe validator. A recipe that
   fails validation is dropped with a logged reason; it MUST NOT abort the others.
4. Downgrade protection: persist the last accepted `generated_at`; reject an index
   older than it. Per recipe, a remote recipe replaces an embedded/cached one only if
   its `version` is greater or equal.
5. Pinning rules from the conventions apply to remote recipes identically (digest,
   revisions). The validator MUST enforce presence of digests; verify it does, extend
   it if not.

## Behavior

- On startup and every 6 hours: fetch, verify, cache to `dataDir/recipes-cache/` (the
  verified bytes, plus the signature for audit). Failures are silent to the user; the
  log line states the reason. Offline forever still works: embedded recipes are the
  floor.
- Effective recipe set = embedded, overlaid by cache, overlaid by fresh fetch, keyed by
  recipe `id`, respecting the version rule above. One function owns this merge;
  unit-test it hard.
- New recipes (ids not embedded) appear in the catalog like any other. Trust labels
  come from the recipe's own `trust` field; do not invent UI badges in this spec.
- **Update surfacing.** An installed model whose effective recipe `version` is greater
  than the installed `recipe_version` shows, in the expanded card facts, a line
  `Recipe updated` with a ghost pill `Update`, which runs the normal install flow
  (pinned artifacts unchanged between versions are already skipped by the shared
  artifact logic; verify and state in report). While a model is actively serving, the
  update action follows spec 01B semantics: the user chooses switch now vs later.

## Non-goals

Publishing tooling beyond `make sign-index`; key rotation; multiple channels
(stable/beta); recipe deletion propagation ("tombstones"). Flag them as open questions
in the report.

## Acceptance

- Unit tests: signature verify (good, bad, truncated), downgrade rejection, per-recipe
  version overlay, invalid recipe dropped without poisoning the batch, offline fallback.
- An integration-style test serving index + sig from `httptest` and asserting the merge.
- Mock harness: fixture where one installed model has a newer recipe; screenshot of the
  `Recipe updated` row and Update pill.
- Full build/vet/test green.
