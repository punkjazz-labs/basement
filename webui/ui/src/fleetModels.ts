import {
  formatBytes, terminal,
  type FleetDeploymentView, type FleetModelSnapshot, type FleetNodeSummary, type FleetSummary,
  type InstallRequest, type NodeInventory, type Peer, type PlacementCandidate, type PlacementPlan,
} from './api'
import { labFor, labKey } from './catalog'
import {
  consoleKey, inFleet, isLocalNode, nodeName, nodeServing, nodeStatus, NO_ANSWER, type NodeStatus,
} from './fleetInvite'

// The Models view, read across the whole fleet. Everything here is pure: one
// row per Spark, which placement owns which model on which Spark, and what a
// placement plan says once each candidate is joined with the machine it runs
// on. Models.tsx owns the polling and the screen.
//
// Only the console that leads the fleet holds membership rows for the other
// Sparks, so these functions only ever see a controller's summary. Nothing
// below reaches for a fact a node has not reported: an absent inventory stays
// absent rather than being read as an empty machine.

// One Spark, as the fleet-wide Models table sees it.
export interface FleetRow {
  nodeID: string
  displayName: string
  isSelf: boolean
  // What that Spark does in the fleet, in the manager's own word for it
  // ("controller" or "member"). A Spark added by address is in no fleet, so
  // it has no role and this is empty.
  role: string
  // Where that Spark's own console answers. It is also the key the rows are
  // de-duplicated on, so every row here has one.
  consoleURL: string
  // Both come from that Spark's last heartbeat, so both are absent until it
  // has sent one.
  inventory?: NodeInventory
  installedModels: FleetModelSnapshot[]
  // The one model that Spark serves now, in the words fleetInvite already
  // uses for it.
  serving?: FleetModelSnapshot
  status: NodeStatus
  // Whether this console can still act on that Spark, which is not the same
  // as it being in the fleet: a member that has stopped sending heartbeats is
  // still a member, and an action sent to it would only fail.
  answering: boolean
  // The power mode that Spark last reported, and its own sentence about a GPU
  // that did not take it. Both come from the same heartbeat as everything
  // else here, so both are empty until one has arrived.
  power: PowerState
  // A Spark added by address that never joined the fleet. This console knows
  // its name and nothing else about it, so its row carries no machine facts
  // and no status word.
  legacyPeerOnly?: boolean
}

const LEGACY_PEER_STATUS: NodeStatus = { word: '', dot: '' }

// The membership states in which a Spark has gone quiet. The controller keeps
// the last thing it was told about such a node, and can act on none of it.
const SILENT_MEMBERSHIP = new Set(['stale', 'unreachable'])

// How old a heartbeat can be and still count as recent. This mirrors the
// manager's own bound, HeartbeatFreshness in internal/fleet/heartbeat.go. It
// has to be repeated here because one status word arrives with that reading
// already thrown away, and for no other reason: everywhere else the manager's
// own word is what this file reads.
const HEARTBEAT_FRESHNESS_MS = 30_000

// Whether that Spark has sent a heartbeat recently enough to be acted on. A
// Spark that has never sent one has reported nothing at all, so it answers
// for nothing. The reading is made against this browser's clock, which is the
// only clock a console has.
const heartbeatRecent = (node: FleetNodeSummary, nowMs: number): boolean => {
  const received = Date.parse(node.last_heartbeat_at ?? '')
  return Number.isFinite(received) && nowMs - received <= HEARTBEAT_FRESHNESS_MS
}

// Whether the controller is still in touch with that Spark. A node it has not
// finished admitting answers for nothing yet, and one that has gone quiet
// answers for nothing any more.
//
// One word needs more than itself. The manager writes "version-mismatch" over
// "stale" and over "unreachable" alike (internal/fleet/membership.go), so a
// Spark of another version that went offline hours ago carries the same word
// as one that answered a second ago. For that word only, the row reads the
// heartbeat time itself. A healthy Spark of another version keeps its buttons.
const nodeAnswering = (node: FleetNodeSummary, nowMs: number): boolean => {
  if (!inFleet(node) || SILENT_MEMBERSHIP.has(node.status)) return false
  if (node.status === 'version-mismatch') return heartbeatRecent(node, nowMs)
  return true
}

// One row per Spark, de-duplicated on the console URL exactly as
// membershipRows does. A membership row comes first because it is the only
// one carrying what that machine reported about itself; a peer no membership
// row speaks for keeps its own row, tagged so the caller knows the difference.
// A node with no console URL is left out for the same reason membershipRows
// leaves it out: it cannot be told apart from another row.
export function fleetRows(
  summary: FleetSummary | null,
  peers: Peer[],
  selfConsoleURL: string,
  nowMs: number,
): FleetRow[] {
  const taken = new Set<string>()
  const rows: FleetRow[] = []
  if (summary) {
    for (const node of summary.nodes) {
      const key = consoleKey(node.console_url)
      if (key === '' || taken.has(key)) continue
      taken.add(key)
      rows.push(membershipRow(summary, node, selfConsoleURL, nowMs))
    }
  }
  for (const peer of peers) {
    const key = consoleKey(peer.base_url)
    if (key === '' || taken.has(key)) continue
    taken.add(key)
    rows.push({
      nodeID: peer.id,
      displayName: peer.name,
      isSelf: false,
      role: '',
      consoleURL: peer.base_url,
      installedModels: [],
      status: LEGACY_PEER_STATUS,
      // This Spark reports nothing to this console, so it reports no power
      // mode either.
      power: UNREPORTED_POWER,
      // The fleet holds no membership row for this Spark, so it reports
      // nothing to this console and nothing here can act on it.
      answering: false,
      legacyPeerOnly: true,
    })
  }
  return rows
}

function membershipRow(
  summary: FleetSummary,
  node: FleetNodeSummary,
  selfConsoleURL: string,
  nowMs: number,
): FleetRow {
  return {
    nodeID: node.node_id,
    displayName: nodeName(node),
    isSelf: isLocalNode(summary, node, selfConsoleURL),
    role: node.role,
    consoleURL: node.console_url,
    inventory: node.inventory,
    installedModels: node.installed_models ?? [],
    serving: nodeServing(node),
    status: nodeStatus(node),
    answering: nodeAnswering(node, nowMs),
    power: { mode: node.power_mode ?? '', failure: node.power_mode_failure ?? '' },
  }
}

// ---- How hard each Spark runs its chip ---------------------------------------

