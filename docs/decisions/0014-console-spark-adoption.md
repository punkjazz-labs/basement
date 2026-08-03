# ADR 0014: Adopting a second Spark from the console

Date: 2026-08-03. Status: accepted. Backend implemented 2026-08-03.

## Context

People do not buy two GB10 machines at once. They buy one, live with it,
and add the second later, when a model they want needs two Sparks or when
the first one is always busy. By then the installer is a thing that
happened months ago on a laptop that may not exist any more.

Everything needed to set the second machine up already exists in the
manager binary that is running on the first one. `internal/discovery`
sweeps the local network for SSH-reachable machines and ranks GB10-class
hostnames. `internal/setup` connects over SSH, confirms the GB10 superchip
with `nvidia-smi` rather than trusting a hostname, stages a binary, writes
the systemd unit, starts the service and waits for the pairing token the
new manager mints. Until now all of that was reachable only from
`basement setup`, a terminal command run from a third machine.

So the owner who wants a second Spark is told to find a laptop, find the
installer, and run a wizard, in order to make two machines they own talk
to each other. Then they are told to open the new console, create an API
key, copy it, open the old console, and paste it into a form. That is a
terminal-shaped answer to a console-shaped need. The console is already
open, the owner is already authenticated on it, and the machine that
would do the work is the one they are looking at.

There is also an accident worth removing. The two machines have to run
the same version of basement, and the installer's release download does
not know what version the first machine is on.

## Decision

The head bootstraps its sibling. Two endpoints, both console-session
authenticated with CSRF, exactly like every other console mutation.

`POST /api/v1/fleet/discover` runs one time-bounded sweep and returns
what it found:

```json
{"candidates": [
  {"name": "spark-worker", "address": "192.168.99.137", "gb10_hint": true,
   "basement": null},
  {"name": "gx10-office", "address": "192.168.99.140", "gb10_hint": true,
   "basement": {"base_url": "http://192.168.99.140:7070"}}
]}
```

`gb10_hint` is the hostname ranking discovery already does, and it is a
hint in the same sense it has always been: OEM machines ship the same
superchip under vendor default names, and the only proof is the SSH
identity probe that runs later. `basement` is null unless something on
that machine's console port answered `GET /api/v1/system` with a 401 in
this manager's own error shape. That endpoint answers nothing else
without credentials, and none are offered, so the fingerprint needs no
authority on a machine that is not ours yet. A machine that already runs
basement does not need adopting, it needs pairing, and the console can
say so.

The sweep excludes this machine's own addresses and names. It changes
nothing and can be run as often as the console likes. It is a POST rather
than a GET because it makes this machine talk to every address on its
network, which is not something a link, a prefetch or a page load should
be able to trigger.

`POST /api/v1/fleet/adopt` takes `{address, username, password}` and runs
the whole flow. The SSH credentials the owner types are the proof of
ownership: being able to log in to a machine as an administrator is what
owning it means on a home network, and no weaker claim would do. The
handler answers immediately with the first status snapshot; the console
follows the run through `GET /api/v1/fleet/adopt/status`, which reports
the six steps, their state, and the progress lines the install engine
emits:

1. connect: SSH in with the typed credentials.
2. verify: `setup.Probe`, and refuse a machine that is not a GB10, naming
   the hardware it did find.
3. install: `setup.Install`, with **this manager's own binary** as the
   source. The two Sparks are then on the same version by construction,
   not by coincidence. A manager that is not itself a linux/arm64 build
   refuses here with a plain sentence rather than uploading a binary the
   target could never execute, which is what a developer running the
   server on a Mac gets.
4. start: wait for the new manager to answer on its own console.
5. pair: use the pairing token the install produced to pair with the new
   console and create one API key on it, named `fleet-<head hostname>`.
6. peer: prove the URL and that key reach a Spark together, then record
   the peer, through the same `addPeer` path the manual add-by-address
   form uses.

The sibling is installed to listen the way the head listens: a head on a
Tailscale address gets a sibling on Tailscale, and everything else gets
the local network. A head that only listens on loopback also gets a
sibling on the local network, because loopback is the one mode that could
never be reachable from the other machine, and a peer that cannot be
reached is not a fleet. That is also what the installer recommends for a
machine set up remotely.

### The pairing token is not consumed

This flow spends the new machine's pairing token before the owner ever
sees it, so the question is whether that locks them out of their own new
console. It does not. `internal/auth` reads the token from
`<data-dir>/pairing-token` at startup and compares every pair against it;
nothing deletes or rotates it, and a token that already opened one
session opens the next one too. It is a file-backed shared secret, not a
one-shot nonce. A test pins that, because the whole flow depends on it.

So the successful run ends with the standard pairing card, in the payload
the console already has open:

```json
{"peer": {"id": "peer_...", "name": "spark-worker",
          "base_url": "http://192.168.99.137:7070"},
 "console_url": "http://192.168.99.137:7070",
 "owner_pairing_url": "http://192.168.99.137:7070",
 "owner_pairing_token": "..."}
```

`owner_pairing_url` opens the new console in the owner's browser and
`owner_pairing_token` is what they type into it, which is exactly the
card `basement pairing-url` prints on the machine itself. No second SSH
round trip is needed to fetch a fresh one.

