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
- The console does not announce a newly arrived recipe or show feed health.
Resolved since:

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
