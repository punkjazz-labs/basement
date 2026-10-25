import {
  stepOperation, terminal,
  type FleetDeploymentView, type InstalledModel, type Job, type Recipe, type SystemInfo, type Telemetry,
} from './api'
import type { FleetRow } from './fleetModels'
import { boxFromInventory, boxFromSystem, fitOn, needOnBox, type Box, type FitResult, type Freed } from './headroom'
import { ABLIT_RECIPE_ID, STOCK_GLM_RECIPE_ID } from './modelVariants'
import { fleetUnits, unitFor, unitOfBox, withLocalModels, type FleetGrouping } from './units'

// Whether loading one model onto real boxes would fit right now. headroom.ts
// holds the arithmetic; this joins it to the fleet as the console reads it:
// which box is which, what each one serves and would stop, and which loads
// are already on their way into memory. Pure, so Models and Roles answer the
// same question the same way.

export interface MemoryView {
  recipes: readonly Recipe[]
  rows: readonly FleetRow[]
  grouping: FleetGrouping
  // This box, from the freshest reading the console holds of it.
  self: Box
  // Memory promised to loads in flight, by box id.
  inFlight: ReadonlyMap<string, number>
}

export function boxOfRow(row: FleetRow, view: MemoryView): Box {
  if (row.isSelf) return { ...view.self, id: row.nodeID || view.self.id, name: row.displayName || view.self.name }
  return boxFromInventory(row.nodeID, row.displayName, row.inventory)
}

// How many boxes a model is split across where it runs now: its unit's width,
// or the recipe's own topology where no unit holds it.
export function splitOf(recipe: Recipe, grouping: FleetGrouping, nodeID?: string): number {
  return unitFor(grouping, [recipe.id], nodeID)?.size ?? recipe.topology.spark_count
}

// The model a box would stop to take another one, and what that gives back.
// A model the catalog does not know gives back nothing, because nothing here
// can say how much it holds.
export function freedOn(row: FleetRow, view: MemoryView, box: Box): Freed | undefined {
  const serving = row.serving
  if (!serving) return undefined
  const recipe = view.recipes.find(item => item.id === serving.recipe_id)
  if (!recipe) return undefined
  return { bytes: needOnBox(recipe, box, splitOf(recipe, view.grouping, row.nodeID)).bytes, name: recipe.display_name }
}

// The ids a placement of this model can be recorded under. The two GLM
// variants run on the same unit and replace each other there, so a unit
// placed under either one is the unit both of them load onto.
export const variantFamily = (recipeID: string): string[] =>
  recipeID === STOCK_GLM_RECIPE_ID || recipeID === ABLIT_RECIPE_ID
    ? [STOCK_GLM_RECIPE_ID, ABLIT_RECIPE_ID]
    : [recipeID]

// Where one start loads a model: the boxes, how many boxes share the model,
// and the box the start runs from.
export interface LoadPlan {
  targets: FleetRow[]
  boxCount: number
  head: FleetRow
}

// The boxes a model loads onto when it starts from a given box: the whole
// unit that runs it there, or that one box. A model that needs more boxes
// than are named here can only be checked on the ones that are.
export function loadTargets(
  recipe: Recipe,
  row: FleetRow,
  grouping: FleetGrouping,
  recipeIDs: readonly string[] = variantFamily(recipe.id),
): LoadPlan {
  const unit = unitFor(grouping, recipeIDs, row.nodeID)
  if (unit && unit.members.some(member => member.row.nodeID === row.nodeID)) {
    return { targets: unit.members.map(member => member.row), boxCount: unit.size, head: row }
  }
  return { targets: [row], boxCount: recipe.topology.spark_count, head: row }
}

// Whether the start stops the model a target box serves now. The engine stops
// the model the head serves, across the boxes that model runs on, and nothing
// else: a box that serves some other model of its own keeps it, and its
// memory is not coming back.
function stoppedBy(row: FleetRow, head: FleetRow, grouping: FleetGrouping): boolean {
  if (row.nodeID === head.nodeID) return true
  const serving = row.serving?.recipe_id
  if (!serving || serving !== head.serving?.recipe_id) return false
  const unit = unitOfBox(grouping, head.nodeID)
  return unit !== undefined && unit === unitOfBox(grouping, row.nodeID)
}

// Loading recipe as the plan says. Every target box is checked on its own; a
// plan that names fewer boxes than the model needs can refuse, but it never
// says the model fits.
export function loadFit(recipe: Recipe, plan: LoadPlan, view: MemoryView): FitResult {
  const rows = new Map<string, FleetRow>()
  const boxes = plan.targets.map(row => {
    const box = boxOfRow(row, view)
    rows.set(box.id, row)
    return box
  })
  const fit = fitOn(recipe, boxes, {
    boxCount: plan.boxCount,
    freed: box => {
      const row = rows.get(box.id)
      return row && stoppedBy(row, plan.head, view.grouping) ? freedOn(row, view, box) : undefined
    },
    inFlight: box => view.inFlight.get(box.id) ?? 0,
  })
  if (plan.targets.length >= plan.boxCount || fit.fits === false) return fit
  return { ...fit, fits: null, partial: true }
}

