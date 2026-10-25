import { describe, expect, it } from 'vitest'
import type { FleetDeploymentView, FleetModelSnapshot, Job, Recipe } from './api'
import type { FleetRow } from './fleetModels'
import type { Box } from './headroom'
import { inFlightLoads, loadFit, loadTargets, loadingJob, variantFamily, type MemoryView } from './loadFit'
import { ABLIT_RECIPE_ID, STOCK_GLM_RECIPE_ID } from './modelVariants'
import { fleetUnits } from './units'

const GB = 1_000_000_000

// A vLLM recipe that claims share of every box it runs on, which is how the
// manager's guard charges it, with a reserve of 12 GB.
const recipe = (id: string, share: string, sparks = 1): Recipe => ({
  id, version: 1, display_name: id, publisher: 'test', trust: 'basement-candidate', verification: 'candidate',
  source: { url: 'https://example.invalid', revision: '0'.repeat(40) },
  topology: { spark_count: sparks },
  artifacts: [],
  requirements: {
    per_node_minimum_memory_bytes: 100 * GB, per_node_memory_reserve_bytes: 12 * GB, safety_margin_bytes: 20 * GB,
    secrets: [], required_licence_acceptance: false,
  },
  service: { internal_port: 8000, default_host_port: 8000, served_model_id: id, vllm: { gpu_memory_utilization: share } },
  runtime: { kind: 'vllm', start_timeout_minutes: 0 },
  artifact_bytes: 10 * GB,
  required_bytes: 0,
  revoked: false,
})

const serving = (id: string): FleetModelSnapshot => ({ recipe_id: id, recipe_version: 1, status: 'ready', active: true })

const box = (name: string, availableGB: number, held: FleetModelSnapshot[] = [], isSelf = false): FleetRow => ({
  nodeID: `node-${name}`, displayName: name, isSelf, role: 'member', consoleURL: `http://${name}.local:7070`,
  inventory: {
    hostname: name, product_name: 'DGX Spark', dgx_spark: true,
    memory_total_bytes: 100 * GB, memory_available_bytes: availableGB * GB,
    storage_total_bytes: 1000 * GB, storage_available_bytes: 500 * GB,
  },
  installedModels: held,
  serving: held.find(model => model.active),
  status: { word: 'Serving', dot: 'on' }, answering: true, power: { mode: 'full', failure: '' },
})

const placement = (id: string, recipeID: string, names: string[], job?: Job): FleetDeploymentView => ({
  deployment_id: id, recipe_id: recipeID, recipe_version: 1, recipe_fingerprint: '', topology_count: names.length,
  owner_node_id: `node-${names[0]}`, owner_job_id: job?.id ?? '', state: 'ready', last_observed_at: '',
  created_at: '2026-10-24T10:00:00Z', updated_at: '', stale: false, job,
  nodes: names.map((name, rank) => ({
    deployment_id: id, node_id: `node-${name}`, node_role: rank === 0 ? 'head' : 'worker', rank, reservation_id: '',
  })),
})

const view = (rows: FleetRow[], deployments: FleetDeploymentView[], recipes: Recipe[], inFlight = new Map<string, number>()): MemoryView => {
  const self = rows.find(row => row.isSelf) ?? rows[0]
  const selfBox: Box = {
    id: self.nodeID, name: self.displayName,
    totalBytes: self.inventory?.memory_total_bytes, availableBytes: self.inventory?.memory_available_bytes,
  }
  return { recipes, rows, grouping: fleetUnits(rows, deployments), self: selfBox, inFlight }
}

const BIG = recipe('big-2s', '0.80', 2)
const SMALL = recipe('small-1s', '0.30')
const OTHER = recipe('other-1s', '0.80')

