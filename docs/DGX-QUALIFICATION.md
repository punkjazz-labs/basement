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
  /var/lib/runonspark-manager/qualification
```

The helper exercises authenticated preflight, installation, persisted job polling, real inference verification, stop, start, a second smoke test, and redacted diagnostic export. Add `--remove` only when the downloaded weights should be deleted and removal/reclaim behavior is the test objective.

Each run produces:

- a timestamped JSONL receipt containing system, recipe pins, preflight, and terminal job objects;
- a redacted diagnostic JSON bundle;
- a non-zero exit status on any failed or missing acceptance signal.

The script reports evidence but never changes a recipe from candidate to verified.

## Evidence still requiring controlled interruption

Run these after the basic lifecycle passes, one machine at a time:

1. During a model download, restart `runonspark-manager`; confirm the same job resumes and completed bytes are retained.
2. With a model ready, reboot the Spark; confirm the UI shows recovery until health and inference reconciliation complete.
3. Fill a manager-owned test volume until preflight reports insufficient disk, then remove only that test data.
4. Occupy port 8000 with an unrelated listener and confirm preflight blocks. Repeat with another manager-owned model active and confirm the UI offers a transactional switch.
5. Block registry or Hugging Face access temporarily and confirm the job fails with a redacted actionable error, then resumes with the same idempotency identity after access returns.
6. Run with `--remove` and verify the receipt's reclaimed bytes match the recipe's owned artifacts while unrelated data remains.
7. Scan the JSONL receipt, diagnostic bundle, manager API, and `journalctl -u runonspark-manager` output for credentials before promoting any recipe.

Record performance separately from acceptance: cold-start time, prompt length, output tokens, tokens per second, peak storage, and whether compilation cache was warm. Tuning changes require a new recipe version and another complete lifecycle run.
