import { useSyncExternalStore } from 'react'
import { formatBytes, type Recipe } from '../api'
import {
  machineSeries, subscribeTelemetry, telemetryRevision,
  type MachineSeries, type MonitorMachine,
} from '../telemetry'

// The live meters, one section per Spark. This view draws history; it does not
// collect it (telemetry.ts does, from the polls App already runs), so the page
// opens with the lines already on the charts. Which Sparks it draws is the
// fleet's own list, so a machine the Sparks page shows is never missing here.

// What a Spark this console cannot ask says instead of a chart. The fleet
// plane carries no telemetry read today, so a Spark this console holds no key
// for reports nothing to it. That read is work of its own; until it exists,
// the section states what it cannot do rather than drawing a machine as idle.
const NO_METERS = 'This console cannot read the meters of this Spark yet.'

function Sparkline({ points, max }: { points: number[]; max?: number }) {
  if (points.length < 2) return <svg role="img" aria-label="Collecting samples" />
  const top = max ?? Math.max(...points, 1e-9)
  const coords = points.map((value, index) => {
    const x = (index / (points.length - 1)) * 100
    const y = 34 - Math.min(value / top, 1) * 30
    return `${x.toFixed(1)},${y.toFixed(1)}`
  })
  const latest = points[points.length - 1]
  return (
    <svg viewBox="0 0 100 36" preserveAspectRatio="none" role="img" aria-label={`Latest ${latest.toFixed(1)}`}>
      <title>{`Latest ${latest.toFixed(1)}`}</title>
      <polygon className="spark-fill" points={`0,36 ${coords.join(' ')} 100,36`} />
      <polyline points={coords.join(' ')} />
    </svg>
  )
}

function Tile({ label, value, unit, points, max }: {
  label: string
  value: string
  unit?: string
  points?: number[]
  max?: number
}) {
  return (
    <div className="tile">
      <span className="label">{label}</span>
      <span className="value">
        {value} {unit && <small>{unit}</small>}
      </span>
      {points && <Sparkline points={points} max={max} />}
    </div>
  )
}

// What one Spark's section says over its tiles: which machine this is, what it
// serves, and whether it is still answering. The dot is the one the fleet
// strip uses, and it reads the same way here: lit while a model serves, quiet
// while the machine is idle, failed while the machine says nothing.
function MachineSection({ machine, held, recipes }: {
  machine: MonitorMachine
  held: MachineSeries
  recipes: Recipe[]
}) {
  // A Spark this console has a way in to: itself, and a Spark it holds a key
  // for. Every other machine gets a section and the honest line above.
  const readable = machine.key !== ''
  const telemetry = held.latest
  const active = telemetry?.active_model
  const servingName = active
    ? recipes.find(recipe => recipe.id === active.recipe_id)?.display_name ?? active.recipe_id
    : ''
  // A Spark that has stopped answering keeps every sample it sent, and says
  // so in one quiet line rather than by emptying its charts.
  const quiet = telemetry !== null && !held.answering
  // Nothing here states a fact this console has not read. A machine that has
  // sent no sample is not idle and is not serving: it is waiting, which is
  // what its dot says and what its body says, and its head says nothing.
  const dot = !readable ? '' : quiet ? 'fail' : active ? 'on' : telemetry === null ? 'busy' : ''
  const line = !readable || telemetry === null
    ? ''
    : quiet
      ? 'No answer now. The last samples stay on screen.'
      : active
        ? `${servingName} · sampled every few seconds`
        : 'No model serving. System metrics only.'
  const series = held.series
  const latest = (values: number[]) => (values.length ? values[values.length - 1] : undefined)
  return (
    <section className="mon-machine">
      <div className="mon-head">
        <span className={`sdot ${dot}`} aria-hidden="true" />
        <h2>{machine.name}</h2>
        {line !== '' && <span className="muted">{line}</span>}
      </div>
      {!readable ? (
        <div className="empty">{NO_METERS}</div>
      ) : telemetry === null ? (
        <div className="empty">Waiting for the first telemetry sample…</div>
      ) : (
        <div className="tiles">
          {active && (
            <>
              <Tile
                label="Generation speed"
                value={latest(series.tps)?.toFixed(1) ?? 'n/a'}
                unit="tok/s"
                points={series.tps}
              />
              <Tile
                label="Requests running"
                value={String(latest(series.running) ?? 0)}
                points={series.running}
              />
              <Tile
                label="Requests waiting"
                value={String(latest(series.waiting) ?? 0)}
                points={series.waiting}
              />
              <Tile
                label="KV cache used"
                value={latest(series.kv)?.toFixed(0) ?? '0'}
                unit="%"
                points={series.kv}
                max={100}
              />
            </>
          )}
          {telemetry.gpu_power_draw_watts > 0 && (
            <Tile
              label="Power draw"
              value={telemetry.gpu_power_draw_watts.toFixed(0)}
              unit="W"
              points={series.power}
            />
          )}
          {telemetry.gpu_temperature_c > 0 && (
            <Tile
              label="GPU temperature"
              value={String(telemetry.gpu_temperature_c)}
              unit="°C"
              points={series.temperature}
              max={100}
            />
          )}
          {telemetry.gpu_clock_mhz > 0 && (
            <Tile
              label="GPU clock"
              value={String(telemetry.gpu_clock_mhz)}
              unit="MHz"
              points={series.clock}
            />
          )}
          {telemetry.gpu_memory_total > 0 && (
            <Tile
              label="GPU memory free"
              value={formatBytes(telemetry.gpu_memory_free)}
              points={series.gpuFree}
              max={telemetry.gpu_memory_total}
            />
          )}
          {telemetry.memory_total > 0 && (
            <Tile label="System RAM free" value={formatBytes(telemetry.memory_available)} />
          )}
          <Tile label="Disk free" value={formatBytes(telemetry.storage_available)} />
        </div>
      )}
    </section>
  )
}

export default function Monitor({ machines, recipes }: { machines: MonitorMachine[]; recipes: Recipe[] }) {
  // Every sample this console has collected since it opened, whichever tab
  // was in front while they arrived.
  useSyncExternalStore(subscribeTelemetry, telemetryRevision, telemetryRevision)
  return (
    <div className="stack mon">
      {machines.map(machine => (
        <MachineSection
          key={machine.id}
          machine={machine}
          held={machineSeries(machine.key)}
          recipes={recipes}
        />
      ))}
    </div>
  )
}
