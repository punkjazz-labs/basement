import { describe, expect, it } from 'vitest'
import type { Recipe } from './api'
import {
  ABLIT_RECIPE_ID, STOCK_GLM_RECIPE_ID, ablitAction, glmActionRecipeID,
  canonicalModelRecipeIDs, stockGLM, variantTransitioning, visibleModelRecipes,
} from './modelVariants'

const recipe = (id: string): Recipe => ({ id, artifacts: [] } as unknown as Recipe)

describe('GLM ablation model projection', () => {
  it('keeps download separate from enabling the ablation', () => {
    expect(ablitAction(false, false)).toBe('Download')
    expect(ablitAction(true, false)).toBe('Enable')
  })

  it('uses disable while ablit is active', () => {
    expect(ablitAction(true, true)).toBe('Disable')
  })

  it('keeps one canonical GLM row while retaining both recipes', () => {
    const recipes = [recipe(ABLIT_RECIPE_ID), recipe(STOCK_GLM_RECIPE_ID)]
    expect(visibleModelRecipes(recipes).map(item => item.id)).toEqual([STOCK_GLM_RECIPE_ID])
    expect(stockGLM(recipes)?.id).toBe(STOCK_GLM_RECIPE_ID)
    expect(canonicalModelRecipeIDs([ABLIT_RECIPE_ID])).toEqual(new Set([ABLIT_RECIPE_ID, STOCK_GLM_RECIPE_ID]))
  })

  it('uses active ablit, including a remote-only GLM row, as the action target', () => {
    expect(glmActionRecipeID({ stockLocal: false, ablitLocal: true, ablitLocalActive: true, ablitRemotePreferred: false, ablitWorking: false }))
      .toBe(ABLIT_RECIPE_ID)
    expect(glmActionRecipeID({ stockLocal: false, ablitLocal: false, ablitLocalActive: false, ablitRemotePreferred: true, ablitWorking: false }))
      .toBe(ABLIT_RECIPE_ID)
  })

  it('does not retarget local stock tools to remote ablit work', () => {
    expect(glmActionRecipeID({ stockLocal: true, ablitLocal: false, ablitLocalActive: false, ablitRemotePreferred: true, ablitWorking: true }))
      .toBe(STOCK_GLM_RECIPE_ID)
  })

  it('keeps the active remote stock owner when another Spark only holds inactive ablit', () => {
    expect(glmActionRecipeID({ stockLocal: false, ablitLocal: false, ablitLocalActive: false, ablitRemotePreferred: false, ablitWorking: false }))
      .toBe(STOCK_GLM_RECIPE_ID)
  })

  it('keeps an installed local ablit variant selected when stock is absent', () => {
    expect(glmActionRecipeID({ stockLocal: false, ablitLocal: true, ablitLocalActive: false, ablitRemotePreferred: false, ablitWorking: false }))
      .toBe(ABLIT_RECIPE_ID)
  })

  it('locks an owner during a reported transition before a job is visible', () => {
    expect(variantTransitioning(['ready', 'recovering'])).toBe(true)
    expect(variantTransitioning(['ready', undefined])).toBe(false)
  })
})
