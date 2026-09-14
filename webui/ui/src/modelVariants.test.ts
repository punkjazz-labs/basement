import { describe, expect, it } from 'vitest'
import type { Recipe } from './api'
import { ABLIT_RECIPE_ID, STOCK_GLM_RECIPE_ID, ablitAction, stockGLM } from './modelVariants'

const recipe = (id: string): Recipe => ({ id, artifacts: [] } as unknown as Recipe)

describe('GLM ablation model actions', () => {
  it('keeps download separate from enabling the ablation', () => {
    expect(ablitAction(recipe(ABLIT_RECIPE_ID), false, false, true)).toBe('Download')
    expect(ablitAction(recipe(ABLIT_RECIPE_ID), true, false, true)).toBe('Enable')
  })

  it('only offers disable when stock GLM can be started', () => {
    expect(ablitAction(recipe(ABLIT_RECIPE_ID), true, true, true)).toBe('Disable')
    expect(ablitAction(recipe(ABLIT_RECIPE_ID), true, true, false)).toBe('Disable')
  })

  it('does not classify the stock recipe as an ablation action', () => {
    expect(ablitAction(recipe(STOCK_GLM_RECIPE_ID), true, true, true)).toBeUndefined()
    expect(stockGLM([recipe(ABLIT_RECIPE_ID), recipe(STOCK_GLM_RECIPE_ID)])?.id).toBe(STOCK_GLM_RECIPE_ID)
  })
})
