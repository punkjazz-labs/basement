import { useSyncExternalStore } from 'react'
import { formatBytes, type Recipe } from '../api'
import type { FleetRow } from '../fleetModels'
import { boxHeadroom, type Box } from '../headroom'
import { HeadroomBar, freeText } from '../headroomBar'
import {
  isHeartbeatKey, machineSeries, subscribeTelemetry, telemetryRevision,
  type MachineSeries, type MonitorMachine,
} from '../telemetry'
import {
  HEALTH_WORD, memberRole, unitHealth, unitPlacement, type FleetGrouping, type UnitMember,
} from '../units'

// The live meters, one section per Spark, grouped by the units the boxes form.
// This view draws history; it does not collect it (telemetry.ts does, from
// the polls App already runs), so the page opens with the lines already on
// the charts. Which Sparks it draws is the fleet's own list, so a machine the
// Sparks page shows is never missing here.
//
// This box reads its own meters, richest of all. Every other box in the fleet
// is read from its heartbeat, which carries its memory, its disk and its GPU
// meters; the history behind its sparklines is kept here, one point for each
// new heartbeat.

// What a Spark this console can neither ask nor hear from says instead of a
// chart: a box added by address that never joined the fleet sends this
// console no heartbeat, and it holds no key for it either.
const NO_METERS = 'Telemetry unavailable from this console'

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
function MachineSection({ machine, held, recipes, row, member }: {
  machine: MonitorMachine
  held: MachineSeries
  recipes: Recipe[]
  row?: FleetRow
  member?: UnitMember
}) {
  // A Spark this console has a way in to: itself, a Spark it holds a key for,
  // and a fleet box that sends a heartbeat. Any other machine gets a section
  // and the honest line above.
  const readable = machine.key !== ''
  const fromHeartbeat = isHeartbeatKey(machine.key)
  const telemetry = held.latest
  // A heartbeat carries no runtime meters, so a box read that way names what
  // it serves from the model list in the same heartbeat.
  const activeID = fromHeartbeat ? row?.serving?.recipe_id : telemetry?.active_model?.recipe_id
  const servingName = activeID
    ? recipes.find(recipe => recipe.id === activeID)?.display_name ?? activeID
    : ''
  // A Spark that has stopped answering keeps every sample it sent, and says
  // so in one quiet line rather than by emptying its charts.
  const quiet = telemetry !== null && !held.answering
  // Nothing here states a fact this console has not read. A machine that has
  // sent no sample is not idle and is not serving: it is waiting, which is
  // what its dot says and what its body says, and its head says nothing.
  // A fleet box whose heartbeat went quiet is a warning, as it is on the
  // Sparks page and in its unit's health; a peer this console polls and that
  // stopped answering keeps the failed dot it has always had.
  const dot = !readable ? '' : quiet ? (fromHeartbeat ? 'warn' : 'fail')
    : activeID ? 'on' : telemetry === null ? 'busy' : ''
  const line = !readable || telemetry === null
    ? ''
    : quiet
      ? 'Disconnected · Last recorded samples'
      : activeID
        ? `${servingName}`
        : 'No model running'
  const series = held.series
  const latest = (values: number[]) => (values.length ? values[values.length - 1] : undefined)
  const box: Box | undefined = telemetry
    ? { id: machine.id, name: machine.name, totalBytes: telemetry.memory_total, availableBytes: telemetry.memory_available }
    : undefined
  const headroom = box ? boxHeadroom(box) : undefined
  return (
    <section className="mon-machine">
      <div className="mon-head">
        <span className={`sdot ${dot}`} aria-hidden="true" />
        <h2>{machine.name}</h2>
        {member && <span className="role-tag">{memberRole(member)}</span>}
        {line !== '' && <span className="muted">{line}</span>}
        {headroom && headroom.state !== 'unknown' && (
          <span className="mon-room">
            <HeadroomBar headroom={headroom} />
            <span>{freeText(headroom)}</span>
          </span>
        )}
      </div>
      {!readable ? (
        <div className="empty">{NO_METERS}</div>
      ) : telemetry === null ? (
        <div className="empty">Waiting for telemetry…</div>
      ) : (
        <div className="tiles">
          {telemetry.active_model && (
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
          {/* GB10 has one unified pool, which the heartbeat reports as both
              memory and GPU memory. A box read from its heartbeat shows it
              once, as memory. */}
          {!fromHeartbeat && telemetry.gpu_memory_total > 0 && (
            <Tile
              label="GPU memory free"
              value={formatBytes(telemetry.gpu_memory_free)}
              points={series.gpuFree}
              max={telemetry.gpu_memory_total}
            />
          )}
          {telemetry.memory_total > 0 && (
            <Tile
              label={fromHeartbeat ? 'Memory free' : 'System RAM free'}
              value={formatBytes(telemetry.memory_available)}
              points={series.memFree}
              max={telemetry.memory_total}
            />
          )}
          <Tile label="Disk free" value={formatBytes(telemetry.storage_available)} />
        </div>
      )}
    </section>
  )
}

export default function Monitor({ machines, recipes, sparks = [], grouping }: {
  machines: MonitorMachine[]
  recipes: Recipe[]
  // The fleet rows and the units they form. Both are absent on a console that
  // leads no fleet, which draws its machines in one plain list.
  sparks?: FleetRow[]
  grouping?: FleetGrouping
}) {
  // Every sample this console has collected since it opened, whichever tab
  // was in front while they arrived.
  useSyncExternalStore(subscribeTelemetry, telemetryRevision, telemetryRevision)
  const rows = new Map(sparks.map(spark => [spark.nodeID, spark]))
  const section = (machine: MonitorMachine, member?: UnitMember) => (
    <MachineSection
      key={machine.id}
      machine={machine}
      held={machineSeries(machine.key)}
      recipes={recipes}
      row={rows.get(machine.id)}
      member={member}
    />
  )
  const units = grouping?.units ?? []
  if (units.length === 0) {
    return <div className="stack mon">{machines.map(machine => section(machine))}</div>
  }
  const byID = new Map(machines.map(machine => [machine.id, machine]))
  const inUnit = new Set(units.flatMap(unit => unit.members.map(member => member.row.nodeID)))
  const independent = machines.filter(machine => !inUnit.has(machine.id))
  const nameOf = (id: string) => recipes.find(recipe => recipe.id === id)?.display_name ?? id
  return (
    <div className="stack mon">
      {units.map(unit => {
        const health = unitHealth(unit)
        return (
          <section className="mon-group" key={unit.key} aria-label={nameOf(unit.recipeID)}>
            <header className="mon-group-head">
              <h2>{nameOf(unit.recipeID)}</h2>
              <span className="faint">{unitPlacement(unit)}</span>
              <span className={`health ${health}`}>{HEALTH_WORD[health]}</span>
            </header>
            {unit.members.map(member => {
              const machine = byID.get(member.row.nodeID)
              return machine ? section(machine, member) : null
            })}
          </section>
        )
      })}
      {independent.length > 0 && (
        <section className="mon-group" aria-label="Independent">
          <header className="mon-group-head">
            <h2>Independent</h2>
            <span className="faint">{independent.length === 1 ? '1 box' : `${independent.length} boxes`}</span>
          </header>
          {independent.map(machine => section(machine))}
        </section>
      )}
    </div>
  )
}
