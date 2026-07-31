# ADR 0005: Automatic discovery and explicit placement

Status: accepted direction, multi-node execution not implemented

## Context

The model catalogue previously asked the user to select one to four Sparks. That count was only a presentation choice. It was not tied to authenticated nodes, live inventory, or a scheduler, so it could imply capacity the manager did not control.

The manager should discover capacity and show only valid deployment choices. A four-Spark owner should eventually be able to use those machines in two different ways:

- deploy a supported distributed recipe across a reserved group of nodes;
- deploy several independent single-node models on different nodes.

Those choices require node identity, membership, leases, per-node resource checks, and failure handling. Network proximity alone is not sufficient trust.

## Decision

The console no longer asks for a Spark count. `GET /api/v1/system` reports a hardware scope derived from manager inventory. The current scope is `local-manager`, which honestly contains only the node running this manager. The UI matches recipes to that detected capacity.

Future multi-node support will add a controller and paired node agents:

1. A node may advertise that a RunOnSpark agent exists, but advertisement never grants membership.
2. An operator pairs a new node once using a short-lived local code. Pairing establishes persistent node identity and mutually authenticated transport.
3. The controller keeps signed heartbeats and fresh per-node inventory. A missing or stale node is excluded from new placements.
4. Recipes declare supported topology counts and whether the topology is independent or distributed. The browser never supplies an arbitrary count.
5. The scheduler offers a recommended placement. An advanced placement control may select eligible nodes, but cannot override recipe or safety policy.
6. Before mutation, the controller obtains a lease for every selected node and runs the existing memory and disk evaluator against each node independently.
7. A distributed deployment starts only after every node reserves RAM, disk, ports, artifacts, and network prerequisites. Any failed reservation releases all leases.
8. Independent deployments receive separate lifecycle jobs and resource leases. Four nodes may therefore host up to four single-node models when every node passes its own guardrails.
9. The first controller has no automatic leader failover. This avoids split-brain scheduling until a separate replicated-control-plane design is verified.

## User experience

The default surface shows detected and paired nodes, current health, and the models that fit. The primary action remains one click with recommended placement.

When more than one valid placement exists, an optional placement sheet explains the concrete outcome, for example `Use 4 Sparks for one model` or `Use spark-03 only`. It shows reserved RAM and disk per node before confirmation.

Deployment progress is represented by one persistent job. The modal reads only persisted job state and typed step receipts. Closing the modal never cancels the job, and reopening it resumes the same timeline.

## Consequences

- The current release detects one local manager node and does not claim network discovery.
- Query parameters and user-selected Spark counts no longer affect recipe availability.
- Multi-node recipes remain unavailable until authenticated membership, leases, topology validation, and failure tests exist.
- Existing per-node OOM and disk guards are reusable by the future scheduler and remain non-aggregating.
