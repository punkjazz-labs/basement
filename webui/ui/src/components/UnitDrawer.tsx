import { useState, useSyncExternalStore } from 'react'
import { formatBytes, type InstalledModel, type Job, type Recipe } from '../api'
import { noticeBox } from '../confirm'
import type { FleetView } from '../fleetView'
import { Mark } from '../mark'
import { LOCAL_MACHINE, machineSeries, subscribeTelemetry, telemetryRevision } from '../telemetry'
import { localModelOf, stopRoute, stopUnit, unitAnswering } from '../unitActions'
import { unitMemory, unitPlacement, unitServing, type FleetUnit } from '../units'
import { HealthDot } from './FleetHealth'
import { MemoryFree } from './Memory'
import { MetricChart } from './Sparkline'

// One unit: a model split across boxes, which stands or falls as one. Its
// speed, its memory, its boxes head first with one memory figure each (the
// PlanetScale topology), and the Stop and Open the Models page has always
// offered for it. A member opens its own box drawer.

// Speed is read where the runtime runs: this Spark reads its own model's
// counters every five seconds. A unit this Spark is not part of reports no
// speed in any heartbeat, so its chart says where the speed can be read.
export function unitSpeed(unit: FleetUnit, models: readonly InstalledModel[]): {
  points: number[]
  stamps: number[]
  measured?: number
  live: boolean
} {
  const local = localModelOf(unit, models)
  if (!local) return { points: [], stamps: [], live: false }
  const held = machineSeries(LOCAL_MACHINE)
  const runs = held.latest?.active_model?.recipe_id === local.recipe_id
  return {
    points: runs ? held.series.tps : [],
    stamps: runs ? held.series.at : [],
    measured: local.tokens_per_second,
    live: runs && held.series.tps.length > 0,
  }
}

export function UnitDrawerBody({ view, unit, recipes, models, onOpenBox, openPlayground, openGenerate, openDeployment, openFleetDeployment, refresh }: {
  view: FleetView
  unit: FleetUnit
  recipes: readonly Recipe[]
  models: readonly InstalledModel[]
  onOpenBox: (nodeID: string) => void
  openPlayground: () => void
  openGenerate: () => void
  openDeployment: (jobID: string) => void
  openFleetDeployment: (deploymentID: string, job: Job) => void
  refresh: () => void
}) {
  useSyncExternalStore(subscribeTelemetry, telemetryRevision, telemetryRevision)
  const [stopping, setStopping] = useState(false)
  const recipe = recipes.find(item => item.id === unit.recipeID)
  const name = recipe?.display_name ?? unit.recipeID
  const status = view.unitStatusOf(unit)
  const serving = unitServing(unit)
  const speed = unitSpeed(unit, models)
  const memory = unitMemory(unit, row => view.headroomOf(row).box)
  const head = unit.members.find(member => member.head) ?? unit.members[0]
  const local = localModelOf(unit, models)
  const isMedia = Boolean(recipe?.media_generation)
  const route = stopRoute(unit, models)
  const latest = speed.points.length > 0 ? speed.points[speed.points.length - 1] : undefined
  const tightest = unit.members
    .map(member => view.healthOf(member.row.nodeID))
    .filter(box => box !== undefined && box.loadsRefused)

  const stop = async () => {
    if (stopping) return
    setStopping(true)
    try {
      await stopUnit(unit, models, { openDeployment, openFleetDeployment, refresh })
    } catch (problem) {
      noticeBox('That did not work', problem instanceof Error ? problem.message : String(problem))
    } finally {
      setStopping(false)
    }
  }

  return (
    <div className="dr">
      <header className="dr-head">
        <Mark recipe={recipe} recipeIDs={[unit.recipeID]} name={unit.recipeID} size={30} />
        <div className="dr-title">
          <h2>{name}</h2>
          <p className="dr-role">
            {unitPlacement(unit)} · <span className={`health ${status.tone}`}>{status.word}</span>
          </p>
          <p className="dr-beat">{serving ? 'serving on every box' : 'not serving on every box'}</p>
        </div>
      </header>

      <div className="dr-facts3">
        <div className="fcard">
          <span className="l">Speed</span>
          <span className="v">
            {latest !== undefined ? latest.toFixed(1) : speed.measured ? speed.measured.toFixed(1) : 'n/a'}
            {!isMedia && (latest !== undefined || speed.measured) ? <small> tok/s</small> : null}
          </span>
          <span className="s">{latest !== undefined ? 'now, on this Spark' : speed.measured ? 'measured on this Spark' : 'read on its head'}</span>
        </div>
        <div className="fcard">
          <span className="l">Memory free</span>
          <span className="v">{memory.reported > 0 ? formatBytes(memory.free) : 'n/a'}</span>
          <span className="s">
            {memory.reported > 0 ? `of ${formatBytes(memory.total)} across ${memory.reported} boxes` : 'not reported yet'}
          </span>
        </div>
        <div className={`fcard ${tightest.length > 0 ? 'tight' : ''}`}>
          <span className="l">Loads</span>
          <span className="v">{tightest.length > 0 ? 'Refused' : 'Allowed'}</span>
          <span className="s">
            {tightest.length > 0
              ? `${tightest.map(box => box?.name).join(', ')} under the reserve`
              : 'every box above its reserve'}
          </span>
        </div>
      </div>

      <section className="dr-chart" aria-label="Throughput">
        <MetricChart
          points={speed.points}
          stamps={speed.stamps}
          title="Throughput"
          unit="tok/s"
          format={value => value.toFixed(1)}
          tone="live"
          empty={local
            ? 'No tokens counted yet. The line starts once the model answers a request.'
            : `Speed is read on the box this console runs on. ${head ? `Open ${head.row.displayName} for this unit's live speed.` : ''}`}
        />
      </section>

      <section className="dr-members" aria-label="Boxes in this unit">
        <h3>Boxes</h3>
        <ol className="topology">
          {unit.members.map(member => (
            <li key={member.row.nodeID} className={member.head ? 'head' : 'member'}>
              <button type="button" onClick={() => onOpenBox(member.row.nodeID)}>
                <HealthDot box={view.healthOf(member.row.nodeID)} />
                <span className="nm">{member.row.displayName}</span>
                <span className="role-tag">{member.head ? 'head' : 'member'}</span>
                <MemoryFree headroom={view.headroomOf(member.row)} size="mini" />
                <svg className="chev" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="M9 6l6 6-6 6" /></svg>
              </button>
            </li>
          ))}
          {unit.members.length < unit.size && (
            <li className="member missing">{unit.size - unit.members.length} more not in the fleet now</li>
          )}
        </ol>
      </section>

      <footer className="dr-actions">
        {unitAnswering(unit) && (
          <button
            type="button"
            className="ghost"
            disabled={stopping || route.where === 'none'}
            title={route.where === 'none' ? route.reason : undefined}
            onClick={stop}
          >
            {stopping ? 'Stopping' : 'Stop'}
          </button>
        )}
        {serving && (local ? (
          <button type="button" className="primary" onClick={isMedia ? openGenerate : openPlayground}>
            {isMedia ? 'Generate' : 'Open'}
          </button>
        ) : head?.row.consoleURL ? (
          <button
            type="button"
            className="primary"
            onClick={() => window.open(head.row.consoleURL, '_blank', 'noopener,noreferrer')}
          >
            Open on {head.row.displayName}
          </button>
        ) : null)}
      </footer>
    </div>
  )
}