// ---- The whole picture, once ------------------------------------------------

export interface FleetMemory {
  // Every box, this one read from its own model list.
  rows: FleetRow[]
  grouping: FleetGrouping
  // This box as a row, whether or not a fleet names it. A standalone Spark
  // has no fleet row, and its own model list is all there is to say of it.
  self: FleetRow
  view: MemoryView
}

// The key this box goes by when no fleet names it.
const LOCAL_ID = 'local'

export function fleetMemory(input: {
  sparks: readonly FleetRow[]
  models: readonly InstalledModel[]
  recipes: readonly Recipe[]
  deployments: readonly FleetDeploymentView[]
  jobs: readonly Job[]
  system: SystemInfo | null
  // This box's own latest meter reading, the freshest figure there is.
  latest: Telemetry | null
}): FleetMemory {
  const rows = withLocalModels(input.sparks, input.models)
  const grouping = fleetUnits(rows, input.deployments)
  const own = rows.find(row => row.isSelf)
  const name = own?.displayName || input.system?.hostname || 'This Spark'
  const id = own?.nodeID || LOCAL_ID
  // Its own five-second meter first, then what the fleet last heard from it,
  // then what it said when the console loaded.
  const latest = input.latest
  const box: Box = latest && latest.memory_total > 0
    ? {
      id, name, totalBytes: latest.memory_total, availableBytes: latest.memory_available,
      gpuTotalBytes: latest.gpu_memory_total,
      gpuAvailableBytes: latest.gpu_memory_total > 0 ? latest.gpu_memory_free : undefined,
    }
    : own?.inventory ? boxFromInventory(id, name, own.inventory) : boxFromSystem(id, name, input.system)
  const self = own ?? withLocalModels([{
    nodeID: id, displayName: name, isSelf: true, role: '', consoleURL: '', installedModels: [],
    status: { word: '', dot: '' }, answering: true, power: { mode: '', failure: '' },
  }], input.models)[0]
  const base = { recipes: input.recipes, rows: rows.length > 0 ? rows : [self], grouping, self: box }
  const placements = [...new Map(input.deployments.map(item => [item.deployment_id, item])).values()]
  return { rows, grouping, self, view: { ...base, inFlight: inFlightLoads(input.jobs, id, placements, base) } }
}

// ---- Loads already on their way --------------------------------------------

// A job that will take memory before it is done: a start, or an install that
// starts the model once it has downloaded. A download-only install plans no
// container start, so it never counts.
export function loadingJob(job: Job): boolean {
  if (terminal(job.state)) return false
  if (job.kind === 'start') return true
  return job.kind === 'install' && job.steps.some(step => stepOperation(step.operation) === 'start_container')
}

// The memory every load in flight will take, box by box. A load shows up in a
// box's free memory only once its weights are in, so until its job is done
// what it will take is counted against the box. Each job counts once, even
// when it is both a local job and the job of a placement.
export function inFlightLoads(
  jobs: readonly Job[],
  selfID: string,
  placements: readonly FleetDeploymentView[],
  view: Omit<MemoryView, 'inFlight'>,
): Map<string, number> {
  const loads = new Map<string, number>()
  const counted = new Set<string>()
  const add = (id: string, bytes: number) => loads.set(id, (loads.get(id) ?? 0) + bytes)
  const recipeOf = (id: string) => view.recipes.find(recipe => recipe.id === id)
  const rowOf = (id: string) => view.rows.find(row => row.nodeID === id)
  const empty: MemoryView = { ...view, inFlight: new Map() }
  for (const job of jobs) {
    if (!loadingJob(job)) continue
    const recipe = recipeOf(job.recipe_id)
    if (!recipe) continue
    counted.add(job.id)
    add(selfID, needOnBox(recipe, view.self, splitOf(recipe, view.grouping, selfID)).bytes)
  }
  for (const placement of placements) {
    const job = placement.job
    if (!job || !loadingJob(job)) continue
    const recipe = recipeOf(placement.recipe_id)
    if (!recipe) continue
    // A placement whose job is also this box's own local job has already
    // been charged here, but not on the other boxes it loads onto.
    const local = counted.has(job.id)
    const width = Math.max(placement.topology_count || 0, placement.nodes.length)
    for (const node of placement.nodes) {
      if (local && node.node_id === selfID) continue
      const row = rowOf(node.node_id)
      const box = row ? boxOfRow(row, empty) : { id: node.node_id, name: node.node_id }
      add(node.node_id, needOnBox(recipe, box, width).bytes)
    }
  }
  return loads
}
