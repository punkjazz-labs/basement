# ADR 0012: Curated model trust

Date: 2026-08-03. Status: proposed (future direction).
Direction from the designer, recorded 2026-08-03.

## Context

Download a random recipe or quant from the internet and you cannot know
what it is. It may be misconfigured, mislabeled, or malicious. Model
weights, runtime images, and chat templates all execute as code or as
code-adjacent input. Right now the model ecosystem asks you to trust a
repository name and hope.

basement already refuses to work that way for its own catalog: every
artifact traces to a primary source, is pinned by revision and byte
count, has its licence read, and is tested on our own hardware before it
is called verified (docs/MODEL-CANDIDATES-2026-08.md is the method;
docs/decisions/0011 covers runtimes). This ADR names that discipline as
the product's trust core, from "our catalog is curated" to "curation is
a pipeline anyone's recipe can enter and a label anyone can check."

## The idea

Curation is not a filter on top of the product. It is the product.
basement's pitch is not "more models" but "models you do not have to take
on faith." Every recipe that reaches a user has been:

1. traced to a primary source (Hugging Face repository and revision, not
   a mirror or a screenshot);
2. pinned by revision and exact byte count, so "the same model" cannot
   quietly become a different one;
3. read for licence terms, recorded, and surfaced before install;
4. installed and run on our own Spark hardware, with measured numbers,
   before the label changes from candidate to verified;
5. signed for delivery, so the feed itself cannot be tampered with in
   transit (docs/decisions/0009).

None of this is new work. It is what the manager already does. What is
new is treating it as a visible, checkable trust chain rather than an
implementation detail.

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

- Nothing here changes shipped behaviour today. Existing labels
  (candidate, verified) and the ADR 0009 feed design already carry this
  weight; this ADR names the direction so future work (registry UI,
  third-party submission flow) has a decision record to point at.
- Where a registry page or provenance chain does not exist yet, say so.
  n/a is honest; an invented number is not.
- This raises the bar going forward: any feature named after trust
  (verified, signed, curated) must correspond to a real check that ran,
  on real hardware, with a receipt.