// The two modes the manager knows, in its own words (internal/store/power.go).
// Nothing else is a mode: an answer with any other word is one this console
// cannot draw, and it is read as no mode at all.
export const FULL_MODE = 'full'
export const COOL_MODE = 'cool'

// What each mode is called on screen, and the small tag a capped Spark
// carries on its strip chip.
export const FULL_MODE_LABEL = 'Full speed'
export const COOL_MODE_LABEL = 'Cool and quiet'
export const COOL_TAG = 'cool'

// The one line under the Power row. It is one string on purpose: every figure
// in it was measured on a GB10 Spark (two qualification run pairs on
// edgexpert-2051, 2026-08-24), so nothing here may be assembled from a
// number this console happens to hold.
export const POWER_MODE_NOTE =
  'Cool and quiet caps the chip at 2200 MHz. Measured on a GB10 Spark: about a third less peak ' +
  'power, 6 degrees cooler, the same answer speed.'

// The quiet line while the call runs.
export const POWER_MODE_BUSY = 'Setting the power mode.'

// The label of the one control that sets the mode on every Spark at once. It
// copies no machine's mode: the control offers both modes itself, so a Spark
// that reports none can never hold the fleet back.
export const EVERY_SPARK = 'Set for every Spark'

// The title over the refusals one fleet-wide run left behind, and the one
// over a refusal from a single Spark. A run over one machine has one machine
// to name, so it names it.
export const POWER_REFUSED_TITLE = 'Not every Spark took the mode'
export const powerRefusedTitle = (displayName: string): string => `${displayName} did not take the mode`

// Where the fleet sets one Spark's power mode. The controller's own node id
// is accepted here too, so this one call serves every row of the dashboard
// and the console holds no second way to write the setting.
export const FLEET_POWER_MODE_PATH = '/api/v1/fleet/power-mode'

// Where a Spark that leads no fleet reads and sets its own mode. A standalone
// Spark and a member hold no fleet node for the machine the console runs on,
// so the fleet door has nothing to name; this is the door that machine has
// always had for itself (internal/httpapi/power.go).
export const LOCAL_POWER_MODE_PATH = '/api/v1/system/power-mode'

// One Spark's power mode as either its heartbeat or a set answer reports it.
// mode is "full", "cool", or empty for a Spark that has reported none.
export interface PowerState {
  mode: string
  failure: string
}

// A Spark that has said nothing about its chip. It is not full speed, and it
// is not capped: it is nothing, and the row has to say nothing.
const UNREPORTED_POWER: PowerState = { mode: '', failure: '' }

// What the Power row shows for one Spark.
export interface PowerRow {
  // Which of the two buttons reads as chosen. Empty selects neither.
  mode: string
  // Both buttons dead: this Spark has reported no mode, or a change is
  // already running on it.
  disabled: boolean
  // That Spark's own sentence about a GPU that did not take the mode, ready
  // to render unchanged. Empty while the mode is in force.
  failure: string
  // Whether the strip chip carries the small "cool" tag.
  tag: boolean
  // Whether the quiet busy line stands under the row.
  busy: boolean
}

// The mode a row shows, once the fleet read and a mode this console set are
// both in hand. The read is the authority, exactly as it is for placements,
// with one window: the controller stores a new mode before it answers, and
// that Spark reports it in a heartbeat seconds later. Inside that window the
// answer is what the machine really holds, so the answer stands whole, mode
// and sentence together. A Spark that has just had its driver fixed reports
// the same mode with an older sentence for a poll or two, which is why the
// sentence is not read from a read that has only caught up with half of it.
//
// The answer is retired the moment the read carries all of it
// (retiredPowerSets), and the read is the only authority from then on.
export function shownPower(reported: PowerState, set?: PowerState): PowerState {
  if (set === undefined || powerSettled(reported, set)) return reported
  return set
}

// Whether the fleet read has caught up with an answer this console holds, so
// the answer has nothing left to add.
export const powerSettled = (reported: PowerState, set: PowerState): boolean =>
  reported.mode === set.mode && reported.failure === set.failure

// The answers a read has made redundant, by node id. A Spark being set right
// now keeps its answer: the read it would be measured against is the one the
// call is about to replace. A Spark this console no longer holds a row for
// loses its answer, because nothing can show it any more.
//
// Retiring is what lets a later change reach this screen at all. An answer
// kept past its window disagrees with every read after it, so a mode set from
// another console, another tab, or by a Spark starting up would never be
// shown: the row would hold the old mode, and the chip the old tag, until
// this console was reloaded.
export function retiredPowerSets(
  sparks: readonly FleetRow[],
  set: ReadonlyMap<string, PowerState>,
  setting: ReadonlySet<string>,
): string[] {
  const rows = new Map(sparks.map(spark => [spark.nodeID, spark]))
  const retired: string[] = []
  for (const [nodeID, answer] of set) {
    if (powerBusy(nodeID, setting)) continue
    const row = rows.get(nodeID)
    if (row === undefined || powerSettled(row.power, answer)) retired.push(nodeID)
  }
  return retired
}

// Whether a change is already running on that Spark, so a second click cannot
// send a second call. This is the placementBusy rule for a setting rather than
// a job: setting names every Spark a run is working through, not only the one
// being called this second, so a row cannot take a click the run is about to
// overwrite.
export const powerBusy = (nodeID: string, setting: ReadonlySet<string>): boolean => setting.has(nodeID)

// One Spark's Power row, read from its own report, from what this console
// last set on it, and from whether a change is running.
export function powerRow(row: FleetRow, set: PowerState | undefined, setting: ReadonlySet<string>): PowerRow {
  const shown = shownPower(row.power, set)
  const known = shown.mode === FULL_MODE || shown.mode === COOL_MODE
  const busy = powerBusy(row.nodeID, setting)
  return {
    mode: known ? shown.mode : '',
    disabled: busy || !known,
    failure: known ? shown.failure : '',
    tag: known && shown.mode === COOL_MODE,
    busy,
  }
}

// The same row for a Spark that answers for itself: a console that leads no
// fleet reads one machine's mode from the local door and shows it with the
// same switch. It asks the same questions powerRow asks, of the one answer
// that door gives, and it keeps the same rule at the heart of it: a mode this
// console has not read is no mode at all, so the switch stays dead rather
// than drawing a machine as running at full speed.
export function localPowerRow(state: PowerState | null, busy: boolean): PowerRow {
  const mode = state?.mode ?? ''
  const known = mode === FULL_MODE || mode === COOL_MODE
  return {
    mode: known ? mode : '',
    disabled: busy || !known,
    failure: known ? state?.failure ?? '' : '',
    tag: known && mode === COOL_MODE,
    busy,
  }
}

