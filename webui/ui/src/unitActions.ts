import { api, idempotency, type InstalledModel, type Job } from './api'
import { ACTION_REFUSAL, deploymentActionPath } from './fleetModels'
import { variantFamily } from './loadFit'
import { servesModel, type FleetUnit } from './units'

// Stop and Open for a whole unit, from its drawer. They take the routes the
// Models page takes for the same unit (sendAction there): a unit this Spark
// is part of is stopped through this Spark's own door, which is where its
// job runs, and any other unit through the placement that joins its boxes.
// Nothing new is asked of the manager.

// The model this Spark holds for the unit, if it is part of it. The two GLM
// variants share one unit, so either one counts.
export function localModelOf(unit: FleetUnit, models: readonly InstalledModel[]): InstalledModel | undefined {
  if (!unit.members.some(member => member.row.isSelf)) return undefined
  const family = variantFamily(unit.recipeID)
  const held = models.filter(model => family.includes(model.recipe_id))
  return held.find(model => model.active) ?? held[0]
}

// Whether any member still serves the unit's model, which is when Stop has
// something to stop.
export const unitAnswering = (unit: FleetUnit): boolean =>
  unit.members.some(member => variantFamily(unit.recipeID).some(id => servesModel(member.row, id)))

export type StopRoute =
  | { where: 'local'; path: string }
  | { where: 'fleet'; path: string; deploymentID: string }
  | { where: 'none'; reason: string }

export function stopRoute(unit: FleetUnit, models: readonly InstalledModel[]): StopRoute {
  const local = localModelOf(unit, models)
  if (local) return { where: 'local', path: `/api/v1/models/${encodeURIComponent(local.recipe_id)}/stop` }
  if (unit.deployment.stale) return { where: 'none', reason: ACTION_REFUSAL['not-answering'] }
  return { where: 'fleet', path: deploymentActionPath(unit.key, 'stop'), deploymentID: unit.key }
}

export async function stopUnit(unit: FleetUnit, models: readonly InstalledModel[], follow: {
  openDeployment: (jobID: string) => void
  openFleetDeployment: (deploymentID: string, job: Job) => void
  refresh: () => void
}): Promise<void> {
  const route = stopRoute(unit, models)
  if (route.where === 'none') throw new Error(route.reason)
  const result = await api<{ job: Job }>(route.path, { method: 'POST', headers: idempotency(), body: '{}' })
  if (route.where === 'local') follow.openDeployment(result.job.id)
  else follow.openFleetDeployment(route.deploymentID, result.job)
  follow.refresh()
}
