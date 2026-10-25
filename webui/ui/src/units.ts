import type { FleetDeploymentView, FleetModelSnapshot, InstalledModel } from './api'
import type { FleetRow } from './fleetModels'

// The fleet, read as the owner reads it: not six boxes, but the units they
// form. A model split across boxes with tensor parallelism runs as one unit
// on all of them at once, and those boxes stand or fall together. Every box
// that is part of no such unit runs on its own and sits in one Independent
// group. Pure: Models, Sparks and Monitor all draw from this one grouping.
//
// The units come from the fleet's own placements (fleet deployments), never
// from guessing at which boxes serve the same model: two boxes can each run
// the same one-box model with nothing joining them.

export interface UnitMember {
  row: FleetRow
  rank: number
  head: boolean
}

export interface FleetUnit {
  // The placement's own id, which no other unit shares.
  key: string
  deployment: FleetDeploymentView
  recipeID: string
  // How many boxes the model is split across, as the placement records it.
  // A member that has left the fleet still counts here, which is what lets
  // the unit say it is short of one.
  size: number
  // Head first, then by rank.
  members: UnitMember[]
}

export interface FleetGrouping {
  units: FleetUnit[]
  independent: FleetRow[]
}

// The states of a placement record that still hold boxes together. A removed
// record is one the fleet let go of.
const released = (deployment: FleetDeploymentView): boolean => deployment.state === 'removed'

// Whether a box reports this model loaded and answering.
export const servesModel = (row: FleetRow, recipeID: string): boolean =>
  row.installedModels.some(model => model.recipe_id === recipeID && model.active && model.status === 'ready')

export function fleetUnits(
  rows: readonly FleetRow[],
  deployments: readonly FleetDeploymentView[],
): FleetGrouping {
  const byID = new Map(rows.map(row => [row.nodeID, row]))
  // A box sits in one unit at most. A placement that is serving claims its
  // boxes before one that is not, and a newer one before an older one, so a
  // stale record never pulls a box out of the unit that really runs on it.
  const candidates = deployments
    .filter(deployment => !released(deployment) && (deployment.nodes ?? []).length > 1)
    .map(deployment => {
      const members = [...deployment.nodes]
        .sort((left, right) => left.rank - right.rank)
        .map(node => ({ node, row: byID.get(node.node_id) }))
      const serving = members.length > 0 && members.every(({ row }) => row !== undefined && servesModel(row, deployment.recipe_id))
      return { deployment, members, serving }
    })
    .sort((left, right) =>
      left.serving !== right.serving
        ? (left.serving ? -1 : 1)
        : right.deployment.created_at.localeCompare(left.deployment.created_at))
  const claimed = new Set<string>()
  const units: FleetUnit[] = []
  for (const { deployment, members } of candidates) {
    const present = members.filter(member => member.row !== undefined)
    if (present.length === 0 || present.some(member => claimed.has(member.node.node_id))) continue
    for (const member of present) claimed.add(member.node.node_id)
    units.push({
      key: deployment.deployment_id,
      deployment,
      recipeID: deployment.recipe_id,
      size: Math.max(deployment.topology_count || 0, deployment.nodes.length),
      members: present
        .map(({ node, row }) => ({
          row: row as FleetRow,
          rank: node.rank,
          head: node.node_role === 'head' || (node.node_role === '' && node.rank === 0),
        }))
        .sort((left, right) => (left.head !== right.head ? (left.head ? -1 : 1) : left.rank - right.rank)),
    })
  }
  // The unit this console runs in leads, then the widest, then by name, so
  // the order holds still from one poll to the next.
  units.sort((left, right) => {
    const selfLeft = left.members.some(member => member.row.isSelf)
    const selfRight = right.members.some(member => member.row.isSelf)
    if (selfLeft !== selfRight) return selfLeft ? -1 : 1
    if (left.size !== right.size) return right.size - left.size
    return (left.members[0]?.row.displayName ?? '').localeCompare(right.members[0]?.row.displayName ?? '')
  })
  return { units, independent: rows.filter(row => !claimed.has(row.nodeID)) }
}

// The unit that runs one of these recipes, preferring the one a given box is
// in. A GLM row reads both of its variants, so it asks with both ids.
export function unitFor(
  grouping: FleetGrouping,
  recipeIDs: readonly string[],
  nodeID?: string,
): FleetUnit | undefined {
  const running = grouping.units.filter(unit => recipeIDs.includes(unit.recipeID))
  if (nodeID !== undefined) {
    const own = running.find(unit => unit.members.some(member => member.row.nodeID === nodeID))
    if (own) return own
  }
  return running[0]
}