// Every Spark "Set for every Spark" sends the mode to, in strip order. A Spark
// added by address is left out: it never joined this fleet, so the controller
// holds no node of that name and the call could only be refused for a machine
// nobody asked about. Every real member stays in, including one that has gone
// quiet: the controller answers for it by name, and a Spark that silently
// dropped out of a fleet-wide change would leave the owner believing it took
// the mode.
export const powerFanOut = (sparks: readonly FleetRow[]): FleetRow[] =>
  sparks.filter(spark => spark.legacyPeerOnly !== true)

// Whether a fleet-wide change can be sent at all. A run is refused here while
// any Spark it would name is already mid-change, so the button that starts it
// has to be dead for exactly as long: a live button that does nothing is
// worse than one that is plainly not ready.
export const powerFanOutBusy = (sparks: readonly FleetRow[], setting: ReadonlySet<string>): boolean =>
  powerFanOut(sparks).some(spark => powerBusy(spark.nodeID, setting))

// One line of the notice a fleet-wide run leaves behind. The manager's fleet
// door opens its refusal with the Spark's own display name, so such a
// sentence is shown unchanged; a refusal that never reached the manager (this
// console offline, the session gone) names no machine, so the Spark is named
// in front of it. The test is the name at the head of the sentence and not
// the name anywhere in it, or a Spark called "off" would read the word
// "offline" as its own name.
export const powerRefusalLine = (displayName: string, message: string): string =>
  message.startsWith(`${displayName} `) ? message : `${displayName}: ${message}`

// ---- Whether a member's own console shows the "ask the controller" banner --

// True only for a Spark that has joined another Spark's fleet as a member.
// The controller sees the fleet-wide table instead, so it never shows this. A
// standalone Spark, and a summary this console has not read yet, show nothing
// either: both have to stay silent rather than guess at a role.
export function shouldShowMemberBanner(summary: FleetSummary | null): boolean {
  return summary !== null && summary.role === 'member'
}

// The key a row and a recipe meet on. One Spark runs one placement of one
// model, so this pair is what an action has to name.
export const deploymentKey = (nodeID: string, recipeID: string): string => `${nodeID}:${recipeID}`

// Which placement owns each model on each Spark. A pair with more than one
// placement keeps the record the fleet still holds, and only then the newest
// of those. A removed record is one the fleet let go of, so a record that is
// still live always wins over it. The creation time alone gives the wrong
// answer here: the manager revives an adopted record with an update, which
// keeps the first creation time, so a live record can be older than a removed
// one. A pair with only a removed record keeps that record, because the row
// still has to read it to offer the Clear tool.
//
// The whole placement is kept, not only its id, because a row has to read
// what the controller last saw of it as well as act on it.
export function deploymentIndex(deployments: FleetDeploymentView[]): Map<string, FleetDeploymentView> {
  const index = new Map<string, FleetDeploymentView>()
  for (const deployment of deployments) {
    for (const node of deployment.nodes ?? []) {
      const key = deploymentKey(node.node_id, deployment.recipe_id)
      const seen = index.get(key)
      if (seen !== undefined && outranks(seen, deployment)) continue
      index.set(key, deployment)
    }
  }
  return index
}

// A record the fleet let go of. The manager never deletes a placement row, it
// marks it removed.
const released = (deployment: FleetDeploymentView): boolean => deployment.state === 'removed'

// Whether the record already kept stays, against one more record for the same
// Spark and model. A live record beats a removed one. Two records of the same
// kind go by creation time, newest first.
function outranks(seen: FleetDeploymentView, next: FleetDeploymentView): boolean {
  if (released(seen) !== released(next)) return !released(seen)
  return seen.created_at > next.created_at
}

// What the table holds after one read of the fleet's placements. The read is
// the authority for everything except a record this console wrote while that
// read was already in flight: the controller stores a placement before it
// answers the console that asked for it, so every read that begins later
// carries it, and one that began earlier cannot. Losing it for one round
// would unlock the row and let a second click start a second real job, so a
// record this console has acted on is kept until a read reports it. The
// manager never deletes a placement row, so nothing is kept alive here that
// the fleet has really let go of.
//
// polled is the map this returns. deploymentIndex builds a fresh one on every
// read, so nothing else is holding it.
export function mergePlacements(
  polled: Map<string, FleetDeploymentView>,
  previous: Map<string, FleetDeploymentView>,
  touched: ReadonlySet<string>,
): Map<string, FleetDeploymentView> {
  if (touched.size === 0) return polled
  const listed = new Set<string>()
  for (const placement of polled.values()) listed.add(placement.deployment_id)
  for (const [key, placement] of previous) {
    if (!touched.has(placement.deployment_id)) continue
    if (listed.has(placement.deployment_id) || polled.has(key)) continue
    polled.set(key, placement)
  }
  return polled
}

// ---- Where one row action goes ----------------------------------------------

// The actions a placement accepts, in the words the fleet API uses for them.
// Anything else has to stay on the machine it belongs to.
export const FLEET_DEPLOYMENT_ACTIONS = ['start', 'stop', 'remove', 'cancel', 'smoke-test', 'benchmark'] as const

export type FleetDeploymentAction = (typeof FLEET_DEPLOYMENT_ACTIONS)[number]

const ACTIONS = new Set<string>(FLEET_DEPLOYMENT_ACTIONS)

// The model one row acts on, and the Spark that holds it.
export interface ActionTarget {
  nodeID: string
  recipeID: string
  // The Spark this console runs on. It answers its own API, so nothing about
  // the fleet applies to it.
  isSelf: boolean
  // The other Spark's own row, when this model lives there. It is what says
  // whether the fleet could adopt the model that Spark already runs. The
  // caller only attaches it for a model one Spark can run on its own, so a
  // model that spans the fleet never reaches the adoption path.
  host?: FleetRow
}

// Where a row action has to be sent. "adopt" means the fleet holds no
// placement yet for a model another Spark already runs: recording one is the
// first half of the action. "none" carries the reason, so a row can say why a
// button is dead rather than offering one that would fail.
export type ActionRoute =
  | { where: 'local' }
  | { where: 'fleet'; deploymentID: string; path: string }
  | { where: 'adopt'; nodeID: string; recipeID: string }
  | { where: 'none'; reason: 'no-placement' | 'not-answering' | 'unsupported' }

// Where the fleet API takes one action on one placement.
export const deploymentActionPath = (deploymentID: string, action: string): string =>
  `/api/v1/fleet/deployments/${encodeURIComponent(deploymentID)}/${action}`

