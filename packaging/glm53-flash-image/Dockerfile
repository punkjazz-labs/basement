# syntax=docker/dockerfile:1.7
# Pinned first-party GLM-5.3-Flash EXL3 image for two GB10 nodes.
#
# The GitHub archive is part of the build provenance. `--checksum` makes a
# changed or substituted archive fail before any upstream source is copied.
FROM ghcr.io/miaai-lab/glm-5.3-flash-2x-dgx-sparks@sha256:eecb36e14dc34c92d46827fde7b09f7e0bf27e27c426ece126376c02dea6cd2f AS source
ADD --checksum=sha256:1ff501f937babca69a09e1373c59a5115f3d249236f22e7b7631f55dd131b2e1 \
    https://github.com/MiaAI-Lab/GLM-5.3-Flash-EXL3-2x-DGX-Sparks/archive/6c228969a81d48372173bee55e5ad1b4753e656e.tar.gz \
    /tmp/upstream.tar.gz
RUN mkdir /source && tar -xzf /tmp/upstream.tar.gz --strip-components=1 -C /source

FROM ghcr.io/miaai-lab/glm-5.3-flash-2x-dgx-sparks@sha256:eecb36e14dc34c92d46827fde7b09f7e0bf27e27c426ece126376c02dea6cd2f
LABEL org.opencontainers.image.source="https://github.com/punkjazz-labs/basement" \
      org.opencontainers.image.description="vLLM for GLM-5.3-Flash EXL3 on two GB10 nodes" \
      ai.basement.base.image="ghcr.io/miaai-lab/glm-5.3-flash-2x-dgx-sparks@sha256:eecb36e14dc34c92d46827fde7b09f7e0bf27e27c426ece126376c02dea6cd2f" \
      ai.basement.patch.source="https://github.com/MiaAI-Lab/GLM-5.3-Flash-EXL3-2x-DGX-Sparks" \
      ai.basement.patch.revision="6c228969a81d48372173bee55e5ad1b4753e656e" \
      ai.basement.patch.archive.sha256="1ff501f937babca69a09e1373c59a5115f3d249236f22e7b7631f55dd131b2e1" \
      ai.basement.patch.licence="AGPL-3.0" \
      ai.basement.patch.historical-notice="MIT"

# Copy only the launcher's runtime overlay, its source self-check, template,
# and licence notices. The full upstream tree is neither vendored nor shipped.
COPY --from=source /source/overlay/ /opt/glm53/
COPY --from=source /source/tests/ /opt/glm53/source-tests/
COPY --from=source /source/files/chat_template.jinja /opt/glm53/chat_template.jinja
COPY --from=source /source/overlay/exl3.py /usr/local/lib/python3.12/dist-packages/vllm/model_executor/layers/quantization/exl3.py

# Keep the launcher's patch order exactly. Each script is idempotent only for
# its intended target, so a changed base or overlay fails the build.
RUN python3 /opt/glm53/patch_glm_video_placeholders.py
RUN python3 /opt/glm53/patch_suppress_stops_in_reasoning.py
RUN python3 /opt/glm53/patch_scheduler_decode_floor.py
RUN python3 /opt/glm53/patch_glm5_drafter_group.py
RUN python3 /opt/glm53/patch_hybrid_prefix_hit.py
RUN python3 /opt/glm53/patch_apc_per_group_retention.py
RUN python3 /opt/glm53/patch_apc_no_store.py
RUN python3 /opt/glm53/patch_kv_capacity_log.py
RUN python3 /opt/glm53/patch_xgrammar_termination.py
RUN python3 /opt/glm53/patch_kpool_tail_slotmap.py
RUN python3 /opt/glm53/patch_spinwait.py
RUN python3 /opt/glm53/patch_adaptive_k.py
RUN python3 /opt/glm53/patch_dense_fp8.py
RUN python3 /opt/glm53/patch_default_max_new_tokens.py
RUN python3 /opt/glm53/patch_indexer_workspace.py
RUN python3 /opt/glm53/patch_cache_reset.py
RUN python3 /opt/glm53/patch_ablit.py
RUN EXL3_SELFCHECK_GPU=0 python3 /opt/glm53/source-tests/test_exl3_overlay.py

COPY --from=source /source/LICENSE /source/LICENSE.MIT /opt/glm53/
