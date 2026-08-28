import { beforeEach, describe, expect, it } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import type { Recipe, Telemetry } from './api'
import Monitor from './views/Monitor'
import { LOCAL_MACHINE, recordSilence, recordTelemetry, resetTelemetry } from './telemetry'

// The fleet in the lab: this machine, a Spark this console holds a key for,
// and two that joined by invitation and have no key here. The peers table
// holds one machine at most, so two unreadable Sparks is the ordinary shape
// of a fleet of four, and the two must never be drawn as one.
const MACHINES = [
  { id: 'node-lead', key: LOCAL_MACHINE, name: 'spark-f1cc' },
  { id: 'node-loft', key: 'peer-1', name: 'spark-a393' },
  { id: 'node-msi-a', key: '', name: 'edgexpert-2051' },
  { id: 'node-msi-b', key: '', name: 'edgexpert-37c4' },
]

const NO_METERS = 'This console cannot read the meters of this Spark yet.'

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
    expect(markup).toContain('spark-f1cc')
    expect(markup).toContain('spark-a393')
    expect(markup).toContain('edgexpert-2051')
    expect(markup).toContain('edgexpert-37c4')
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
    expect(markup).toContain('edgexpert-2051')
  })

  // Nothing on this page states a fact the console has not read. A machine
  // that has sent no sample is not idle and is not serving.
  it('claims nothing about a Spark that has not answered yet', () => {
    const markup = draw()
    expect(markup).not.toContain('No model serving')
    expect(markup).toContain('Waiting for the first telemetry sample')
    expect(markup).toContain('sdot busy')
  })

  it('says a Spark serves nothing only once that Spark has said so', () => {
    recordTelemetry(LOCAL_MACHINE, sample('2026-08-28T10:00:00Z'))
    const markup = draw()
    expect(markup).toContain('No model serving. System metrics only.')
  })

  // A Spark that stopped answering keeps its samples and takes the failed
  // dot, which is the fleet strip's own language.
  it('keeps the samples of a Spark that stopped answering', () => {
    recordTelemetry('peer-1', sample('2026-08-28T10:00:00Z'))
    recordSilence('peer-1')
    const markup = draw()
    expect(markup).toContain('No answer now. The last samples stay on screen.')
    expect(markup).toContain('sdot fail')
    expect(markup).toContain('Power draw')
  })
})
