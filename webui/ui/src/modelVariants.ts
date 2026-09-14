import type { Recipe } from './api'

export const ABLIT_RECIPE_ID = 'glm53-flash-exl3-ablit-2s'
export const STOCK_GLM_RECIPE_ID = 'glm53-flash-exl3-2s'

export const isAblitRecipe = (recipe: Recipe): boolean => recipe.id === ABLIT_RECIPE_ID

export const stockGLM = (recipes: readonly Recipe[]): Recipe | undefined =>
  recipes.find(recipe => recipe.id === STOCK_GLM_RECIPE_ID)

export function ablitAction(recipe: Recipe, installed: boolean, active: boolean, _stockAvailable: boolean): 'Download' | 'Enable' | 'Disable' | undefined {
  if (!isAblitRecipe(recipe)) return undefined
  if (active) return 'Disable'
  return installed ? 'Enable' : 'Download'
}
