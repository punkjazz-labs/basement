import type {
  FleetDeploymentView, FleetModelSnapshot, FleetNodeSummary, FleetSummary, NodeInventory, Peer,
  PlacementCandidate, PlacementPlan,
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
// last and the only one a new action should act on.
export function deploymentIndex(deployments: FleetDeploymentView[]): Map<string, string> {
  const index = new Map<string, string>()
  const createdAt = new Map<string, string>()
  for (const deployment of deployments) {
    for (const node of deployment.nodes ?? []) {
      const key = deploymentKey(node.node_id, deployment.recipe_id)
      const seen = createdAt.get(key)
      if (seen !== undefined && seen > deployment.created_at) continue
      index.set(key, deployment.deployment_id)
      createdAt.set(key, deployment.created_at)
    }
  }
  return index
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
