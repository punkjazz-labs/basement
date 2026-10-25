import { beforeEach, describe, expect, it } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import type { Telemetry } from './api'
import { BoxDrawerBody, heartbeatLine } from './components/BoxDrawer'
import { AttentionBanner, FleetHealthLine } from './components/FleetHealth'
import { MemoryFree } from './components/Memory'
import { MetricChart, Sparkline, linePoints, windowText } from './components/Sparkline'
import { UnitDrawerBody } from './components/UnitDrawer'
import { localPowerRow } from './fleetModels'
import { boxHeadroom } from './headroom'
import { GB, LOCAL_MODELS, RECIPES, sixBoxes, viewOf } from './testFleet'
import { LOCAL_MACHINE, heartbeatKey, recordTelemetry, resetTelemetry } from './telemetry'

const noop = () => {}

const sample = (at: string, over: Partial<Telemetry> = {}): Telemetry => ({
  sampled_at: at, memory_total: 128.5 * GB, memory_available: 18.5 * GB, gpu_memory_total: 128.5 * GB,
  gpu_memory_free: 18.5 * GB, gpu_power_draw_watts: 71, gpu_clock_mhz: 2405, gpu_temperature_c: 63,
  storage_total: 995 * GB, storage_available: 315 * GB, ...over,
})

describe('the health line', () => {
  it('draws one dot per box and the fleet in one phrase', () => {
    const view = viewOf(sixBoxes(), { ages: { 'spark-9a07': 45 } })
    const markup = renderToStaticMarkup(<FleetHealthLine summary={view.summary} boxes={view.health} onOpenBox={noop} />)
    expect(markup.match(/class="led /g)).toHaveLength(6)
    // The tight box is amber, the quiet one hollow.
    expect(markup).toContain('led warn ')
    expect(markup).toContain('led warn stale')
    expect(markup).toContain('2 need attention')
    expect(markup).toContain('aria-label="Open edgexpert-5e12: memory tight (7.2 GB free, loads refused)"')
  })

  it('renders nothing for a console with no boxes to speak of', () => {
    const markup = renderToStaticMarkup(
      <FleetHealthLine summary={{ total: 0, attention: [], light: 'ok', text: '' }} boxes={[]} onOpenBox={noop} />)
    expect(markup).toBe('')
  })
})

describe('the attention banner', () => {
  it('renders nothing while all is well', () => {
    expect(renderToStaticMarkup(<AttentionBanner banner={null} onOpen={noop} />)).toBe('')
  })

  it('says what is wrong, with one action', () => {
    const view = viewOf(sixBoxes())
    const markup = renderToStaticMarkup(<AttentionBanner banner={view.banner} onOpen={noop} />)
    expect(markup).toContain('edgexpert-5e12 is tight on memory')
    expect(markup).toContain('Open edgexpert-5e12')
    expect(markup.match(/<button/g)).toHaveLength(1)
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

  it('shows the average in the chart header and waits for a second reading', () => {
    const markup = renderToStaticMarkup(
      <MetricChart points={[60, 70, 80]} title="GPU power" unit="W" format={value => value.toFixed(0)} />)
    expect(markup).toContain('70<small>W avg</small>')
    const empty = renderToStaticMarkup(<MetricChart points={[60]} title="GPU power" unit="W" format={String} empty="Waiting" />)
    expect(empty).toContain('Waiting')
    expect(empty).not.toContain('<svg')
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
    // One chart, with a metric switch over it, and its average in the header.
    expect(markup.match(/<figure/g)).toHaveLength(1)
    for (const metric of ['Power', 'Temp', 'Clock', 'Memory free']) expect(markup).toContain(`>${metric}</button>`)
    expect(markup).toContain('W avg')
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
