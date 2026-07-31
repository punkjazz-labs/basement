# ADR 0009: Signed recipe feed — design

Status: design accepted; not yet implemented (PRD decision 9 gate holds)

## Goal

Let a Spark learn about newly verified recipes without a manager release,
without ever weakening PRD §11's trust rules, and provide the delivery pipe
for the recorded automated-discovery direction (PRD §7.4: an agent scanning
X/Hugging Face for new Spark-capable models and drafting candidate recipes).

## Design

1. **Authoring flow.** Drafts (human- or agent-authored) enter the
   `runonspark-manager` repository as pull requests. CI runs the same strict
   validator that gates embedded recipes. A human approves and merges;
   nothing self-publishes.
2. **Feed format.** `https://runonspark.ai/recipes/v1/feed.json`: a manifest
   `{generated_at, recipes: [{id, version, sha256, url}]}` plus one JSON file
   per recipe version. Recipe files are immutable once published; a change is
   a new version.
3. **Signing.** The manifest is signed with ed25519 (minisign format). The
   public key ships inside the manager binary. The manager verifies the
   manifest signature, then each recipe file's sha256 against the manifest.
   Key rotation ships as a manager release carrying old + new keys.
4. **Trust labels are not transferable.** A feed recipe arrives at most as
   `runonspark-candidate`. `runonspark-verified` requires DGX qualification
   receipts recorded in the repository; the feed only distributes labels the
   repository evidence supports.
5. **Consent and execution.** The manager fetches the feed read-only and
   shows "new model available". Installing still runs the full local
   validator, preflight, licence acceptance, and confirmation. No fetched
   recipe executes anything by virtue of arriving; `run_shell` remains
   impossible by schema.
6. **Failure posture.** Unsigned, tampered, stale (manifest older than the
   last seen one), or unparseable feeds are ignored with a diagnostic —
   embedded recipes always keep working offline.

Prerequisite to implementation: release-signing infrastructure from ADR 0008
(same key-management story), and a decision record fixing the feed key
ceremony.
