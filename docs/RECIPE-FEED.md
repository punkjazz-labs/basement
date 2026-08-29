# Runbook: the recipe feed

This runbook tells you how to publish the remote recipe feed and how to
manage it after launch. It uses Simplified Technical English. Each sentence
gives one fact or one instruction.

The feed's design record is ADR 0009
(`docs/decisions/0009-signed-recipe-feed-design.md`). Read that record first
if you want the reasons behind a rule stated here. This runbook does not
repeat those reasons. It states what to do.

## 1. What the feed is

The feed is two files at a fixed URL: `index.json` and `index.json.sig`.

`index.json` lists recipes. Each recipe can install one model. `index.json.sig`
is a detached signature over `index.json`. The manager checks the signature
before it reads the recipe list.

The manager fetches both files every hour. It also fetches them once at
startup.

A recipe in the feed can carry a higher version number than the matching
built-in recipe. When it does, the feed recipe replaces the built-in recipe
in the catalog. The catalog is the list of recipes a user can install.

A catalog change does not change an installed model. The model keeps running
with the recipe version it was installed with. A user must start an update
by hand. Nothing in the manager starts an update on its own.

The feed can also carry revocations. A revocation marks one recipe version as
withdrawn. Section 5 of this runbook gives the full rule. One fact matters
here: a revocation is permanent. Once a manager accepts a revocation, that
manager keeps it forever, even if a later index drops the entry.

## 2. One-time activation

The feed is not live yet. Four steps make it live. Do them in order.

### 2a. Run the key ceremony

Generate one ed25519 keypair. `cmd/sign-index/main.go` documents the exact
key format: the private key file holds the base64 encoding of the raw 64-byte
private key that `crypto/ed25519.GenerateKey` returns.

Keep the private key out of this repository. Never commit it. Never place it
in a log file. Never place it in an environment variable's value.

Record the private key's location the same way you record the release
signing keys. Ask Simone where that record lives before you generate the key,
so the new key ends up in the same place.

### 2b. Set the public key in the binary

Open `internal/recipe/signature.go`. Find the constant
`IndexPublicKeyBase64`. Replace its placeholder value with the real public
key from step 2a.

Build and commit this change like any other code change.

### 2c. Point the manager at the feed repository

Two choices exist. Pick one.

Choice one: create the repository `punkjazz-labs/runonspark-recipes`. This is
the repository name `internal/recipefeed/fetch.go` already assumes.

Choice two: change the constant `IndexURL` in
`internal/recipefeed/fetch.go` to a different host. This constant is the only
line that needs to change for a different host.

### 2d. Ship one release

Build and ship one manager release that carries the real public key from step
2b.

State this plainly to every reader of this runbook: a machine trusts the real
feed only after it runs a release built with the real key. A machine on an
older release ignores the real feed. It keeps trusting only its embedded
recipes.

## 3. The publish flow

Follow these steps to publish a new index.

1. Build a manager binary from the commit you want to publish.
2. Run `build-index` from that binary: `go run ./cmd/build-index -out
   index.json`, or `make build-index OUT=index.json`. This step reads
   `docs/RECIPE-FEED.md` section 2a for key handling and writes an unsigned
   `index.json`.
3. Add revocations if you have any. Pass `-revoked
   path/to/revoked.json` to `build-index`. Section 5 of this runbook gives
   the file format.
4. Sign the index: run `BASEMENT_SIGN_KEY=<path to the private key file> make
   sign-index INDEX=index.json`. This writes `index.json.sig` beside
   `index.json`.
5. Verify the signature locally before you publish. `cmd/sign-index` has no
   built-in verify mode, so use a short Go program:

   ```go
   package main

   import (
       "fmt"
       "os"

       "github.com/punkjazz-labs/basement/internal/recipe"
   )

   func main() {
       indexBytes, err := os.ReadFile("index.json")
       if err != nil {
           panic(err)
       }
       sigBytes, err := os.ReadFile("index.json.sig")
       if err != nil {
           panic(err)
       }
       if err := recipe.VerifySignature(indexBytes, sigBytes, recipe.IndexPublicKey()); err != nil {
           fmt.Println("signature does not verify:", err)
           os.Exit(1)
       }
       fmt.Println("signature verifies")
   }
   ```

   Run this program with `go run`. Fix the problem before you publish if it
   prints an error.
6. Push `index.json` and `index.json.sig` to the feed repository. Push both
   files together. A manager that fetches one without the other treats the
   fetch as failed and keeps its old catalog.

One rule governs the whole flow: `build-index` reads only the recipes
embedded in the binary you built in step 1. It never reads a YAML directory
or any other outside source. This means a recipe change always lands in this
repository first, as a normal commit, and only then gets published through
this flow. The published index can never contain a recipe that is not also
in this repository's history.

## 4. How an upstream update becomes a version bump

`cmd/feed-watch` is the tool that watches for upstream changes. It has two
modes.

`feed-watch -mode check` scans every embedded recipe. For each artifact, it
compares the pinned revision to the Hugging Face API's current revision. For
each GitHub source, it compares the pinned revision to the repository's
current default-branch commit. It writes a JSON report and prints one line
per change. It never writes to a recipe file.

`feed-watch -mode bump` runs the same scan. Then it applies one fixed, safe
set of changes on its own. It writes every other change to the report
instead, for a maintainer to read later.

feed-watch bumps a recipe on its own in exactly two cases:

