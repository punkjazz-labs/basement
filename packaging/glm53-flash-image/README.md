# GLM-5.3-Flash EXL3 image

This directory defines the arm64 vLLM image used by the two-GB10
GLM-5.3-Flash EXL3 recipe. GitHub Actions builds and publishes it from this
directory on a `glm53-flash-image-v*` tag.

## Provenance

The runtime base is pinned to
`ghcr.io/miaai-lab/glm-5.3-flash-2x-dgx-sparks@sha256:eecb36e14dc34c92d46827fde7b09f7e0bf27e27c426ece126376c02dea6cd2f`.

The launcher's source is pinned to
[`6c228969a81d48372173bee55e5ad1b4753e656e`](https://github.com/MiaAI-Lab/GLM-5.3-Flash-EXL3-2x-DGX-Sparks/tree/6c228969a81d48372173bee55e5ad1b4753e656e).
The Dockerfile downloads GitHub's archive for that exact revision and requires
sha256 `1ff501f937babca69a09e1373c59a5115f3d249236f22e7b7631f55dd131b2e1`.
It copies only `overlay/`, `tests/`, `files/chat_template.jinja`, `LICENSE`,
and `LICENSE.MIT` from the verified source stage.

The image applies the launcher's 17 runtime overlays in the launcher's order,
then runs its CPU-only EXL3 overlay self-check. The model checkpoint is not in
the image and remains pinned and licence-gated by the recipe.

## Licences

The current upstream source carries AGPL-3.0 in `LICENSE`. Its retained
historical MIT notice is copied as `LICENSE.MIT`; both notices are retained at
`/opt/glm53/` in the published image. The checkpoint and optional drafter have
their own licences in the recipe and are not redistributed here.

## Publish

Push an annotated or lightweight tag named `glm53-flash-image-v*` after main
is green. The existing `glm53-flash-image` GitHub workflow builds on an arm64
runner and prints the immutable GHCR digest and image size. Pin that digest in
the recipe; never use the tag as a runtime image reference.
