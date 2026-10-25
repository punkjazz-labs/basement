import { useSyncExternalStore } from 'react'
import { formatBytes, type FleetDeploymentView, type InstalledModel, type Job, type Recipe } from '../api'
import { AttentionBanner, FleetHealthLine, HealthDot } from '../components/FleetHealth'
import { MemoryFree } from '../components/Memory'
import { MetricChart, Sparkline, windowText } from '../components/Sparkline'
import { relativeTime } from '../feed'
import { eventsRevision, jobEvents, latestEvents, seenEvents, subscribeEvents, type FleetEvent } from '../fleetEvents'
import { temperatureLight } from '../fleetHealth'
import type { FleetRow } from '../fleetModels'
import type { FleetView } from '../fleetView'
import { Mark } from '../mark'
import {
  LOCAL_MACHINE, isHeartbeatKey, machineSeries, subscribeTelemetry, telemetryRevision,
  type MachineSeries, type MonitorMachine,
} from '../telemetry'
import { unitPlacement, type FleetUnit } from '../units'

// The live meters, read the way Modal and Render read theirs: no wall of
// tiles. Each unit is one card with the one chart that matters for it (how
// fast its model answers) and a compact table of its boxes, where history is
// a column and not a chart per box. A box's full charts are one click away,
// in its drawer. Beside the cards, what happened lately, so a change in a
// line has its reason next to it.
//
// This view draws history; it does not collect it (telemetry.ts does, from
// the polls App already runs), so the page opens with the lines already
// drawn. Which boxes it draws is the fleet's own list, so a machine the Sparks
// page shows is never missing here.

// What a Spark this console can neither ask nor hear from says instead of
// figures: a box added by address that never joined the fleet sends this
// console no heartbeat, and it holds no key for it either.
const NO_METERS = 'Telemetry unavailable from this console'

const GB = 1_000_000_000
const EVENTS_SHOWN = 8

// One box in a card: the machine the meters are kept for, and the fleet's row
// for it where there is one.
interface Member {
  machine: MonitorMachine
  row?: FleetRow
  role: string
}

function MemberRow({ member, view, onOpen }: { member: Member; view: FleetView; onOpen: (id: string) => void }) {
  const { machine, row } = member
  const readable = machine.key !== ''
  const held: MachineSeries = machineSeries(machine.key)
  const telemetry = held.latest
  const quiet = telemetry !== null && !held.answering
  const health = row ? view.healthOf(row.nodeID) : undefined
  const temperature = telemetry?.gpu_temperature_c || (row ? view.temperatureOf(row) : undefined)
  const watts = telemetry?.gpu_power_draw_watts || (row ? view.powerOf(row) : undefined)
  const heat = temperatureLight(temperature)
  const span = windowText(held.series.at.slice(-held.series.power.length))
  const open = row ? () => onOpen(row.nodeID) : undefined
  // A machine that has sent no sample is not idle and is not serving: it is
  // waiting, and its row says so and nothing else.
  const dot = !readable ? '' : quiet ? (isHeartbeatKey(machine.key) ? 'warn' : 'fail') : telemetry === null ? 'busy' : ''
  return (
    <div
      className={`urow ${open ? 'link' : ''}`}
      role={open ? 'button' : undefined}
      tabIndex={open ? 0 : undefined}
      onClick={open}
      onKeyDown={open ? event => {
        if (event.key === 'Enter' || event.key === ' ') {
          event.preventDefault()
          open()
        }
      } : undefined}
    >
      <div className="u-box">
        {health && telemetry !== null ? <HealthDot box={health} /> : <span className={`sdot ${dot}`} aria-hidden="true" />}
        <span className="nm">{machine.name}</span>
        {member.role && <span className="role-tag">{member.role}</span>}
        {quiet && <span className="u-note">Disconnected · Last recorded samples</span>}
      </div>
      {!readable ? (
        <div className="u-span faint">{NO_METERS}</div>
      ) : telemetry === null ? (
        <div className="u-span faint">Waiting for telemetry…</div>
      ) : (
        <>
          <div className="u-trend">
            <Sparkline points={held.series.power} label={`GPU power${span ? `, ${span}` : ''}`} />
            <span className="n">{watts ? `${watts.toFixed(0)} W` : 'n/a'}</span>
          </div>
          <div className={`u-num ${heat}`}>{temperature ? `${temperature} °C` : 'n/a'}</div>
          <div className="u-mem">
            {row ? <MemoryFree headroom={view.headroomOf(row)} /> : <span className="num">{formatBytes(telemetry.memory_available)}</span>}
          </div>
        </>
      )}
      <span className="u-chev" aria-hidden="true">
        {open && <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="M9 6l6 6-6 6" /></svg>}
      </span>
    </div>
  )
}

