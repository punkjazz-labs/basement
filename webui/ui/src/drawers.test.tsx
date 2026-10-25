import { beforeEach, describe, expect, it } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import type { Telemetry } from './api'
import { BoxDrawerBody, heartbeatLine } from './components/BoxDrawer'
import { FleetStatus } from './components/FleetHealth'
import { MemoryFree } from './components/Memory'
import { MetricChart, Sparkline, chartDomain, linePoints, smoothPath, windowText } from './components/Sparkline'
import { UnitDrawerBody } from './components/UnitDrawer'
import { localPowerRow } from './fleetModels'
import { boxHeadroom } from './headroom'
import { DS, GB, LOCAL_MODELS, RECIPES, box, serving, sixBoxes, viewOf } from './testFleet'
import { LOCAL_MACHINE, heartbeatKey, recordTelemetry, resetTelemetry } from './telemetry'

const noop = () => {}

const sample = (at: string, over: Partial<Telemetry> = {}): Telemetry => ({
  sampled_at: at, memory_total: 128.5 * GB, memory_available: 18.5 * GB, gpu_memory_total: 128.5 * GB,
  gpu_memory_free: 18.5 * GB, gpu_power_draw_watts: 71, gpu_clock_mhz: 2405, gpu_temperature_c: 63,
  storage_total: 995 * GB, storage_available: 315 * GB, ...over,
})

