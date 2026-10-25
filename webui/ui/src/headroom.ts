// Memory headroom, box by box. The owner's rule is plain: a model is never
// loaded or swapped into memory on a box that has no room for it, and two
// loads never run side by side unless each box can hold both. Downloading is
// not loading, so nothing here ever stands in the way of a download.
//
// Pure: no DOM, no fetch, no React. The views read one answer from here and
// show it the same way everywhere a box appears.
//
// Memory is never pooled. A model split across N boxes needs its share on
// every one of them, and each box answers for itself, exactly as the
// manager's own guard does (internal/resourceguard). The figures below are
// estimates, and they are built never to be looser than that guard: where
// the guard would refuse, this module refuses first.

import { formatBytes, type NodeInventory, type Recipe, type SystemInfo } from './api'
import { fitBytes } from './memory'
import { pinnedContext } from './roles'

const GB = 1_000_000_000

// What every box keeps free when no recipe in view asks for more. Most
// recipes in the shipped catalog declare 12 GB as their own
// per_node_memory_reserve_bytes, so a box with less than this free could not
// start most of them even before their own claim is counted. A recipe that
// declares more raises the bar for itself; nothing lowers it.
export const DEFAULT_RESERVE_BYTES = 12 * GB

// Below this a box has nothing left for its own system. That is a failure in
// progress rather than a warning, and it is the one memory state drawn red.
export const FULL_BYTES = 2 * GB

export type HeadroomState = 'ok' | 'tight' | 'full' | 'unknown'

// One machine, as far as memory goes. Absent figures are ones the box has not
// reported, which is not the same as zero.
export interface Box {
  id: string
  name: string
  totalBytes?: number
  availableBytes?: number
  // The device pool an engine sizes its share from, and what of it is free.
  // GB10 reports the one unified pool for both, so an unread GPU total falls
  // back to totalBytes. The manager's guard checks the free GPU figure as well
  // as the free system figure, so this module does too wherever it is read.
  gpuTotalBytes?: number
  gpuAvailableBytes?: number
}

const reported = (value?: number): value is number => typeof value === 'number' && value > 0

export const boxFromInventory = (id: string, name: string, inventory?: NodeInventory): Box => ({
  id,
  name,
  totalBytes: inventory?.memory_total_bytes,
  availableBytes: inventory?.memory_available_bytes,
  gpuTotalBytes: inventory?.gpu_memory_total_bytes,
  gpuAvailableBytes: inventory?.gpu_memory_free_bytes,
})

export const boxFromSystem = (id: string, name: string, system: SystemInfo | null): Box => ({
  id,
  name,
  totalBytes: system?.memory_total_bytes,
  availableBytes: system?.memory_available_bytes,
})

// The memory a box keeps free with this recipe in view: the larger of what
// the recipe declares for itself and the console's own floor.
export const reserveFor = (recipe?: Recipe): number =>
  Math.max(recipe?.requirements.per_node_memory_reserve_bytes ?? 0, DEFAULT_RESERVE_BYTES)

export interface BoxHeadroom {
  box: Box
  state: HeadroomState
  reserve: number
  // For the bar: how much of the box is in use, and where the reserve starts,
  // both as a share of the whole. Zero while the box has reported nothing.
  usedFraction: number
  reserveFraction: number
}

export function boxHeadroom(box: Box, recipe?: Recipe): BoxHeadroom {
  const reserve = reserveFor(recipe)
  const total = box.totalBytes
  const available = box.availableBytes
  // Zero free is what a failed read reports, not a box with nothing left, so
  // it is drawn as unread. fitOn still refuses a load against it.
  if (!reported(total) || !reported(available)) {
    return { box, state: 'unknown', reserve, usedFraction: 0, reserveFraction: 0 }
  }
  const state: HeadroomState = available < FULL_BYTES ? 'full' : available < reserve ? 'tight' : 'ok'
  return {
    box,
    state,
    reserve,
    usedFraction: Math.min(Math.max((total - available) / total, 0), 1),
    reserveFraction: Math.min(reserve / total, 1),
  }
}

// The box with the least memory free, among the boxes that reported any.
export function tightestBox(headrooms: readonly BoxHeadroom[]): BoxHeadroom | undefined {
  let tightest: BoxHeadroom | undefined
  for (const headroom of headrooms) {
    if (headroom.state === 'unknown') continue
    if (!tightest || (headroom.box.availableBytes ?? 0) < (tightest.box.availableBytes ?? 0)) tightest = headroom
  }
  return tightest
}

