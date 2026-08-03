# ADR 0013: Delegated placement

Date: 2026-08-03. Status: accepted. Backend implemented 2026-08-03.

## Context

Every GB10 machine runs a whole manager. Each one has its own engine, its
own SQLite store, its own embedded recipe catalog, its own licence
acceptances and its own preflight guards. Nothing about that is
accidental: per-node resource evaluation never aggregates (ADR 0004), and
a node decides for itself what it can run.

A head machine can hold exactly one configured peer today (ADR 0005
defers real membership, leases and scheduling). Until now that peer
relationship was read-only. The head fetched three endpoints from the
peer, merged them into a fleet summary, and could do nothing else.

There is already a second machine-to-machine path, the internal node API,
and it deliberately refuses this job. It exists so a head can drive the
worker rank of a two-Spark deployment, and it allows only the mechanical
steps of putting that rank in place. Its policy is explicit in
`internal/httpapi/node.go`:

```go
if !local.Distributed() {
    return recipe.Recipe{}, errors.New("a single-Spark recipe is never run on behalf of another Spark")
}
```

That rule is correct and stays. A single-Spark model on the peer is not a
rank in someone else's deployment. It is a model that machine is serving,
with its own job, its own lifecycle and its own console entry. Driving it
step by step from the head would put the peer's runtime state in a
database on a different machine.

So the question this ADR answers is narrow: when the owner is looking at
the head's console and wants a single-Spark model installed on the peer
instead, how does that happen without either machine losing authority
over itself?

## Decision

The head delegates to the peer's own public API, over the API key the
owner already configured when adding that peer. The head is a remote
control. It carries the request across the network and relays the answer.
Every decision is made by the peer.

The peer resolves the recipe from its own embedded catalog by id, runs
its own preflight, applies its own licence gate, and creates its own job
in its own store. What the head sends is an id and an intent, never a
recipe body, a step list or a placement.

Two head-side endpoints, both console-session authenticated with CSRF,
exactly like every other console mutation:

- `GET /api/v1/peers/{id}/preflight?recipe_id=<id>` forwards to the
  peer's `GET /api/v1/preflight?recipe_id=...` and returns the peer's
  JSON and status code.
- `POST /api/v1/peers/{id}/models/{recipe_id}/install` forwards
  `{confirmed, accept_licence, activate}` and the caller's
  `Idempotency-Key` to the peer's
  `POST /api/v1/models/{recipe_id}/install`, and returns the peer's
  response and status code.

The head refuses to delegate a recipe whose `topology.spark_count` is not
1, with a 400. A distributed recipe already has a path in which the head
drives the peer's worker rank; handing the peer the whole recipe as well
would run it twice, once per machine. To answer that question the head
must know the recipe, so it reads its own catalog for the topology and
refuses ids it does not have. Everything else about the recipe, including
which version gets installed, remains the peer's to decide.

That head-side refusal is a courtesy, not the guarantee. It arrives early
and in the console the owner is looking at, which is worth having, but it
is an opinion formed from the head's catalog about a recipe another
machine will resolve for itself. The peer applies the same rule to the
recipe it actually resolved, and refuses a delegated install of a
distributed recipe with a 400 of its own. Catalogs at different versions
therefore cannot smuggle a two-Spark deployment onto one machine: the
machine that would run it is the machine that decides.

For the same reason the head reads only its effective catalog here, and
not the version history it keeps for already-installed models. Delegation
always starts a fresh install, which the peer resolves against its own
effective entry, and an older version of an id can carry a different
topology than the current one. Answering from history would be a guess
about a machine we do not own, so an id the head cannot currently install
itself gets the honest 400 it already had for unknown ids.

The only fact the head speaks for is the network between the two
machines. An unreachable peer, a truncated reply, or a reply that is not
JSON becomes a 502 with a plain sentence. A peer that answers is relayed
as it answered: if it says the disk is full, the console shows the disk
is full, with the peer's own status code.

### Bearer scope on the receiving side

For this to work the peer must accept its own API key on two more
endpoints. The scope is deliberately narrow:

- `GET /api/v1/preflight` now accepts a bearer key as well as a console
  session. It is read-only. It runs the recipe's `verify_` steps, which
  inspect disk, memory and ports without touching runtime state.
- On `/api/v1/models/{id}/...`, a bearer key may trigger install and
  nothing else. Start, stop, smoke-test, benchmark and remove stay
  console-session-only. A key that leaks never becomes control over what
  a machine is serving right now, and never becomes a way to delete
  anything.

Install is not quite as inert as it sounds, so the install body is scoped
as well. An install can activate the model it just placed, which is a
change to what the machine is serving, reached by a different door than
start. A delegated install therefore defaults `activate` to false when the
field is absent, where a console install still defaults it to true. An
explicit `activate: true` is honoured, because that is a placement the
owner asked for on the other console, and the head always states it.

The honest summary of the bearer scope is this. A key holder can change
what the peer serves, but only by explicitly asking to activate an
install of a recipe that is already in the peer's own catalog, that
passes the peer's own preflight, and whose licence the peer accepts. It
is one narrow, auditable action that leaves a job in the peer's own store,
not open control. Start and stop stay console-only because they are the
general form of that power: they can switch to anything already on the
machine, repeatedly, with no install to show for it, and nothing about
delegated placement needs them.

This is enforced in the auth wrapper, before the handler is entered, and
checked a second time inside the handler for a request that arrived
marked as delegated. Negative tests pin both, and pin that `/api/v1/keys`
and `/api/v1/peers` remain console-only.

A delegated install carries no CSRF token and needs none. CSRF exists to
stop a browser's ambient cookie authority from being spent by another
site. An `Authorization` header is never attached by a browser on its
own, so there is nothing to forge. Cookie-authenticated installs still
require CSRF, unchanged.

## Consequences

- Chat and playground for a model hosted on the peer happen on the peer's
  own console. The head can install it and can see it in the fleet
  summary. It does not proxy inference to it. The peer's `/v1` endpoint
  belongs to the peer.
- There is no cross-machine model registry. The head knows what the peer
  reports when asked, at the moment it asks. It does not keep a copy, and
  a model installed on the peer does not appear in the head's own store.
- One peer, per the ADR 0005 deferral. This is a two-machine remote
  control, not a scheduler. There is no placement recommendation, no
  lease across machines, and no automatic choice of where a model should
  go. The owner picks the machine.
- The peer allowlist grew from three GET paths to four, plus one POST
  matched by pattern because it carries a recipe id. It is still an
  enforced allowlist: every outbound call to a peer passes the same gate,
  and nothing outside it can be requested.
- Two managers can now disagree about the same recipe id if their
  catalogs are at different versions. That is correct rather than a bug:
  the peer installs what its own catalog says, and reports back what it
  did. The one thing that disagreement cannot do is widen what delegation
  is allowed to place, because the single-Spark rule is enforced on the
  peer against the recipe the peer resolved.
- A model placed on the peer without an explicit `activate` lands
  installed and not serving. The owner starts it from the peer's own
  console, which is where start has always lived.
