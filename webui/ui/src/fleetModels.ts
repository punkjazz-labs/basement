import {
  formatBytes, terminal,
  type FleetDeploymentView, type FleetModelSnapshot, type FleetNodeSummary, type FleetSummary,
  type InstallRequest, type NodeInventory, type Peer, type PlacementCandidate, type PlacementPlan,
} from './api'
import { consoleKey, inFleet, isLocalNode, nodeName, nodeServing, nodeStatus, type NodeStatus } from './fleetInvite'

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
      consoleURL: peer.base_url,
      installedModels: [],
      status: LEGACY_PEER_STATUS,
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
    consoleURL: node.console_url,
    inventory: node.inventory,
    installedModels: node.installed_models ?? [],
    serving: nodeServing(node),
    status: nodeStatus(node),
    answering: nodeAnswering(node, nowMs),
  }
}

// The key a row and a recipe meet on. One Spark runs one placement of one
// model, so this pair is what an action has to name.
export const deploymentKey = (nodeID: string, recipeID: string): string => `${nodeID}:${recipeID}`

// Which placement owns each model on each Spark. A pair with more than one
// placement keeps the newest, because that is the one the controller created
// last and the only one a new action should act on. The whole placement is
// kept, not only its id, because a row has to read what the controller last
// saw of it as well as act on it.
export function deploymentIndex(deployments: FleetDeploymentView[]): Map<string, FleetDeploymentView> {
  const index = new Map<string, FleetDeploymentView>()
  for (const deployment of deployments) {
    for (const node of deployment.nodes ?? []) {
      const key = deploymentKey(node.node_id, deployment.recipe_id)
      const seen = index.get(key)
      if (seen !== undefined && seen.created_at > deployment.created_at) continue
      index.set(key, deployment)
    }
  }
  return index
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

// The word a row shows for a Spark that has stopped answering for its own
// placement. The controller keeps the last state it saw, and this says plainly
// that it is only the last one.
export const NOT_ANSWERING = 'Not answering'

// Why a row action could not be sent. A row only offers a button its route
// allows, so these are what the owner reads when the fleet moved between the
// render and the click.
export const ACTION_REFUSAL: Record<'no-placement' | 'not-answering' | 'unsupported', string> = {
  'no-placement': 'The fleet holds no placement for this model on that Spark.',
  'not-answering': 'That Spark is not answering for this model.',
  unsupported: 'The deployment action is not supported.',
}

// An adoption that answered without the record it was asked to make. The
// action after it has nothing to act on, so it is never sent.
export const NO_PLACEMENT_BACK = 'The controller gave no placement back.'

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
export function rowPlacement(
  target: ActionTarget,
  placements: Map<string, FleetDeploymentView>,
): FleetDeploymentView | undefined {
  if (target.isSelf) return undefined
  return placements.get(deploymentKey(target.nodeID, target.recipeID))
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

// One name beside a model. live means that model is serving there now.
export interface NodeChip {
  key: string
  name: string
  live: boolean
}

// Which Sparks hold this model, as the row names them. A model that needs
// more than one Spark runs across all of them at once and cannot be moved
// between them, so it gets one chip that counts the machines rather than a
// chip for each.
export function modelChips(sparks: FleetRow[], recipeID: string, sparkCount: number): NodeChip[] {
  const holders = sparks.filter(spark => spark.installedModels.some(model => model.recipe_id === recipeID))
  if (holders.length === 0) return []
  const live = holders.some(spark => spark.serving?.recipe_id === recipeID)
  if (sparkCount > 1) return [{ key: 'topology', name: `${sparkCount} Sparks`, live }]
  return holders.map(spark => ({
    key: spark.nodeID,
    name: spark.displayName,
    live: spark.serving?.recipe_id === recipeID,
  }))
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

export function placementTargets(
  plan: PlacementPlan | null,
  summary: FleetSummary | null,
  sparks: FleetRow[],
  selfNodeID: string,
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
    const eligible = candidate.eligible && reachable
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
      reason: eligible ? '' : candidate.eligible ? NO_FLEET_ROW : candidate.reason || PLACEMENT_REFUSED,
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
  for (const placement of placements.values()) {
    if (placement.recipe_id !== recipeID) continue
    // A placement this console has not read a job kind for is treated as work
    // that changes what runs. Locking a row for a moment is safe; offering a
    // second install is not.
    const kind = placement.job?.kind ?? ''
    if (kind !== '' && !kinds.has(kind)) continue
    if (placementBusy(placement, startedJobs.get(placement.deployment_id))) return placement
  }
  return undefined
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
