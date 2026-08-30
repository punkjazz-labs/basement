# ADR 0015: Roles on the stable endpoint

Date: 2026-08-04. Status: accepted. Implemented 2026-08-04.

## Context

ADR 0007 made the address stable. A client is pointed at `/v1` once and
keeps working across every model switch, because the manager proxies to
whichever model is active rather than exposing the runtime's own port.

The model name was left out of that promise. A request carries
`model: "<served model id>"`, and that id belongs to the recipe: it names
a publisher, a parameter count and a quantization. So an owner who
installs a better model still has to open every app, every script and
every LiteLLM config and edit a string, which is the same relinking ADR
0007 removed from the URL. The name is also the part a person is most
likely to get wrong, since nothing in the app tells them what this Spark
is serving today.

A basement Spark also serves one model at a time (ADR 0003). A client
that names a model which is installed but not running is answered by the
runtime that is up, with the runtime's own error about an unknown model,
and the owner has to go to the console and press Start before the request
can be sent again. The manager knows how to start that model; the request
just has no way to ask for it.

## Decision

`/v1` accepts a second kind of model name: `role/<name>`. A role is a
stable name for a job, and the owner decides in the console which
installed model answers to it today. Everything else about the endpoint
is unchanged, including how a client authenticates and the fact that the
response is proxied untouched.

### Names and storage

A role is a row in a new `roles` table: `name` (primary key), `recipe_id`,
`created_at`, `updated_at`. A row exists only while a role has a model, so
an unassigned role is an absent row rather than a row pointing at nothing.
Reassigning keeps `created_at`, because the role is the thing that
persists and the model behind it is not.

Names are lowercase slugs of letters, digits and inner dashes, at most 32
characters, normalized on the way in. They have to survive a round trip
through an OpenAI model field as `role/<name>`, and a name that only
differs by case would be two roles that clients cannot tell apart.

`GET /api/v1/roles` lists assignments and nothing else. The four names the
console always shows (`standard`, `fast`, `reasoning`, `vision`) are
console copy, not seeded rows: seeding them would make the API claim four
assignments that do not exist, and the console would still need its own
labels and descriptions for them.

`POST /api/v1/roles` assigns, `DELETE /api/v1/roles/{name}` clears. Both
are console-session mutations with the usual CSRF gate. Neither consumes
an `Idempotency-Key`: an assignment is not a job, and sending the same one
twice leaves the same row, so a retried click is already safe. A bearer
API key reaches neither. What a key holder may do through `/v1` is
inference; deciding which model answers to a name is the owner's, at the
console.

Assigning a model that is not installed is refused at assignment time,
with a sentence naming the Models page. The alternative is learning about
it at the moment a client's request arrives, which is the worst possible
time.

### Resolution

The manager reads the request body far enough to find the top-level
`model` field, and only the top level: a `model` key inside a message or a
tool definition is content, not addressing. A value that does not start
with `role/` is left alone and the body is forwarded byte for byte, so
every existing client behaves exactly as it did before this ADR.

A role name resolves to the exact installed recipe version, never the
catalog's current entry for that id, for the same reason the active-model
lookup does not: the running container is the one that was installed.

Before proxying, the model field is rewritten to that recipe's
`served_model_id`, so the runtime is asked for a name it knows. Only the
already-read head of the body is rewritten and the declared length is
corrected, so a long request is not held in memory to change a few bytes
near its front. Nothing about the response is touched.

Reading stops at 32 MB, and no read goes past it. A request that is still
an open JSON object where the reading stopped is refused with a 413 and a
sentence saying so, rather than being forwarded to whatever is serving:
silently answering with a model the client did not ask for is the one
outcome worth failing loudly to avoid.

Only that shape is refused, and the test is a parse rather than a first
character. A multipart upload, a JSON array, an object that closed without
naming a model, a malformed body, and an opaque payload that merely starts
with a brace all have no model field that could have been missed, so they
are forwarded whatever their size, exactly as before roles existed.

An unassigned role, an assigned model that is no longer installed, and a
recipe that has gone missing are all answered with a 404 or 503 in the
OpenAI error shape, each naming the Roles page. A store read that fails is
a 500 and says it could not read, rather than reporting a missing row that
may well exist.

### The standard role

`role/standard` is the role an app gets without anyone setting anything
up. While it has no assignment it resolves to whatever model is serving,
which is exactly what a request naming a concrete model already gets, so
an app can point at it on day one and never meet a configuration error.
It never starts anything in that state: it follows, and following is not
switching. Assigned, it is a role like any other, including bringing its
model up.

This is why the Connect page's snippets name `role/standard` rather than
the active model's id. The copy that ships with basement should produce a
client that keeps working after the owner changes their mind about models,
and this is the one name that does that from the first request.

### Holding a request while the model starts

When the model a role names is installed but not the one serving, the
request is held while the manager starts it, and then answered. The start
is the same job the console's Start button creates, so there is one
activation path and one place where a switch can go wrong.

