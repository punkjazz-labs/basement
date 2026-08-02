# Deferred workstreams (do not build without a dedicated spec)

Recorded so scope stays visible and nobody improvises them into an unrelated branch.

## Advanced configuration (context size first)

User-adjustable `max_model_len` per installed model: lowering it frees gigabytes of
pre-reserved KV cache, raising it enables long-context work when it fits. Pattern:
an Advanced section in the expanded card, every field showing the recipe default with a
reset, overrides visibly marked as the user's, restart required and said plainly.
Blocked on spec 05's math (the slider and the calculator are the same widget) and on a
mockup. Later candidates: gpu_memory_utilization, max_num_seqs, reasoning toggle.

## Gated Hugging Face models

Llama- and Gemma-class weights require licence acceptance plus a token. Not needed for
the current catalog (Qwen, poolside: ungated). When the first gated recipe lands:
`gated: true` + licence URL in the recipe; `verify_artifact_access` already catches the
401/403; UI walks the user through accepting upstream and pasting a token stored
server-side only. Two-day job when actually needed; zero value before.

## Unmanaged workload detection

Detect foreign vLLM/Ollama containers, occupied ports, and GPU memory held by processes
we did not start; show them honestly ("Running outside basement"), count their memory
in preflights, never touch them. The preflight verifications already fail politely; the
missing piece is naming the occupant. Needs design for the Docker/process introspection
boundary before speccing.

## Discovery and popularity pipeline

Lives OUTSIDE this binary. A scheduled job scans X (watchlist first: MiaAI Lab, 0xsero,
lab accounts, vLLM releases; keyword sweep second), triages candidate models, researches
facts per the no-invented-facts rule, and drafts recipe PRs for human review and
hardware qualification. Popularity scoring (mention volume + sentiment) publishes into
the spec 04 index as metadata. Hard constraint agreed with the owner: paid placement or
sponsorship must never influence curation or ranking. Blocked on X API credentials.

## Hosted relay for remote access

Kills the Tailscale-installation objection. Hard constraint decided up front: pure
zero-knowledge TCP relay (DERP-style), end-to-end encrypted, self-hostable, or it does
not get built; anything that can read user traffic contradicts the product's core
promise. Needs its own design doc; do not prototype casually.

## Dual-Spark distributed serving

The flagship story. The recipe schema already carries `topology.spark_count: 2`; the
engine and operations do not yet execute multi-node topologies. Requires fleet phase 1
(peers exist), a node-interconnect qualification runbook (ConnectX/RDMA), and a
qualified 2-Spark recipe. Spec after fleet phase 1 ships and the hardware runbook has
been executed on the owner's DGX pair.

## Native GUI installers (macOS .app / Windows GUI .exe)

Promoted: this is now spec 08 (browser-served wizard inside a double-clickable
.app / windowsgui .exe, no GUI framework). This entry stays only as history of
the earlier Wails deliberation; the framework route was dropped in favor of the
loopback browser wizard, which needs no new dependency and cross-compiles from
one machine.

## Naming and licence

The naming decision: basement (basement.punkjazz.ai) is the product name outright, not
a public-facing alias over a separate working name — see spec 10 for the rename itself.
A LICENSE file must land before the repo ever goes public. That is not executor work.
