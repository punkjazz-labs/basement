// Test fixture only: never imported by the console itself, so the build never
// carries it. The owner's fleet as the tests read it: four Sparks running one
// model on TP4 (the first one is the box the console runs on), two EdgeXperts
// running another on TP2, and the summary and placements that join them.

import type { FleetDeploymentView, FleetModelSnapshot, FleetSummary, InstalledModel, Recipe } from './api'
import type { FleetRow } from './fleetModels'
import { buildFleetView, type FleetView } from './fleetView'
import { fleetMemory } from './loadFit'
import type { MonitorMachine } from './telemetry'
import { heartbeatKey, LOCAL_MACHINE } from './telemetry'

export const GB = 1_000_000_000
export const GLM = 'glm53-flash-exl3-2s'
export const DS = 'deepseek-v4-flash-0731-2s'
export const NOW = Date.parse('2026-10-25T12:00:00Z')

export const RECIPES = [
  { id: GLM, display_name: 'GLM 5.3 Flash' },
  { id: DS, display_name: 'DeepSeek V4 Flash 0731' },
] as unknown as Recipe[]

export const serving = (recipeID: string): FleetModelSnapshot =>
  ({ recipe_id: recipeID, recipe_version: 1, status: 'ready', active: true })

export function box(name: string, product: string, freeGB: number, held: FleetModelSnapshot[] = [], overrides: Partial<FleetRow> = {}): FleetRow {
  return {
    nodeID: `node-${name}`,
    displayName: name,
    isSelf: false,
    role: 'member',
    consoleURL: `http://${name}.local:7070`,
    inventory: {
      hostname: name, product_name: product, dgx_spark: true,
      memory_total_bytes: 128.5 * GB, memory_available_bytes: freeGB * GB,
      gpu_memory_total_bytes: 128.5 * GB, gpu_memory_free_bytes: freeGB * GB,
      storage_total_bytes: 1000 * GB, storage_available_bytes: 300 * GB,
      gpu_temperature_c: 63, gpu_power_draw_watts: 71,
      os: 'Ubuntu 24.04.3 LTS', kernel: '6.11.0-1016-nvidia', gpu_description: 'NVIDIA GB10',
    },
    installedModels: held,
    serving: held.find(model => model.active && model.status === 'ready'),
    status: held.some(model => model.active) ? { word: 'Serving', dot: 'on' } : { word: 'Idle', dot: '' },
    answering: true,
    power: { mode: 'full', failure: '' },
    ...overrides,
  }
}

export function placement(id: string, recipeID: string, names: string[], overrides: Partial<FleetDeploymentView> = {}): FleetDeploymentView {
  return {
    deployment_id: id,
    recipe_id: recipeID,
    recipe_version: 1,
    recipe_fingerprint: 'fixture',
    topology_count: names.length,
    owner_node_id: `node-${names[0]}`,
    owner_job_id: '',
    state: 'ready',
    last_observed_at: '2026-10-25T10:00:00Z',
    created_at: '2026-10-24T10:00:00Z',
    updated_at: '2026-10-25T10:00:00Z',
    nodes: names.map((name, rank) => ({
      deployment_id: id, node_id: `node-${name}`, node_role: rank === 0 ? 'head' : 'worker', rank, reservation_id: '',
    })),
    stale: false,
    ...overrides,
  }
}

// What this Spark itself says it runs: GLM, serving.
export const LOCAL_MODELS: InstalledModel[] = [
  { recipe_id: GLM, recipe_version: 1, status: 'ready', active: true, updated_at: '2026-10-25T10:00:00Z', tokens_per_second: 47.6 },
]

export const SPARK_NAMES = ['spark-f7e0', 'spark-2b4f', 'spark-9a07', 'spark-c3d1']
export const EDGE_NAMES = ['edgexpert-41b8', 'edgexpert-5e12']

// The six boxes. edgexpert-5e12 is the tight one, as in the owner's fixture.
export function sixBoxes(overrides: Record<string, Partial<FleetRow>> = {}): FleetRow[] {
  const free: Record<string, number> = {
    'spark-f7e0': 18.5, 'spark-2b4f': 19.4, 'spark-9a07': 17.8, 'spark-c3d1': 20.1,
    'edgexpert-41b8': 19.6, 'edgexpert-5e12': 7.2,
  }
  return [
    ...SPARK_NAMES.map((name, index) => box(name, 'DGX Spark', free[name], [serving(GLM)], { isSelf: index === 0, ...overrides[name] })),
    ...EDGE_NAMES.map(name => box(name, 'EdgeXpert', free[name], [serving(DS)], overrides[name])),
  ]
}

export const PLACEMENTS = [
  placement('dep_glm_tp4', GLM, SPARK_NAMES),
  placement('dep_ds_tp2', DS, EDGE_NAMES),
]

// The summary the controller answers with: every box's heartbeat time, the
// given number of seconds before NOW.
export function summaryOf(rows: readonly FleetRow[], ageSeconds: Record<string, number> = {}): FleetSummary {
  return {
    fleet_id: 'fleet_fixture',
    role: 'controller',
    controller_node_id: rows.find(row => row.isSelf)?.nodeID ?? '',
    controller_console_url: '',
    migration_state: '',
    nodes: rows.map(row => ({
      node_id: row.nodeID,
      display_name: row.displayName,
      role: row.isSelf ? 'controller' : 'member',
      status: 'fresh',
      console_url: row.consoleURL,
      node_url: '',
      manager_version: 'dev',
      last_heartbeat_at: new Date(NOW - (ageSeconds[row.displayName] ?? 4) * 1000).toISOString(),
      inventory: row.inventory,
      installed_models: row.installedModels,
      power_mode: row.power.mode,
      power_mode_failure: row.power.failure,
    })),
  }
}

export const machinesOf = (rows: readonly FleetRow[]): MonitorMachine[] =>
  rows.map(row => ({
    id: row.nodeID,
    key: row.isSelf ? LOCAL_MACHINE : row.legacyPeerOnly ? '' : heartbeatKey(row.nodeID),
    name: row.displayName,
  }))

export function viewOf(rows: FleetRow[], options: {
  placements?: FleetDeploymentView[]
  summary?: FleetSummary | null
  ages?: Record<string, number>
  models?: InstalledModel[]
} = {}): FleetView {
  const placements = options.placements ?? PLACEMENTS
  const memory = fleetMemory({
    sparks: rows, models: options.models ?? LOCAL_MODELS, recipes: RECIPES, deployments: placements, jobs: [], system: null, latest: null,
  })
  return buildFleetView({
    memory,
    summary: options.summary === undefined ? summaryOf(rows, options.ages) : options.summary,
    recipes: RECIPES,
    machines: machinesOf(rows),
    selfLatest: null,
    nowMs: NOW,
  })
}