// The unit a box is in, if any.
export const unitOfBox = (grouping: FleetGrouping, nodeID: string): FleetUnit | undefined =>
  grouping.units.find(unit => unit.members.some(member => member.row.nodeID === nodeID))

// ---- What the boxes are called ---------------------------------------------

// The product a box reported, as the owner calls it. Only the names below are
// recognised; any other product name, and a box that reported none, is a box.
const KINDS: [RegExp, string][] = [
  [/edgexpert/i, 'EdgeXpert'],
  [/spark/i, 'Spark'],
  [/gx10/i, 'GX10'],
]

export function boxKind(productName?: string): string {
  if (!productName) return ''
  for (const [pattern, kind] of KINDS) if (pattern.test(productName)) return kind
  return ''
}

// "4 Sparks", "2 EdgeXperts", "1 Spark". Boxes of more than one kind, or of
// a kind not reported, are boxes: the count is certain, the kind is not.
export function boxesWord(rows: readonly FleetRow[], count = rows.length): string {
  const kinds = new Set(rows.map(row => boxKind(row.inventory?.product_name)))
  const kind = rows.length > 0 && kinds.size === 1 ? [...kinds][0] : ''
  if (count === 1) return `1 ${kind || 'box'}`
  return `${count} ${kind ? `${kind}s` : 'boxes'}`
}

// Where a unit runs, in the words every screen uses: "TP4 · 4 Sparks".
export const unitPlacement = (unit: FleetUnit): string =>
  `TP${unit.size} · ${boxesWord(unit.members.map(member => member.row), unit.size)}`

// What a member is within its unit.
export const memberRole = (member: UnitMember): string => (member.head ? 'head' : `rank ${member.rank}`)

// ---- How a unit is doing ------------------------------------------------------

export type UnitHealth = 'healthy' | 'degraded' | 'failed'

export const HEALTH_WORD: Record<UnitHealth, string> = {
  healthy: 'Healthy',
  degraded: 'Degraded',
  failed: 'Failed',
}

// Healthy while every member answers. A member that has gone quiet, a member
// missing from the fleet, or a placement the controller can no longer read
// makes the unit degraded, which is a warning. Only a state that really
// failed is a failure.
export function unitHealth(unit: FleetUnit): UnitHealth {
  const failedHere = unit.members.some(member =>
    member.row.installedModels.some(model => model.recipe_id === unit.recipeID && model.status === 'failed'))
  if (unit.deployment.state === 'failed' || failedHere) return 'failed'
  if (unit.deployment.stale || unit.members.length < unit.size || unit.members.some(member => !member.row.answering)) {
    return 'degraded'
  }
  return 'healthy'
}

// Whether the unit is serving its model now: every member says so in its own
// heartbeat. One member short of that and the model is not answering.
export const unitServing = (unit: FleetUnit): boolean =>
  unit.members.length === unit.size && unit.members.every(member => servesModel(member.row, unit.recipeID))

// ---- This box's own word for itself -------------------------------------------

// The box this console runs on knows its own models first hand, and that is
// fresher than the heartbeat the fleet relays about it. Every grouping and
// every serving read on this console takes the box's own list for itself.
export function withLocalModels(rows: readonly FleetRow[], models: readonly InstalledModel[]): FleetRow[] {
  const snapshots: FleetModelSnapshot[] = models.map(model => ({
    recipe_id: model.recipe_id,
    recipe_version: model.recipe_version,
    status: model.status,
    active: model.active,
  }))
  return rows.map(row => row.isSelf
    ? {
      ...row,
      installedModels: snapshots,
      serving: snapshots.find(model => model.active && model.status === 'ready'),
    }
    : row)
}

// ---- Disk across the fleet -------------------------------------------------

export interface FleetDisk {
  bytes: number
  // How many boxes reported a figure, out of how many there are. A total over
  // fewer boxes than the fleet holds says so.
  reported: number
  boxes: number
}

export function fleetDisk(rows: readonly FleetRow[]): FleetDisk {
  let bytes = 0
  let reported = 0
  for (const row of rows) {
    const free = row.inventory?.storage_available_bytes
    if (typeof free !== 'number' || !(row.inventory?.storage_total_bytes)) continue
    bytes += free
    reported += 1
  }
  return { bytes, reported, boxes: rows.length }
}
