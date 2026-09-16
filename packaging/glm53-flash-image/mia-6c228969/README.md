# GLM runtime candidate at Mia 6c228969

This candidate packages the launch-time overlays from Mia commit
`6c228969a81d48372173bee55e5ad1b4753e656e` for Basement, which replaces the
upstream entrypoint. It does not change weights or enable additional runtime
performance settings. The existing Basement donor hook remains the owner of
optional ablation; upstream's installed ablation hook is inert without `ABLIT=1`.

Source: https://github.com/MiaAI-Lab/GLM-5.3-Flash-EXL3-2x-DGX-Sparks/tree/6c228969a81d48372173bee55e5ad1b4753e656e

## Build context and pinning

Use the extracted source archive at:

https://codeload.github.com/MiaAI-Lab/GLM-5.3-Flash-EXL3-2x-DGX-Sparks/tar.gz/6c228969a81d48372173bee55e5ad1b4753e656e

Verify the downloaded archive before extracting:

`SHA256: 1ff501f937babca69a09e1373c59a5115f3d249236f22e7b7631f55dd131b2e1`

The extracted upstream directory is the Docker build context. Pass this
Dockerfile by absolute path to avoid resolving it against the caller's
working directory:

```sh
docker build --file /absolute/path/to/this/Dockerfile \
  --tag basement-glm53-candidate:6c228969 \
  /absolute/path/to/extracted/upstream
```

The base image is pinned in the Dockerfile. Its native `exl3_fat_moe.cu` and
`exl3_fat_moe.cuh` layers were compared byte-for-byte with this upstream
revision; their SHA256 values are respectively
`21e625aa439367ed13feb88bbb55b72a06f2e5ce56d3615b3d4e1a28cb231e44` and
`0b3ae15e9d42a582c32368ec4f36e386580d4207408cedb0cd44784ee85a8e0e`.
The overlays therefore reuse the matching compiled extension.

All 17 launch overlays execute in upstream order and fail the build on error.
The CPU EXL3 registration, sharding and native-architecture self-check runs
at build time. GPU and real serving qualification are separate requirements.
Do not treat a successful build as a production qualification.

Stock recipe v3 and ablated recipe v2 pin the published image built from this
source. The old recipe definitions remain embedded for exact installed-version
resolution and rollback. A manager update advertises the new catalog; an explicit
Basement install/update activates it. See the parent README for the immutable
image identity. Successful build checks alone do not qualify live inference.

The pinned source archive carries the GNU Affero General Public License
version 3 text, reproduced in UPSTREAM-LICENSE. This differs from the
older Basement image's MIT-labelled patch snapshot; preserve the current
upstream license and notices when distributing this candidate.
