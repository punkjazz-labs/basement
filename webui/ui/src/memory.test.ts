import { describe, expect, it } from 'vitest'
import type { Recipe } from './api'
import { fitBytes, fleetFit, memoryBudgetBytes } from './memory'

// Minimal-but-complete Recipe fixture; only requirements and memory_model
// vary between tests, everything else is filler that satisfies the type.
function makeRecipe(overrides: {
  id: string
  memoryModel?: Recipe['memory_model']
  memoryReserveBytes?: number
  safetyMarginBytes?: number
}): Recipe {
  return {
    id: overrides.id,
    version: 1,
    display_name: overrides.id,
    publisher: 'test',
    trust: 'basement-candidate',
    verification: 'candidate',
    source: { url: 'https://huggingface.co/test/test', revision: '0'.repeat(40) },
    topology: { spark_count: 1 },
    artifacts: [],
    requirements: {
      per_node_minimum_memory_bytes: 1,
      per_node_memory_reserve_bytes: overrides.memoryReserveBytes ?? 0,
      safety_margin_bytes: overrides.safetyMarginBytes ?? 0,
      secrets: [],
      required_licence_acceptance: false,
    },
    service: { internal_port: 8000, default_host_port: 8000, served_model_id: 'test' },
    runtime: { start_timeout_minutes: 0 },
    memory_model: overrides.memoryModel,
    artifact_bytes: 0,
    required_bytes: 0,
  }
}

const qualified = makeRecipe({
  id: 'qualified-a',
  memoryModel: { weights_bytes: 1000, kv_bytes_per_token: 10, runtime_overhead_bytes: 100 },
})

const unqualified = makeRecipe({ id: 'unqualified-a' })

describe('fitBytes', () => {
  it('computes weights, kv, overhead, and total from the memory_model block', () => {
    const fit = fitBytes(qualified, 50, 1)
    expect(fit).toEqual({ weights: 1000, kv: 500, overhead: 100, total: 1600, estimate: true })
  })

  it('scales kv linearly with seats', () => {
    const oneSeat = fitBytes(qualified, 50, 1)
    const fourSeats = fitBytes(qualified, 50, 4)
    if ('unknown' in oneSeat || 'unknown' in fourSeats) throw new Error('expected known fits')
    expect(fourSeats.kv).toBe(oneSeat.kv * 4)
    expect(fourSeats.total).toBe(oneSeat.total + oneSeat.kv * 3)
  })

  it('defaults seats to 1 when omitted', () => {
    const withDefault = fitBytes(qualified, 50)
    const explicit = fitBytes(qualified, 50, 1)
    expect(withDefault).toEqual(explicit)
  })

  it('reports unknown for a recipe without a memory_model block', () => {
    expect(fitBytes(unqualified, 4096, 1)).toEqual({ unknown: true, estimate: true })
  })

  it('is monotonically non-decreasing as context tokens increase', () => {
    const contextLengths = [0, 1, 128, 4096, 32768, 131072]
    const totals = contextLengths.map(tokens => {
      const fit = fitBytes(qualified, tokens, 1)
      if ('unknown' in fit) throw new Error('expected a known fit')
      return fit.total
    })
    for (let i = 1; i < totals.length; i += 1) {
      expect(totals[i]).toBeGreaterThanOrEqual(totals[i - 1])
    }
  })

  it('is monotonically non-decreasing as seats increase, for fixed context', () => {
    const seatCounts = [1, 2, 3, 8]
    const totals = seatCounts.map(seats => {
      const fit = fitBytes(qualified, 4096, seats)
      if ('unknown' in fit) throw new Error('expected a known fit')
      return fit.total
    })
    for (let i = 1; i < totals.length; i += 1) {
      expect(totals[i]).toBeGreaterThanOrEqual(totals[i - 1])
    }
  })
})

describe('memoryBudgetBytes', () => {
  it('subtracts nothing when there are no selected recipes', () => {
    expect(memoryBudgetBytes(1_000_000, [])).toBe(1_000_000)
  })

  it('reserves the max of memory-reserve and safety-margin fields across selected recipes', () => {
    const a = makeRecipe({ id: 'a', memoryReserveBytes: 200, safetyMarginBytes: 50 })
    const b = makeRecipe({ id: 'b', memoryReserveBytes: 100, safetyMarginBytes: 300 })
    expect(memoryBudgetBytes(1_000_000, [a, b])).toBe(1_000_000 - 300)
  })
})

describe('fleetFit', () => {
  it('returns an empty, zero-total fit for zero models', () => {
    const fit = fleetFit([], [], 1_000_000)
    expect(fit).toEqual({ perRecipe: [], total: 0, headroom: 1_000_000, estimate: true })
  })

  it('excludes a model without a memory_model block from totals and flags it unknown', () => {
    const fit = fleetFit([qualified, unqualified], [{ contextTokens: 50, seats: 1 }, { contextTokens: 4096 }], 10_000)
    expect(fit.perRecipe).toEqual([
      { recipeId: 'qualified-a', weights: 1000, kv: 500, overhead: 100, total: 1600, estimate: true },
      { recipeId: 'unqualified-a', unknown: true, estimate: true },
    ])
    expect(fit.total).toBe(1600)
    expect(fit.headroom).toBe(10_000 - 1600)
  })

  it('sums totals across several qualified models', () => {
    const second = makeRecipe({
      id: 'qualified-b',
      memoryModel: { weights_bytes: 2000, kv_bytes_per_token: 20, runtime_overhead_bytes: 200 },
    })
    const fit = fleetFit(
      [qualified, second],
      [{ contextTokens: 100, seats: 1 }, { contextTokens: 100, seats: 2 }],
      100_000,
    )
    // qualified: 1000 + 10*100 + 100 = 2100
    // second: 2000 + 20*100*2 + 200 = 6200
    expect(fit.total).toBe(2100 + 6200)
    expect(fit.headroom).toBe(100_000 - (2100 + 6200))
  })

  it('reports negative headroom when the fleet exceeds the budget', () => {
    const fit = fleetFit([qualified], [{ contextTokens: 100, seats: 1 }], 1000)
    expect(fit.total).toBe(1000 + 10 * 100 + 100)
    expect(fit.headroom).toBeLessThan(0)
    expect(fit.headroom).toBe(1000 - fit.total)
  })

  it('treats a missing per-recipe settings entry as zero context and one seat', () => {
    const fit = fleetFit([qualified], [], 100_000)
    const [only] = fit.perRecipe
    if ('unknown' in only) throw new Error('expected a known fit')
    expect(only.kv).toBe(0)
    expect(only.total).toBe(1000 + 100)
  })
})
