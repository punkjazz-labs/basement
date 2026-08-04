# ADR 0012: Curated model trust

Date: 2026-08-03. Status: proposed (future direction).
Direction from the designer, recorded 2026-08-03.

## Status note, 2026-08-05

This ADR records a trust direction, not current end-to-end behavior.

- All eight embedded recipes declare `trust: basement-candidate` and
  `verification: candidate`.
- Immutable source revisions, expected artifact byte counts, and runtime image
  digests are present in the recipes and enforced. Pinning is live today.
- One exact recipe version has a hardware qualification PASS recorded in
  `docs/DGX-QUALIFICATION.md`, but its label remains candidate because evidence
  does not promote a recipe by itself and its image provenance has not earned
  `basement-verified` trust.
- The Models table does not display either trust field. The first-run
  recommendation includes a candidate sentence, but the catalog rows and row
  expansions have no candidate or verified label.
- Signed-feed verification code exists, but signed delivery does not. The key
  and URL are placeholders, no feed is published, and no workflow signs one.
  Recipes reach users inside the manager binary today.

## Context

Download a random recipe or quant from the internet and you cannot know
what it is. It may be misconfigured, mislabeled, or malicious. Model
weights, runtime images, and chat templates all execute as code or as
code-adjacent input. Right now the model ecosystem asks you to trust a
repository name and hope.

basement already implements part of that discipline. Recipes record source
URLs, immutable revisions, expected artifact byte counts, licence fields, and
runtime image digests. The validator refuses `basement-verified` unless the
verification state is `dgx-spark-verified`. Hardware qualification and signed
delivery remain gates, not properties every current recipe has.

This ADR names the full discipline as the product's trust core, from "our
catalog is curated" to "curation is a pipeline anyone's recipe can enter and a
label anyone can check."

## The idea

Curation is not a filter on top of the product. It is the product.
basement's pitch is not "more models" but "models you do not have to take
on faith." Under this proposed trust chain, a recipe will earn a verified
label only after it has been:

1. traced to a primary source (Hugging Face repository and revision, not
   a mirror or a screenshot);
2. pinned by revision and exact byte count, so "the same model" cannot
   quietly become a different one;
3. read for licence terms, recorded, and surfaced before install;
4. installed and run on our own Spark hardware, with measured numbers,
   before the label changes from candidate to verified;
5. approved for delivery through a feed whose index has been signed, so the
   manager can reject tampering in transit (docs/decisions/0009).

Steps 1 through 3 have corresponding fields in current recipe data; that does
not by itself claim every associated review is complete. Step 4 is the
promotion gate, and step 5 is future delivery infrastructure. Treating the
whole chain as visible and checkable is also future work.

Feed signing and recipe verification answer different questions. A valid feed
signature would prove that the delivered index came from the feed signer and
was not changed in transit. It would not prove that a model ran correctly or
that its evidence deserved a verified label.

## What verified requires

**Evidence.** The exact recipe version, including its source revision, artifact
bytes, runtime digest, configuration, and declared topology, must pass the
real-hardware protocol in `docs/DGX-QUALIFICATION.md`. Evidence must cover the
complete install and serving lifecycle, real inference, stop and restart,
recovery and resource guardrails, removal behavior when required, credential
redaction, and the receipts produced by those checks. Performance measurements
are recorded evidence, not a substitute for lifecycle acceptance. Any change
to a pin or runtime setting creates a new recipe version and requires a new
decision.

**Decision.** A passing script or receipt does not change trust automatically.
Today one person makes the curation decision: the owner runs the model on
hardware they control and reviews their own evidence against the protocol.
There is no independent lab, second reviewer, or external certification. The
decision must therefore be explicit and recorded with the evidence it relies
on.

**State.** The authoritative label lives in the versioned recipe fields. A
genuinely verified recipe declares `trust: basement-verified` and
`verification: dgx-spark-verified`; the validator rejects verified trust with
any other verification state. This repository contains the procedure and
published run summaries in `docs/DGX-QUALIFICATION.md`, but no raw
qualification receipts. It does not identify a public registry or separate
evidence repository. Until the recipe fields are changed through the curation
process, the recipe is a candidate even if part of its qualification has
passed.

## Threat model

What curation defends against, named plainly:

- **Typosquatted repositories.** A convincing near-match name hosting
  something other than what it claims to be.
- **Swapped revisions.** A repository that looked right at review time
  and points somewhere else later. Pinning by revision and byte count is
  the defence; an unannounced change fails the check, not the install.
- **Oversized or mislabeled quants.** A "4-bit" file that is not, or a
  quant that silently drops the features the card advertises.
- **Licence laundering.** A derivative repository that drops or
  obscures the base model's licence terms.
- **Malicious chat templates or tool parsers.** Both are effectively
  code paths (Jinja templates, tool-call parsing) and a plausible
  injection surface once a recipe is running in front of real tool use.

Each of these is a reason "download and run" is not an acceptable
default, and a reason a research and validation pipeline is not
bureaucracy but the actual safety mechanism.

## Future shape (proposal, not promise)

None of the following exists today. It is the direction curation points
toward once the signed feed (docs/decisions/0009) and multi-runtime
support (docs/decisions/0011) are further along:

- **A public registry page per recipe.** What we checked, when, on what
  hardware, and the measured numbers, so "verified" is a page you can
  open, not a label you take our word for.
- **A provenance chain.** A visible line from the Hugging Face revision
  we pinned to the recipe file we shipped, so the path from source to
  install is inspectable end to end.
- **Third-party recipes entering the same pipeline.** Community- or
  vendor-submitted recipes going through the same research, validation,
  and hardware qualification steps, and earning the same candidate or
  verified label on the same evidence, not a separate lower bar.

## Consequences

- Nothing here changes shipped behaviour today. The recipe schema can carry
  candidate and verified states, but every embedded recipe is a candidate and
  the Models table does not display those states. This ADR names the direction
  so future work, including the trust label, registry UI, and third-party
  submission flow, has a decision record to point at.
- Where a registry page or provenance chain does not exist yet, say so.
  n/a is honest; an invented number is not.
- This raises the bar going forward: any feature named after trust
  (verified, signed, curated) must correspond to a real check that ran,
  on real hardware, with a receipt.
