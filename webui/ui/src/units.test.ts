import { describe, expect, it } from 'vitest'
import type { FleetDeploymentView, FleetModelSnapshot, InstalledModel } from './api'
import type { FleetRow } from './fleetModels'
import {
  boxKind, boxesWord, fleetDisk, fleetUnits, memberRole, unitFor, unitHealth, unitMemory, unitOfBox, unitPlacement,
  unitServing, withLocalModels, HEALTH_WORD,
} from './units'

const GB = 1_000_000_000
const GLM = 'glm53-flash-exl3-2s'
const DS = 'deepseek-v4-flash-0731-2s'

const serving = (recipeID: string): FleetModelSnapshot =>
  ({ recipe_id: recipeID, recipe_version: 1, status: 'ready', active: true })

const box = (name: string, product: string, held: FleetModelSnapshot[] = [], overrides: Partial<FleetRow> = {}): FleetRow => ({
  nodeID: `node-${name}`,
  displayName: name,
  isSelf: false,
  role: 'member',
  consoleURL: `http://${name}.local:7070`,
  inventory: {
    hostname: name, product_name: product, dgx_spark: true,
    memory_total_bytes: 128 * GB, memory_available_bytes: 18 * GB,
    storage_total_bytes: 1000 * GB, storage_available_bytes: 300 * GB,
  },
  installedModels: held,
  serving: held.find(model => model.active && model.status === 'ready'),
  status: { word: 'Serving', dot: 'on' },
  answering: true,
  power: { mode: 'full', failure: '' },
  ...overrides,
})

const deployment = (id: string, recipeID: string, nodes: string[], overrides: Partial<FleetDeploymentView> = {}): FleetDeploymentView => ({
  deployment_id: id,
  recipe_id: recipeID,
  recipe_version: 1,
  recipe_fingerprint: 'fixture',
  topology_count: nodes.length,
  owner_node_id: `node-${nodes[0]}`,
  owner_job_id: '',
  state: 'ready',
  last_observed_at: '2026-10-25T10:00:00Z',
  created_at: '2026-10-24T10:00:00Z',
  updated_at: '2026-10-25T10:00:00Z',
  nodes: nodes.map((name, rank) => ({
    deployment_id: id, node_id: `node-${name}`, node_role: rank === 0 ? 'head' : 'worker', rank, reservation_id: '',
  })),
  stale: false,
  ...overrides,
})

// The owner's fleet: four Sparks running one model on TP4, two EdgeXperts
// running another on TP2, and nothing else.
const sparks = ['spark-f7e0', 'spark-2b4f', 'spark-9a07', 'spark-c3d1']
  .map((name, index) => box(name, 'DGX Spark', [serving(GLM)], { isSelf: index === 0 }))
const edges = ['edgexpert-41b8', 'edgexpert-5e12'].map(name => box(name, 'EdgeXpert', [serving(DS)]))
const fleet = [...sparks, ...edges]
const placements = [
  deployment('dep_ds_tp2', DS, ['edgexpert-41b8', 'edgexpert-5e12']),
  deployment('dep_glm_tp4', GLM, ['spark-f7e0', 'spark-2b4f', 'spark-9a07', 'spark-c3d1']),
]

describe('the units a fleet forms', () => {
  it('groups six boxes into a TP4 unit and a TP2 unit', () => {
    const { units, independent } = fleetUnits(fleet, placements)
    expect(units.map(unit => unit.key)).toEqual(['dep_glm_tp4', 'dep_ds_tp2'])
    expect(units.map(unit => unit.size)).toEqual([4, 2])
    expect(independent).toEqual([])
  })

  it('names a unit by its width and its boxes, never as both Sparks', () => {
    const { units } = fleetUnits(fleet, placements)
    expect(units.map(unitPlacement)).toEqual(['TP4 · 4 Sparks', 'TP2 · 2 EdgeXperts'])
  })

  it('puts the head first and the rest by rank', () => {
    const shuffled = deployment('dep_glm_tp4', GLM, ['spark-f7e0', 'spark-2b4f', 'spark-9a07', 'spark-c3d1'])
    shuffled.nodes.reverse()
    const [unit] = fleetUnits(sparks, [shuffled]).units
    expect(unit.members.map(member => member.row.displayName)).toEqual(sparks.map(row => row.displayName))
    expect(unit.members.map(memberRole)).toEqual(['head', 'rank 1', 'rank 2', 'rank 3'])
  })

  // A box that is part of no multi-box placement runs on its own.
  it('keeps every other box in the Independent group', () => {
    const loose = box('spark-loft', 'DGX Spark')
    const { units, independent } = fleetUnits([...fleet, loose], [placements[0]])
    expect(units.map(unit => unit.key)).toEqual(['dep_ds_tp2'])
    expect(independent.map(row => row.displayName)).toEqual([...sparks.map(row => row.displayName), 'spark-loft'])
  })

  // A one-box placement joins nothing to anything.
  it('does not make a unit of a one-box placement', () => {
    const single = deployment('dep_q', 'qwen36-27b-nvfp4-1s', ['spark-2b4f'])
    expect(fleetUnits(fleet, [single]).units).toEqual([])
  })

  it('ignores a placement the fleet let go of', () => {
    const removed = deployment('dep_ds_tp2', DS, ['edgexpert-41b8', 'edgexpert-5e12'], { state: 'removed' })
    expect(fleetUnits(fleet, [removed]).units).toEqual([])
  })

  // A box sits in one unit at most, and the unit that is really serving on it
  // wins over an older record that is not.
  it('gives a box to the unit that serves on it', () => {
    const old = deployment('dep_old', 'inkling-small-nvfp4-2s', ['edgexpert-41b8', 'edgexpert-5e12'], {
      created_at: '2026-10-25T09:00:00Z',
    })
    const { units } = fleetUnits(fleet, [old, ...placements])
    expect(units.map(unit => unit.key)).toEqual(['dep_glm_tp4', 'dep_ds_tp2'])
  })

  it('finds the unit that runs a model, and the unit a box is in', () => {
    const grouping = fleetUnits(fleet, placements)
    expect(unitFor(grouping, [DS])?.key).toBe('dep_ds_tp2')
    expect(unitFor(grouping, ['glm53-flash-exl3-ablit-2s', GLM], 'node-spark-9a07')?.key).toBe('dep_glm_tp4')
    expect(unitFor(grouping, ['qwen36-27b-nvfp4-1s'])).toBeUndefined()
    expect(unitOfBox(grouping, 'node-edgexpert-5e12')?.key).toBe('dep_ds_tp2')
  })
})

