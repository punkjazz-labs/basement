# DGX Spark qualification

The embedded recipes remain candidates until real hardware produces the complete evidence required by `PRD.md`. Local unit tests, registry metadata, and ARM64 cross-builds do not promote recipe trust.

## One recipe per Spark

Use separate clean Sparks to qualify the three candidates in parallel:

| Recipe | Suggested machine assignment |
|---|---|
| `qwen36-35b-a3b-nvfp4-1s` | Spark A |
| `qwen36-27b-nvfp4-1s` | Spark B |
| `laguna-s-2-1-nvfp4-dflash-1s` | Spark C |

After installing and starting the manager, run the qualification helper locally on each Spark. The pairing token is read from its protected file, passed over standard input, and never written to the receipt.

```sh
sudo packaging/qualify-dgx.sh \
  http://127.0.0.1:7070 \
  qwen36-35b-a3b-nvfp4-1s \
  /var/lib/basement/qualification
```

The helper exercises authenticated preflight, installation, persisted job polling, real inference verification, stop, start, a second smoke test, and redacted diagnostic export. Add `--remove` only when the downloaded weights should be deleted and removal/reclaim behavior is the test objective.

Each run produces:

- a timestamped JSONL receipt containing system, recipe pins, preflight, and terminal job objects;
- a redacted diagnostic JSON bundle;
- a non-zero exit status on any failed or missing acceptance signal.

The script reports evidence but never changes a recipe from candidate to verified.

## Evidence still requiring controlled interruption

Run these after the basic lifecycle passes, one machine at a time:

1. During a model download, restart `basement`; confirm the same job resumes and completed bytes are retained.
2. With a model ready, reboot the Spark; confirm the UI shows recovery until health and inference reconciliation complete.
3. Fill a manager-owned test volume until preflight reports insufficient disk, then remove only that test data.
4. Occupy port 8000 with an unrelated listener and confirm preflight blocks. Repeat with another manager-owned model active and confirm the UI offers a transactional switch.
5. Block registry or Hugging Face access temporarily and confirm the job fails with a redacted actionable error, then resumes with the same idempotency identity after access returns.
6. Run with `--remove` and verify the receipt's reclaimed bytes match the recipe's owned artifacts while unrelated data remains.
7. Scan the JSONL receipt, diagnostic bundle, manager API, and `journalctl -u basement` output for credentials before promoting any recipe.
8. Consume unified memory with a controlled manager-owned test process until live preflight blocks the start. Confirm no model container start is attempted and the process can be removed without rebooting the Spark.
9. During a switch, force the target's live-memory check to fail after the previous model stops. Confirm rollback restores and re-verifies the previous model.
10. Begin a controlled download with sufficient space, then consume only the manager-owned test volume until the declared safety margin would be crossed. Confirm the job fails with a resumable partial download before the filesystem is exhausted.

For future multi-Spark qualification, capture the memory and disk receipt for every node. Deliberately constrain one node while leaving aggregate cluster capacity above the recipe total; preflight must still fail and identify the exact node. Do not promote multi-Spark support from aggregate-only evidence.

Record performance separately from acceptance: cold-start time, prompt length, output tokens, tokens per second, idle and peak unified memory, GPU-visible free memory, planned runtime allocation, host reserve, peak storage, minimum disk headroom, and whether compilation cache was warm. Tuning changes require a new recipe version and another complete lifecycle run.

## 2026-08-04: DeepSeek V4 Flash two-Spark qualification run

Eight install attempts on the two EdgeXpert machines, each clearing a real
layer. What now works, proven on hardware: the fabric preflight answers
before any staging (verify_fabric passed as step zero on every attempt),
persistent link-local addresses survive on both machines, NCCL rendezvous
over the cable, synchronized 74 GiB weight loads per rank in under three
minutes, drift rebuilds when a recipe changes its mounts or image, and the
full install lifecycle to ready including the built-in inference check.

