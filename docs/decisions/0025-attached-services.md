# ADR 0025: Attached services, shown and guarded but never run

Date: 2026-10-25. Status: accepted.

## Context

An owner can run a model on their Sparks that basement did not install and
cannot drive. The first real case is a distilled video model served as one
model across a two-Spark pair: a Ray cluster with one rank per Spark, an
OpenAI-style video API on the head, and a memory guard, all run by systemd
units outside basement. It is not a recipe, not a container, and not a
`spark_count` basement can express (the parallelism is sequence-parallel
across the pair, not the tensor-parallel group of ADR 0016).

basement only knows models it installs through Docker. So the console shows
nothing for this model, and, worse, nothing stops basement from placing or
starting another model on those two Sparks while the external service holds
most of their memory. ADR 0003 and ADR 0004 guard one active model per node
against models basement manages; a process basement does not manage is
invisible to both. A start would pass the planner, then either fail the live
memory check or, in the window before the external service has loaded, land
beside it and take both down.

Turning the service into a recipe is not on offer: its lifecycle, its
distributed launcher and its memory guard belong to the operator, and
basement's recipe contract (pinned image digest, closed operation list,
`verify_*` steps, rollback) cannot describe it without either weakening that
contract or pretending to control what it does not.

## Decision

### An operator-declared, read-only concept

An attached service is a model service basement displays and health-checks
but never installs, starts, stops, updates or calls in any other way. The
operator declares each one in its own file:

```
<data-dir>/attached-services.d/<name>.yaml
```

The directory is the operator's. basement only reads it. A missing directory
means the feature is inert, which is every existing installation. Files are
read once at startup, before any job can be admitted, and again within a
second when the directory's entries change (add, remove, rename) and at least
every 60 seconds otherwise, which covers an edit in place.

The schema is deliberately small and strict (`internal/attached`):
`schema_version` (1), `id`, `display_name`, `kind`, `managed_by` and
`health_url` are required; `model_by`, `served_model_name`, `endpoint`,
`health_interval_seconds` (5 to 3600, default 30), `licence`, `licence_url`
(HTTPS only), `licence_territory_exclusions` and `notes` are optional; and
`topology` gives `spark_count` (1 to 4), free-text `parallelism`, and exactly
`spark_count` nodes, each named by `node_name` or `node_id`. Unknown keys are
an error, as they are for recipes: a misspelt key that was silently ignored
could leave a Spark unguarded while the file looked right. Every field is the
operator's own words and is shown as written. basement adds no attribution,
licence or topology fact; an absent optional field reads `n/a`.

A file that cannot be used (malformed YAML, a failed rule, a duplicate `id`,
an unreadable directory) is logged once per distinct problem and listed as an
error entry. It never stops the manager, and it guards nothing: a declaration
that cannot be read cannot say which Sparks it means. The console says so in
those words rather than implying protection it does not have.

`endpoint` is display text and is never dialled; it usually names a loopback
address on another machine or one reached through a tunnel. `health_url` is
the only address basement contacts.

### Health

A prober per manager GETs each `health_url` on its own interval with a
5-second bound, from the node that holds the declaration. HTTP 200 is
healthy; every other answer, a redirect included (redirects are not
followed), a timeout or a refused connection is unhealthy. There is no proxy
and no kept-alive connection. Status (`healthy`, `unhealthy`, `unknown`),
`last_checked_at`, `latency_ms` and `last_error` live in memory only: a
restarted manager says `unknown` until it has checked again, which is the
truth. A changed `health_url` starts again from `unknown`. Generation
endpoints are never called, so a health check can never spend the service's
GPU time.

### API and console

`GET /api/v1/attached-services`, behind the console session like the other
`/api/v1` reads, returns each declaration with its live health and the list of
refused files. It answers from memory and the local store, never by asking
the service. Each named node carries `protected`: whether this manager itself
keeps its own models off that Spark (see below). A name it cannot match is
reported unprotected, so a misspelt node name shows up on the card instead of
failing silently.

