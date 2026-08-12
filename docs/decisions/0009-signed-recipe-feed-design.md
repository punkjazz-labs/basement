# ADR 0009: Signed recipe feed design

Status: design accepted; implementation status amended 2026-08-05 below

## Goal

Let a Spark learn about new recipes without a manager release,
without ever weakening PRD §11's trust rules, and provide the delivery pipe
for the recorded automated-discovery direction (PRD §7.4: an agent scanning
X/Hugging Face for new Spark-capable models and drafting candidate recipes).

## Accepted design

The numbered list records the delivery design. Future tense is deliberate: it
does not describe an operational feed today.

1. **Authoring flow.** Drafts (human- or agent-authored) will enter a recipe
   repository as pull requests. CI will run the same strict validator that
   gates embedded recipes. A human will approve and merge; nothing will
   self-publish.
2. **Feed format.** Amended 2026-08-12 to match what was built and tested: one
   signed JSON index `{schema_version, generated_at, recipes: [...]}` with
   complete recipe objects inline, plus a detached signature file named
   `index.json.sig` holding a base64-encoded raw ed25519 signature. The
   original multi-file manifest (a manifest of per-recipe sha256+url entries
   plus one JSON file per recipe version) bought only per-file caching at the
   cost of a second integrity mechanism, and the single-file shape keeps the
   trust boundary in exactly one function. Published indexes are immutable; a
   change is a new `generated_at`. The signature file is deliberately not
   minisign-compatible and no longer pretends to be by its name.
3. **Signing.** The manifest will be signed with ed25519. The public key will
   ship inside the manager binary. The manager will verify the manifest
   signature, then each recipe file's sha256 against the manifest. Key
   rotation will ship as a manager release carrying old and new keys.
4. **Trust labels are not transferable.** A feed recipe will arrive at most as
   `basement-candidate`. `basement-verified` will require DGX qualification
   evidence recorded in the repository; the feed will only distribute labels
   the repository evidence supports.
5. **Consent and execution.** The manager will fetch the feed read-only and
   show "new model available". Installing will still run the full local
   validator, preflight, licence acceptance, and confirmation. No fetched
   recipe will execute anything by virtue of arriving; `run_shell` will remain
   impossible by schema.
6. **Failure posture.** Unsigned, tampered, stale, or unparseable feeds will be
   ignored with a diagnostic, while embedded recipes will keep working
   offline.
7. **Revocation.** Added 2026-08-12, before any feed goes live, because
   retrofitting revocation is much harder than designing it in. Signing proves
   a recipe came from us; revocation is how we say we no longer stand behind
   one — wrong weights, a licence problem, a compromised runtime image.

   - **Where it lives.** The signed index gains a `revoked` array of
     `{id, version, reason, revoked_at}` entries. Same document, same
     signature, same freshness and downgrade protection as the recipes
     themselves; there is no second channel to secure or to miss. `reason` is
     required and human-readable, because the console will show it to the
     person whose model it concerns.
   - **Scope.** An entry names exactly one recipe id and version. The schema
     deliberately cannot express "all versions", "all recipes", or any
     product-wide statement, so the mechanism cannot become a kill switch for
     the fleet. The issuer is whoever holds the feed signing key, under the
     same custody ceremony the feed already requires; indexes are immutable
     and published, so every revocation is public and auditable by
     construction.
   - **Permanence.** Once a manager has accepted an index revoking a version,
     that version stays revoked on that machine even if a later index omits
     the entry. Un-revoking would let a compromised key quietly restore a
     pulled recipe; the honest remedy for an over-broad revocation is
     publishing a fixed new version, which is already how any recipe change
     ships.
   - **Effect on installs.** A revoked version cannot be newly installed. The
     refusal shows the reason, not a generic error.
   - **Effect on a model already serving.** The manager never stops a running
     model on its own: stopping someone's model without warning is its own
     harm. The console tells the truth instead — a visible notice on the
     model and on the recipe, carrying the reason — and the owner decides.
     Saying nothing is the only forbidden option.
   - **Offline machines.** A machine that has not fetched in a long time is
     protected by honesty rather than enforcement: when the accepted index is
     older than the staleness bound (30 days), the console reports that
     revocations may have been missed, alongside the same feed-health surface
     item 6 requires. Never fetching cannot un-revoke anything already
     accepted, and embedded recipes are revoked the way they ship: by a
     manager release.
   - **Wire compatibility.** The `revoked` field enters the current schema
     version now, while no feed is live and no fielded manager parses one, so
     no version bump is spent on it. The strict index decoder means this is
     the last free moment to add it: after a feed exists, any new top-level
     field is a schema version bump by definition.

