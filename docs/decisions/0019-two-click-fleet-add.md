# ADR 0019: Adding a Spark to the fleet is two clicks

Date: 2026-08-12. Status: accepted. Backend implemented; console follows.

## Context

ADR 0016 made fleet membership safe: persistent node identity, mutual TLS on
the fleet transport, and one explicit owner proof before any node joins. The
proof it shipped is a short-lived join code minted on the machine being
adopted. In practice that means the owner opens the new Spark's console,
presses a button, reads a long opaque string, and types or pastes it into a
field on the controller's console. ADR 0014's SSH adoption avoids the typing
but only for a machine that is not running basement yet: it asks for an
administrator password instead.

Every one of those is a person handling a credential. The join code is bearer
authority for ten minutes; a rendered code can be photographed, pasted into a
chat window, or typed into the wrong console. It is also the part of the
product that most obviously does not feel like a home appliance: two consoles,
one clipboard, and a string nobody can check by eye.

The two machines can already authenticate each other. Each holds an
address-independent Ed25519 identity, the fleet transport is mutual TLS, and
the join code already carries the fingerprint of the certificate it belongs
to. What was missing was a way for the owner to say yes on the machine being
added without becoming the transport for the secret.

## Decision

Adding a Spark is two clicks: Add on the controller console, Approve on the
member console. No address, fingerprint or code is ever shown to a person.

### The exchange

1. **Invite.** The controller console calls
   `POST /api/v1/fleet/invite {console_url, display_name?}` in an owner
   session. The manager derives the member's fleet listener from the console
   URL it already knows (console port plus one, the existing
   `adjacentFleetNodeURL` rule), makes first contact over the fleet transport,
   and pins whatever certificate that listener presents. Over that channel it
   sends `POST /internal/fleet/v1/invite {fleet_id, controller_name,
   controller_console_url}`. The member records a pending invitation against
   the requester's certificate fingerprint and answers with its own node id
   and the name it calls itself.
2. **Approve or deny.** The member's console lists what is waiting at
   `GET /api/v1/fleet/invitations` and answers it at
   `POST /api/v1/fleet/invitations/{id}/approve` or `.../deny`, both in an
   owner session with CSRF, exactly like every other membership change.
   Approving mints a join code through the existing `CreateJoinCode` path and
   attaches it to the invitation. Approval by itself changes no membership.
3. **Collect.** The controller polls
   `GET /internal/fleet/v1/invite/status` on the member, over the channel it
   pinned at invite time. The member answers `pending`, `denied` or `expired`
   to anyone; it answers `approved` with the join code only to the exact
   certificate fingerprint that sent the invitation, and it clears the code as
   it hands it over, so the code exists in one place at a time.
4. **Adopt.** The controller requires the fingerprint embedded in the code to
   equal the certificate it pinned at first contact. If they disagree it stops
   with "the approving Spark is not the one that was asked". Otherwise it runs
   the unchanged `Adopt`, which pins its own prepare call to the fingerprint
   inside the code. The console URL is passed through exactly as it was asked
   for, so a legacy peer row still merges on its stored base URL instead of
   leaving a duplicate behind.

The controller console follows one attempt through
`GET /api/v1/fleet/invite/status?console_url=...`:
`inviting` to `waiting` to `adopting` to `done`, `denied`, `expired` or
`failed` with a reason.

### What holds the state

Both halves live in memory on the fleet manager: the member's pending
invitations and the controller's in-flight attempts. They are one conversation
a person is watching, not a record. A manager restart during those ten minutes
costs one more click rather than leaving an approval that outlives the
conversation that produced it. Invitations expire ten minutes after they
arrive, matching the join code an approval can turn them into.

A standalone node holds at most three invitations, one per requesting
certificate, and a machine that asks twice replaces its own request. That
bounds what an owner has to read through, because this is the one endpoint a
Spark with no fleet answers for a certificate it has never seen.

### Discovery says which machines are already running

The existing sweep already fingerprints each candidate's console. It now also
asks that console `/healthz` with a one-second budget and reports
`{running, version}` alongside the base URL, so the console can offer Add for a
machine that is already running basement and Install for one that is not. No
new scanning infrastructure: it is a second short request to a machine that
has just proved it is one of ours.

## Security invariants

- **Approval is an owner session on the machine being added.** Nothing else
  mints a join code. A published API key reaches neither the list nor either
  answer, and the same CSRF and same-origin rules apply as to every other
  console mutation.
- **The join code never travels except member to controller**, on the mutual
  TLS channel pinned when the invitation was sent, to the one certificate that
  sent it. It is delivered once and never rendered anywhere.
- **The pin and the code must agree.** First contact is trust on first use, so
  the controller additionally requires the fingerprint inside the approved code
  to be the certificate it pinned, and `Adopt` separately pins its prepare call
  to the fingerprint inside the code. A machine in the middle that answers the
  invitation cannot produce a code that names it, and one that relays another
  Spark's code fails this comparison before any row changes.
- **Invitations expire in ten minutes** on both sides, and an expired one is
  neither listed nor approvable.
- **The headless join-code API is unchanged.** `POST /api/v1/fleet/join-code`
  and `POST /api/v1/fleet/join` keep working exactly as ADR 0016 defined them,
  for scripted and headless installs.

This changes one admission rule from ADR 0016. A standalone node used to
complete a mutual TLS handshake with an unknown certificate only during an
open join-code window; it now completes one whenever it belongs to no fleet,
because being asked to join is what opens the conversation and there is no
code yet. TLS admission was never authority: every other handler on the fleet
transport separately requires this node to be a member and the caller to be
its adopted controller, and `join/prepare` still requires a code an owner
approved. What an unknown certificate can do to a standalone node is
therefore: leave one of three invitations for its owner to answer, read the
status of its own invitation, and attempt a join it has no code for. The
store's `HasOpenFleetJoinCode` query has no remaining caller and is removed.

## Consequences

Good:

- Adding a Spark stops being a credential-handling exercise. Two clicks, no
  clipboard, and the owner's yes is given on the machine it is about.
- The approving machine proves who it is, which typing a code never did: the
  owner reading a string cannot tell whether it came from the right Spark.
- Legacy peers from ADR 0014 use the same flow with no special case, because
  the console URL is carried through verbatim and the existing merge rule
  still applies.
- A denial is reported as a denial, so the controller console can say what
  happened instead of timing out.

Costs and limits:

- A standalone Spark on a hostile network can be asked to join by anything
  that speaks the fleet protocol. The cost of that is up to three prompts an
  owner declines. It cannot become membership without an owner session on that
  machine.
- First contact is trust on first use. If an attacker holds the address at the
  moment the owner presses Add, the controller pins the attacker's
  certificate. The owner then approves on their real Spark, whose code names a
  different fingerprint, and the addition fails rather than silently adopting
  the wrong machine.
- Invitations do not survive a manager restart, so an addition interrupted by
  an upgrade needs one more click.
- The status poll advances the attempt, including running the adoption, behind
  a console read. That is deliberate: the owner authorized the addition when
  they sent the invitation, and the poll is what a console watching a two-click
  flow can do.
