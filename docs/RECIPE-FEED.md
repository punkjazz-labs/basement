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

The manager fetches both files every 6 hours. It also fetches them once at
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

This section describes a direction. No part of it is built yet. Treat every
sentence in this section as a plan, not as a description of running code.

A watcher will run on a schedule. It will check each recipe's upstream
source (the model repository, the container image) for a change.

When the watcher finds a change, it will draft a version bump. The draft will
carry new pins: a new artifact revision, a new expected byte count, a new
runtime image digest, or some combination of these.

Every pin in a draft will be verified live before anyone acts on it. The
verification will check four things: the actual bytes, the actual digest,
the actual licence, and a pass through the same strict validator that gates
every recipe today.

A human owner will approve each draft, or the owner's stated policy will
allow automatic approval for the narrow case of a pin bump that passes every
live verification. The owner sets that policy. This runbook does not set it.

Once a draft is approved, it becomes a normal commit in this repository. The
publish flow in section 3 then carries it to the feed, the same way it
carries any other recipe change.

This direction connects to one other recorded plan. PRD section 7.4 records a
future discovery agent that scans public sources (X, Hugging Face) and
drafts new candidate recipes, not version bumps of existing ones. Both paths
end at the same gate: a draft, live verification, human or policy approval,
then the publish flow above. Neither path skips the gate.

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
