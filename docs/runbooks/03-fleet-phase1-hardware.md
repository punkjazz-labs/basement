# Runbook: fleet phase 1 hardware verification

For the owner, on real machines. Not run by the executor that implemented
`docs/plans/03-fleet-phase1.md`; extracted here verbatim from that spec's
"Hardware runbook" section so it has its own place once the console work
merges.

1. Upgrade the primary MSI (edgexpert-alpha) with current main, then install the manager
   on the second MSI: build arm64 binary, `setup --binary` against edgexpert-beta.
2. On 37c4's console: Connect tab, generate an API key named `fleet-2051`.
3. On 2051's console: Fleet tab, Add a Spark, `http://edgexpert-beta.local:<port>`, the
   key. Confirm identity, model state, and unreachability (power 37c4 off) all render.
4. Repeat from a DGX Spark (spark-head) to confirm cross-vendor display.
