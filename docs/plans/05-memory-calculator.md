# Spec 05: memory-fit calculator (data layer only)

Branch `spec/05-memory-calculator`. The visual half of this feature is mockup-gated:
Claude produces a static mockup and the owner approves it before ANY UI is built. This
spec covers everything that can be built before that: the schema, the math, the API,
and tests. Do not build UI beyond what is listed here.

## Purpose

Let a user select downloaded models and see whether they fit together in the machine's
unified memory at chosen context lengths, with honest accounting for weights, KV cache,
and runtime overhead. This is the planning front door for future multi-model serving
and a centerpiece for the public site later; the math must therefore live in one
reusable, tested place.

## Schema

`internal/recipe/types.go`: new optional block on the recipe:

```yaml
memory_model:
  weights_bytes: 21941623844        # equals primary artifact expected_bytes unless overridden
  kv_bytes_per_token: 98304         # bytes of KV cache per context token, all layers
  runtime_overhead_bytes: 8589934592 # engine + CUDA graphs + activations, measured at qualification
```

Comment on the struct: values are measured or derived by maintainers during recipe
qualification; the console shows estimates only when the block is present. **The
executor does not fill these values for the existing recipes.** Maintainers add them
(derivation: kv_bytes_per_token from the model's public config
= layers × kv_heads × head_dim × 2 (K and V) × dtype bytes, honoring the recipe's
`kv_cache_dtype`; overhead measured on hardware).

## Math

New file `webui/ui/src/memory.ts` (frontend owns the interactive math; keep it pure):

- `fitBytes(recipe, contextTokens, seats)` returns
  `{weights, kv, overhead, total}` where `kv = kv_bytes_per_token × contextTokens ×
  seats`. `seats` is concurrent sequences, default 1.
- `fleetFit(recipes[], perRecipeSettings[], budgetBytes)` returns per-recipe totals,
  the grand total, and `headroom = budget − total` (may be negative).
- Budget comes from the machine: `system.memory_total_bytes` minus a fixed safety
  reserve; reuse the reserve constants the recipes already declare
  (`per_node_memory_reserve_bytes`, `safety_margin_bytes`) by taking the max across
  selected recipes. Document the choice in a comment as a constraint.
- Every returned object carries `estimate: true`. There is no code path that presents
  these numbers as measured.

Unit-test the math exhaustively in `memory.test.ts` (the project has no frontend test
runner configured; add `vitest` as a devDependency, minimal config, `npm test` script.
This is the one permitted new dependency).

## API

None needed: recipes already flow to the console in full, and `memory_total` arrives
via system/telemetry. Verify both and note field names in the report.

## UI (stub only)

Nothing user-visible in this spec. Export the functions and stop. The approved mockup
will define where it lives (likely a Storage-adjacent panel) and its visual form (a
horizontal budget bar with stacked per-model segments, free headroom, and a per-model
context slider). Implementing that arrives as spec 05b after mockup approval.

## Acceptance

- `memory.ts` + `memory.test.ts` with passing tests: zero models, one model without a
  `memory_model` block (excluded from totals, reported as `unknown: true`), several
  models, negative headroom, seats > 1, context sweep monotonicity.
- Type check and full build green. No visual changes anywhere.
