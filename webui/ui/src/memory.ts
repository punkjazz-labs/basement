// Pure math for the memory-fit calculator: whether a set of models fits
// together in the machine's unified memory at chosen context lengths. No DOM,
// no fetch, no React — this module is the single reusable source of truth for
// the console and, later, the public site. Every returned figure is marked
// `estimate: true`; nothing here is a measurement.

import type { Recipe } from './api'

export interface FitBreakdown {
  weights: number
  kv: number
  overhead: number
  total: number
  estimate: true
}

export interface UnknownFit {
  unknown: true
  estimate: true
}

export interface PerRecipeSettings {
  contextTokens: number
  seats?: number
}

export type RecipeFit = { recipeId: string } & (FitBreakdown | UnknownFit)

export interface FleetFit {
  perRecipe: RecipeFit[]
  total: number
  headroom: number
  estimate: true
}

// fitBytes computes one recipe's memory footprint at a given context length
// and concurrent-sequence count. Returns UnknownFit when the recipe has no
// memory_model block — that recipe has no estimate yet, not a zero one.
export function fitBytes(recipe: Recipe, contextTokens: number, seats = 1): FitBreakdown | UnknownFit {
  const model = recipe.memory_model
  if (!model) return { unknown: true, estimate: true }
  const kv = model.kv_bytes_per_token * contextTokens * seats
  const weights = model.weights_bytes
  const overhead = model.runtime_overhead_bytes
  return { weights, kv, overhead, total: weights + kv + overhead, estimate: true }
}

// memoryBudgetBytes derives the fleet-wide memory budget from the machine's
// total memory minus a safety reserve. Recipes do not yet declare a
// dedicated memory-budget reserve, so the most conservative of each selected
// recipe's existing reserve fields (per_node_memory_reserve_bytes for
// memory, safety_margin_bytes for disk) stands in as that reserve; taking
// the max across selections means adding a model can only hold the budget
// steady or shrink it, never quietly grow it.
export function memoryBudgetBytes(systemMemoryTotalBytes: number, recipes: Recipe[]): number {
  const reserve = recipes.reduce(
    (max, recipe) =>
      Math.max(max, recipe.requirements.per_node_memory_reserve_bytes, recipe.requirements.safety_margin_bytes),
    0,
  )
  return systemMemoryTotalBytes - reserve
}

// fleetFit totals the per-recipe fits against a budget. Recipes without a
// memory_model are reported as unknown and excluded from the total, so an
// unqualified recipe never silently reads as free.
export function fleetFit(recipes: Recipe[], perRecipeSettings: PerRecipeSettings[], budgetBytes: number): FleetFit {
  const perRecipe: RecipeFit[] = recipes.map((recipe, index) => {
    const settings = perRecipeSettings[index]
    const fit = fitBytes(recipe, settings?.contextTokens ?? 0, settings?.seats ?? 1)
    return { recipeId: recipe.id, ...fit }
  })
  const total = perRecipe.reduce((sum, fit) => sum + ('unknown' in fit ? 0 : fit.total), 0)
  return { perRecipe, total, headroom: budgetBytes - total, estimate: true }
}
