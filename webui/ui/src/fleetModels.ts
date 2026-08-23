import {
  terminal,
  type FleetDeploymentView, type FleetModelSnapshot, type FleetNodeSummary, type FleetSummary,
  type NodeInventory, type Peer, type PlacementCandidate, type PlacementPlan,
} from './api'
import { consoleKey, isLocalNode, nodeName, nodeServing, nodeStatus, type NodeStatus } from './fleetInvite'

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
  // A Spark added by address that never joined the fleet. This console knows
  // its name and nothing else about it, so its row carries no machine facts
  // and no status word.
  legacyPeerOnly?: boolean
}

const LEGACY_PEER_STATUS: NodeStatus = { word: '', dot: '' }

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
): FleetRow[] {
  const taken = new Set<string>()
  const rows: FleetRow[] = []
  if (summary) {
    for (const node of summary.nodes) {
      const key = consoleKey(node.console_url)
      if (key === '' || taken.has(key)) continue
      taken.add(key)
      rows.push(membershipRow(summary, node, selfConsoleURL))
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
      legacyPeerOnly: true,
    })
  }
  return rows
}

function membershipRow(
  summary: FleetSummary,
  node: FleetNodeSummary,
  selfConsoleURL: string,
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
}

// Where a row action has to be sent. "none" carries the reason, so a row can
// say why a button is dead rather than offering one that would fail.
export type ActionRoute =
  | { where: 'local' }
  | { where: 'fleet'; deploymentID: string; path: string }
  | { where: 'none'; reason: 'no-placement' | 'not-answering' | 'unsupported' }

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
// the model on it, and only while that Spark still answers for it.
export function rowActionRoute(
  target: ActionTarget,
  placements: Map<string, FleetDeploymentView>,
  action: string,
): ActionRoute {
  if (target.isSelf) return { where: 'local' }
  if (!ACTIONS.has(action)) return { where: 'none', reason: 'unsupported' }
  const placement = rowPlacement(target, placements)
  if (placement === undefined) return { where: 'none', reason: 'no-placement' }
  if (placement.stale) return { where: 'none', reason: 'not-answering' }
  return {
    where: 'fleet',
    deploymentID: placement.deployment_id,
    path: `/api/v1/fleet/deployments/${encodeURIComponent(placement.deployment_id)}/${action}`,
  }
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