// Every metric each box reports, for the owner who wants them all. One row
// per box and a sparkline per metric, so even this stays a table. Collapsed
// until asked for.
function AllMetrics({ members }: { members: Member[] }) {
  const readable = members.filter(member => member.machine.key !== '' && machineSeries(member.machine.key).latest)
  if (readable.length === 0) return null
  const local = members.find(member => member.machine.key === LOCAL_MACHINE)
  const runtime = local ? machineSeries(LOCAL_MACHINE).latest?.active_model?.runtime_metrics : undefined
  const last = (values: number[]) => (values.length ? values[values.length - 1] : undefined)
  return (
    <details className="allm">
      <summary>All metrics</summary>
      <div className="allm-table" role="table" aria-label="All metrics">
        <div className="allm-row head" role="row">
          <span role="columnheader">Box</span>
          <span role="columnheader">GPU power</span>
          <span role="columnheader">GPU temp</span>
          <span role="columnheader">GPU clock</span>
          <span role="columnheader">Memory free</span>
        </div>
        {readable.map(member => {
          const series = machineSeries(member.machine.key).series
          const cell = (points: number[], text: string, label: string) => (
            <span role="cell" className="allm-cell">
              <Sparkline points={points} label={label} />
              <span className="n">{text}</span>
            </span>
          )
          const power = last(series.power)
          const temperature = last(series.temperature)
          const clock = last(series.clock)
          const free = last(series.memFree)
          return (
            <div className="allm-row" role="row" key={member.machine.id}>
              <span role="cell" className="nm">{member.machine.name}</span>
              {cell(series.power, power ? `${power.toFixed(0)} W` : 'n/a', 'GPU power')}
              {cell(series.temperature, temperature ? `${temperature} °C` : 'n/a', 'GPU temperature')}
              {cell(series.clock, clock ? `${clock} MHz` : 'n/a', 'GPU clock')}
              {cell(series.memFree.map(value => value / GB), free ? formatBytes(free) : 'n/a', 'Memory free')}
            </div>
          )
        })}
      </div>
      {runtime && (
        <p className="allm-runtime">
          {[
            typeof runtime.requests_running === 'number' ? `Requests running ${runtime.requests_running}` : '',
            typeof runtime.requests_waiting === 'number' ? `Requests waiting ${runtime.requests_waiting}` : '',
            typeof runtime.kv_cache_usage === 'number' ? `KV cache used ${(runtime.kv_cache_usage * 100).toFixed(0)}%` : '',
          ].filter(Boolean).join(' · ')}
        </p>
      )}
    </details>
  )
}

// The one chart a card carries: how fast its model answers. Only the runtime
// on this Spark can be read for that, so a card for any other unit says where
// its speed can be read instead of drawing an empty frame.
function Throughput({ members, models, head }: { members: Member[]; models: readonly InstalledModel[]; head?: FleetRow }) {
  const local = members.some(member => member.machine.key === LOCAL_MACHINE)
  const held = machineSeries(LOCAL_MACHINE)
  // What this Spark serves, from its meter, or from its own model list while
  // the meter has not named it yet.
  const active = held.latest?.active_model?.recipe_id ??
    models.find(model => model.active && model.status === 'ready')?.recipe_id
  if (local && active) {
    const measured = models.find(model => model.recipe_id === active)?.tokens_per_second
    return (
      <MetricChart
        points={held.series.tps}
        stamps={held.series.at}
        title="Throughput"
        unit="tok/s"
        format={value => value.toFixed(1)}
        tone="live"
        empty={measured
          ? `No tokens counted yet. Measured speed ${measured.toFixed(1)} tok/s.`
          : 'No tokens counted yet. The line starts once the model answers a request.'}
      />
    )
  }
  if (!head || head.isSelf) return null
  return (
    <p className="ucard-note">
      Throughput is read on the box this console runs on.{' '}
      {head.consoleURL ? (
        <button type="button" className="linkish" onClick={() => window.open(head.consoleURL, '_blank', 'noopener,noreferrer')}>
          Open {head.displayName}
        </button>
      ) : null}{' '}
      for this unit's live speed.
    </p>
  )
}

function EventList({ events, now, onOpenBox, openDeployment, openFleetDeployment }: {
  events: FleetEvent[]
  now: number
  onOpenBox: (id: string) => void
  openDeployment: (jobID: string) => void
  openFleetDeployment: (deploymentID: string, job: Job) => void
}) {
  const open = (event: FleetEvent) => {
    if (event.deploymentID && event.job) openFleetDeployment(event.deploymentID, event.job)
    else if (event.jobID) openDeployment(event.jobID)
    else if (event.boxID) onOpenBox(event.boxID)
  }
  return (
    <aside className="events" aria-label="Events">
      <h2>Events</h2>
      {events.length === 0 ? (
        <p className="faint events-empty">Nothing has happened yet.</p>
      ) : (
        <ol>
          {events.map(event => (
            <li key={event.id}>
              <button type="button" onClick={() => open(event)}>
                <i className={`edot ${event.tone}`} aria-hidden="true" />
                <span className="ev-body">
                  <span className="ev-sub">{event.subject}</span>
                  <span className={`ev-text ${event.tone}`}>{event.text}</span>
                  <span className="ev-meta">
                    {[event.boxName && event.boxName !== event.subject ? event.boxName : '',
                      relativeTime(new Date(event.at).toISOString(), now)].filter(Boolean).join(' · ')}
                  </span>
                </span>
              </button>
            </li>
          ))}
        </ol>
      )}
    </aside>
  )
}

