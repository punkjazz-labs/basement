import { describe, expect, it } from 'vitest'
import type { InstalledModel } from './api'
import { ABLIT_RECIPE_ID, STOCK_GLM_RECIPE_ID } from './modelVariants'
import { DS, GLM, LOCAL_MODELS, PLACEMENTS, sixBoxes, viewOf } from './testFleet'
import { localModelOf, stopRoute, unitAnswering } from './unitActions'

// The unit drawer's Stop takes the routes the Models page takes for the same
// unit: this Spark's own door for a unit it is part of, the placement for any
// other. Nothing new is asked of the manager.
describe('stopping a unit from its drawer', () => {
  const view = viewOf(sixBoxes())
  const glm = view.findUnit('dep_glm_tp4')!
  const ds = view.findUnit('dep_ds_tp2')!

  it('stops a unit this Spark is part of through its own door', () => {
    expect(stopRoute(glm, LOCAL_MODELS)).toEqual({ where: 'local', path: `/api/v1/models/${GLM}/stop` })
  })

  it('stops any other unit through the placement that joins its boxes', () => {
    expect(stopRoute(ds, LOCAL_MODELS)).toEqual({
      where: 'fleet', path: '/api/v1/fleet/deployments/dep_ds_tp2/stop', deploymentID: 'dep_ds_tp2',
    })
    expect(ds.recipeID).toBe(DS)
  })

  it('refuses a placement the controller can no longer read', () => {
    const stale = viewOf(sixBoxes(), { placements: [PLACEMENTS[0], { ...PLACEMENTS[1], stale: true }] })
    expect(stopRoute(stale.findUnit('dep_ds_tp2')!, LOCAL_MODELS).where).toBe('none')
  })

  // The two GLM variants share one unit, so the one this Spark serves is the
  // one stopped.
  it('stops the variant this Spark is serving', () => {
    const models: InstalledModel[] = [
      { recipe_id: STOCK_GLM_RECIPE_ID, recipe_version: 1, status: 'stopped', active: false, updated_at: '' },
      { recipe_id: ABLIT_RECIPE_ID, recipe_version: 1, status: 'ready', active: true, updated_at: '' },
    ]
    expect(localModelOf(glm, models)?.recipe_id).toBe(ABLIT_RECIPE_ID)
    expect(localModelOf(ds, models)).toBeUndefined()
  })

  it('offers Stop while any box still serves the model', () => {
    expect(unitAnswering(ds)).toBe(true)
    const idle = viewOf(sixBoxes({
      'edgexpert-41b8': { installedModels: [], serving: undefined },
      'edgexpert-5e12': { installedModels: [], serving: undefined },
    }))
    expect(unitAnswering(idle.findUnit('dep_ds_tp2')!)).toBe(false)
  })
})