describe('what the boxes are called', () => {
  it('reads the box kind from the product name', () => {
    expect(boxKind('DGX Spark')).toBe('Spark')
    expect(boxKind('NVIDIA DGX Spark')).toBe('Spark')
    expect(boxKind('EdgeXpert')).toBe('EdgeXpert')
    expect(boxKind('ASUS Ascent GX10')).toBe('GX10')
    expect(boxKind('To Be Filled By O.E.M.')).toBe('')
    expect(boxKind('')).toBe('')
  })

  // The count is certain; the kind is only named when every box reports it.
  it('falls back to boxes for a mixed or unreported set', () => {
    expect(boxesWord(sparks)).toBe('4 Sparks')
    expect(boxesWord(edges)).toBe('2 EdgeXperts')
    expect(boxesWord([sparks[0], edges[0]])).toBe('2 boxes')
    expect(boxesWord([box('spark-x', '')])).toBe('1 box')
    expect(boxesWord([sparks[0]])).toBe('1 Spark')
    expect(boxesWord([], 3)).toBe('3 boxes')
  })
})

describe('how a unit is doing', () => {
  it('is healthy and serving while every member answers and serves', () => {
    const [unit] = fleetUnits(fleet, placements).units
    expect(unitHealth(unit)).toBe('healthy')
    expect(HEALTH_WORD[unitHealth(unit)]).toBe('Healthy')
    expect(unitServing(unit)).toBe(true)
  })

  // A quiet member is a warning, not a failure.
  it('is degraded when a member stops answering', () => {
    const quiet = fleet.map(row => (row.displayName === 'edgexpert-5e12' ? { ...row, answering: false } : row))
    const ds = fleetUnits(quiet, placements).units.find(unit => unit.key === 'dep_ds_tp2')
    expect(ds && unitHealth(ds)).toBe('degraded')
  })

  it('is degraded when a member has left the fleet', () => {
    const ds = fleetUnits(fleet.filter(row => row.displayName !== 'edgexpert-5e12'), placements)
      .units.find(unit => unit.key === 'dep_ds_tp2')
    expect(ds?.members).toHaveLength(1)
    expect(ds && unitHealth(ds)).toBe('degraded')
    expect(ds && unitPlacement(ds)).toBe('TP2 · 2 EdgeXperts')
    expect(ds && unitServing(ds)).toBe(false)
  })

  it('is failed only for a state that really failed', () => {
    const failed = fleetUnits(fleet, [deployment('dep_ds_tp2', DS, ['edgexpert-41b8', 'edgexpert-5e12'], { state: 'failed' })])
    expect(unitHealth(failed.units[0])).toBe('failed')
  })

  it('is not serving while one member is still starting', () => {
    const starting = fleet.map(row => (row.displayName === 'edgexpert-41b8'
      ? { ...row, installedModels: [{ ...serving(DS), status: 'starting' }] }
      : row))
    const ds = fleetUnits(starting, placements).units.find(unit => unit.key === 'dep_ds_tp2')
    expect(ds && unitServing(ds)).toBe(false)
  })
})

describe('this box and the whole fleet', () => {
  // This box's own model list is fresher than the heartbeat about it.
  it('reads this box from its own model list', () => {
    const models: InstalledModel[] = [
      { recipe_id: GLM, recipe_version: 3, status: 'ready', active: true, updated_at: '' },
    ]
    const rows = withLocalModels([{ ...sparks[0], installedModels: [], serving: undefined }, sparks[1]], models)
    expect(rows[0].serving?.recipe_id).toBe(GLM)
    expect(rows[1]).toBe(sparks[1])
  })

  it('totals free disk over the boxes that reported it', () => {
    const silent = box('spark-quiet', 'DGX Spark', [], { inventory: undefined })
    expect(fleetDisk([...edges, silent])).toEqual({ bytes: 600 * GB, reported: 2, boxes: 3 })
  })
})

// The sum a group header shows. A load never draws on it: each box answers
// for its own share. It is only how a group of boxes is read at a glance.
describe('what a unit has free between its boxes', () => {
  const boxOf = (row: FleetRow) => ({
    totalBytes: row.inventory?.memory_total_bytes, availableBytes: row.inventory?.memory_available_bytes,
  })

  it('adds up the boxes that reported', () => {
    const [unit] = fleetUnits(fleet, placements).units
    expect(unitMemory(unit, boxOf)).toEqual({ free: 72 * GB, total: 512 * GB, reported: 4, members: 4 })
  })

  it('says how many reported when some have not', () => {
    const quiet = fleet.map((row, index) => (index === 1 ? { ...row, inventory: undefined } : row))
    const [unit] = fleetUnits(quiet, placements).units
    expect(unitMemory(unit, boxOf)).toMatchObject({ free: 54 * GB, reported: 3, members: 4 })
  })
})
