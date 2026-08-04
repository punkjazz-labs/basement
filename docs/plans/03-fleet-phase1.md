# Spec 03: fleet, phase 1 (read-only)

Branch `spec/03-fleet-phase1`. Strategically the highest-priority workstream: the
product's flagship story is more than one Spark. Phase 1 is observability only; no
remote actions, no distributed serving. Do not exceed this scope.

## Concept

Every machine runs the manager. The machine whose console you open is "this Spark".
Other machines are peers, added by the user with a URL and an API key generated on the
peer. The console gains a Fleet tab showing every machine's identity, health, and
serving state, read-only.

## Backend

1. **Store.** New table `peers`: `id` (uuid), `name` (user label), `base_url`,
   `api_key`, `created_at`. API keys live server-side only; after creation the key is
   never returned by any endpoint.
2. **Endpoints** (all behind the existing console auth):
   - `GET /api/v1/peers` returns `[{id, name, base_url}]` (no keys).
   - `POST /api/v1/peers` body `{name, base_url, api_key}`. Before saving, prove the
     pair works: call the peer's `/api/v1/system` with the key; on failure return 400
     with `could not reach that Spark with this key, so check the URL and key`.
   - `DELETE /api/v1/peers/{id}`.
   - `GET /api/v1/peers/{id}/summary` proxies, server-side, the peer's `/api/v1/system`,
     `/api/v1/models`, `/api/v1/telemetry` and returns one merged JSON object plus
     `reachable: bool`. Timeout 3s per call. **Allowlist exactly these three paths**;
     the proxy must not be a general forwarder.
3. **Peer-side auth.** Verify that the existing API-key auth accepts key-authenticated
   requests to those three GET endpoints (`withReadAuth`). If keys currently only gate
   `/v1/*` model traffic, extend key auth to read-only `/api/v1/{system,models,telemetry}`
   and nothing else. Document what you found in the report.
4. **URL constraint.** Accept `http://` and `https://` base URLs; reject anything with
   a path, query, or credentials in the URL. LAN HTTP is acceptable at this phase;
   an mTLS story is a later spec and must not be improvised here.

## Console

New tab `Fleet` between Monitor and Storage.

- Row 1: this Spark, from existing state (hostname, serving model with logo, live
  tok/s if serving, memory free, manager version). Labeled `This Spark`.
- One row per peer: name, hostname, serving model (logo + name), memory free, manager
  version, and a state dot: green serving, neutral idle, red `Unreachable`.
  Data from `/summary`, polled every 10s while the tab is visible.
- Row action (ghost pill): `Open console` links to the peer's base_url in a new tab.
  `Remove` lives inside a row expansion, danger pill, with confirm.
- Empty state: `One Spark here. Add another to see your whole fleet on one screen.`
  plus an `Add a Spark` primary pill opening a dialog: fields Name, URL, API key, and
  helper copy `Generate an API key on that Spark's Connect tab, then paste it here.`
- Reuse the models-table row idiom and the pill family; no new visual language. This
  spec is not mockup-gated because it composes existing components; if you find
  yourself inventing new visuals, stop and flag it.

## Non-goals

Remote install/start/stop; aggregated activity; distributed (2-Spark) serving; peer
autodiscovery; TLS provisioning. All later phases.

## Acceptance

- Unit tests: peer CRUD; proxy allowlist rejects `/api/v1/keys` and arbitrary paths;
  summary marks unreachable peers instead of erroring the whole response.
- Mock harness: two fake peers (one reachable fixture, one down); screenshots of the
  fleet table and the add dialog.
- Full build/vet/test green.

## Hardware runbook (for the owner, not the executor)

1. Upgrade the primary MSI (edgexpert-alpha) with current main, then install the manager
   on the second MSI: build arm64 binary, `setup --binary` against edgexpert-beta.
2. On edgexpert-beta's console: Connect tab, generate an API key named `fleet-alpha`.
3. On edgexpert-alpha's console: Fleet tab, Add a Spark, `http://edgexpert-beta.local:<port>`,
   the key. Confirm identity, model state, and unreachability (power edgexpert-beta off) all
   render.
4. Repeat from a DGX Spark (spark-head) to confirm cross-vendor display.
