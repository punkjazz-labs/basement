import type { FleetSummary, FleetNodeSummary, Recipe, Telemetry } from './api'
import type { FleetRow } from './fleetModels'
import {
  bannerFor, boxHealth, summarize, unitStatus, type Banner, type BoxHealth, type FleetHealthSummary, type UnitStatus,
} from './fleetHealth'
import { boxHeadroom, type BoxHeadroom } from './headroom'
import { boxOfRow, type FleetMemory } from './loadFit'
import { LOCAL_MACHINE, heartbeatKey, type MonitorMachine } from './telemetry'
import { boxesWord, unitHealth, unitOfBox, type FleetGrouping, type FleetUnit } from './units'

// The fleet as every screen reads it, built once per render in App: the boxes
// in the order the screens draw them (unit by unit, head first, then the boxes
// in no unit), what each one's health is, and the few facts the health line,
// the Sparks table, the Monitor cards and the detail drawer all need. Pure.

export interface FleetView {
  // Every box, in drawing order. A Spark added by address that never joined
  // the fleet is here too, last: it has a row on the Sparks page, but it
  // reports nothing to the fleet and has no health.
  rows: FleetRow[]
  grouping: FleetGrouping
  // The boxes in no unit, this one included when no fleet names it.
  unassigned: FleetRow[]
  self: FleetRow
  memory: FleetMemory
  // One per box that reports to the fleet, in drawing order.
  health: BoxHealth[]
  summary: FleetHealthSummary
  banner: Banner | null
  // Whether there is a fleet to speak of: more than this one box.
  multi: boolean
  headroomOf: (row: FleetRow) => BoxHeadroom
  healthOf: (id: string) => BoxHealth | undefined
  rowOf: (id: string) => FleetRow | undefined
  // The row a name or an id points at, for a link that says ?box=<name>.
  findBox: (nameOrID: string) => FleetRow | undefined
  unitOf: (row: FleetRow) => FleetUnit | undefined
  findUnit: (id: string) => FleetUnit | undefined
  // A unit in one word, its own state read together with its boxes'.
  unitStatusOf: (unit: FleetUnit) => UnitStatus
  node: (id: string) => FleetNodeSummary | undefined
  // The key its meters are kept under in telemetry.ts, or empty for a box
  // this console cannot read.
  seriesKey: (row: FleetRow) => string
  // "head · TP4 · GLM 5.3 Flash", "member · TP2 · DeepSeek V4 Flash",
  // "unassigned · idle".
  roleLine: (row: FleetRow) => string
  temperatureOf: (row: FleetRow) => number | undefined
  powerOf: (row: FleetRow) => number | undefined
  ageOf: (row: FleetRow) => number | undefined
  nameOf: (recipeID: string) => string
}

export function buildFleetView(input: {
  memory: FleetMemory
  summary: FleetSummary | null
  recipes: readonly Recipe[]
  machines: readonly MonitorMachine[]
  // This box's own latest meter reading.
  selfLatest: Telemetry | null
  nowMs: number
}): FleetView {
  const { memory, summary, recipes, machines, selfLatest, nowMs } = input
  const nameOf = (recipeID: string) => recipes.find(recipe => recipe.id === recipeID)?.display_name ?? recipeID
  const grouping = memory.grouping
  const self = memory.self
  const known = memory.rows
  const selfListed = known.some(row => row.isSelf)
  const unassigned = [...(selfListed ? [] : [self]), ...grouping.independent]
  const inUnits = grouping.units.flatMap(unit => unit.members.map(member => member.row))
  const ordered = [...inUnits, ...unassigned.filter(row => !row.legacyPeerOnly), ...unassigned.filter(row => row.legacyPeerOnly)]
  const nodes = new Map((summary?.nodes ?? []).map(node => [node.node_id, node]))
  const keys = new Map(machines.map(machine => [machine.id, machine.key]))

  const headroomOf = (row: FleetRow) => boxHeadroom(boxOfRow(row, memory.view))
  const temperatureOf = (row: FleetRow) =>
    (row.isSelf && selfLatest ? selfLatest.gpu_temperature_c : row.inventory?.gpu_temperature_c) || undefined
  const powerOf = (row: FleetRow) =>
    (row.isSelf && selfLatest ? selfLatest.gpu_power_draw_watts : row.inventory?.gpu_power_draw_watts) || undefined
  const ageOf = (row: FleetRow) => {
    if (row.isSelf) return undefined
    const at = Date.parse(nodes.get(row.nodeID)?.last_heartbeat_at ?? '')
    return Number.isFinite(at) ? Math.max(nowMs - at, 0) : undefined
  }
  const seriesKey = (row: FleetRow) => {
    if (row.isSelf) return LOCAL_MACHINE
    const key = keys.get(row.nodeID)
    if (key !== undefined) return key
    return row.legacyPeerOnly ? row.nodeID : heartbeatKey(row.nodeID)
  }
  const unitOf = (row: FleetRow) => unitOfBox(grouping, row.nodeID)
  const roleLine = (row: FleetRow) => {
    const unit = unitOf(row)
    if (unit) {
      const head = unit.members.find(member => member.row.nodeID === row.nodeID)?.head
      return `${head ? 'head' : 'member'} · TP${unit.size} · ${nameOf(unit.recipeID)}`
    }
    if (row.legacyPeerOnly) return 'added by address · not in the fleet'
    return `unassigned · ${row.serving ? nameOf(row.serving.recipe_id) : 'idle'}`
  }

  const health = ordered.filter(row => !row.legacyPeerOnly).map(row => {
    const failed = row.installedModels.find(model => model.status === 'failed')
    return boxHealth({
      id: row.nodeID,
      name: row.displayName,
      self: row.isSelf,
      // This box answers: the console is talking to it now. Every other box
      // answers when the fleet can still act on it.
      answering: row.isSelf || row.answering,
      joining: !row.isSelf && row.status.dot === 'busy',
      ageMs: ageOf(row),
      headroom: headroomOf(row),
      temperature: temperatureOf(row),
      failedModel: failed ? nameOf(failed.recipe_id) : undefined,
    })
  })
  const byID = new Map(health.map(box => [box.id, box]))
  const reporting = ordered.filter(row => !row.legacyPeerOnly)
  const summaryLine = summarize(health, boxesWord(reporting))
  const findBox = (nameOrID: string) =>
    ordered.find(row => row.displayName === nameOrID) ?? ordered.find(row => row.nodeID === nameOrID)
  return {
    rows: ordered,
    grouping,
    unassigned,
    self,
    memory,
    health,
    summary: summaryLine,
    banner: bannerFor(summaryLine),
    multi: ordered.length > 1,
    headroomOf,
    healthOf: id => byID.get(id),
    rowOf: id => ordered.find(row => row.nodeID === id),
    findBox,
    unitOf,
    findUnit: id => grouping.units.find(unit => unit.key === id),
    unitStatusOf: unit => unitStatus(unitHealth(unit), unit.members.map(member => byID.get(member.row.nodeID))),
    node: id => nodes.get(id),
    seriesKey,
    roleLine,
    temperatureOf,
    powerOf,
    ageOf,
    nameOf,
  }
}