- A whole-snapshot artifact (no `files:` list) moved to a new revision, the
  live licence tag still matches the recipe's own `licence` field, and a
  LICENSE or LICENSE.md file still exists at the new revision if one existed
  at the old revision. feed-watch rewrites the artifact's `revision`, its
  `expected_bytes`, and the revision inside `licence_url` when that URL
  carries the old revision. It raises the recipe's `version` by one.
- A per-file-pinned artifact moved to a new revision, and every pinned file
  still exists there under the same name and the same size. feed-watch
  rewrites the artifact's `revision` and the revision inside `licence_url`.
  It raises the recipe's `version` by one. It leaves `expected_bytes` alone,
  because every pinned file's own size did not change.

feed-watch never bumps a recipe on its own in any other case: a moved source
repository, a changed licence, a pinned file that changed size or vanished,
or any artifact change outside the two cases above. It writes these to the
report with the reason "needs judgment". A maintainer session reads the
report and decides what to do.

Every edit feed-watch makes is a surgical text edit on the recipe's own raw
YAML file. It is never a full rewrite. A recipe's comments survive the edit
byte for byte, outside the lines that changed. After editing, feed-watch
decodes the file with the same strict decoder and validator every recipe
already passes today. A result that fails that check is discarded before it
is written, so the file on disk stays exactly as it was.

feed-watch never runs git and never publishes anything.
`packaging/publish-feed.sh` owns commit, sign, and publish. One command runs
the whole flow:

```
make publish-feed
```

This command applies the safe subset of bumps, commits and pushes them if
there were any, builds and signs a fresh `index.json`, publishes it to the
feed repository, and verifies the published bytes match what was pushed
byte for byte. Section 6 describes the schedule that runs this command on
its own.

A drift a maintainer has judged goes into `docs/feed-acknowledged.yaml`.
Each entry names the recipe, the kind, the role for an artifact, the exact
upstream revision the ruling covered, and the reason. feed-watch reports a
covered finding as acknowledged, not open, and it does not count toward the
exit code. When upstream moves past the covered revision, the finding opens
again. The file never authorizes a bump. It only records a decision.

This tool connects to one other recorded plan. PRD section 7.4 records a
future discovery agent that scans public sources (X, Hugging Face) and
drafts new candidate recipes, not version bumps of existing ones. That path
still ends at a human commit to this repository, the same as every recipe
change does today.

## 5. Revocations

### When to use one

Use a revocation when a published recipe version must never be installed
again. Three examples: the pinned weights are wrong, the recipe has a
licence problem, or the pinned runtime image is compromised.

A revocation does not stop a model that is already running. The manager
never stops a running model on its own. The console shows a notice instead,
on the model and on the recipe, and the owner of that model decides what to
do.

### The file format the tool accepts

Pass the `-revoked` flag to `build-index` with a path to a JSON file. The
file holds a JSON array. Each entry is one object with four fields:

```json
[
  {
    "id": "some-recipe-1s",
    "version": 2,
    "reason": "the published weights were the wrong quantisation",
    "revoked_at": "2026-08-12T00:00:00Z"
  }
]
```

Every field is required.

- `id` names one recipe. It cannot be empty.
- `version` names one exact recipe version. It must be 1 or higher. It
  cannot name a range and it cannot mean "every version".
- `reason` is the human-readable reason. It cannot be empty or blank. The
  console shows this text to the person whose model it concerns, word for
  word.
- `revoked_at` is an RFC3339 timestamp. It cannot be zero or missing.

The tool rejects the whole file if any entry is missing a field, if any
field is empty where a value is required, or if the file carries a field
these four do not name. The tool also rejects the whole file if two entries
name the same recipe at the same version. Fix the file and run the tool
again.

### Revocations are permanent

Once a manager accepts an index that revokes a recipe version, that manager
keeps the version revoked forever. This stays true even if a later index
drops the entry. A dropped entry says nothing new. It does not undo the
revocation.

If a revocation turns out to be too broad, do not try to undo it. Publish a
fixed new version of the recipe instead, the same way any other recipe
change ships. The reason for this rule: allowing an un-revoke would let a
compromised signing key quietly restore a recipe version that was pulled for
a real reason.

### The reason is always shown verbatim

The console shows a revoked recipe's reason exactly as written in the
`reason` field. It never shortens the reason and it never replaces the
reason with a generic message. Write every reason so it can stand alone in
front of the person who owns the affected model.

## 6. Automation

`make publish-feed` is safe to run on a schedule. Every step in it verifies
its own result, or the step fails and the run stops before it. A step that
cannot confirm its own result never hands control to the step after it.

One `launchd` job runs `make publish-feed` once a day, on the owner's laptop.
The job file is `packaging/macos/com.punkjazz.basement.feed-publish.plist`.
It runs at 10:00. It does not run the moment it is loaded. It writes its log
to `~/Library/Logs/basement-feed-publish.log`.

Install the job with one command:

```
launchctl load ~/Library/LaunchAgents/com.punkjazz.basement.feed-publish.plist
```

Copy the plist file into `~/Library/LaunchAgents/` first. The plist carries
literal paths for the owner's own laptop, because `launchd` does not expand
`~` or `$HOME` inside a plist file. Update those paths if the checkout ever
moves, then unload and load the job again.

A green scheduled run does not mean nothing needs attention. feed-watch can
still leave a "needs judgment" entry in its report for a recipe that moved
in a way it will not bump on its own. Read the log now and then, and act on
what it finds.