### The password

The password is the one secret the owner types into this console, and it
belongs to their user account on another machine, not to basement. It
lives in the request body, in the handler's arguments and in the
goroutine that runs the adoption. It is never written to the store, the
jobs table, a log line, a progress line, an error message or any status
payload. The manager has no request-body logging middleware, so there was
nothing to exclude, and this route adds none.

That is the intent; the enforcement is mechanical. The adoption's only
handle on its own progress scrubs every string on the way in, removing
the typed password and then applying `internal/redact` on top, so an SSH
library that echoes the credentials it was handed back into an error
string cannot get them into the status endpoint. The success result goes
through the same scrubber as the failure path: a machine that answers
`hostname` with the password it was just handed would otherwise get it
back out through the peer name. A test drives a failed authentication
whose error contains the password verbatim, and a second test drives a
machine that returns the password as its hostname; neither may appear in
any byte of any progress, status, result or stored row.

Scrubbing is the last thing that happens to remote text, and that is a
rule about ordering rather than a detail of it. The same text is also
transformed: control characters and invalid UTF-8 are stripped out of it
and its length is capped to what the store accepts. A scrub that runs
before either of those is not a scrub. A machine that answers `hostname`
with the password split by an escape byte defeats the match, and the
strip that runs afterwards puts the password back together and hands it
to the store. A cap that runs after the scrub is safe; a cap that runs
before it cuts the password into a fragment the scrub no longer
recognises and stores that fragment instead. So remote text goes through
one door, `adoptionRun.safeRemoteText`, which strips, scrubs, caps and
scrubs again: the middle scrub is what makes the cap safe, and the last
one is what everything persisted or displayed is held to. The reported
console URLs are scrubbed after they are parsed for the same reason,
because parsing unescapes what it is given. Tests drive a hostname
carrying the password split by a control character, split by an invalid
byte, and padded so it straddles the length cap; none of them may leave
the password, or any eight-character run of it, in the peers table, the
peers endpoint, the status payload or the result.

Server-supplied text is never printed on this path either. A
keyboard-interactive SSH server chooses the instruction it sends with a
challenge, so it can ask for the password and then send a second
challenge whose instruction repeats it. On the console's path the
"output" is the systemd journal, which the scrubber never sees, so
`internal/setup` discards that text by default and hands it only to a
prompter that opted in through `ServerNotice`. The terminal wizard opts
in, because a person is reading it and typed the password themselves;
the console's prompter does not implement it at all. Everything that
does reach a person has its control characters stripped and its length
capped first.

### One address, checked once and used everywhere

The address the owner types is resolved once, by the handler, before any
credential is spent. What the run then uses is the IP that check accepted,
never the name again. Checking a name and letting each later step look it
up again is not checking it: a name with a short TTL and an attacker at
the other end of it can answer with a private address for the check and
with loopback or a public address for the SSH login that carries the
owner's password, the binary upload, the pairing, the key minting and the
reachability handshake. Any one of those would be a different machine
than the one that was approved, and the first of them hands over the
owner's credentials.

So `checkAdoptionTarget` returns the address it accepted, the SSH dial
connects to that literal, and every HTTP call in the run is built from
it. There is no name left in any URL on this path, so there is no dialer
override and no hostname-versus-certificate question to answer; the
console speaks plain HTTP here, as the peer URL rules already allow on a
LAN. The rules on what may be accepted have not changed: every record in
the answer must be off loopback, off this machine's own interfaces and
inside the private, link-local or CGNAT ranges, so a mixed answer is
still refused outright. When several acceptable addresses come back, one
of them is pinned and it is the only one used.

The peer's stored `base_url` is that same pinned address, and this is the
trade that was made deliberately. The alternative, storing the hostname
the owner typed, reads better in the Fleet table and survives the machine
moving, but every later call to that peer carries the fleet API key, and
those calls would resolve the name again on a machine nobody is watching.
That is the same rebinding window reopened, permanently, against a stored
credential rather than a typed one. The residual risk of the choice made
is the other one: the Fleet table shows an IP address rather than a name,
and if DHCP moves the second Spark the peer stops answering. The recovery
is the one the console already has, removing the peer under Fleet and
adding it again, and the recommendation for anyone who wants this to be
stable is a DHCP reservation or a Tailscale address. A wrong address is a
row the owner can see and fix; a leaked fleet key is not.

### Console session only

Neither endpoint accepts a bearer API key, and neither is on
`peerAllowedPaths`. A fleet key is what another manager holds; adoption
spends the owner's authority over their own hardware, which is a
different thing entirely. The two bootstrap calls this manager makes on
the machine it is adopting (`/api/v1/auth/pair` and `/api/v1/keys`) are
deliberately not added to that allowlist either: the allowlist governs
calls made with a stored peer credential, and nothing on it may mutate
the other machine. These two are a one-time bootstrap against a machine
this manager just installed, with a token that machine just minted, and
both paths are fixed strings rather than anything a caller can steer.

## Consequences

