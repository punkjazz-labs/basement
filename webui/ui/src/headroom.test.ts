import { describe, expect, it } from 'vitest'
import type { NodeInventory, Recipe } from './api'
import {
  boxFromInventory, boxHeadroom, fitMargin, fitOn, fitWord, memoryShare, modelNeedBytes, needOnBox, reserveFor,
  tightestBox, DEFAULT_RESERVE_BYTES, FULL_BYTES, TIGHT_MARGIN_BYTES, type Box,
} from './headroom'

const GB = 1_000_000_000

// A recipe with only what the headroom module reads set to something real.
function recipe(overrides: {
  id?: string
  kind?: string
  share?: string
  sparks?: number
  artifact?: number
  reserve?: number
  minimum?: number
  memoryModel?: Recipe['memory_model']
}): Recipe {
  const kind = overrides.kind ?? 'vllm'
  return {
    id: overrides.id ?? 'test-1s',
    version: 1,
    display_name: overrides.id ?? 'Test',
    publisher: 'test',
    trust: 'basement-candidate',
    verification: 'candidate',
    source: { url: 'https://example.invalid', revision: '0'.repeat(40) },
    topology: { spark_count: overrides.sparks ?? 1 },
    artifacts: [],
    requirements: {
      per_node_minimum_memory_bytes: overrides.minimum ?? 120 * GB,
      per_node_memory_reserve_bytes: overrides.reserve ?? 12 * GB,
      safety_margin_bytes: 20 * GB,
      secrets: [],
      required_licence_acceptance: false,
    },
    service: {
      internal_port: 8000,
      default_host_port: 8000,
      served_model_id: 'test',
      ...(kind === 'vllm' && overrides.share ? { vllm: { gpu_memory_utilization: overrides.share } } : {}),
      ...(kind === 'sglang' && overrides.share ? { sglang: { mem_fraction_static: overrides.share } } : {}),
      ...(kind === 'llamacpp' ? { llamacpp: { context_size: 8192 } } : {}),
    },
    runtime: { kind, start_timeout_minutes: 0 },
    memory_model: overrides.memoryModel,
    artifact_bytes: overrides.artifact ?? 20 * GB,
    required_bytes: 0,
    revoked: false,
  }
}

const box = (name: string, availableGB?: number, totalGB = 128.5): Box => ({
  id: `node-${name}`,
  name,
  totalBytes: totalGB * GB,
  availableBytes: availableGB === undefined ? undefined : availableGB * GB,
})

describe('the state of one box', () => {
  it('is ok with more than the reserve free', () => {
    expect(boxHeadroom(box('spark-f7e0', 40)).state).toBe('ok')
  })

  // Tight is a warning, drawn amber, and it starts below the reserve.
  it('is tight below the default reserve of 12 GB', () => {
    expect(DEFAULT_RESERVE_BYTES).toBe(12 * GB)
    expect(boxHeadroom(box('edgexpert-41b8', 9.6)).state).toBe('tight')
    expect(boxHeadroom(box('edgexpert-41b8', 12)).state).toBe('ok')
  })

  // A recipe in view can raise the bar for itself, never lower it.
  it('takes the larger of the recipe reserve and the default', () => {
    expect(reserveFor(recipe({ reserve: 16 * GB }))).toBe(16 * GB)
    expect(reserveFor(recipe({ reserve: 4 * GB }))).toBe(DEFAULT_RESERVE_BYTES)
    expect(reserveFor()).toBe(DEFAULT_RESERVE_BYTES)
    expect(boxHeadroom(box('spark-2b4f', 14), recipe({ reserve: 16 * GB })).state).toBe('tight')
  })

  // Red is for failures only: a box with almost nothing left is the one
  // memory state that qualifies.
  it('is full only when the box has almost nothing left', () => {
    expect(boxHeadroom(box('edgexpert-5e12', 1.5)).state).toBe('full')
    expect(boxHeadroom(box('edgexpert-5e12', FULL_BYTES / GB)).state).toBe('tight')
  })

  // A box that has not reported is not an empty box.
  it('says nothing about a box that has not reported', () => {
    expect(boxHeadroom(box('spark-9a07')).state).toBe('unknown')
    expect(boxHeadroom(boxFromInventory('node-x', 'spark-9a07', undefined)).state).toBe('unknown')
  })

  it('draws the bar from what is in use', () => {
    const headroom = boxHeadroom(box('spark-c3d1', 32, 128))
    expect(headroom.usedFraction).toBeCloseTo(0.75)
    expect(headroom.reserveFraction).toBeCloseTo(12 / 128)
  })

  it('finds the tightest box among those that reported', () => {
    const boxes = [box('spark-f7e0', 18.9), box('edgexpert-5e12', 7.2), box('spark-9a07')].map(item => boxHeadroom(item))
    expect(tightestBox(boxes)?.box.name).toBe('edgexpert-5e12')
    expect(tightestBox([boxHeadroom(box('spark-9a07'))])).toBeUndefined()
  })

  it('reads a heartbeat inventory', () => {
    const inventory = {
      hostname: 'edgexpert-41b8', product_name: 'EdgeXpert', dgx_spark: true,
      memory_total_bytes: 128 * GB, memory_available_bytes: 9 * GB,
      storage_total_bytes: 1900 * GB, storage_available_bytes: 1200 * GB, gpu_memory_total_bytes: 128 * GB,
    } satisfies NodeInventory
    expect(boxFromInventory('node-41b8', 'edgexpert-41b8', inventory)).toEqual({
      id: 'node-41b8', name: 'edgexpert-41b8', totalBytes: 128 * GB, availableBytes: 9 * GB, gpuTotalBytes: 128 * GB,
    })
  })
})

