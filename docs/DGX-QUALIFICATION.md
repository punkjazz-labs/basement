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
