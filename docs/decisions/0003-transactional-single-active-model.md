# ADR 0003: Transactional single-active-model switching

Status: accepted for local implementation; awaiting DGX Spark validation

## Decision

The initial manager keeps one active model endpoint on host port 8000. Starting an installed model while another model is active is a switch, not an independent start.

The job engine:

1. records the previous active model as the rollback target;
2. permits downloads and configuration to finish while the previous model remains active;
3. treats port 8000 occupied by that manager-owned model as switchable, while unrelated port occupancy remains a preflight failure;
4. records switch intent transactionally before stopping the previous container;
5. starts the target and requires both HTTP health and non-empty inference verification;
6. marks the target as the sole active model only after verification;
7. on failure or cancellation, stops the target and starts, health-checks, and inference-checks the previous model;
8. records rollback steps and reports whether restoration succeeded.

If rollback also fails, neither model is represented as active or ready. The job reports both the target failure and rollback failure.

## Why

All three initial recipes expose port 8000 and target the full Spark GPU. Attempting to run them concurrently would create a port conflict and could overcommit unified memory. A stable endpoint plus explicit transactional switching is simpler and safer for the first release.

## Consequences

- The browser must say `Switch` and explain that the current model will stop.
- Installation may download a second model without downtime, but the final start phase is exclusive.
- Completed rollback operations are persisted as typed job steps.
- Real DGX validation must prove both successful switching and forced target-failure restoration before the three-model release is considered complete.
- A future reverse proxy or multi-model scheduler requires a separate decision and migration path.
