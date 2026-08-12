import { describe, expect, it } from 'vitest'
import type { Recipe, Role } from './api'
import { DEFAULT_ROLE, combinedFit, distinctModelsAfter, distinctRecipes, isValidRoleName, pinnedContext, roleRows } from './roles'

const role = (name: string, recipeID: string): Role => ({
  name,
  recipe_id: recipeID,
  created_at: '2026-08-01T00:00:00Z',
  updated_at: '2026-08-01T00:00:00Z',
})

const recipe = (id: string, memory?: { weights: number; kv: number; overhead: number }, context = 0): Recipe =>
  ({
    id,
    version: 1,
    display_name: id,
    publisher: 'test',
    trust: 'curated',
    verification: 'pinned',
    source: { url: '', revision: '' },
    topology: { spark_count: 1 },
    artifacts: [],
    requirements: {
      per_node_minimum_memory_bytes: 0,
      per_node_memory_reserve_bytes: 4,
      safety_margin_bytes: 0,
      secrets: [],
      required_licence_acceptance: false,
    },
    service: { internal_port: 8000, default_host_port: 8000, served_model_id: id, vllm: { max_model_len: context } },
    runtime: { start_timeout_minutes: 20 },
    memory_model: memory
      ? { weights_bytes: memory.weights, kv_bytes_per_token: memory.kv, runtime_overhead_bytes: memory.overhead }
      : undefined,
    artifact_bytes: 0,
    required_bytes: 0,
    revoked: false,
  }) as Recipe

describe('role rows', () => {
  it('always shows the named roles first, standard before the rest, then custom roles as added', () => {
    const rows = roleRows([role('code-review', 'a'), role('fast', 'b')])
    expect(rows.map(row => row.name)).toEqual([DEFAULT_ROLE, 'fast', 'reasoning', 'vision', 'code-review'])
    // A named role keeps its written label; a custom role is called what it
    // was named, never something invented for it.
    expect(rows[0].label).toBe('Standard model')
    expect(rows[1].label).toBe('Fast model')
    expect(rows[4].label).toBe('code-review')
  })
})

describe('swapping', () => {
  it('counts the models roles would point at, because one runs at a time', () => {
    const assigned = [role('fast', 'one'), role('reasoning', 'one')]
    // Both roles on the same model: nothing swaps.
    expect(distinctModelsAfter(assigned, 'vision', 'one')).toBe(1)
    // A second model behind any role is what makes models swap in and out.
    expect(distinctModelsAfter(assigned, 'vision', 'two')).toBe(2)
    // Moving the only other role back onto the same model removes the swap.
    expect(distinctModelsAfter([role('fast', 'one'), role('vision', 'two')], 'vision', 'one')).toBe(1)
  })
})

describe('role names', () => {
  it('accepts only what a model field can carry back as role/<name>', () => {
    for (const name of ['fast', 'code-review', 'gpt5', 'a']) expect(isValidRoleName(name)).toBe(true)
    for (const name of ['', 'Fast Model', '-fast', 'fast-', 'fast/er', 'fast_er', 'f'.repeat(33)]) {
      expect(isValidRoleName(name)).toBe(false)
    }
    // Casing and spacing are normalized rather than refused, so a typed name
    // is accepted in the form it will actually be stored under.
    expect(isValidRoleName('  Fast  ')).toBe(true)
  })
})

describe('combined memory fit', () => {
  it('counts a model once however many roles point at it', () => {
    const one = recipe('one', { weights: 10, kv: 0, overhead: 2 })
    expect(distinctRecipes([one, one])).toHaveLength(1)
    expect(combinedFit([one, one], 100).bytes).toBe(12)
  })

  it('warns only when the models behind roles are estimated above the budget', () => {
    const small = recipe('small', { weights: 10, kv: 0, overhead: 2 })
    const large = recipe('large', { weights: 90, kv: 0, overhead: 4 })
    // Budget is memory minus the most conservative declared reserve (4 here).
    expect(combinedFit([small], 100).overBudget).toBe(false)
    expect(combinedFit([small, large], 100).overBudget).toBe(true)
  })

  it('reports unknown rather than free when a recipe carries no memory model', () => {
    const known = recipe('known', { weights: 10, kv: 0, overhead: 2 })
    const unqualified = recipe('unqualified')
    const fit = combinedFit([known, unqualified], 100)
    expect(fit.bytes).toBeNull()
    expect(fit.overBudget).toBe(false)
  })

  it('sizes the KV cache at the context length the recipe itself pins', () => {
    const pinned = recipe('pinned', { weights: 10, kv: 2, overhead: 0 }, 5)
    expect(pinnedContext(pinned)).toBe(5)
    expect(combinedFit([pinned], 100).bytes).toBe(20)
  })
})
