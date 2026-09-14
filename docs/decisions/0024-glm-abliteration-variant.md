# ADR 0024: Optional GLM ablation through model lifecycle

Status: accepted for implementation; hardware qualification recorded separately

## Decision

Expose the stock and abliterated GLM configurations as separate curated recipe IDs.
They share the exact primary artifact and runtime image. Only the abliterated recipe
sets `runtime.abliteration` and declares the pinned donor artifact. No stock checkpoint
file is modified, converted or re-quantized.

The console offers Download without activation, Enable through the existing start/switch
job, and Disable by starting the installed stock recipe on the same machine. Both
recipes remain visible, making the active configuration explicit. Disable is unavailable
until stock GLM is installed. A model switch reloads both ranks and interrupts requests
on that pair. The stable manager endpoint does not change.

Artifact ranges follow ADR 0023. The manager embeds a narrowly scoped GLM load hook,
its manifest, and an additive Python import bootstrap. Generated files are mounted
read-only into the pinned image and are included in container configuration identity.
The hook replaces only the selected in-memory attention output projection tensors,
including the MTP layer, after ordinary loading. Every donor is checked against its
pinned size and SHA-256 before copying the TP-local slice. Unsupported layouts or
missing/corrupt donors fail the load rather than serving stock under the ablated identity.

The recipe identity is the persisted setting. Existing transactional switching,
per-recipe jobs, distributed worker coordination, recovery and rollback apply. This
adds no alternate launcher, sidecar manager, mutable environment override, or UI-only
boolean. Stored recipes preserve the mode across manager restarts.

## Scope and evidence

The mode follows MiaAI-Lab's load-time transplant design at commit
`f906ee990596486e10ddbe381efa6f0e496f77e3`. Donor tensors are copied unchanged from
`dealignai/GLM-5.3-Flash-UNCENSORED-NVFP4` revision
`f75389c0919bd6d3abd4f08c7a0f22545edc95d7`. Model and donor licences remain visible
in the normal installation flow. Reduced refusal behavior is an upstream claim;
Basement does not claim improved coding quality or unchanged throughput.

A runtime load hook is part of the manager's embedded, versioned source and generated
configuration. It does not permit arbitrary third-party Python hooks. The image's
existing startup behavior must remain intact; the bootstrap is additive.
