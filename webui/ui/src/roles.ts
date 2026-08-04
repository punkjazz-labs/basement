// Pure logic for roles: which rows the Roles view shows, what a role name may
// be, and whether the models assigned to roles can be held in memory together.
// No DOM, no fetch, no React, so the rules can be tested on their own.

import type { Recipe, Role } from './api'
import { fitBytes, memoryBudgetBytes } from './memory'

export interface RoleDefinition {
  name: string
  label: string
  use: string
}

// The role an app gets without setting anything up. While it has no model
// assigned it follows whatever model is serving, so it answers from the
// first request; assigned, it behaves like every other role.
export const DEFAULT_ROLE = 'standard'

// The roles basement names itself. They always have a row, assigned or not,
// because they are the names apps are told to point at; a custom role exists
// only once it has been given a model. The manager lists assignments only, so
// these four names live here, in the console, and standard is first because it
// is the one an app uses when it does not care which model answers.
export const BUILTIN_ROLES: RoleDefinition[] = [
  { name: DEFAULT_ROLE, label: 'Standard model', use: "Your apps' default. Follows whatever is serving until you assign it." },
  { name: 'fast', label: 'Fast model', use: 'Quick, low-latency requests' },
  { name: 'reasoning', label: 'Reasoning model', use: 'Multi-step problems, planning, code' },
  { name: 'vision', label: 'Vision model', use: 'Requests that include images or documents' },
]

// The same rule the manager applies (store.NormalizeRoleName): the name has to
// survive a round trip through an OpenAI model field as "role/<name>".
export function normalizeRoleName(value: string): string {
  return value.trim().toLowerCase()
}

export function isValidRoleName(value: string): boolean {
  return /^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/.test(normalizeRoleName(value)) && normalizeRoleName(value).length <= 32
}

// roleRows is every row the table shows: the three named roles first, always,
// then custom roles in the order the manager returned them.
export function roleRows(assigned: Role[]): RoleDefinition[] {
  const builtin = new Set(BUILTIN_ROLES.map(role => role.name))
  const custom = assigned
    .filter(role => !builtin.has(role.name))
    .map(role => ({ name: role.name, label: role.name, use: 'Custom role' }))
  return [...BUILTIN_ROLES, ...custom]
}

// distinctModelsAfter counts the models roles would point at if roleName were
// assigned recipeID. This Spark serves one model at a time, so more than one
// is exactly when models start swapping in and out on demand.
export function distinctModelsAfter(assigned: Role[], roleName: string, recipeID: string): number {
  const models = new Set(assigned.filter(role => role.name !== roleName).map(role => role.recipe_id))
  models.add(recipeID)
  return models.size
}

// The context length the recipe itself pins, which is what its KV cache is
// sized for. 0 when the recipe pins none, which leaves fitBytes counting
// weights and overhead only.
export const pinnedContext = (recipe: Recipe): number =>
  recipe.service.vllm?.max_model_len ?? recipe.service.sglang?.context_length ?? 0

// One entry per distinct model: two roles pointing at the same model load it
// once, so counting it twice would invent memory pressure that does not exist.
export function distinctRecipes(recipes: Recipe[]): Recipe[] {
  const seen = new Set<string>()
  return recipes.filter(recipe => (seen.has(recipe.id) ? false : (seen.add(recipe.id), true)))
}

export interface CombinedFit {
  // Null when any model in the set has no memory model of its own: that
  // recipe has no estimate yet, and treating it as free would invent headroom.
  bytes: number | null
  budgetBytes: number
  overBudget: boolean
}

// combinedFit estimates what a set of models needs in memory when they are
// loaded together, at the context length each recipe pins. Every figure is an
// estimate from the recipe's declared memory model, never a measurement.
export function combinedFit(recipes: Recipe[], memoryTotalBytes: number): CombinedFit {
  const models = distinctRecipes(recipes)
  const budgetBytes = memoryBudgetBytes(memoryTotalBytes, models)
  let bytes = 0
  for (const recipe of models) {
    const fit = fitBytes(recipe, pinnedContext(recipe))
    if ('unknown' in fit) return { bytes: null, budgetBytes, overBudget: false }
    bytes += fit.total
  }
  return { bytes, budgetBytes, overBudget: memoryTotalBytes > 0 && bytes > budgetBytes }
}