// One element under the title: a quiet line while every box is well, and one
// banner listing every box that needs a look while any does.
describe('how the fleet is doing', () => {
  it('lists every box that needs attention, each with its own way in', () => {
    const view = viewOf(sixBoxes(), { ages: { 'spark-9a07': 45 } })
    const markup = renderToStaticMarkup(<FleetStatus summary={view.summary} boxes={view.health} onOpenBox={noop} />)
    expect(markup).toContain('2 of 6 boxes need attention')
    // Every box at a glance: the tight box amber, the quiet one hollow.
    expect(markup.match(/class="led /g)).toHaveLength(6)
    expect(markup).toContain('led warn ')
    expect(markup).toContain('led warn stale')
    expect(markup).toContain('aria-label="Open edgexpert-5e12: memory tight (7.2 GB free, loads refused)"')
    // One line per box, worst first, each opening its own box.
    expect(markup.match(/<li /g)).toHaveLength(2)
    expect(markup).toContain('edgexpert-5e12 is tight on memory')
    expect(markup).toContain('spark-9a07 has gone quiet')
    expect(markup).toContain('aria-label="Open edgexpert-5e12"')
    expect(markup).toContain('aria-label="Open spark-9a07"')
    // The fleet line and the banner no longer say the same thing twice.
    expect(markup).not.toContain('need attention ·')
  })

  it('says one needs attention in the singular, without the long phrase', () => {
    const view = viewOf(sixBoxes())
    const markup = renderToStaticMarkup(<FleetStatus summary={view.summary} boxes={view.health} onOpenBox={noop} />)
    expect(markup).toContain('1 of 6 boxes needs attention')
    expect(markup).toContain('New models cannot load there until the one it runs stops.')
    expect(markup).not.toContain('more box needs attention')
  })

  it('is one quiet line while every box is well', () => {
    const calm = sixBoxes().map(row =>
      row.displayName === 'edgexpert-5e12' ? box('edgexpert-5e12', 'EdgeXpert', 30, [serving(DS)]) : row)
    const view = viewOf(calm)
    const markup = renderToStaticMarkup(<FleetStatus summary={view.summary} boxes={view.health} onOpenBox={noop} />)
    expect(markup).toContain('All 6 boxes healthy')
    expect(markup).not.toContain('<section')
    expect(markup).not.toContain('<button')
  })

  it('renders nothing for a console with no boxes to speak of', () => {
    const markup = renderToStaticMarkup(
      <FleetStatus summary={{ total: 0, attention: [], light: 'ok', text: '' }} boxes={[]} onOpenBox={noop} />)
    expect(markup).toBe('')
  })
})

describe('a box\'s free memory in a cell', () => {
  const room = (free: number) => boxHeadroom({ id: 'a', name: 'edgexpert-5e12', totalBytes: 128.5 * GB, availableBytes: free * GB })

  it('says loads are refused only while they are', () => {
    expect(renderToStaticMarkup(<MemoryFree headroom={room(7.2)} />)).toContain('loads refused')
    expect(renderToStaticMarkup(<MemoryFree headroom={room(19.4)} />)).not.toContain('loads refused')
    expect(renderToStaticMarkup(<MemoryFree headroom={room(7.2)} refused={false} />)).not.toContain('loads refused')
  })

  it('draws the bar with its reserve tick and the figure', () => {
    const markup = renderToStaticMarkup(<MemoryFree headroom={room(19.4)} />)
    expect(markup).toContain('class="tick"')
    expect(markup).toContain('19.4 GB')
  })
})

describe('history', () => {
  it('says how long a window covers', () => {
    expect(windowText([0, 9 * 60_000])).toBe('last 9 min')
    expect(windowText([0, 40_000])).toBe('last 40 s')
    expect(windowText([5])).toBe('')
  })

  it('draws a flat series through the middle', () => {
    expect(linePoints([3, 3, 3], 10, 10)).toBe('0.0,5.0 5.0,5.0 10.0,5.0')
    expect(linePoints([1], 10, 10)).toBe('')
  })

  it('heads the chart with its low, average and latest reading and waits for a second reading', () => {
    const markup = renderToStaticMarkup(
      <MetricChart points={[60, 70, 80]} title="GPU power" unit="W" format={value => value.toFixed(0)} />)
    expect(markup).toContain('<small>min</small><b>60</b>')
    expect(markup).toContain('<small>avg</small><b>70</b>')
    expect(markup).toContain('<small>now</small><b>80</b>')
    // A smooth line, a soft fill under it, and a dot where it ends.
    expect(markup).toMatch(/<path class="area" d="M[^"]*C[^"]*Z" fill="url\(#fade[^"]+\)"/)
    expect(markup).toMatch(/<path class="line" d="M[^"]*C/)
    expect(markup).toContain('class="end"')
    const empty = renderToStaticMarkup(<MetricChart points={[60]} title="GPU power" unit="W" format={String} empty="Waiting" />)
    expect(empty).toContain('Waiting')
    expect(empty).not.toContain('<svg')
  })

  // The line is drawn to the window's own range, with room under it and
  // over it, so it neither hugs the top nor lies on the floor.
  it('pads the scale around the window, never below zero', () => {
    expect(chartDomain([40, 50])).toEqual([37, 51.5])
    expect(chartDomain([2, 12])).toEqual([0, 13.5])
    const [low, high] = chartDomain([60, 60])
    expect(low).toBeLessThan(60)
    expect(high).toBeGreaterThan(60)
    // A metric with a natural top keeps it.
    expect(chartDomain([7.2, 7.4], 128.5)).toEqual([0, 128.5])
  })

  it('draws a smooth line that passes through every reading and never overshoots', () => {
    const xy: [number, number][] = [[0, 10], [10, 0], [20, 0], [30, 10]]
    const path = smoothPath(xy)
    expect(path.startsWith('M0.00,10.00')).toBe(true)
    expect(path.endsWith('30.00,10.00')).toBe(true)
    // Between the two equal readings the curve stays flat on them: no
    // control point rises above the line they share.
    const controls = [...path.matchAll(/(-?[\d.]+),(-?[\d.]+)/g)].map(match => Number(match[2]))
    expect(Math.min(...controls)).toBeGreaterThanOrEqual(0)
    expect(smoothPath([[0, 1], [10, 2]])).toBe('M0.00,1.00L10.00,2.00')
    expect(smoothPath([[0, 1]])).toBe('')
  })

  it('labels a sparkline for a screen reader', () => {
    expect(renderToStaticMarkup(<Sparkline points={[1, 2, 3]} label="GPU power, last 9 min" />)).toContain('aria-label="GPU power, last 9 min"')
  })
})