describe('what a start frees', () => {
  // The engine stops the model the head serves and nothing else. A box that
  // serves a model of its own keeps it, so its memory is not credited.
  it('credits only the model the start really stops', () => {
    const a = box('spark-a', 15, [serving(SMALL.id)], true)
    const b = box('spark-b', 15, [serving(OTHER.id)])
    const rows = [a, b]
    const deployments = [placement('dep_big', BIG.id, ['spark-a', 'spark-b'])]
    const memory = view(rows, deployments, [BIG, SMALL, OTHER])
    // The stopped BIG unit is still placed on both boxes, but neither serves
    // it, so neither belongs to its unit.
    const plan = { targets: rows, boxCount: 2, head: a }
    const fit = loadFit(BIG, plan, memory)
    expect(fit.fits).toBe(false)
    expect(fit.blocking?.box.name).toBe('spark-b')
    expect(fit.boxes[0].freed).toBeGreaterThan(0)
    expect(fit.boxes[1].freed).toBe(0)
  })

  // A unit switching models frees the old model on every member.
  it('credits every member of the head unit for the head model', () => {
    const a = box('spark-a', 15, [serving(BIG.id)], true)
    const b = box('spark-b', 15, [serving(BIG.id)])
    const rows = [a, b]
    const memory = view(rows, [placement('dep_big', BIG.id, ['spark-a', 'spark-b'])], [BIG, OTHER])
    const fit = loadFit(OTHER, { targets: [a], boxCount: 1, head: a }, memory)
    expect(fit.fits).toBe(true)
    expect(fit.boxes[0].freedName).toBe(BIG.id)
  })
})

describe('which boxes a start loads onto', () => {
  // The two GLM variants run on the same unit, whichever id it was placed
  // under, so the switch between them checks every box of it.
  it('finds a GLM unit placed under the other variant', () => {
    expect(variantFamily(ABLIT_RECIPE_ID)).toEqual([STOCK_GLM_RECIPE_ID, ABLIT_RECIPE_ID])
    expect(variantFamily('qwen36-27b-nvfp4-1s')).toEqual(['qwen36-27b-nvfp4-1s'])
    const a = box('spark-a', 15, [serving(STOCK_GLM_RECIPE_ID)], true)
    const b = box('spark-b', 15, [serving(STOCK_GLM_RECIPE_ID)])
    const grouping = fleetUnits([a, b], [placement('dep_glm', STOCK_GLM_RECIPE_ID, ['spark-a', 'spark-b'])])
    const ablit = recipe(ABLIT_RECIPE_ID, '0.87', 2)
    const plan = loadTargets(ablit, a, grouping)
    expect(plan.targets.map(row => row.displayName)).toEqual(['spark-a', 'spark-b'])
    expect(plan.boxCount).toBe(2)
  })

  // A model that needs two boxes, checked on the one box this console knows,
  // can be refused there but is never said to fit.
  it('never says a model fits on fewer boxes than it needs', () => {
    const a = box('spark-a', 95, [], true)
    const memory = view([a], [], [BIG])
    const plan = loadTargets(BIG, a, memory.grouping)
    expect(plan.targets).toHaveLength(1)
    const fit = loadFit(BIG, plan, memory)
    expect(fit.fits).toBeNull()
    expect(fit.partial).toBe(true)
    const tight = box('spark-a', 5, [], true)
    expect(loadFit(BIG, loadTargets(BIG, tight, memory.grouping), view([tight], [], [BIG])).fits).toBe(false)
  })
})

describe('loads already on their way', () => {
  const job = (id: string, recipeID: string, kind = 'start', state = 'starting'): Job => ({
    id, kind, recipe_id: recipeID, state, created_at: '', updated_at: '', steps: [],
  })

  it('counts a start or an activating install, never a download only', () => {
    expect(loadingJob(job('j1', BIG.id))).toBe(true)
    expect(loadingJob(job('j2', BIG.id, 'start', 'ready'))).toBe(false)
    expect(loadingJob(job('j3', BIG.id, 'install', 'downloading_models'))).toBe(false)
    expect(loadingJob({
      ...job('j4', BIG.id, 'install', 'downloading_models'),
      steps: [{ index: 0, operation: 'start_container', state: 'pending' }],
    })).toBe(true)
  })

  // This box's own job is also the job of the placement it leads. It is
  // charged once here, and still charged on every other box it loads onto.
  it('charges a distributed load on every member, and this box once', () => {
    const a = box('spark-a', 90, [], true)
    const b = box('spark-b', 90)
    const start = job('j-big', BIG.id)
    const deployments = [placement('dep_big', BIG.id, ['spark-a', 'spark-b'], start)]
    const memory = view([a, b], deployments, [BIG])
    const loads = inFlightLoads([start], a.nodeID, deployments, memory)
    expect(loads.get('node-spark-a')).toBe(80 * GB)
    expect(loads.get('node-spark-b')).toBe(80 * GB)
  })
})
