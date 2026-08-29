# Spec 15: signed recipe feed, the parts that are still missing

Branch per section: `spec/15a-keys`, `spec/15b-trust-clamp`, `spec/15c-feed-state`,
`spec/15d-publishing`. Sections A and B are independent; C depends on B; D can run any
time.

Read first: `docs/decisions/0009-signed-recipe-feed-design.md` and
`docs/plans/04-remote-recipe-index.md`, then the code, because the code is ahead of both
documents in some places and behind them in others.

## What already exists

Spec 04 shipped the fetch and verification chain. Before writing a line, an executor
should read `internal/recipefeed/fetch.go`, `internal/recipe/index.go`,
`internal/recipe/signature.go`, `internal/recipe/merge.go`, and `cmd/sign-index/`.
Working today:

- `recipefeed.Fetcher` fetches `IndexURL` every `RefreshInterval` (1 hour), verifies a
  detached ed25519 signature over the exact bytes **before** parsing, drops individually
  invalid recipes without poisoning the batch, caches verified bytes plus signature in
  `dataDir/recipes-cache/`, re-verifies the cache on every start, rejects an index older
  than the last accepted `generated_at`, and leaves the registry untouched on any
  failure. Embedded recipes are the permanent floor.
- `recipe.Merge(embedded, cached, fresh)` overlays by id and never downgrades a version;
  `Fetcher.all` only grows, so an installed model always resolves to the exact version it
  was installed with.
- `make sign-index` signs an index with a private key read from a file path in
  `BASEMENT_SIGN_KEY`. The key never appears in the repo, in CI, or in argv.
- Hardening the ADR does not mention and which must not be lost: redirects refused,
  8 MB index cap, 4 KB signature cap, 15 second timeout, `DisallowUnknownFields` on the
  envelope and on every recipe, strict schema version.

Four things from ADR 0009 are missing, and one part of the ADR describes a design the
code deliberately did not build.

## A. Keys that are real, and a second one that can replace the first

**Problem.** `recipe.IndexPublicKeyBase64` is a placeholder whose private half was
generated and discarded on purpose. Nothing can be published until a real key exists, and
`IndexPublicKey()` returns exactly one key, so the rotation story in ADR 0009 point 3
("key rotation ships as a manager release carrying old + new keys") cannot be executed.

**Build.**
1. `internal/recipe/signature.go`: `IndexPublicKeys() []ed25519.PublicKey` from a slice
   of base64 constants, ordered newest first. `VerifySignature` accepts a signature that
   any of them verifies. Keep the single-key function as a wrapper if callers are
   tidier for it, but the verification path takes the set.
2. A rotation is then: add the new key at the front, release, publish signed with the
   new key, and drop the old constant one release later. Write those three steps as a
   comment on the constant block, because whoever does it will not be whoever wrote it.
3. Generation and custody are not executor work and not code. They need a decision
   record: how the key is generated, where the private half lives, who can use it, what
   happens when it is lost, and the fact that losing it means every Spark stops accepting
   new recipes until a manager release ships a new key. Spec 16 needs exactly the same
   record for the release-signing key. **Write one ADR covering both keys.** ADR 0009
   already names this record as a prerequisite and it does not exist.

**Tests.** Signature verifies against the second key in the set; a signature from a key
outside the set is refused; an empty or malformed constant still panics at startup the
way `IndexPublicKey` does today, which is the correct behaviour for a build mistake.

## B. A feed cannot hand out trust it has not earned

**Problem.** ADR 0009 point 4 says a feed recipe arrives at most as a candidate. Nothing
in the code does this. `VerifyAndParseIndex` runs the same `recipe.Validate` used for
embedded recipes and accepts whatever `trust` field the index carries, so a signed index
containing `trust: basement-verified` and `verification: dgx-spark-verified` flows
straight into the effective catalog and the console.

Note two vocabulary facts before writing anything: the code's labels are
`basement-candidate` and `basement-verified` (`internal/recipe/validator.go:68`), not the
`runonspark-*` labels ADR 0009 names; and every shipped recipe is currently
`basement-candidate`, so nothing visible changes when the clamp lands.

**Build.**
1. `internal/recipe/index.go`, inside the per-recipe loop of `VerifyAndParseIndex`: after
   `Validate` passes, force `trust = "basement-candidate"` and
   `verification = "candidate"` on every recipe from the index. Do it there rather than
   in `recipefeed`, so there is one door and no second parser can bypass it.
2. Track where a recipe came from, outside the signed document. The recipe body is
   signed and decoded with `DisallowUnknownFields`, so provenance cannot be a yaml field
   on `recipe.Recipe` that a publisher fills in. Add an unmarshalled-to-nothing field
   the fetcher sets (`Origin string \`json:"origin" yaml:"-"\`` with an explicit
   `json:"-"` on the decode path if that is what it takes), or a parallel map on the
   `Fetcher`. Values: `embedded`, `cache`, `feed`. Whichever shape is chosen, the
   invariant to test is that a publisher cannot set it.
3. Surface it on `GET /api/v1/recipes` through the existing view-struct pattern in
   `internal/httpapi/server.go` `listRecipes` (which already adds `artifact_bytes` and
   `required_bytes` without touching the recipe type).

**Consequence, and it is a real cost.** With the clamp in place, a `basement-verified`
recipe can only ever reach a Spark inside a manager release. The feed becomes a
candidate-delivery channel. That is what ADR 0009 says, and it is defensible: the
verified label means DGX qualification receipts exist in the repository, and a Spark
cannot check that a signature over a JSON file implies a qualification run happened. The
alternative (trust the signer, since the signer holds the key anyway) collapses the
distinction between the two labels. **Owner decision required before this ships.**