describe('what a model needs on one box', () => {
  // vLLM and SGLang take a share of the device up front. That share is what
  // the manager's guard charges, so it is a floor the console never goes under.
  it('reads the runtime share from the recipe', () => {
    expect(memoryShare(recipe({ share: '0.78' }))).toBe(0.78)
    expect(memoryShare(recipe({ kind: 'sglang', share: '0.85' }))).toBe(0.85)
    expect(memoryShare(recipe({ kind: 'llamacpp' }))).toBeUndefined()
    expect(memoryShare(recipe({ share: 'lots' }))).toBeUndefined()
  })

  it('charges the runtime share when it is larger than the download', () => {
    const need = needOnBox(recipe({ share: '0.5', artifact: 20 * GB }), box('spark-f7e0', 100, 100))
    expect(need.bytes).toBe(50 * GB)
    expect(need.basis).toBe('runtime-share')
    expect(need.estimate).toBe(true)
  })

  // A memory model is the recipe's own qualified estimate.
  it('uses the memory model when the recipe has one', () => {
    const qualified = recipe({
      kind: 'llamacpp',
      memoryModel: { weights_bytes: 29 * GB, kv_bytes_per_token: 0, runtime_overhead_bytes: 4 * GB },
    })
    expect(modelNeedBytes(qualified)).toEqual({ bytes: 33 * GB, basis: 'memory-model' })
    expect(needOnBox(qualified, box('spark-2b4f', 100)).basis).toBe('memory-model')
  })

  it('falls back to the download size and says so', () => {
    const bare = recipe({ kind: 'other', artifact: 40 * GB })
    expect(needOnBox(bare, box('spark-2b4f', 100))).toEqual({ bytes: 40 * GB, basis: 'download-size', estimate: true })
  })

  // Memory is never pooled: a TP-N model needs its own share on every box.
  it('splits the need evenly across the boxes of a unit', () => {
    const big = recipe({ kind: 'other', artifact: 160 * GB, sparks: 2 })
    expect(needOnBox(big, box('spark-2b4f', 100), 4).bytes).toBe(40 * GB)
    expect(needOnBox(big, box('spark-2b4f', 100)).bytes).toBe(80 * GB)
  })
})

