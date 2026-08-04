# ComfyUI image for GB10

This directory defines the arm64 ComfyUI image intended for NVIDIA GB10
machines. It is built natively on the target machine because no official
arm64 ComfyUI image exists.

## Pins

- CUDA base tag: `nvidia/cuda:13.0.2-runtime-ubuntu24.04`
- CUDA base digest: unresolved. Network access was unavailable while this
  definition was authored. The loud TODO in the Dockerfile must be resolved
  against the Docker Hub linux/arm64 manifest before this image is published.
- ComfyUI release: `v0.30.0`
- ComfyUI commit: `b1693ecba9f5b65f8c80ab36b195ab963ec92413`
- PyTorch: `torch==2.10.0+cu130`
- TorchVision: `torchvision==0.25.0+cu130`
- TorchAudio: `torchaudio==2.10.0+cu130`
- Wheel index: `https://download.pytorch.org/whl/cu130`

The wheel index is the one in ComfyUI's NVIDIA installation guidance used for
DGX Spark. The Dockerfile installs the exact PyTorch family before installing
ComfyUI's requirements. The remaining dependency declarations come from
`requirements.txt` as shipped at the pinned ComfyUI commit. No unverified
versions are added to replace constraints that upstream leaves open.

## Rebuild

First resolve the Dockerfile's base-image TODO through the Docker Hub registry
API and replace the tag-only `FROM` line with the linux/arm64 manifest digest.
Keep the selected tag in the adjacent comment.

From the repository root on the Mac, run:

```sh
./packaging/comfyui-image/build.sh
```

The script synchronizes this directory to
`spark@192.168.10.148:/tmp/comfyui-image-build/`, builds it natively as
`basement-comfyui:v0.30.0` with `MAX_JOBS=4`, then prints the image ID and size.
It is safe to run again against the same temporary directory and image tag.

## Runtime filesystem contract

The image routes ComfyUI's runtime writes into four directories. A basement
recipe using this image must declare all four as writable paths:

- `/root/comfyui-input`
- `/root/comfyui-output`
- `/root/comfyui-temp`
- `/root/comfyui-user`

`TMPDIR`, Python and CUDA caches, Hugging Face caches, Torch caches, and Triton
caches all resolve inside `/root/comfyui-temp`. Generic user configuration and
data resolve inside `/root/comfyui-user`. Python bytecode writes are disabled.
This keeps the rest of the container root filesystem read-only.

Basement must mount the model artifact read-only at `/models` with this layout:

```text
/models/
  diffusion_models/
  text_encoders/
  vae/
```

The baked `/opt/ComfyUI/extra_model_paths.yaml` maps those three model classes
to the mounted directories. The image contains no custom nodes, and its command
also disables custom-node loading.

GPU smoke testing on a GB10 machine and publishing the qualified image to a
registry are separate later steps. This build script performs neither step.