// ---- What a model needs on one box -------------------------------------------

// Where a need came from. A memory model is the recipe's own qualified
// estimate; a runtime share is the slice of the device the engine claims up
// front, which is what the manager's guard charges; a download size is the
// last resort, for a recipe that declares neither.
export type NeedBasis = 'memory-model' | 'runtime-share' | 'download-size'

export interface MemoryNeed {
  bytes: number
  basis: NeedBasis
  estimate: true
}

// The share of the device vLLM or SGLang is told to take, read from the
// recipe's own decimal. Any other runtime takes a byte count instead, which
// the memory model already states.
export function memoryShare(recipe: Recipe): number | undefined {
  const kind = recipe.runtime.kind
  const raw = kind === 'vllm'
    ? recipe.service.vllm?.gpu_memory_utilization
    : kind === 'sglang' ? recipe.service.sglang?.mem_fraction_static : undefined
  if (raw === undefined || raw === null || String(raw).trim() === '') return undefined
  const share = Number(raw)
  return Number.isFinite(share) && share > 0 && share < 1 ? share : undefined
}

// What the whole model needs, before it is split: the memory model at the
// context the recipe pins, or the download itself when there is none.
export function modelNeedBytes(recipe: Recipe): { bytes: number; basis: NeedBasis } {
  const fit = fitBytes(recipe, pinnedContext(recipe))
  if (!('unknown' in fit)) return { bytes: fit.total, basis: 'memory-model' }
  return { bytes: recipe.artifact_bytes + (recipe.memory_model?.runtime_overhead_bytes ?? 0), basis: 'download-size' }
}

// What one box has to hold of this model when it is split across boxCount
// boxes. The even split of the model's own need is the floor; an engine that
// claims a share of the device takes that share on every box whatever the
// split, and the larger of the two is what the box must find free.
export function needOnBox(recipe: Recipe, box: Box, boxCount = recipe.topology.spark_count): MemoryNeed {
  const whole = modelNeedBytes(recipe)
  const split = Math.ceil(whole.bytes / Math.max(1, boxCount))
  const share = memoryShare(recipe)
  const pool = reported(box.gpuTotalBytes) ? box.gpuTotalBytes : box.totalBytes
  const claim = share !== undefined && reported(pool) ? Math.ceil(pool * share) : 0
  return claim > split
    ? { bytes: claim, basis: 'runtime-share', estimate: true }
    : { bytes: split, basis: whole.basis, estimate: true }
}

// ---- Whether a model fits ------------------------------------------------------

// A model that stops on a box before the new one starts. Its memory comes
// back, so it counts as free for the one that replaces it.
export interface Freed {
  bytes: number
  name: string
}

export interface FitOptions {
  // How many boxes share the model. Defaults to the recipe's own topology.
  boxCount?: number
  // The model that has to stop on each box first, if any.
  freed?: (box: Box) => Freed | undefined
  // Memory already promised to another load on each box that has not shown
  // up in its free memory yet.
  inFlight?: (box: Box) => number
}

export interface BoxFit {
  box: Box
  need: MemoryNeed
  reserve: number
  freed: number
  freedName: string
  inFlight: number
  // What is free once the stopping model is gone and the loads in flight
  // have landed. Absent while the box has reported nothing.
  after?: number
  // The same for the GPU pool, where the box reports it.
  gpuAfter?: number
  // The box itself is smaller than the recipe allows, whatever is free.
  belowMinimum: boolean
  // null is a box that has not reported its memory: nothing can be said.
  fits: boolean | null
}

export interface FitResult {
  fits: boolean | null
  boxes: BoxFit[]
  // The box that says no, when one does. A refusal names one box, the one
  // furthest short.
  blocking?: BoxFit
  // The sentence a dead button carries. Empty unless fits is false.
  reason: string
  // Only some of the boxes the model needs could be checked, so a yes cannot
  // be given: the answer is a refusal or nothing.
  partial?: boolean
}