describe('whether a model fits', () => {
  const small = recipe({ kind: 'other', artifact: 28 * GB })

  // The brief's own sentence, for the box the fixture shows tightest.
  it('refuses a box without room, in one plain sentence', () => {
    const fit = fitOn(small, [box('edgexpert-5e12', 7.2)])
    expect(fit.fits).toBe(false)
    expect(fit.reason).toBe('Not enough memory on edgexpert-5e12: 7.2 GB free, needs ~40.0 GB')
    expect(fitWord(fit)).toEqual({ word: 'No room', tone: 'warn', title: fit.reason })
  })

  // One gigabyte to spare above the reserve still loads: the rule says yes.
  // The word says how close it is.
  it('fits a box with the need and the reserve free, and says when it is tight', () => {
    const fit = fitOn(small, [box('spark-f7e0', 41)])
    expect(fit.fits).toBe(true)
    expect(fit.reason).toBe('')
    expect(fitWord(fit).word).toBe('Tight')
    expect(fitWord(fit).tone).toBe('tight')
    expect(fitWord(fit).title).toBe('Fits on spark-f7e0, with about 1.0 GB to spare above the reserve on spark-f7e0 (estimate)')
  })

  it('reads Fits once there is room to spare above the reserve', () => {
    const roomy = fitOn(small, [box('spark-f7e0', 40 + TIGHT_MARGIN_BYTES / GB)])
    expect(fitWord(roomy).word).toBe('Fits')
    const close = fitOn(small, [box('spark-f7e0', 40 + TIGHT_MARGIN_BYTES / GB - 0.1)])
    expect(fitWord(close).word).toBe('Tight')
  })

  // A unit is as tight as its tightest box.
  it('measures the margin on the box with the least to spare', () => {
    const fit = fitOn(small, [box('spark-2b4f', 90), box('spark-9a07', 43)])
    expect(fitMargin(fit)?.box.name).toBe('spark-9a07')
    expect((fitMargin(fit)?.bytes ?? 0) / GB).toBeCloseTo(3)
    expect(fitWord(fit).word).toBe('Tight')
    expect(fitMargin(fitOn(small, [box('edgexpert-5e12', 7.2)]))).toBeUndefined()
  })

  // Switching on the same box: what stops is counted as free for what starts.
  it('counts the memory of the model being stopped as freed', () => {
    const fit = fitOn(small, [box('spark-f7e0', 18.9)], {
      freed: () => ({ bytes: 100 * GB, name: 'GLM 5.3 Flash' }),
    })
    expect(fit.fits).toBe(true)
    expect((fit.boxes[0].after ?? 0) / GB).toBeCloseTo(118.9)
    expect(fitWord(fit).title).toContain('once GLM 5.3 Flash stops')
  })

  // A stopping model cannot give back more than the box has in use.
  it('never frees more than the box has in use', () => {
    const fit = fitOn(small, [box('spark-f7e0', 100, 128)], { freed: () => ({ bytes: 500 * GB, name: 'x' }) })
    expect(fit.boxes[0].after).toBe(128 * GB)
  })

  it('names what would still be short after the switch', () => {
    const huge = recipe({ share: '0.85' })
    const fit = fitOn(huge, [box('edgexpert-41b8', 9.6, 128.5)], {
      freed: () => ({ bytes: 100.2 * GB, name: 'DeepSeek V4 Flash 0731' }),
    })
    expect(fit.fits).toBe(false)
    expect(fit.reason).toBe('Not enough memory on edgexpert-41b8: 110 GB free once DeepSeek V4 Flash 0731 stops, needs ~121 GB')
  })

  // Every box answers for itself, and one box short refuses the whole unit.
  it('never pools memory across the boxes of a unit', () => {
    const split = recipe({ kind: 'other', artifact: 80 * GB, sparks: 2 })
    const fit = fitOn(split, [box('edgexpert-41b8', 200, 256), box('edgexpert-5e12', 30)], { boxCount: 2 })
    expect(fit.fits).toBe(false)
    expect(fit.blocking?.box.name).toBe('edgexpert-5e12')
  })

  // Two loads side by side need room for both: memory already promised to a
  // load in flight is not free yet.
  it('counts a load already in flight against the box', () => {
    const fit = fitOn(small, [box('spark-f7e0', 60)], { inFlight: () => 30 * GB })
    expect(fit.fits).toBe(false)
    expect(fit.reason).toBe('Not enough memory on spark-f7e0: 60.0 GB free and 30.0 GB already loading, needs ~40.0 GB')
  })

  // The manager refuses a box smaller than the recipe allows, so this does too.
  it('refuses a box smaller than the recipe allows', () => {
    const fit = fitOn(recipe({ kind: 'other', minimum: 120 * GB }), [box('spark-tiny', 60, 64)])
    expect(fit.fits).toBe(false)
    expect(fit.reason).toBe('spark-tiny has 64.0 GB of memory; this model needs 120 GB')
  })

  // Unknown is unknown: it is not a refusal and not a promise.
  it('says nothing for a box that has not reported', () => {
    const fit = fitOn(small, [box('spark-9a07')])
    expect(fit.fits).toBeNull()
    expect(fitWord(fit).word).toBe('n/a')
    expect(fitOn(small, []).fits).toBeNull()
  })

  // The manager's guard wants the claim free in the GPU pool too, without
  // the reserve. Where the box reports that pool, so does this.
  it('refuses a box whose GPU pool is short even when system memory is not', () => {
    const gpuShort: Box = { ...box('spark-f7e0', 100, 128), gpuTotalBytes: 128 * GB, gpuAvailableBytes: 20 * GB }
    const fit = fitOn(small, [gpuShort])
    expect(fit.fits).toBe(false)
    expect(fit.reason).toBe('Not enough GPU memory on spark-f7e0: 20.0 GB free, needs ~28.0 GB')
    const gpuRoomy: Box = { ...gpuShort, gpuAvailableBytes: 60 * GB }
    expect(fitOn(small, [gpuRoomy]).fits).toBe(true)
  })

  // Zero free is what a failed read reports. It is drawn as unread, never as
  // red, but nothing is loaded against it either.
  it('draws a zero reading as unread and still refuses a load against it', () => {
    const zero = box('spark-f7e0', 0)
    expect(boxHeadroom(zero).state).toBe('unknown')
    expect(fitOn(small, [zero]).fits).toBe(false)
  })

  it('lets a known refusal win over an unknown box', () => {
    expect(fitOn(small, [box('spark-9a07'), box('edgexpert-5e12', 7.2)]).fits).toBe(false)
  })
})