// Where the fleet API records a model a Spark already runs.
export const ADOPT_PATH = '/api/v1/fleet/deployments/adopt'

// Why a row action could not be sent. A row only offers a button its route
// allows, so these are what the owner reads when the fleet moved between the
// render and the click. The keys are for this code alone; only the sentences
// reach a screen.
export const ACTION_REFUSAL: Record<'no-placement' | 'not-answering' | 'unsupported', string> = {
  'no-placement': 'The fleet holds no placement for this model on that Spark.',
  'not-answering': 'That Spark does not answer for this model.',
  unsupported: 'The deployment action is not supported.',
}

// An adoption that answered without the record it was asked to make. The
// action after it has nothing to act on, so it is never sent.
export const NO_PLACEMENT_BACK = 'The controller gave no placement back.'

// ---- A record the fleet can no longer read -----------------------------------

// A placement the controller cannot read pins its row to "No answer" and
// leaves every button on it dead. The record itself is what keeps the row
// there: the fleet holds a placement for a model it can no longer follow, and
// adoption cannot write a fresh one while that record stands. Ending the
// record is the way out, and the fleet ends one by removing the placement.
//
// The row offers this as a tool, and only while the placement is really
// stale. The words say what really happens, because the model goes with the
// record.
export const CLEAR_RECORD = 'Clear'
export const CLEAR_RECORD_CONFIRM = 'Clear record'

// Where the fleet ends a record it can no longer act on. The Clear tool asks
// the owner Spark to remove the model first, because that is the honest thing
// to do while it still answers. This is the fallback for when that call cannot
// be addressed at all: the record names no job, or the Spark no longer knows
// the job, or the Spark has left the fleet. The manager refuses it for a
// record it can still read, so trying it after a refused remove is safe.
export const releasePath = (deploymentID: string): string =>
  `/api/v1/fleet/deployments/${encodeURIComponent(deploymentID)}/release`

export const clearRecordTitle = (modelName: string): string => `Clear the record for ${modelName}?`

export const clearRecordBody = (sparkName: string): string =>
  `The fleet cannot read this model on ${sparkName}. Basement asks ${sparkName} to remove the model. ` +
  'This ends the record, and the downloaded files stay.'

// Whether a placement is already mid-change, so its row must not send it a
// second action. A local row learns this from the job list it polls every few
// seconds; a placement is two polls away from saying the same thing, so the
// job this console just started counts until the controller's own read has
// caught up with it and reported it finished. A placement that has answered
// with no job at all, or with an older job than the one just started, is
// treated as still working rather than as free.
export function placementBusy(
  placement: FleetDeploymentView | undefined,
  startedJobID: string | undefined,
): boolean {
  if (placement === undefined) return false
  if (placement.job && !terminal(placement.job.state)) return true
  if (startedJobID === undefined) return false
  return !(placement.job?.id === startedJobID && terminal(placement.job.state))
}

// The placement that owns this model on this Spark. The Spark this console
// runs on never has one here: its own API is the authority for it.
//
// A removed record owns nothing. The manager never deletes a placement row, it
// marks it removed, so the read keeps reporting one long after the fleet let
// it go. Reading it as an owner would pin the row to whatever that record last
// said, and would keep adopt-on-demand from writing a fresh one. This is what
// lets a cleared record give its row back.
export function rowPlacement(
  target: ActionTarget,
  placements: Map<string, FleetDeploymentView>,
): FleetDeploymentView | undefined {
  if (target.isSelf) return undefined
  const placement = placements.get(deploymentKey(target.nodeID, target.recipeID))
  return placement?.state === 'removed' ? undefined : placement
}

// The one decision every row action makes. This Spark keeps the local call it
// has always used. Another Spark is reached through the placement that owns
// the model on it, and only while that Spark still answers for it. A model
// another Spark already runs that the fleet never placed is adopted first, so
// that every row offers the same actions whether the fleet installed the
// model or found it there.
export function rowActionRoute(
  target: ActionTarget,
  placements: Map<string, FleetDeploymentView>,
  action: string,
): ActionRoute {
  if (target.isSelf) return { where: 'local' }
  if (!ACTIONS.has(action)) return { where: 'none', reason: 'unsupported' }
  const placement = rowPlacement(target, placements)
  if (placement === undefined) return adoptRoute(target)
  if (placement.stale) return { where: 'none', reason: 'not-answering' }
  return {
    where: 'fleet',
    deploymentID: placement.deployment_id,
    path: deploymentActionPath(placement.deployment_id, action),
  }
}

// What to do about a model the fleet holds no placement for. Only a Spark in
// the fleet can be adopted onto, and only for a model it reports it holds:
// the manager refuses anything else, and a row must not offer a button that
// would only be refused. A Spark added by address is not in the fleet, and
// one that has gone quiet cannot be asked.
function adoptRoute(target: ActionTarget): ActionRoute {
  const host = target.host
  if (host === undefined || host.legacyPeerOnly === true ||
    !host.installedModels.some(model => model.recipe_id === target.recipeID)) {
    return { where: 'none', reason: 'no-placement' }
  }
  if (!host.answering) return { where: 'none', reason: 'not-answering' }
  return { where: 'adopt', nodeID: target.nodeID, recipeID: target.recipeID }
}

// ---- The Sparks named on a model's row ---------------------------------------

// The part of a Spark's name the owner already says out loud: everything after
// the last dash. "edgexpert-37c4" is "37c4" and "spark-f1cc" is "f1cc".
const lastToken = (displayName: string): string => {
  const at = displayName.lastIndexOf('-')
  return at > 0 && at < displayName.length - 1 ? displayName.slice(at + 1) : displayName
}

// The short name a chip, a band or a state line calls a Spark by. A name with
// no dash has no shorter form, and a short form that two Sparks would share
// names neither of them, so in both cases the whole name stands. The fleet
// strip keeps whole names always: it is the one place that says which machine
// is which.
export function shortSparkName(displayName: string, fleet: readonly string[]): string {
  const short = lastToken(displayName)
  if (short === displayName) return displayName
  const shared = fleet.some(other => other !== displayName && lastToken(other) === short)
  return shared ? displayName : short
}

// The chip a model that spans the pair carries. Two Sparks are "both": the
// fleet holds two machines and the model runs on the two of them.
export const BOTH_SPARKS_CHIP = 'both'
// The same fact in the band's own line, where the word stands alone.
export const BOTH_SPARKS = 'both Sparks'

// One name beside a model. live means that model is serving there now.
export interface NodeChip {
  key: string
  name: string
  live: boolean
}