The bound is the recipe's `runtime.start_timeout_minutes`, with the same
20-minute default used by the engine when that field is omitted. A role
request must not give up while the runtime's own verified startup window is
still open. Failure and timeout are answered in the OpenAI error shape, naming
the model, the actual bound and the Activity page, where the job is.

The switch runs in the engine on its own context, so a client that hangs
up mid-switch leaves the model coming up rather than half started.

### One switch at a time, and what a hold protects

A Spark that serves one model at a time has two overlaps worth preventing.

Two switches must not overlap. Each would stop a model the other is
counting on, and the second would act on a stale answer to "what is
running here". The engine's runtime lock is what prevents that, and it
already did: one job holds it, and every container this manager starts or
stops is started or stopped under it.

A switch must not overlap a request that has been let through but has not
reached the runtime yet. That request was admitted because its model was
serving; stopping that model a moment later would answer it with a bad
gateway, a truncated stream, or another model's output. So a request holds
a gate in the HTTP layer from the moment it is admitted until the
runtime's response headers come back, and a switch waits for the holds on
other models to clear before it begins. Headers are as far as a hold can
usefully reach: holding for a whole streamed answer would let one long
stream block every switch, and once the headers are back the request is
committed to the model that produced them. What a hold cannot promise is
the rest of a stream. A model being stopped ends the streams it was
producing, and that is what one model at a time means.

Which switch it is does not matter, so the gate is not something the role
path takes for itself. A switch announces itself through a hook the engine
calls (`SetSwitchGuard`), at the moment a job begins changing what serves
and not before, and every door goes through it: a request naming a role,
the console's Start button, an install activating what it downloaded, a
stop, and an uninstall. A gate that only the role path took would leave
the console able to cut off the requests roles are holding, which is the
same defect one layer down.

The wait for holds to clear is bounded at 30 seconds and the switch then
proceeds regardless. The job on the other end already holds the runtime
lock and is committed; a request that was admitted and never reached its
runtime must not be able to hold up something the owner asked for. New
requests are held off while a switch is waiting, so a busy endpoint cannot
starve the switch away from the model it is busy with, and requests naming
a concrete model are answered in that window exactly as this endpoint
answered them before roles existed: nothing is serving to be let through
to.

Two requests for the same role cost one switch, not two: whoever arrives
second either finds the model serving, or joins the start job that is
already running rather than asking for a second one. Finding and creating
that job is one step, so requests arriving in the same instant cannot each
create one. The store is what says a start is already running, so this
holds across a client that hung up, an owner who pressed Start a moment
ago, and a manager restart. After the model comes up, admission is asked
for again rather than assumed, because a switch queued behind this one can
take the model away in between; the recipe's startup budget bounds the whole
thing.

A job also re-reads its own premise. Everything a plan does follows from
one reading of the live active model, taken when the plan was made, and
that reading can be an hour old on an install or one switch out of date on
a start that queued. So at the moment a job takes the runtime lock, and
before it mutates anything, it re-reads which model is serving and fails
cleanly when a third model has taken the slot: stopping the model the plan
names would stop nothing and leave the real one running. Re-reading there
is sufficient precisely because containers only ever start and stop under
that lock, so the answer cannot change again while this job holds it.
`BeginSwitch` applies the same rule transactionally at the step that
records the switch. A previous model that has simply stopped is not this
case, and neither are two installs that both began with nothing serving;
both proceed.

The preflight a job runs before that point holds nothing. A two-Spark
recipe checks its cable and asks the other Spark about its own hardware,
which can take minutes when that machine is unreachable, and none of that
may block stopping an unrelated model while the current one is serving
perfectly well.

## Consequences

- Two roles pointing at two different models means the models swap in and
  out on demand, and the first request after each swap waits for a load.
  That is a property of one model at a time, not of memory pressure, so
  the console says it whenever a choice would leave roles on more than one
  model rather than only when an estimate says they do not fit together.
- Requests for two different roles are both answered, one after the other,
  each paying for its own switch. Alternating traffic between two roles
  therefore alternates model loads. Nothing here batches, queues by role,
  or holds a switch back to group requests; the honest description is that
  a Spark serves one model and roles make the switching automatic.
- A role assignment does not survive being pointed at a model that is then
  uninstalled, in the sense that the role stops answering: the row stays,
  the console shows "Not installed", and `/v1` says so and names the Roles
  page. Clearing the row on uninstall was rejected because reinstalling
  the model restores the role, and silently forgetting an owner's decision
  is worse than an honest error.
- `GET /v1/models` still lists what the runtime reports, so roles do not
  appear there. Adding them would mean rewriting a proxied response, and
  this ADR rewrites requests only.
- Roles are per Spark. A fleet peer has its own roles, its own console and
  its own `/v1`; nothing here federates them, and no role resolution
  crosses machines.
- The manager now reads part of every `/v1` request body before forwarding
  it. For the common case that is the first 64 KB and one parse, because
  clients put `model` at the front; a body that buries it costs a handful
  of parses of what has been read so far.
- A concrete model id remains a first-class way to use this endpoint. Apps
  that want a specific model, and the console's own playground, keep
  naming one, and nothing about those requests changed.