describe('the box drawer', () => {
  beforeEach(() => resetTelemetry())

  it('opens on who the box is, its three facts and its one chart', () => {
    const view = viewOf(sixBoxes(), { ages: { 'edgexpert-5e12': 3 } })
    recordTelemetry(heartbeatKey('node-edgexpert-5e12'), sample('2026-10-25T11:59:00Z', { memory_available: 7.2 * GB }))
    recordTelemetry(heartbeatKey('node-edgexpert-5e12'), sample('2026-10-25T11:59:10Z', { memory_available: 7.2 * GB, gpu_power_draw_watts: 74 }))
    const row = view.findBox('edgexpert-5e12')!
    const markup = renderToStaticMarkup(
      <BoxDrawerBody view={view} row={row} system={null} power={{ row: localPowerRow({ mode: 'full', failure: '' }, false), set: noop }} onOpenUnit={noop} />)
    expect(markup).toContain('edgexpert-5e12')
    expect(markup).toContain('member · <button')
    expect(markup).toContain('TP2 · DeepSeek V4 Flash 0731')
    expect(markup).toContain('heartbeat 3 s ago')
    expect(markup).toContain('Loads refused')
    for (const label of ['Memory free', 'GPU temp', 'Power']) expect(markup).toContain(`<span class="l">${label}</span>`)
    // One chart, with a metric switch over it, and its low, average and
    // latest reading in the header.
    expect(markup.match(/<figure/g)).toHaveLength(1)
    for (const metric of ['Power', 'Temp', 'Clock', 'Memory free']) expect(markup).toContain(`>${metric}</button>`)
    expect(markup).toContain('<small>avg</small><b>73</b>')
    expect(markup).toContain('<small>now</small><b>74</b><')
    expect(markup).toContain('<span class="u">W</span>')
    expect(markup).toContain('<dt>OS</dt><dd>Ubuntu 24.04.3 LTS</dd>')
    expect(markup).toContain('Open console')
    expect(markup).toContain('aria-label="Power mode for edgexpert-5e12"')
  })

  it('says plainly when it cannot read a box', () => {
    const view = viewOf(sixBoxes())
    const markup = renderToStaticMarkup(
      <BoxDrawerBody view={view} row={view.findBox('spark-2b4f')!} system={null} power={null} onOpenUnit={noop} />)
    expect(markup).toContain('Waiting for telemetry')
    expect(markup).not.toContain('Power mode for')
  })

  it('reads this box as live and offers no console to open', () => {
    const view = viewOf(sixBoxes())
    const self = view.findBox('spark-f7e0')!
    expect(heartbeatLine(self, undefined)).toBe('this console · live')
    const markup = renderToStaticMarkup(<BoxDrawerBody view={view} row={self} system={null} power={null} onOpenUnit={noop} />)
    expect(markup).toContain('this console · live')
    expect(markup).not.toContain('Open console')
  })
})

describe('the unit drawer', () => {
  beforeEach(() => resetTelemetry())

  const draw = (id: string) => {
    const view = viewOf(sixBoxes())
    return renderToStaticMarkup(
      <UnitDrawerBody
        view={view} unit={view.findUnit(id)!} recipes={RECIPES} models={LOCAL_MODELS}
        onOpenBox={noop} openPlayground={noop} openGenerate={noop} openDeployment={noop} openFleetDeployment={noop} refresh={noop}
      />)
  }

  it('lists its boxes head first, one memory figure each', () => {
    const markup = draw('dep_ds_tp2')
    expect(markup).toContain('TP2 · 2 EdgeXperts')
    const list = markup.slice(markup.indexOf('class="topology"'))
    expect(list.indexOf('edgexpert-41b8')).toBeLessThan(list.indexOf('edgexpert-5e12'))
    expect(markup).toContain('class="head"')
    expect(markup).toContain('19.6 GB')
    expect(markup).toContain('7.2 GB')
    // Its speed is read on its head, not here.
    expect(markup).toContain('Speed is read on the box this console runs on')
    expect(markup).toContain('>Stop<')
    expect(markup).toContain('Open on edgexpert-41b8')
    expect(markup).toContain('Refused')
  })

  it('draws this Spark\'s own speed and opens its own chat', () => {
    recordTelemetry(LOCAL_MACHINE, sample('2026-10-25T11:59:00Z', {
      active_model: { recipe_id: 'glm53-flash-exl3-2s', served_model_id: 'glm', runtime_metrics: { generation_tokens_total: 1000 } },
    }))
    recordTelemetry(LOCAL_MACHINE, sample('2026-10-25T11:59:05Z', {
      active_model: { recipe_id: 'glm53-flash-exl3-2s', served_model_id: 'glm', runtime_metrics: { generation_tokens_total: 1250 } },
    }))
    recordTelemetry(LOCAL_MACHINE, sample('2026-10-25T11:59:10Z', {
      active_model: { recipe_id: 'glm53-flash-exl3-2s', served_model_id: 'glm', runtime_metrics: { generation_tokens_total: 1500 } },
    }))
    const markup = draw('dep_glm_tp4')
    expect(markup).toContain('50.0')
    expect(markup).toContain('now, on this Spark')
    expect(markup).toContain('>Open<')
    expect(markup).toContain('Allowed')
  })
})