// Which Sparks hold this model, as the row names them. A model that needs
// more than one Spark runs across all of them at once and cannot be moved
// between them, so it gets one chip that speaks for the set rather than a
// chip for each.
export function modelChips(sparks: FleetRow[], recipeID: string, sparkCount: number): NodeChip[] {
  const holders = sparks.filter(spark => spark.installedModels.some(model => model.recipe_id === recipeID))
  if (holders.length === 0) return []
  const live = holders.some(spark => spark.serving?.recipe_id === recipeID)
  if (sparkCount > 1) {
    return [{ key: 'topology', name: sparkCount === 2 ? BOTH_SPARKS_CHIP : `${sparkCount} Sparks`, live }]
  }
  const fleet = sparks.map(spark => spark.displayName)
  return holders.map(spark => ({
    key: spark.nodeID,
    name: shortSparkName(spark.displayName, fleet),
    live: spark.serving?.recipe_id === recipeID,
  }))
}

// ---- The band over the table -------------------------------------------------

// Where a model serves right now. Models.tsx fills both fields with the reads
// its rows have always made: this Spark's own model list, and what another
// Spark reports about the same model.
export interface ServingRead {
  here: boolean
  elsewhere: boolean
}

export const servesNow = (read: ServingRead): boolean => read.here || read.elsewhere

// The model that serves is the page's answer, so it leaves the table and
// stands over it in a band of its own. Two Sparks can each serve one, so this
// answers with a list rather than with one model, in the order the table held
// them. Everything else stays a row, and nothing appears twice.
export function splitServing<T extends { serving: ServingRead }>(
  models: readonly T[],
): { bands: T[]; rows: T[] } {
  const bands: T[] = []
  const rows: T[] = []
  for (const model of models) {
    if (servesNow(model.serving)) bands.push(model)
    else rows.push(model)
  }
  return { bands, rows }
}

// Where the band says the model runs: across the pair, across more machines
// than that, or on the one Spark that holds it. A console that can name no
// Spark says nothing rather than naming the wrong one.
export function servingPlace(sparkCount: number, sparkName: string): string {
  if (sparkCount === 2) return BOTH_SPARKS
  if (sparkCount > 2) return `${sparkCount} Sparks`
  return sparkName
}

// Which machine the band names. A model another Spark in the fleet holds runs
// on that Spark, whatever else is true of it. Otherwise this Spark names
// itself while it serves, and the Spark that serves it names itself when this
// one does not. Naming the wrong machine here is a false claim on screen, not
// a decoration, so the choice is made in one place and tested.
export interface ServingHost {
  // Another Spark in the fleet holds this model, and this one does not.
  onHost: boolean
  here: boolean
  elsewhere: boolean
}

export function servingSparkName(place: ServingHost, selfName: string, otherName: string): string {
  if (place.onHost) return otherName
  if (place.here) return selfName
  return place.elsewhere ? otherName : ''
}

// ---- The one number a speed cell shows ---------------------------------------

// What a community-reported figure carries and a measured one does not. The
// note under the table looks for this mark, so a number that loses it stops
// the note that explains it as well.
export const TYPICAL_MARK = '~'
export const NO_SPEED = 'n/a'

// The speed of one model in the words the cell and the band both use: what
// this Spark measured, or the community figure under the mark, or n/a. A
// model that generates media serves no tokens, so it has no speed of this kind
// at all.
export function speedText(measured: number | undefined, reference: number | undefined, tokenBased: boolean): string {
  if (!tokenBased) return NO_SPEED
  if (measured) return measured.toFixed(1)
  return reference ? `${TYPICAL_MARK}${reference}` : NO_SPEED
}

// Whether that number is a community report rather than a measurement. It is
// the whole gate on the note under the table: the note speaks for the mark, so
// it is shown only while the mark is on screen.
export const isTypical = (speed: string): boolean => speed.startsWith(TYPICAL_MARK)

// ---- The one orange button on the page ---------------------------------------

// The pill family the console draws its actions with. Orange is the primary
// action and nothing else, so exactly one control on this page may wear it.
export const PRIMARY_PILL = 'primary'
export const GHOST_PILL = 'ghost'

// Which of the two the Open button wears. The band is the page's answer and
// carries its one orange pill; the same button inside the table is a ghost,
// with every other action beside it.
export const openPillClass = (asBand: boolean): string => (asBand ? PRIMARY_PILL : GHOST_PILL)

// ---- What a row says about its own state -------------------------------------

// The two words a row keeps to itself. Under the first tab every model is
// installed and in the catalog none of them is, so the word would only repeat
// what the tab and the chips have already said.
const QUIET_STATES: ReadonlySet<string> = new Set(['Installed', 'Not installed'])

// The states that read as a warning rather than as work under way. Each one
// leads the line it appears in, so the whole line is read from its first word.
const WARN_STATES: readonly string[] = ['Failed', 'Revoked', NO_ANSWER]

export interface StateLine {
  text: string
  warn: boolean
}

// The word another Spark's note leads with, or nothing at all when the note
// only names a place. "Installed on 37c4" leads with "Installed"; "on 37c4"
// leads with no word, because it says where the model is and not what it does,
// and the chip beside the name has already said where it is.
const noteWord = (otherNote: string): string => {
  if (otherNote === '' || otherNote.startsWith('on ')) return ''
  const at = otherNote.indexOf(' on ')
  return at === -1 ? otherNote : otherNote.slice(0, at)
}

// The line under a model's name, in place of its spec line, while the model is
// in a state worth a line. It composes the words the row already worked out:
// its own state word, and what another Spark said about the same model. A
// serving model never reaches here, because a serving model is a band above
// the table.
//
// Neither machine's quiet word ever takes the line. Under the first tab every
// model is installed, on this Spark or on another one, so "Installed on 37c4"
// and the bare "on 37c4" both state what the tab and the chip have already
// stated, and the line the reader wants there is what the model is.
export function rowStateLine(statusText: string, otherNote: string): StateLine | null {
  const own = QUIET_STATES.has(statusText) ? '' : statusText
  const theirWord = noteWord(otherNote)
  const theirs = theirWord !== '' && !QUIET_STATES.has(theirWord) ? otherNote : ''
  const line = (text: string): StateLine =>
    ({ text, warn: WARN_STATES.some(state => text.startsWith(state)) })
  // Only the other Spark has something to say, or neither has.
  if (own === '') return theirs === '' ? null : line(theirs)
  // This row has a word of its own. A note that only names a place belongs to
  // that word; a second state stands beside it rather than running into it.
  if (theirs !== '') return line(`${own} · ${theirs}`)
  return line(theirWord === '' && otherNote !== '' ? `${own} ${otherNote}` : own)
}

