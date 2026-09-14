import type { Recipe } from './api'

export const ABLIT_RECIPE_ID = 'glm53-flash-exl3-ablit-2s'
export const STOCK_GLM_RECIPE_ID = 'glm53-flash-exl3-2s'

export type AbliterationAction = 'Download' | 'Enable' | 'Disable'

export const isAblitRecipe = (recipe: Recipe): boolean => recipe.id === ABLIT_RECIPE_ID

export const stockGLM = (recipes: readonly Recipe[]): Recipe | undefined =>
  recipes.find(recipe => recipe.id === STOCK_GLM_RECIPE_ID)

// The catalog owns two lifecycle recipes, but the console owns one GLM row.
// Keep the second recipe out of the table while retaining it as the exact
// target for an ablit action or a row that is currently serving ablit.
export const visibleModelRecipes = (recipes: readonly Recipe[]): Recipe[] =>
  recipes.filter(recipe => !isAblitRecipe(recipe))

export const canonicalModelRecipeIDs = (ids: Iterable<string>): Set<string> => {
  const canonical = new Set(ids)
  if (canonical.has(ABLIT_RECIPE_ID)) canonical.add(STOCK_GLM_RECIPE_ID)
  return canonical
}

export function ablitAction(installed: boolean, active: boolean): AbliterationAction {
  if (active) return 'Disable'
  return installed ? 'Enable' : 'Download'
}

export interface GLMVariantTarget {
  // A local stock installation remains this console's target even if a
  // different Spark is serving ablit. Remote state must never retarget local
  // tools.
  stockLocal: boolean
  ablitLocal: boolean
  ablitLocalActive: boolean
  ablitRemotePreferred: boolean
  ablitWorking: boolean
}

export function glmActionRecipeID(target: GLMVariantTarget): string {
  if (target.ablitLocalActive || (target.ablitLocal && !target.stockLocal)) return ABLIT_RECIPE_ID
  if (!target.stockLocal && (target.ablitRemotePreferred || target.ablitWorking)) return ABLIT_RECIPE_ID
  return STOCK_GLM_RECIPE_ID
}

const TRANSITIONING = new Set(['recovering', 'starting', 'switching', 'stopping'])

export const variantTransitioning = (statuses: Iterable<string | undefined>): boolean =>
  [...statuses].some(status => status !== undefined && TRANSITIONING.has(status))