Prerequisites to operational use include a real signing key, a decision record
fixing the feed key ceremony, and a publication path that holds that key.

## Implementation status, 2026-08-05

The safety mechanism is partly built, but the delivery service is not
operational.

Built:

- The manager can fetch an index and detached signature in the background,
  verify the signature before parsing, strictly validate each inline recipe,
  reject stale indexes, cache accepted bytes, and keep embedded recipes as its
  offline floor.
- A local `sign-index` command can sign an index with a private key supplied by
  file path.
- The recipe schema carries `basement-candidate`, `basement-verified`,
  `candidate`, and `dgx-spark-verified` states.
- Embedded recipes pin artifact revisions and expected byte counts, and pin
  runtime images by SHA-256 digest. These pins are enforced independently of
  whether a remote feed exists.

Not operational or not built:

- The embedded feed public key is a placeholder whose private half was
  discarded. There is no production signing key, custody decision, or working
  rotation path.
- The configured index URL is explicitly marked as a placeholder. No
  production index or signature is published there.
- No repository workflow creates a recipe index, validates one as a publication
  artifact, signs it, or publishes it.
- The console does not announce a newly arrived recipe, render the revocation
  notice, or draw the feed-health surface the manager now serves.

Resolved since:

- Revocation (item 7) is built in the manager, 2026-08-12. The signed index
  carries a top-level `revoked` array of `{id, version, reason, revoked_at}`,
  entering the current schema version while no feed is live and no fielded
  manager parses one. The decoder is strict in both directions: an unknown
  top-level field is still refused, a revocation entry with an unknown field,
  a missing or blank reason, a non-exact version, or a `revoked_at` that is
  not RFC3339 refuses the whole index rather than being dropped with a
  reason, and the schema still cannot express "all versions" or anything
  product-wide. Accepted revocations are recorded in SQLite
  (`recipe_revocations`, added by its own migration) with the reason and both
  timestamps; the table is insert-only and no code path above it can remove a
  row, so a later index that omits an entry, a restart, and a machine that
  never fetches again all leave the version revoked. A revoked version cannot
  be newly installed: the refusal is raised as soon as the recipe is resolved
  and carries the publisher's reason verbatim. Nothing stops a model that is
  already serving — ingest writes one row and generates no plan, stop, or
  switch — and the console is given what it needs to say so: `revoked` and
  `revoked_reason` on both the catalog and the installed models, plus a
  `recipe_feed` health object on `/api/v1/system` reporting
  `{state, accepted_generated_at, fetched_at, stale}` with the 30-day
  staleness bound. Still not built, and not to be described otherwise: the
  console does not yet render the notice or the feed-health surface, the
  signing key is still the discarded placeholder, and no publication workflow
  produces or signs an index, so no revocation can actually be issued today.
- Feed ingest now enforces item 4 (2026-08-12): whatever a signed index
  claims, a recipe arrives at most as `basement-candidate` with `candidate`
  verification, demoted at the trust boundary in `VerifyAndParseIndex` with a
  logged reason.
- The wire format question is settled by the amended item 2: the single
  signed index is the accepted design, and the signature file is named
  `index.json.sig` to say what it actually is.

Recipes reaching users today are the recipes embedded in the manager binary.
They do not arrive through a signed recipe feed. The future feed design remains
the delivery goal, but none of its missing operational parts may be described
as live until a real key, URL, publication workflow, and evidence policy are in
place.