// A placement candidate with the free memory and free disk of the machine it
// names. The plan itself carries no machine facts, so they are joined here
// from the membership summary by node id, and stay absent for a node that has
// reported nothing.
export interface PlacementCandidateView extends PlacementCandidate {
  memoryAvailableBytes?: number
  storageAvailableBytes?: number
}

export function joinCandidatesWithInventory(
  plan: PlacementPlan | null,
  summary: FleetSummary | null,
): PlacementCandidateView[] {
  const machines = new Map<string, NodeInventory | undefined>(
    (summary?.nodes ?? []).map(node => [node.node_id, node.inventory]),
  )
  return (plan?.candidates ?? []).map(candidate => {
    const machine = machines.get(candidate.node_id)
    return {
      ...candidate,
      memoryAvailableBytes: machine?.memory_available_bytes,
      storageAvailableBytes: machine?.storage_available_bytes,
    }
  })
}

// ---- Which Spark a new install runs on ---------------------------------------

// The last row of the "Run on" list. It is not a node id, and no node id can
// look like it. It stands for the Spark the controller recommends, and it is
// read at the moment the install is sent rather than when the dialog opened.
export const CHOOSE_FOR_ME = 'choose-for-me'

// The words that row shows, from the approved dialog.
export const CHOOSE_FOR_ME_NAME = 'Choose for me'
export const CHOOSE_FOR_ME_NOTE = 'Basement picks the Spark with room.'

// What a refused Spark says when the plan sends no reason with it. The
// manager always sends one, so this covers only an answer that arrives short
// of that. A dead row must never be dead without a word for why.
export const PLACEMENT_REFUSED = 'That Spark cannot take this model now.'

// What the dialog says when every Spark in the list is refused. Each row
// already carries its own reason; this is the whole list in one line, and the
// install button stays dead beside it.
export const NO_PLACEMENT_LEFT = 'No Spark in this fleet can take this model now.'

// A Spark the plan named that the fleet table holds no row for. The table
// drops a node with no console URL and de-duplicates two nodes that share
// one, so such a Spark is one this console cannot open, cannot follow, and
// cannot tell apart from another. It is named in the list and refused.
export const NO_FLEET_ROW = 'This console cannot reach that Spark now.'

// A Spark the fleet is already working on this same model on. The controller
// plans against what each Spark last reported, and a Spark only names a model
// in a heartbeat once its install has finished, so the plan can offer a
// machine that is downloading that very model. Sending a second install there
// would write a second record for one model on one Spark, which the manager
// now refuses; the list refuses it first, and says why.
export const PLACEMENT_WORKING = 'That Spark is already busy with this model.'

// One Spark the "Run on" list offers, as both the screen and the request read
// it. The plan says what the controller would do; the fleet table's row for
// that same machine says what this console can reach. Both are resolved once,
// here, so the facts the dialog shows and the Spark the install is sent to can
// never name two different machines.
export interface PlacementTarget {
  nodeID: string
  name: string
  // The Spark this console runs on, which installs through its own API.
  isSelf: boolean
  memoryAvailableBytes?: number
  storageAvailableBytes?: number
  // The model that Spark holds active, as the plan reports it. It counts
  // while it is still starting, because a model that is starting still has to
  // stop before another one can have the machine.
  currentModel?: FleetModelSnapshot
  // Which version of this same model that Spark already holds, if any.
  installedVersion?: number
  eligible: boolean
  // Why this Spark cannot be picked. Empty while it can.
  reason: string
}

// busyNodeIDs names the Sparks the fleet is already working on this same model
// on (workingNodes). The plan cannot know them: it reads what each Spark last
// reported, and a Spark names a model only once its install has finished.
export function placementTargets(
  plan: PlacementPlan | null,
  summary: FleetSummary | null,
  sparks: FleetRow[],
  selfNodeID: string,
  busyNodeIDs: ReadonlySet<string> = new Set(),
): PlacementTarget[] {
  const rows = new Map(sparks.map(spark => [spark.nodeID, spark]))
  const recipeID = plan?.recipe_id ?? ''
  return joinCandidatesWithInventory(plan, summary).map(candidate => {
    const spark = rows.get(candidate.node_id)
    // Two independent signals say this is the machine the console runs on:
    // the controller's own node id, and the row the table already marked. A
    // console always reaches its own Spark, with or without a row for it.
    const isSelf = (selfNodeID !== '' && candidate.node_id === selfNodeID) || spark?.isSelf === true
    const reachable = isSelf || spark !== undefined
    const working = busyNodeIDs.has(candidate.node_id)
    const eligible = candidate.eligible && reachable && !working
    return {
      nodeID: candidate.node_id,
      // The plan and the table read the same membership summary, so both
      // carry the same name. The row's name only fills a blank.
      name: candidate.display_name || spark?.displayName || '',
      isSelf,
      memoryAvailableBytes: candidate.memoryAvailableBytes,
      storageAvailableBytes: candidate.storageAvailableBytes,
      currentModel: candidate.current_model,
      installedVersion: spark?.installedModels
        .find(model => model.recipe_id === recipeID)?.recipe_version,
      eligible,
      // The plan's own refusal comes first, because the controller knows more
      // about that machine than this console does. A Spark the plan would
      // take is refused here for the two things only this console can see:
      // no row to reach it by, and work already running on this model.
      reason: eligible ? ''
        : !candidate.eligible ? candidate.reason || PLACEMENT_REFUSED
          : !reachable ? NO_FLEET_ROW
            : PLACEMENT_WORKING,
    }
  })
}

// One row of the "Run on" list.
export interface PlacementOption {
  // The Spark this row installs on, or CHOOSE_FOR_ME.
  key: string
  name: string
  // The line under the name: what that machine has free, or why it is
  // refused.
  note: string
  eligible: boolean
}

// What one candidate Spark has free, in the dialog's own words. A number that
// Spark has not reported reads n/a, exactly as the fleet line above the table
// reads it, rather than showing an unreported machine as an empty one.
export const machineNote = (
  machine: { memoryAvailableBytes?: number; storageAvailableBytes?: number },
): string =>
  `${formatBytes(machine.memoryAvailableBytes)} memory free · ${formatBytes(machine.storageAvailableBytes)} disk free`

