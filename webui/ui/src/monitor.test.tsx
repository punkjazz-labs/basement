import { beforeEach, describe, expect, it } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import type { Job, Recipe, Telemetry } from './api'
import Monitor from './views/Monitor'
import type { FleetRow } from './fleetModels'
import { resetEvents } from './fleetEvents'
import { buildFleetView } from './fleetView'
import { fleetMemory } from './loadFit'
import { LOCAL_MACHINE, heartbeatKey, recordSilence, recordTelemetry, resetTelemetry, type MonitorMachine } from './telemetry'
import { GB, LOCAL_MODELS, NOW, RECIPES, machinesOf, sixBoxes, viewOf } from './testFleet'

// The fleet in the lab: this machine, a Spark this console holds a key for,
// and two that joined by invitation and have no key here. The peers table
// holds one machine at most, so two unreadable Sparks is the ordinary shape
// of a fleet of four, and the two must never be drawn as one.
const MACHINES: MonitorMachine[] = [
  { id: 'node-lead', key: LOCAL_MACHINE, name: 'spark-f7e0' },
  { id: 'node-loft', key: 'peer-1', name: 'spark-a5d6' },
  { id: 'node-msi-a', key: '', name: 'edgexpert-1c47' },
  { id: 'node-msi-b', key: '', name: 'edgexpert-41b8' },
]

const NO_METERS = 'Telemetry unavailable from this console'

const sample = (at: string, over: Partial<Telemetry> = {}): Telemetry => ({
  sampled_at: at,
  memory_total: 128,
  memory_available: 64,
  gpu_memory_total: 100,
  gpu_memory_free: 40,
  gpu_power_draw_watts: 60,
  gpu_clock_mhz: 2200,
  gpu_temperature_c: 48,
  storage_total: 900,
  storage_available: 400,
  ...over,
})

const serving = (at: string, total = 100): Telemetry =>
  sample(at, {
    active_model: {
      recipe_id: 'qwen36-35b-a3b-nvfp4-1s',
      served_model_id: 'qwen',
      runtime_metrics: { generation_tokens_total: total, requests_running: 1, requests_waiting: 0, kv_cache_usage: 0.25 },
    },
  })

const CATALOG = [{ id: 'qwen36-35b-a3b-nvfp4-1s', display_name: 'Qwen 3.6 35B' }] as unknown as Recipe[]

// A console that leads no fleet: this box, read from its own meter, and the
// machines the console knows of besides.
const lone = () => buildFleetView({
  memory: fleetMemory({ sparks: [], models: [], recipes: CATALOG, deployments: [], jobs: [], system: null, latest: null }),
  summary: null, recipes: CATALOG, machines: MACHINES, selfLatest: null, nowMs: NOW,
})

const draw = (recipes: Recipe[] = []) =>
  renderToStaticMarkup(<Monitor machines={MACHINES} recipes={recipes} view={lone()} />)

