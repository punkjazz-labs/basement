# Base tag: nvidia/cuda:13.0.2-runtime-ubuntu24.04, pinned by its multi-arch
# index digest (resolved from the Docker Hub registry API 2026-08-04; the
# linux/arm64 child is sha256:bbfc230dfd4c3bb2eadacdc22cb7e2ea3453e93bb3ee40e241357eb0bcd5c6a6).
FROM nvidia/cuda@sha256:6a0e31b59e70890446f2c17356d6efc0d54260090a8c63d6ca7c2ad049db95d2

ARG MAX_JOBS=4
ARG COMFYUI_COMMIT=b1693ecba9f5b65f8c80ab36b195ab963ec92413

ENV DEBIAN_FRONTEND=noninteractive \
    VIRTUAL_ENV=/opt/venv \
    PATH=/opt/venv/bin:${PATH} \
    MAX_JOBS=${MAX_JOBS} \
    PYTHONDONTWRITEBYTECODE=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PIP_NO_CACHE_DIR=1 \
    HOME=/root/comfyui-user \
    TMPDIR=/root/comfyui-temp \
    XDG_CACHE_HOME=/root/comfyui-temp/cache \
    XDG_CONFIG_HOME=/root/comfyui-user/config \
    XDG_DATA_HOME=/root/comfyui-user/data \
    CUDA_CACHE_PATH=/root/comfyui-temp/cuda-cache \
    HF_HOME=/root/comfyui-temp/huggingface \
    NUMBA_CACHE_DIR=/root/comfyui-temp/numba-cache \
    TORCH_HOME=/root/comfyui-temp/torch \
    TORCHINDUCTOR_CACHE_DIR=/root/comfyui-temp/torchinductor \
    TRITON_CACHE_DIR=/root/comfyui-temp/triton

RUN apt-get update \
    && apt-get install --yes --no-install-recommends \
        ca-certificates \
        ffmpeg \
        git \
        python3 \
        python3-venv \
    && rm -rf /var/lib/apt/lists/* \
    && python3 -m venv "${VIRTUAL_ENV}" \
    && python -m pip install --no-cache-dir \
        pip==25.1.1 \
        setuptools==80.9.0 \
        wheel==0.45.1

# Source: ComfyUI manual NVIDIA install guidance for DGX Spark.
# https://download.pytorch.org/whl/cu130
RUN python -m pip install --no-cache-dir \
        --extra-index-url https://download.pytorch.org/whl/cu130 \
        torch==2.10.0+cu130 \
        torchvision==0.25.0+cu130 \
        torchaudio==2.10.0+cu130

RUN git init /opt/ComfyUI \
    && git -C /opt/ComfyUI remote add origin https://github.com/Comfy-Org/ComfyUI.git \
    && git -C /opt/ComfyUI fetch --depth 1 origin tag v0.30.0 \
    && test "$(git -C /opt/ComfyUI rev-parse 'refs/tags/v0.30.0^{commit}')" = "${COMFYUI_COMMIT}" \
    && git -C /opt/ComfyUI checkout --detach "${COMFYUI_COMMIT}" \
    && test "$(git -C /opt/ComfyUI rev-parse HEAD)" = "${COMFYUI_COMMIT}" \
    && rm -rf /opt/ComfyUI/.git \
    && python -m pip install --no-cache-dir -r /opt/ComfyUI/requirements.txt

RUN printf '%s\n' \
        'basement:' \
        '  base_path: /models' \
        '  diffusion_models: diffusion_models/' \
        '  text_encoders: text_encoders/' \
        '  vae: vae/' \
        > /opt/ComfyUI/extra_model_paths.yaml \
    && mkdir -p \
        /models/diffusion_models \
        /models/text_encoders \
        /models/vae \
        /root/comfyui-input \
        /root/comfyui-output \
        /root/comfyui-temp \
        /root/comfyui-user

WORKDIR /opt/ComfyUI
EXPOSE 8188

CMD ["python", "main.py", "--listen", "0.0.0.0", "--port", "8188", "--input-directory", "/root/comfyui-input", "--output-directory", "/root/comfyui-output", "--temp-directory", "/root/comfyui-temp", "--user-directory", "/root/comfyui-user", "--extra-model-paths-config", "/opt/ComfyUI/extra_model_paths.yaml", "--disable-all-custom-nodes", "--disable-auto-launch"]