export default function Monitor({
  machines, recipes, view, models = [], jobs = [], deployments = [],
  onOpenBox = () => {}, onOpenUnit = () => {}, openDeployment = () => {}, openFleetDeployment = () => {},
}: {
  machines: MonitorMachine[]
  recipes: Recipe[]
  view: FleetView
  models?: readonly InstalledModel[]
  jobs?: readonly Job[]
  deployments?: readonly FleetDeploymentView[]
  onOpenBox?: (nodeID: string) => void
  onOpenUnit?: (deploymentID: string) => void
  openDeployment?: (jobID: string) => void
  openFleetDeployment?: (deploymentID: string, job: Job) => void
}) {
  // Every sample this console has collected since it opened, whichever tab
  // was in front while they arrived, and every change it has seen.
  useSyncExternalStore(subscribeTelemetry, telemetryRevision, telemetryRevision)
  useSyncExternalStore(subscribeEvents, eventsRevision, eventsRevision)
  const now = Date.now()
  const nameOf = (id: string) => recipes.find(recipe => recipe.id === id)?.display_name ?? id
  const byID = new Map(machines.map(machine => [machine.id, machine]))
  // This box is the machine whose meters are its own, whatever id the list
  // gave it; every other box is matched by the fleet's id for it.
  const own = machines.find(machine => machine.key === LOCAL_MACHINE)
  const covered = new Set<string>()
  const memberOf = (row: FleetRow, role: string): Member => {
    const machine = (row.isSelf ? own : undefined) ?? byID.get(row.nodeID) ??
      { id: row.nodeID, key: view.seriesKey(row), name: row.displayName }
    covered.add(machine.id)
    return { machine, row, role }
  }
  const unitCards = view.grouping.units.map(unit => ({
    unit,
    members: unit.members.map(member => memberOf(member.row, member.head ? 'head' : 'member')),
  }))
  const loose = view.unassigned.map(row => memberOf(row, row.serving ? nameOf(row.serving.recipe_id) : ''))
  // A machine the fleet holds no row for still has a row here: a Spark left
  // off this screen is a Spark the owner cannot see is there.
  for (const machine of machines) {
    if (!covered.has(machine.id)) loose.push({ machine, role: '' })
  }
  const events = latestEvents([
    jobEvents({
      jobs, deployments, nameOf,
      self: { id: view.self.nodeID, name: view.self.displayName },
      boxName: id => view.rowOf(id)?.displayName ?? '',
    }),
    seenEvents(),
  ], EVENTS_SHOWN)

  const card = (key: string, title: React.ReactNode, members: Member[], unit?: FleetUnit) => {
    const head = unit?.members.find(member => member.head)?.row
    const status = unit ? view.unitStatusOf(unit) : undefined
    return (
      <section className="ucard" key={key} aria-label={unit ? nameOf(unit.recipeID) : 'Unassigned'}>
        <header className="ucard-head">
          {title}
          {status && <span className={`health ${status.tone}`}>{status.word}</span>}
        </header>
        <Throughput members={members} models={models} head={head} />
        <div className="utable" role="table" aria-label="Boxes">
          <div className="urow head" role="row" aria-hidden="true">
            <span>Box</span><span>GPU power</span><span className="r">Temp</span><span>Memory free</span><span />
          </div>
          {members.map(member => <MemberRow key={member.machine.id} member={member} view={view} onOpen={onOpenBox} />)}
        </div>
        <AllMetrics members={members} />
      </section>
    )
  }

  return (
    <div className="stack mon">
      <FleetHealthLine summary={view.summary} boxes={view.health} onOpenBox={box => onOpenBox(box.id)} />
      <AttentionBanner banner={view.banner} onOpen={onOpenBox} />
      <div className="mon-cols">
        <div className="mon-main">
          {unitCards.map(({ unit, members }) => card(unit.key, (
            <>
              <Mark recipe={recipes.find(recipe => recipe.id === unit.recipeID)} recipeIDs={[unit.recipeID]} name={unit.recipeID} size={24} />
              <button type="button" className="ucard-title" onClick={() => onOpenUnit(unit.key)}>
                <strong>{nameOf(unit.recipeID)}</strong>
                <span>{unitPlacement(unit)}</span>
              </button>
            </>
          ), members, unit))}
          {loose.length > 0 && card('unassigned', (
            <div className="ucard-title static">
              <strong>{unitCards.length > 0 ? 'Unassigned' : 'Boxes'}</strong>
              <span>{loose.length === 1 ? '1 box' : `${loose.length} boxes`}</span>
            </div>
          ), loose)}
        </div>
        <EventList
          events={events}
          now={now}
          onOpenBox={onOpenBox}
          openDeployment={openDeployment}
          openFleetDeployment={openFleetDeployment}
        />
      </div>
    </div>
  )
}