What blocks acceptance: sustained generation dies with a driver-level
NV_ERR_NO_MEMORY whose onset tracks sequence length, not the memory budget.
It reproduced at 0.85 and 0.80 utilization and with breakable CUDA graphs
disabled, always minutes into a long answer while short answers complete.
This matches the reported sm_121 decode-kernel behavior where per-step
allocations grow with the KV history. Upstream tracking: vllm-project/vllm
issues 48054 (fixed by the v0.26.0 pin this recipe now carries) and 50773
(open). A crashed engine also leaves its container in a state Docker still
reports as running, so recovery needs a container restart, not a retry.

Verdict at the time: recipe stays candidate, revisit when upstream ships a
fix for the long-decode allocation growth.

Superseded 2026-08-04: a working daily-use deployment of this exact model on
another two-Spark pair was inspected read-only. It does not run stock vLLM
at all; it runs the Anemll GB10 fork (ghcr.io/anemll/dspark-vllm-gx10) with
an NVFP4 MLA KV cache, DSpark multi-token speculative decoding and the
flashinfer_b12x MoE backend, on identical driver, kernel and CUDA versions
to ours. That rules out an environment difference and confirms the failure
above is specific to the stock vLLM stack. Recipe version 2 pins the fork
image by digest and mirrors the observed serve configuration; it stays
candidate until it passes this same qualification on our hardware.

## 2026-08-04: DeepSeek V4 Flash two-Spark qualification, second run: PASS

Recipe version 2 (Anemll GB10 vLLM fork, nvfp4_ds_mla KV cache, dspark
speculative decoding, flashinfer_b12x MoE), commit 0b0a19a, on the same two
GB10 machines that failed the first run.

Install: fabric preflight 1 ms round trip, image digest-pinned and pulled on
both nodes, 167 GB weights downloaded and byte-verified per node (sequential
head then worker at about 48 MB/s, roughly four hours end to end), engine
started, health and inference probes passed. One operational note: the
manager's HTTP API became unresponsive for minutes at a time while hashing
the downloaded artifact.

The critical test, sustained generation, the exact scenario that killed the
stock stack at 400-1500 output tokens:

- essay, 4500/4500 tokens, finish_reason length, 36.4 tok/s
- technical, 4500/4500 tokens, finish_reason length, 44.9 tok/s
- story, 3876 tokens, finish_reason stop with a real ending, 34.7 tok/s

Zero new NVRM, Xid or NV_ERR lines in dmesg on either node across the whole
battery (watch pipeline liveness-verified). Host memory flat at 105/104 GB of
121 GB throughout. Three-way concurrency completed coherently at about 70
tok/s aggregate versus about 38 single-stream. Reasoning arrives in its own
field, named `reasoning` in the response JSON, with no think tags or DeepSeek
special tokens leaking; a tool call executed with well-formed arguments.

Pre-existing NV_ERR_NO_MEMORY lines exist in both nodes' dmesg from engine
start (head 10, worker 7, some coinciding with this deployment's own
container startup). Those are load-time allocator probes; the counts did not
move during any test.

Verdict: PASS. The evidence for dgx-spark-verified now exists, but the
built-in pack pins every recipe as candidate by policy (the validator test
enforces it, and this document's own rule is that evidence never promotes a
label by itself), so the recipe labels are unchanged until the curation
process promotes them. Trust would in any case stay basement-candidate on
the image-provenance grounds documented in the recipe.

Addendum, same day: the throughput numbers above were measured with
non-streaming requests. Direct probes against the same serving stack show
streaming responses decode at a steady 12 to 15.5 tok/s while identical
non-streaming requests reach 29 to 38 tok/s, a roughly 2x server-side gap in
the fork runtime that is not explained by include_usage handling. The console
benchmark measures a 30-second streaming window, so the model card's measured
number reflects what streaming clients actually get. Tracked for
investigation; the fork upstream may want a report.