The Models view shows one card per service among the serving cards, built
from their pieces: the same frame, mark and box chips, a status chip (green
while the service answers, red when it does not, plain before the first
check), and the thin green edge only while it answers. The card names the
service, says "Attached service · managed outside Basement", and gives the
topology, each named Spark with its role, and a live line with how long ago
it was checked and how fast it answered, or why the check failed. Opened, as
a serving card opens, it lists the endpoint, managed-by line, attribution,
kind, served name and licence, and the licence's territory exclusions in the
wording the install dialog already uses. It opens by itself when a named
Spark cannot be matched. It has no button.

### Safety: a declared node is occupied

basement already has one admission authority per node, `fleet.Allocator`,
which every runtime claimant goes through: local install and start jobs,
independent fleet placements, legacy distributed ranks, recovery and manager
updates. A `fleet.RuntimeHolder` hook is installed on it. When a valid
declaration names this node (by fleet node id, or by display name compared
without case or domain suffix), a **new** runtime claim of kind `local-job`,
`independent` or `legacy-rank` is refused, at prepare and again at
activation, with a sentence naming the service. Two kinds pass: a
`recovered-model` claim only records what already serves, and refusing it
would turn a manager restart into a crash loop; a `manager-update` claim
takes the maintenance latch, not the serving slot. Disk-only claims pass, as
they do beside a serving model. A claim already active is never revisited,
so nothing basement runs is stopped by a declaration appearing.

A declaration usually lives on one node (the health URL is probed from
there), so the same hook also guards the other places that node decides
for others:

- The fleet controller's planner marks a named Spark ineligible with the
  service's name, so the install dialog greys it out and a placement onto it
  is refused before any member is asked. A `start` deployment action on a
  named Spark is refused the same way; stop, remove and the rest stay open,
  so a model placed before the declaration can still be taken down.
- A two-Spark job whose worker is named is refused before it plans, both when
  the console creates it and when the engine runs it.
- A delegated install (ADR 0013) onto a named paired Spark is refused before
  the peer is asked.

Health does not release a node. A service that stopped answering still holds
the memory it was given until its owner says otherwise by removing the file.

## Why read-only

- **Ownership.** The service's lifecycle, launcher and memory guard belong to
  units basement does not own. A basement that could stop them would be a
  second authority over the same processes, exactly the split ADR 0016 refuses
  for managed nodes.
- **Trust.** Every guarantee basement makes about a model it runs (pinned
  digest, verified artifacts, receipts, rollback) depends on running it
  itself. Controlling an attached service would put basement's name on a
  lifecycle with none of those guarantees.
- **Blast radius.** The only request basement sends is a bounded GET of a URL
  the operator chose. It cannot change the service, spend its GPU, or follow a
  redirect somewhere else.
- **Safety by declaration, not detection.** basement does not guess what holds
  a node from process lists or memory readings. The operator states it, the
  file is the record, and removing it is the release.

## Deliberately refused or deferred

- Services declared on members are not merged into the controller's list.
  That needs the signed heartbeat to carry them, which is a fleet protocol
  change; a declaration on the controller that names every node is enough for
  the console and for placement.
- A member does not learn of a declaration held by the controller. Its own
  allocator guards it only if the declaration is on that member too, but a
  member does not plan placements, its console refuses model changes while it
  is managed (ADR 0016), and the controller refuses on its behalf. A
  declaration on a member is probed from that member, so its `health_url`
  has to be reachable from there.
- Download-only installs are still admitted on a declared node from its own
  console, as beside any serving model. Fleet placement refuses the node
  outright, because the plan does not yet carry the activation choice.
- A malformed file guards nothing and is reported loudly, rather than
  locking the node. Failing closed would let one typo block every model on a
  machine with no console control to clear it.
- No inference proxying through `/v1`, no roles, no Playground or Generate for
  attached services. They keep their own endpoints.
- No automatic discovery of external services, and no release on health.

## Consequences

- An owner who runs a model outside basement writes one file and gets a card,
  a health check, and a basement that will not put another model on those
  Sparks.
- The guard is only as accurate as the node names in the file. The card says
  when a name matches no Spark this console places models on.
- Releasing the Sparks is removing the file; basement never stops the service
  to do it, so the owner stops it first.
- The allocator, planner, engine and delegated-install path each gained one
  check against the same hook; nothing in the recipe contract, the job
  lifecycle or ADR 0003's switching changed.