- Everything after the install talks to the pinned address: the address
  the owner adopted, resolved once and used as a literal from then on.
  The machine being adopted answers `hostname -I` and `tailscale ip`, and
  it is not ours yet: an SSH endpoint that names an accomplice would
  otherwise get the console wait, the pairing, the fleet key and the
  stored peer row pointed at that other host, and the owner would end up
  with a peer they never chose. The bootstrap URL is built from the
  address that was signed in to. Addresses the target reported are kept
  as `alt_url` for display, and only when they parse as a bare origin.
  `setup.Options.ConsoleHost` carries the same rule into
  `internal/setup`, so the result that path returns is anchored too. The
  terminal wizard leaves it empty and keeps its old behaviour, because
  there a person chose the address and reads the result before acting on
  it.
- Adoption only reaches machines on the owner's own network. The address
  is resolved rather than compared as a string: any resolved loopback
  address or address this machine holds on an interface is refused as
  itself, and anything outside the private, link-local and CGNAT ranges
  is refused as somebody else's. That is the product (Sparks on your own
  network), and it keeps a console session from being usable as an SSH
  prober aimed at the internet. A Spark reachable only through a hostname
  in the owner's own DNS still works, because what is checked is what the
  name resolves to, and that answer is then what the run and the stored
  peer row use.
- Everything the other machine says about itself is untrusted text. Its
  hostname becomes the peer name, which is stored and rendered, so it is
  stripped of control characters, capped to what the store accepts and
  scrubbed of the typed password last, in that order. The same holds for names that come
  off an mDNS sweep. An mDNS answer only contributes the address it
  actually came from, and only when that address is on a local range: an
  advertised A record naming anything else is dropped, because a machine
  on the segment can advertise any address at all and every candidate is
  something this manager then connects to. A sweep is capped at 64
  candidates and its fingerprints run through a pool of eight, so a flood
  of announcements costs the same as a quiet network.
- One peer, per the ADR 0005 deferral. Adoption refuses with a 409 when a
  peer is already configured, the same rule `cmd/basement/main.go`
  enforces when it picks a worker. Two peers would not be a fleet, they
  would be a row that breaks every two-Spark model. The rule is enforced
  in the store rather than in the handlers: `CreatePeer` inserts
  conditionally in one statement and returns `ErrPeerExists` otherwise,
  and a unique index says the same thing in the schema. Reading the table
  and then inserting leaves a window, and there are two doors into it,
  since a manual add can arrive while an adoption is running.
- A run that fails after the fleet key exists hands it back. The key is
  minted on the other machine in step 5, and any failure after that
  revokes it with the bootstrap session this manager still holds. When
  the revocation cannot be made, the failure sentence names the key so
  the owner can delete it under Connect on that machine, rather than
  leaving a credential nobody knows about on hardware they own.
- The point of no return is the moment the mint request is sent, not the
  moment a key comes back. The other machine writes the key and then
  answers, so a dropped connection, a timeout or an answer this manager
  cannot parse says nothing about whether the key exists; it only says
  that its id will never be learned. Treating those as ordinary failures
  left a working credential on the owner's other Spark with nothing
  naming it. So every failure from that request onward runs the same
  cleanup, and the cleanup does not need the id: the key's name is
  `fleet-<head hostname>`, decided before the request was sent and the
  same on both ends, so the keys on the new machine are listed with the
  bootstrap session and the one carrying that name is deleted. A key
  already named that is a leftover from an earlier attempt by this same
  head, and removing it is the point. If the listing, the match or the
  delete fails, the sentence the owner reads names the key and says to
  delete it under Connect on that Spark's own console.
- One adoption at a time. A second POST while one runs gets a 409 with a
  plain sentence. Two runs would race for the single peer row, and each
  one holds an SSH session installing a systemd service.
- Adoption progress is in memory. It survives page reloads, which is what
  the console needs, and it does not survive a manager restart, which
  nothing needs: the run cannot be resumed after a restart anyway,
  because the password that made it possible is gone by then, on purpose.
  The jobs table stays the engine's, keyed by recipe id and replayed by
  `ResumeInterrupted`; teaching it a job kind it can never finish would
  be a worse trade than losing narration nobody can act on.
- A failed adoption leaves no peer row. The store is written in the last
  step and nowhere else, so every earlier failure leaves this machine
  exactly as it was. The other machine is a different matter: a failure
  after step 3 leaves basement installed there, running, with its own
  console and its own pairing token. That is a working Spark, not
  wreckage, and the failure message says which step it reached.
- The manual add-by-address path stays. A Spark on another subnet, behind
  a router the sweep cannot cross, or reachable only over Tailscale, is
  not going to turn up in a local /24 sweep, and adoption is not the only
  way in.
- Typing SSH credentials into a console served over plain LAN HTTP puts
  them on the wire. The recommendation is to run the head on loopback
  (over an SSH tunnel) or on Tailscale when doing this. Transport
  security for the console itself is a separate piece of work; this ADR
  does not pretend to have done it.
- The head installs its own bytes, so the sibling can never be on a newer
  or older release than the head. The corollary is that upgrading the
  fleet is still two upgrades, and nothing here makes the head able to
  update its sibling.