**Tests.** An index carrying `basement-verified` yields `basement-candidate` after
ingest; the embedded catalog's own labels are untouched; a cached index re-verified at
startup is clamped the same way (a clamp applied only on the network path would let a
poisoned cache through); origin is `feed` for fetched recipes and `embedded` for the
floor; a publisher-supplied origin field does not survive.

## C. Feed state the owner can see

**Problem.** Every failure is silent by design ("failures are silent to the user; the log
line states the reason", spec 04). That was right for a feature nobody knew existed. It
is wrong for two states: a signature that does not verify, and a feed that has been
unreachable for a long time. It is also wrong for the ADR's own point 5, "shows new model
available", which does not exist: a new recipe id simply appears in the catalog with
nothing marking it as new.

Being offline is not a failure and must never be presented as one.

**Build.**
1. `internal/recipefeed/fetch.go`: keep the last attempt's outcome on the `Fetcher`
   (`checked_at`, `ok`, a machine-readable `reason` for the failure class, and the
   accepted index's `generated_at`). Classes that matter and must stay distinct:
   `unreachable`, `signature`, `stale`, `malformed`. Add an accessor; do not widen
   `Snapshot`.
2. `GET /api/v1/feed`, read auth, returning that state plus the recipe count and the
   count from each origin. Do not put it on `withPeerReadAuth`: a peer has no business
   asking.
3. First-seen tracking, so "new" means something. A small table in `internal/store`
   (`recipe_first_seen(recipe_id, first_seen_at, origin)`), written by whatever calls
   `SetRecipes`. Additive schema, per ADR 0008 point 4.
4. Console, mockup-gated (this is new visual language):
   - a quiet line where the catalog lives, in the Models view, along the lines of
     `Recipe list checked 20 minutes ago.` Absent entirely when everything is normal and
     recent, if the mockup says that reads better.
   - `unreachable`: quiet, factual, no alarm. `Recipe list last updated 3 days ago.
     basement is using the list it already has.`
   - `signature`: this one is loud, because someone served bytes that did not verify.
     Copy must say what happened and what basement did, and must not speculate about
     why. Draft: `A recipe list arrived that basement could not verify, so it was
     ignored. The models below are the ones basement already trusted.`
   - a `New` marker on catalog entries first seen within some window (the mockup picks
     the window and the shape).

**Tests.** Feed state after each failure class; the state survives a refresh that fails
after a success; first-seen is written once and not updated on later refreshes; mock
harness screenshots for the normal, unreachable, and signature states plus a new-recipe
marker.

## D. Publishing, and the format the ADR describes but the code does not build

**Problem 1: the format.** ADR 0009 point 2 describes a manifest of pointers
(`{id, version, sha256, url}`) plus one file per recipe version, with per-file sha256
checked against the signed manifest. The code implements a single `index.json` carrying
whole recipe objects inline, with one signature over the whole thing. There is no second
verification stage because there is no second stage.

The implemented shape is better for this product: one fetch, one signature, no partial
states where the manifest is newer than a file, and immutability is already guaranteed by
the version rule in `recipe.Merge`. The right resolution is to amend the ADR to describe
what was built, not to build the ADR. That amendment is part of this section, and it also
fixes the `runonspark-*` label names and the `https://runonspark.ai/recipes/v1/feed.json`
URL that the code does not use.

**Problem 2: nothing validates a published index.** ADR 0009 point 1 says CI runs the
strict validator over recipe pull requests. `.github/workflows/ci.yml` does `go vet` and
`go test` and nothing else, and the recipes repository does not exist.

**Build.**
1. Amend `docs/decisions/0009-signed-recipe-feed-design.md`: a dated amendment section
   recording the single-file format, the raw-ed25519-in-a-`.minisig`-named-file decision
   already documented in `internal/recipe/signature.go`, the actual label vocabulary, and
   the actual URL. Do not rewrite the original decision text; append.
2. `cmd/sign-index`: add `-verify` which checks an index and signature against the
   embedded public keys and validates every recipe, printing one line per recipe and a
   non-zero exit on any problem. This is the tool a publishing pipeline runs before
   uploading and a human runs before believing anything.
3. `make verify-index` alongside `make sign-index`.
4. A CI job in this repository that runs `-verify` over a checked-in fixture index, so
   the verifier itself is exercised on every push. Nothing else in `.github/` changes;
   the recipes repository gets its own workflow when it exists, and that is not executor
   work.

**Tests.** `-verify` accepts a good index, rejects a tampered one, rejects an index whose
recipes fail validation, and its exit codes are asserted (`cmd/sign-index/main_test.go`
is the existing pattern).

## Open questions (owner)

- **Where does the feed live?** `IndexURL` points at
  `https://raw.githubusercontent.com/punkjazz-labs/runonspark-recipes/main/index.json`,
  a repository that does not exist, under the pre-rename name. The product is basement at
  `basement.punkjazz.ai`. Decide the URL before the key ceremony, because the URL is
  baked into a release just as firmly as the key is.
- **The trust clamp in section B.** Clamping means a verified recipe requires a manager
  release. Accept that, or accept that the signer's word is what `basement-verified`
  means. There is no third option that is honest.
- **Tombstones.** A recipe that turns out to be broken cannot be withdrawn: the merge
  rule only ever moves versions forward, and the embedded floor never goes away.
  Publishing a higher version that is fixed is the only recall mechanism today. Is that
  enough?
- **Channels.** Spec 04 listed stable and beta as a non-goal. Still a non-goal?