export function fitOn(recipe: Recipe, boxes: readonly Box[], options: FitOptions = {}): FitResult {
  const boxCount = options.boxCount ?? recipe.topology.spark_count
  const reserve = reserveFor(recipe)
  const minimum = recipe.requirements.per_node_minimum_memory_bytes
  const checks: BoxFit[] = boxes.map(box => {
    const need = needOnBox(recipe, box, boxCount)
    const stopping = options.freed?.(box)
    const inFlight = Math.max(options.inFlight?.(box) ?? 0, 0)
    const total = box.totalBytes
    const available = box.availableBytes
    if (!reported(total) || typeof available !== 'number' || available < 0) {
      return { box, need, reserve, freed: 0, freedName: '', inFlight, belowMinimum: false, fits: null }
    }
    // A stopping model cannot give back more than the box has in use.
    const freed = Math.min(Math.max(stopping?.bytes ?? 0, 0), Math.max(total - available, 0))
    const after = Math.min(total, available + freed) - inFlight
    // The guard also wants the model's whole claim free in the GPU pool,
    // without the reserve, which belongs to the system.
    const gpuTotal = box.gpuTotalBytes
    const gpuFree = box.gpuAvailableBytes
    const gpuAfter = reported(gpuTotal) && typeof gpuFree === 'number' && gpuFree >= 0
      ? Math.min(gpuTotal, gpuFree + Math.min(Math.max(stopping?.bytes ?? 0, 0), Math.max(gpuTotal - gpuFree, 0))) - inFlight
      : undefined
    const belowMinimum = minimum > 0 && total < minimum
    return {
      box, need, reserve, freed, freedName: freed > 0 ? stopping?.name ?? '' : '', inFlight, after, gpuAfter,
      belowMinimum,
      fits: !belowMinimum && after - need.bytes >= reserve && (gpuAfter === undefined || gpuAfter >= need.bytes),
    }
  })
  const refused = checks.filter(check => check.fits === false)
  if (refused.length > 0) {
    const shortOf = (check: BoxFit) => (check.belowMinimum ? Infinity : Math.max(
      check.need.bytes + check.reserve - (check.after ?? 0),
      check.gpuAfter === undefined ? -Infinity : check.need.bytes - check.gpuAfter,
    ))
    const blocking = refused.reduce((worst, check) => (shortOf(check) > shortOf(worst) ? check : worst))
    return { fits: false, boxes: checks, blocking, reason: refusal(recipe, blocking) }
  }
  const unknown = checks.length === 0 || checks.some(check => check.fits === null)
  return { fits: unknown ? null : true, boxes: checks, reason: '' }
}

// The one sentence a refused load carries: which box, what it has free, and
// what this model needs there. The figure needed includes the reserve, so it
// is the free memory the box would have to show, the same figure the
// manager's own refusal names.
function refusal(recipe: Recipe, check: BoxFit): string {
  const name = check.box.name
  if (check.belowMinimum) {
    return `${name} has ${formatBytes(check.box.totalBytes)} of memory; this model needs ${
      formatBytes(recipe.requirements.per_node_minimum_memory_bytes)}`
  }
  const needs = `needs ~${formatBytes(check.need.bytes + check.reserve)}`
  // The system pool has room and only the GPU pool is short.
  if (check.gpuAfter !== undefined && check.gpuAfter < check.need.bytes &&
    (check.after ?? 0) - check.need.bytes >= check.reserve) {
    return `Not enough GPU memory on ${name}: ${formatBytes(Math.max(check.gpuAfter, 0))} free, needs ~${
      formatBytes(check.need.bytes)}`
  }
  if (check.freed > 0) {
    return `Not enough memory on ${name}: ${formatBytes(Math.max(check.after ?? 0, 0))} free once ${
      check.freedName || 'the current model'} stops, ${needs}`
  }
  if (check.inFlight > 0) {
    return `Not enough memory on ${name}: ${formatBytes(check.box.availableBytes)} free and ${
      formatBytes(check.inFlight)} already loading, ${needs}`
  }
  return `Not enough memory on ${name}: ${formatBytes(check.box.availableBytes)} free, ${needs}`
}

// The short word a Fits cell shows, and the sentence behind it.
export interface FitWord {
  word: string
  tone: 'ok' | 'warn' | 'unknown'
  title: string
}

export function fitWord(fit: FitResult): FitWord {
  if (fit.fits === true) {
    const first = fit.boxes[0]
    const where = fit.boxes.length === 1 ? first.box.name : `${fit.boxes.length} boxes`
    const after = first?.freedName ? ` once ${first.freedName} stops` : ''
    return { word: 'Fits', tone: 'ok', title: `Fits in memory on ${where}${after} (estimate)` }
  }
  if (fit.fits === false) return { word: 'No room', tone: 'warn', title: fit.reason }
  if (fit.partial) {
    return { word: 'n/a', tone: 'unknown', title: 'Only some of the boxes this model needs can be checked' }
  }
  return { word: 'n/a', tone: 'unknown', title: 'Memory not reported yet' }
}