// The "Run on" list, in plan order, with "Choose for me" last. A refused Spark
// keeps its row and states the refusal, because a machine that silently
// disappears from the list explains nothing. "Choose for me" is offered only
// while the recommended Spark can really take the model: with none, an offer
// to choose would promise what the fleet cannot do.
export function placementOptions(
  targets: PlacementTarget[],
  recommendedNodeID?: string,
): PlacementOption[] {
  const options: PlacementOption[] = targets.map(target => ({
    key: target.nodeID,
    name: target.name,
    note: target.eligible ? machineNote(target) : target.reason,
    eligible: target.eligible,
  }))
  if (targets.some(target => target.nodeID === recommendedNodeID && target.eligible)) {
    options.push({ key: CHOOSE_FOR_ME, name: CHOOSE_FOR_ME_NAME, note: CHOOSE_FOR_ME_NOTE, eligible: true })
  }
  return options
}

// The row the dialog opens on: the Spark the controller recommends. Without a
// usable recommendation it opens on the first Spark that could take the
// model, and with none of those it opens on nothing, which leaves the install
// button dead rather than pointing it at a machine that would refuse.
export function initialPlacement(targets: PlacementTarget[], recommendedNodeID?: string): string {
  if (targets.some(target => target.nodeID === recommendedNodeID && target.eligible)) {
    return recommendedNodeID ?? ''
  }
  return targets.find(target => target.eligible)?.nodeID ?? ''
}

// The Spark one choice names, with every fact about it. "Choose for me" reads
// the recommendation at the moment it is used, so both rows answer with the
// same kind of row from here on.
export function placedTarget(
  choice: string,
  targets: PlacementTarget[],
  recommendedNodeID?: string,
): PlacementTarget | undefined {
  const nodeID = choice === CHOOSE_FOR_ME ? recommendedNodeID ?? '' : choice
  if (nodeID === '') return undefined
  return targets.find(target => target.nodeID === nodeID)
}

// Where a confirmed install goes. This Spark keeps the local install call it
// has always used. Another Spark is installed on through the fleet, which
// reserves and starts the model there. The route carries the whole target, so
// the request and the dialog cannot read different machines. "none" is what a
// list with nothing to pick answers, so the button stays dead instead of
// sending a request the controller would only refuse.
export type InstallRoute =
  | { where: 'local' }
  | { where: 'fleet'; target: PlacementTarget }
  | { where: 'none' }

export function installRoute(
  choice: string,
  targets: PlacementTarget[],
  recommendedNodeID?: string,
): InstallRoute {
  const target = placedTarget(choice, targets, recommendedNodeID)
  if (target === undefined || !target.eligible) return { where: 'none' }
  return target.isSelf ? { where: 'local' } : { where: 'fleet', target }
}

// Where the controller plans a placement, and where the fleet makes one.
export const PLACEMENT_PLAN_PATH = '/api/v1/fleet/placements/plan'
export const FLEET_DEPLOYMENTS_PATH = '/api/v1/fleet/deployments'

// The body the fleet's create-placement API reads (fleetDeployments POST in
// internal/httpapi/fleet_deployments.go). It is the local install request with
// the Spark to run on named beside it, so neither path can drop a
// confirmation the other one sends.
export interface FleetInstallRequest extends InstallRequest {
  recipe_id: string
  node_id: string
}

export const fleetInstallRequest = (
  recipeID: string,
  nodeID: string,
  intent: InstallRequest,
): FleetInstallRequest => ({ recipe_id: recipeID, node_id: nodeID, ...intent })

// What the install button says for a Spark that is not this one. That Spark
// reports which version it holds and nothing about part-finished downloads,
// so a resume is never claimed for it.
export const placementVerb = (target: PlacementTarget, version: number): string =>
  (target.installedVersion !== undefined && target.installedVersion < version ? 'Update' : 'Install')

// Which model has to stop on that Spark before this install can serve. The
// plan names the model that Spark holds active, whether or not it has
// finished starting, so a model that is still coming up is named too: it has
// to stop all the same. A Spark holding this same model active restarts it on
// the new version, and a Spark holding nothing active gives up nothing.
export const placementSwitchFrom = (target: PlacementTarget | undefined): string | undefined =>
  target?.currentModel?.recipe_id

// ---- A model the fleet is still working on -----------------------------------

// The job kinds that change what a model is doing, and so the only ones that
// lock a row. A benchmark and a smoke test run against a model that is
// already serving, so Open must stay live through them. An adopt job is not
// here either: it only writes down a model a Spark already runs, and it is
// finished the moment it exists. The table rows, the first-run hero and the
// fleet's own placements all read this one set.
export const DISRUPTIVE_KINDS: ReadonlySet<string> = new Set(['install', 'start', 'stop', 'remove'])

// The placement that is still working on this model, on whichever Spark runs
// it. A remote install is recorded as a placement the moment the controller
// accepts it, but the Spark running it only names the model in a heartbeat
// once that install has finished. In between, nothing a row reads knows the
// work started: the table finds the Spark that holds a model by reading
// heartbeats, so the row would keep a live Install button for the whole
// download and a second click would create a second placement under a new
// idempotency key. This reads the record instead of the heartbeat, so the row
// locks from the first click, on every Spark.
// kinds is the set of job kinds that change what runs, so a benchmark or a
// smoke test on another Spark leaves the row alone, exactly as a local one
// does: both run against a model that is already serving.
export function workingPlacement(
  placements: Map<string, FleetDeploymentView>,
  startedJobs: ReadonlyMap<string, string>,
  recipeID: string,
  kinds: ReadonlySet<string>,
): FleetDeploymentView | undefined {
  return workingPlacements(placements, startedJobs, recipeID, kinds)[0]
}

// Every Spark the fleet is working on this model on, by node id. The install
// dialog reads this: the "Run on" list would otherwise offer a machine that is
// already downloading this very model, because a Spark names a model in a
// heartbeat only once its install has finished. The row lock above reads the
// same placements, so a row and the dialog can never disagree about what the
// fleet is doing.
export function workingNodes(
  placements: Map<string, FleetDeploymentView>,
  startedJobs: ReadonlyMap<string, string>,
  recipeID: string,
  kinds: ReadonlySet<string>,
): Set<string> {
  return new Set(workingPlacements(placements, startedJobs, recipeID, kinds)
    .map(placement => placement.owner_node_id))
}

