import { beforeEach, describe, expect, it } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import type { Recipe, Telemetry } from './api'
import Monitor from './views/Monitor'
import type { FleetDeploymentView } from './api'
import type { FleetRow } from './fleetModels'
import { LOCAL_MACHINE, heartbeatKey, recordSilence, recordTelemetry, resetTelemetry } from './telemetry'
import { fleetUnits } from './units'

// The fleet in the lab: this machine, a Spark this console holds a key for,
// and two that joined by invitation and have no key here. The peers table
// holds one machine at most, so two unreadable Sparks is the ordinary shape
// of a fleet of four, and the two must never be drawn as one.
const MACHINES = [
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

const serving = (at: string): Telemetry =>
  sample(at, {
    active_model: {
      recipe_id: 'qwen36-35b-a3b-nvfp4-1s',
      served_model_id: 'qwen',
      runtime_metrics: { generation_tokens_total: 100 },
    },
  })

const CATALOG = [{ id: 'qwen36-35b-a3b-nvfp4-1s', display_name: 'Qwen 3.6 35B' }] as unknown as Recipe[]

const draw = (recipes: Recipe[] = []) =>
  renderToStaticMarkup(<Monitor machines={MACHINES} recipes={recipes} />)

describe('the live meters', () => {
  beforeEach(() => resetTelemetry())

  // The Sparks page lists every machine, so this page lists every machine. A
  // Spark left off the screen is a Spark the owner cannot see is there.
  it('draws a section for every Spark the fleet holds', () => {
    recordTelemetry(LOCAL_MACHINE, serving('2026-08-28T10:00:00Z'))
    const markup = draw(CATALOG)
    expect(markup.match(/mon-machine/g)).toHaveLength(4)
    expect(markup).toContain('spark-f7e0')
    expect(markup).toContain('spark-a5d6')
    expect(markup).toContain('edgexpert-1c47')
    expect(markup).toContain('edgexpert-41b8')
    expect(markup).toContain('Qwen 3.6 35B')
  })

  // Two Sparks this console cannot read are two machines, not one. They share
  // the empty samples key, so the page is keyed on the fleet's own id: with
  // the key alone, React holds one section for both.
  it('keeps two unreadable Sparks apart', () => {
    expect(new Set(MACHINES.map(machine => machine.id)).size).toBe(MACHINES.length)
    const markup = draw()
    expect(markup.match(new RegExp(NO_METERS, 'g'))).toHaveLength(2)
  })

  // A Spark this console cannot ask says so, in its own section. It is never
  // drawn with a chart, and never left out.
  it('says plainly which Spark it cannot read', () => {
    const markup = draw()
    expect(markup).toContain(NO_METERS)
    expect(markup).toContain('edgexpert-1c47')
  })

  // Nothing on this page states a fact the console has not read. A machine
  // that has sent no sample is not idle and is not serving.
  it('claims nothing about a Spark that has not answered yet', () => {
    const markup = draw()
    expect(markup).not.toContain('No model serving')
    expect(markup).toContain('Waiting for telemetry')
    expect(markup).toContain('sdot busy')
  })

  it('says a Spark serves nothing only once that Spark has said so', () => {
    recordTelemetry(LOCAL_MACHINE, sample('2026-08-28T10:00:00Z'))
    const markup = draw()
    expect(markup).toContain('No model running')
  })

  // A Spark that stopped answering keeps its samples and takes the failed
  // dot, which is the fleet strip's own language.
  it('keeps the samples of a Spark that stopped answering', () => {
    recordTelemetry('peer-1', sample('2026-08-28T10:00:00Z'))
    recordSilence('peer-1')
    const markup = draw()
    expect(markup).toContain('Disconnected · Last recorded samples')
    expect(markup).toContain('sdot fail')
    expect(markup).toContain('Power draw')
  })
})

// Every other box in a fleet reports its meters in its heartbeat, so it gets
// tiles rather than the line a box this console cannot hear from gets. The
// boxes are drawn unit by unit.
describe('the meters of a fleet', () => {
  beforeEach(() => resetTelemetry())

  const box = (nodeID: string, name: string, isSelf = false): FleetRow => ({
    nodeID, displayName: name, isSelf, role: isSelf ? 'controller' : 'member',
    consoleURL: `http://${name}.local:7070`, installedModels: [], status: { word: 'Idle', dot: '' },
    answering: true, power: { mode: 'full', failure: '' },
  })
  const rows = [box('node-f1cc', 'spark-f7e0', true), box('node-41b8', 'edgexpert-41b8'), box('node-5e12', 'edgexpert-5e12')]
  const machines = [
    { id: 'node-f1cc', key: LOCAL_MACHINE, name: 'spark-f7e0' },
    { id: 'node-41b8', key: heartbeatKey('node-41b8'), name: 'edgexpert-41b8' },
    { id: 'node-5e12', key: heartbeatKey('node-5e12'), name: 'edgexpert-5e12' },
  ]
  const placement = {
    deployment_id: 'dep_ds_tp2', recipe_id: 'deepseek-v4-flash-0731-2s', recipe_version: 4, recipe_fingerprint: '',
    topology_count: 2, owner_node_id: 'node-41b8', owner_job_id: '', state: 'ready', last_observed_at: '',
    created_at: '2026-10-24T10:00:00Z', updated_at: '', stale: false,
    nodes: [
      { deployment_id: 'dep_ds_tp2', node_id: 'node-41b8', node_role: 'head', rank: 0, reservation_id: '' },
      { deployment_id: 'dep_ds_tp2', node_id: 'node-5e12', node_role: 'worker', rank: 1, reservation_id: '' },
    ],
  } satisfies FleetDeploymentView

  it('draws a box from its heartbeat instead of saying it cannot', () => {
    recordTelemetry(heartbeatKey('node-41b8'), sample('2026-10-25T10:00:00Z', { gpu_memory_total: 128 }))
    const markup = renderToStaticMarkup(
      <Monitor machines={machines} recipes={CATALOG} sparks={rows} grouping={fleetUnits(rows, [])} />,
    )
    expect(markup).not.toContain(NO_METERS)
    expect(markup).toContain('Memory free')
    expect(markup).toContain('Power draw')
    // One unified pool on GB10: a heartbeat box shows it once, as memory.
    expect(markup.match(/GPU memory free/g)).toBeNull()
  })

  it('groups the boxes by the unit they form', () => {
    const markup = renderToStaticMarkup(
      <Monitor machines={machines} recipes={CATALOG} sparks={rows} grouping={fleetUnits(rows, [placement])} />,
    )
    expect(markup).toContain('TP2 · 2 boxes')
    expect(markup).toContain('Independent')
    expect(markup).toContain('>head<')
    expect(markup).toContain('>rank 1<')
    expect(markup.indexOf('edgexpert-41b8')).toBeLessThan(markup.indexOf('Independent'))
  })
})