describe('the live meters', () => {
  beforeEach(() => {
    resetTelemetry()
    resetEvents()
  })

  // The Sparks page lists every machine, so this page lists every machine. A
  // Spark left off the screen is a Spark the owner cannot see is there.
  it('draws a row for every Spark the console knows', () => {
    recordTelemetry(LOCAL_MACHINE, serving('2026-08-28T10:00:00Z'))
    const markup = draw(CATALOG)
    expect(markup.match(/class="urow (?!head)/g)).toHaveLength(4)
    for (const name of ['spark-f7e0', 'spark-a5d6', 'edgexpert-1c47', 'edgexpert-41b8']) expect(markup).toContain(name)
  })

  // Two Sparks this console cannot read are two machines, not one.
  it('keeps two unreadable Sparks apart', () => {
    expect(new Set(MACHINES.map(machine => machine.id)).size).toBe(MACHINES.length)
    expect(draw().match(new RegExp(NO_METERS, 'g'))).toHaveLength(2)
  })

  // Nothing on this page states a fact the console has not read. A machine
  // that has sent no sample is not idle and is not serving.
  it('claims nothing about a Spark that has not answered yet', () => {
    const markup = draw()
    expect(markup).not.toContain('No model')
    expect(markup).toContain('Waiting for telemetry')
    expect(markup).toContain('sdot busy')
  })

  // A Spark that stopped answering keeps its samples and says so in one
  // quiet line, with the failed dot it has always had.
  it('keeps the last readings of a Spark that stopped answering', () => {
    recordTelemetry('peer-1', sample('2026-08-28T10:00:00Z'))
    recordSilence('peer-1')
    const markup = draw()
    expect(markup).toContain('Disconnected · Last recorded samples')
    expect(markup).toContain('sdot fail')
    expect(markup).toContain('60<small class="fig-unit"> W</small>')
  })

  // The one chart a card carries is the speed of the model this Spark serves.
  it('draws this Spark\'s throughput, and only that, as a chart', () => {
    recordTelemetry(LOCAL_MACHINE, serving('2026-08-28T10:00:00Z', 100))
    recordTelemetry(LOCAL_MACHINE, serving('2026-08-28T10:00:05Z', 350))
    recordTelemetry(LOCAL_MACHINE, serving('2026-08-28T10:00:10Z', 600))
    const markup = draw(CATALOG)
    expect(markup.match(/<figure/g)).toHaveLength(1)
    expect(markup).toContain('Throughput')
    expect(markup).toContain('<small>avg</small>')
    expect(markup).toContain('<span class="u">tok/s</span>')
  })

  it('draws no chart while this Spark serves nothing', () => {
    recordTelemetry(LOCAL_MACHINE, sample('2026-08-28T10:00:00Z'))
    expect(draw()).not.toContain('<figure')
  })
})

// The owner's six boxes: one card per unit, no wall of tiles, and events
// beside them.
describe('the meters of a fleet', () => {
  beforeEach(() => {
    resetTelemetry()
    resetEvents()
  })

  const rows: FleetRow[] = sixBoxes()
  const view = viewOf(rows)
  const machines = machinesOf(rows)
  const render = (jobs: Job[] = []) => renderToStaticMarkup(
    <Monitor machines={machines} recipes={RECIPES} view={view} models={LOCAL_MODELS} jobs={jobs} />,
  )

  it('draws one card per unit, head first, and no tile grid', () => {
    const markup = render()
    expect(markup.match(/class="ucard"/g)).toHaveLength(2)
    expect(markup).toContain('TP4 · 4 Sparks')
    expect(markup).toContain('TP2 · 2 EdgeXperts')
    expect(markup).not.toContain('class="tile')
    expect(markup).not.toContain('Unassigned')
    const ds = markup.slice(markup.indexOf('DeepSeek V4 Flash 0731'))
    expect(ds.indexOf('edgexpert-41b8')).toBeLessThan(ds.indexOf('edgexpert-5e12'))
    expect(markup).toContain('>head<')
    expect(markup).toContain('>member<')
  })

  it('opens with the one banner that says what needs attention', () => {
    const markup = render()
    expect(markup).toContain('1 of 6 boxes needs attention')
    expect(markup).toContain('edgexpert-5e12 is tight on memory')
    expect(markup.match(/aria-label="Needs attention"/g)).toHaveLength(1)
  })

  // History is a column: one sparkline per box, and the figure beside it.
  it('reads a box from its heartbeat into one row', () => {
    recordTelemetry(heartbeatKey('node-edgexpert-41b8'), sample('2026-10-25T10:00:00Z', { gpu_power_draw_watts: 74, memory_total: 128.5 * GB }))
    recordTelemetry(heartbeatKey('node-edgexpert-41b8'), sample('2026-10-25T10:00:10Z', { gpu_power_draw_watts: 73, memory_total: 128.5 * GB }))
    const markup = render()
    expect(markup).not.toContain(NO_METERS)
    expect(markup).toContain('73<small class="fig-unit"> W</small>')
    expect(markup).toContain('class="sparkline plain"')
    expect(markup).toContain('loads refused')
  })

  // Speed is read on the box this console runs on. A unit elsewhere says
  // where to read it rather than drawing an empty frame.
  it('says where a unit elsewhere has its speed', () => {
    expect(render()).toContain('Throughput is read on the box this console runs on.')
  })

  it('keeps every metric one click away, closed', () => {
    recordTelemetry(heartbeatKey('node-edgexpert-41b8'), sample('2026-10-25T10:00:00Z'))
    const markup = render()
    expect(markup).toContain('<details class="allm"><summary>All metrics</summary>')
    expect(markup).not.toContain('<details class="allm" open')
  })

  it('lists what happened lately beside the cards', () => {
    const markup = render([
      { id: 'j1', kind: 'install', state: 'failed', recipe_id: 'nemotron', created_at: '2026-10-24T09:42:41Z', updated_at: '2026-10-24T09:54:41Z', steps: [] },
      { id: 'j2', kind: 'start', state: 'ready', recipe_id: 'glm53-flash-exl3-2s', created_at: '2026-10-24T10:50:56Z', updated_at: '2026-10-24T11:02:56Z', steps: [] },
    ])
    expect(markup).toContain('aria-label="Events"')
    expect(markup).toContain('install failed')
    expect(markup.indexOf('started')).toBeLessThan(markup.indexOf('install failed'))
  })

  it('puts a box in no unit in its own card', () => {
    const extra = [...rows, { ...rows[1], nodeID: 'node-spark-ab12', displayName: 'spark-ab12', consoleURL: 'http://spark-ab12.local:7070', installedModels: [], serving: undefined }]
    const markup = renderToStaticMarkup(
      <Monitor machines={machinesOf(extra)} recipes={RECIPES} view={viewOf(extra)} models={LOCAL_MODELS} />)
    expect(markup).toContain('Unassigned')
    expect(markup.lastIndexOf('spark-ab12')).toBeGreaterThan(markup.indexOf('Unassigned'))
  })
})