// The placements still working on one model, whichever Spark holds each one.
// One placement can be reached under more than one key, so the caller reading
// node ids out of this must de-duplicate them.
function workingPlacements(
  placements: Map<string, FleetDeploymentView>,
  startedJobs: ReadonlyMap<string, string>,
  recipeID: string,
  kinds: ReadonlySet<string>,
): FleetDeploymentView[] {
  const working: FleetDeploymentView[] = []
  for (const placement of placements.values()) {
    if (placement.recipe_id !== recipeID) continue
    // A placement this console has not read a job kind for is treated as work
    // that changes what runs. Locking a row for a moment is safe; offering a
    // second install is not.
    const kind = placement.job?.kind ?? ''
    if (kind !== '' && !kinds.has(kind)) continue
    if (placementBusy(placement, startedJobs.get(placement.deployment_id))) working.push(placement)
  }
  return working
}

// What that Spark is doing to the model, from the kind of job the placement
// runs. A model the Spark does not name in a heartbeat yet is one being
// installed, which is the case this exists for; the rest are named so a row
// never invents "Installing" for work of another kind.
const PLACEMENT_WORDS: Record<string, string> = {
  install: 'Installing',
  start: 'Starting',
  stop: 'Stopping',
  remove: 'Removing',
}

export const placementWord = (placement: FleetDeploymentView | undefined): string => {
  if (placement === undefined) return ''
  return PLACEMENT_WORDS[placement.job?.kind ?? ''] ?? 'Working'
}

// Whether a row can act at all, once local work and fleet work are both read.
//
// Local work always locks it. Work the fleet is doing elsewhere locks only a
// row this Spark holds no model for. A model installed here is its own: a
// remote install of the same recipe downloads other files onto another
// machine, and it neither stops this one, starts it, nor touches what it
// serves. So Stop, Update, Open, Generate and the row's tools answer to local
// work alone, and stay live through an install running on another Spark.
//
// A row with no model here has no local work to read. The placement record is
// then the only thing that knows an install started at all, because the Spark
// running it names the model in a heartbeat only once that install has
// finished. There, the fleet's work is the whole lock: without it the row
// would offer Install again for the length of the download.
export const recipeBusy = (
  localBusy: boolean,
  hasLocalModel: boolean,
  working: FleetDeploymentView | undefined,
): boolean => localBusy || (!hasLocalModel && working !== undefined)

// ---- Which tab a model sits under --------------------------------------------

// The two tabs over the models table. With many models, the owner asks two
// different questions: what do I have, and what can I get. "mine" answers the
// first, "catalog" answers the second. Nothing here is remembered between
// visits: the tab is a state of the screen, not of the machine.
export type ModelsTab = 'mine' | 'catalog'

// The words the tabs and the catalog groups show. They live beside the split
// that fills them, so the tests read the same strings the screen does.
export const ONE_SPARK_TAB = 'On your Spark'
export const MANY_SPARKS_TAB = 'On your Sparks'
export const CATALOG_TAB = 'Catalog'

// What the catalog says when it holds nothing. Every model is already on a
// Spark, which is good news, so the line states it and stops there.
export const CATALOG_EMPTY = 'Every model is installed.'

// The first tab's label. It counts the Sparks this console knows, not the
// models on them: a console that has met no other machine speaks of one
// Spark, and a console that leads a fleet speaks of all of them. A console
// with no fleet row at all still runs on its own Spark, so it reads as one.
export const heldTabLabel = (knownSparks: number): string =>
  knownSparks > 1 ? MANY_SPARKS_TAB : ONE_SPARK_TAB

// The least a recipe has to say for the split to place it. The catalog groups
// by the lab that made the model, so the split reads the same two fields
// catalog.ts reads to name that lab.
export interface TabbedRecipe {
  id: string
  model_by?: string
  publisher?: string
}

// Whether this model already lives on a machine this console speaks for.
//
// Three sources answer, because three kinds of machine report differently.
// The local model list answers for this Spark. The fleet rows answer for
// every Spark in the fleet, and carry what each machine last reported it
// holds, so a model on another Spark and a model that spans two Sparks are
// both found the same way. A Spark added by address is in no fleet, so its
// row carries no model list at all; its own summary is the only thing that
// knows what it holds, and the row on screen already reads that summary.
// Without it the tab and the row would contradict each other: the row would
// say "Installed on shed" from under a tab that means "not installed".
export function heldSomewhere(
  recipeID: string,
  localRecipeIDs: ReadonlySet<string>,
  peerRecipeIDs: ReadonlySet<string>,
  sparks: readonly FleetRow[],
): boolean {
  if (localRecipeIDs.has(recipeID) || peerRecipeIDs.has(recipeID)) return true
  return sparks.some(spark => spark.installedModels.some(model => model.recipe_id === recipeID))
}

// One lab's models in the catalog, under the lab's own name.
export interface LabGroup<T> {
  label: string
  models: T[]
}

// The table, cut into the two tabs. The catalog keeps its groups, because it
// is the list that grows.
export interface ModelsSplit<T> {
  // Every model on a Spark, in the order it was given in.
  held: T[]
  // The catalog, in the same order, under one group for each lab. How many
  // Sparks a model needs is no longer the axis: with many models the maker is
  // the question the owner asks first, and the models that need two Sparks
  // say so in their own description line and in the install flow.
  labs: LabGroup<T>[]
  // How many models the catalog holds in all, which is the count on its tab.
  catalogCount: number
}

// Which tab owns each model. The order inside each list is the order the
// recipes arrived in, so the order sortCatalog chose is kept unchanged: it
// already puts the labs in the order they read on screen, and the newest
// model of each lab at the top of its group.
//
// A lab is added the first time one of its models appears, and every later
// model of that lab joins the group it opened. Sorted input therefore yields
// groups that run one after the other, and unsorted input still yields one
// group per lab rather than the same lab twice. Groups are held on the folded
// lab key, so one lab written two ways draws one divider, under the first
// spelling the catalog gave.
export function splitModels<T extends TabbedRecipe>(
  recipes: readonly T[],
  localRecipeIDs: ReadonlySet<string>,
  peerRecipeIDs: ReadonlySet<string>,
  sparks: readonly FleetRow[],
): ModelsSplit<T> {
  const split: ModelsSplit<T> = { held: [], labs: [], catalogCount: 0 }
  const groups = new Map<string, LabGroup<T>>()
  for (const recipe of recipes) {
    if (heldSomewhere(recipe.id, localRecipeIDs, peerRecipeIDs, sparks)) {
      split.held.push(recipe)
      continue
    }
    const label = labFor(recipe)
    const key = labKey(label)
    let group = groups.get(key)
    if (!group) {
      group = { label, models: [] }
      groups.set(key, group)
      split.labs.push(group)
    }
    group.models.push(recipe)
    split.catalogCount += 1
  }
  return split
}
