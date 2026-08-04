# Companion prompt model evidence

Fetched on 2026-08-05 in Europe/Paris.

This note records the evidence used to make the optional companion-model
comparison in ADR 0017. It is not a model qualification or a recipe proposal.

Candidate:
Qwen/Qwen2.5-1.5B-Instruct-GGUF at repository revision
91cad51170dc346986eccefdc2dd33a9da36ead9.

Sources:

- https://huggingface.co/api/models/Qwen/Qwen2.5-1.5B-Instruct-GGUF
- https://huggingface.co/api/models/Qwen/Qwen2.5-1.5B-Instruct-GGUF/tree/main?recursive=true&expand=false
- https://huggingface.co/Qwen/Qwen2.5-1.5B-Instruct-GGUF/raw/main/README.md

The repository API reports Apache-2.0, text generation, and an official
Q4_K_M file named qwen2.5-1.5b-instruct-q4_k_m.gguf. The tree response reports
that file at 1,117,320,736 bytes. The model card identifies the model as a
1.54B-parameter instruction-tuned model and states that the family improved
instruction following and structured output generation.

Fetched-response SHA-256 values:

| Response | SHA-256 |
| --- | --- |
| Model API | 65f8e1ed20d29d43715aa2cef9ee35475ed821225bdf4e9d3036e85c5ee57cf4 |
| Tree API | c5cb24bba656257555b900b278d794ee86f7ce123e5bf30b869e66408dc168e3 |
| Model card | 57c72d4010fb11a98fc8e62dc8ef112b23a7a7525b71e571ca978d35cb3b2eec |

What these sources do not prove:

- peak runtime memory on a GB10;
- memory use for a chosen context and cache;
- concurrent operation with MiniMax H3;
- prompt quality for MiniMax H3;
- safe integration with basement's active-model contract.

Those remain unverified until a pinned runtime and a real paired evaluation
are measured.
