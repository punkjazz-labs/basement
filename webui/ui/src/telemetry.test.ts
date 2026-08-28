import { beforeEach, describe, expect, it } from 'vitest'
import type { Telemetry } from './api'
import type { FleetRow } from './fleetModels'
import {
  EMPTY_MACHINE, LOCAL_MACHINE, WINDOW, foldSample, forgetMachines, machineSeries, monitorMachines,
  recordSilence, recordTelemetry, resetTelemetry, subscribeTelemetry, telemetryRevision,
} from './telemetry'

// One sample, with only the fields a test cares about set. Everything else is
// zero, which is what a manager that reads no GPU reports.
const sample = (at: string, over: Partial<Telemetry> = {}): Telemetry => ({
  sampled_at: at,
  memory_total: 128,
  memory_available: 64,
  gpu_memory_total: 100,
  gpu_memory_free: 40,
  gpu_power_draw_watts: 0,
  gpu_clock_mhz: 0,
  gpu_temperature_c: 0,
  storage_total: 900,
  storage_available: 400,
  ...over,
})

const generating = (at: string, total: number): Telemetry =>
  sample(at, {
    active_model: {
      recipe_id: 'demo',
      served_model_id: 'demo',
      runtime_metrics: { generation_tokens_total: total, requests_running: 1 },
    },
  })

describe('foldSample', () => {
  it('keeps the samples in order and marks the machine answering', () => {
    const first = foldSample(EMPTY_MACHINE, sample('2026-08-28T10:00:00Z', { gpu_memory_free: 10 }))
    const second = foldSample(first, sample('2026-08-28T10:00:05Z', { gpu_memory_free: 20 }))
    expect(second.series.gpuFree).toEqual([10, 20])
    expect(second.answering).toBe(true)
    expect(second.latest?.gpu_memory_free).toBe(20)
  })

  // The rate is measured against the server's own stamps, so a poll that
  // arrived late cannot make the machine look faster than it was.
  it('reads the rate from the server stamps and not from the clock here', () => {
    const first = foldSample(EMPTY_MACHINE, generating('2026-08-28T10:00:00Z', 1000))
    // No rate yet: one total is not two.
    expect(first.series.tps).toEqual([])
    const second = foldSample(first, generating('2026-08-28T10:00:10Z', 1500))
    expect(second.series.tps).toEqual([50])
  })

  // A runtime that restarted reports a smaller total than the one before it.
  // That is not a negative rate, so no point is pushed at all.
  it('pushes no point when the total went backwards', () => {
    const first = foldSample(EMPTY_MACHINE, generating('2026-08-28T10:00:00Z', 1000))
    const second = foldSample(first, generating('2026-08-28T10:00:10Z', 10))
    expect(second.series.tps).toEqual([])
    const third = foldSample(second, generating('2026-08-28T10:00:20Z', 110))
    expect(third.series.tps).toEqual([10])
  })

  it('folds the same server sample only once', () => {
    const first = foldSample(EMPTY_MACHINE, sample('2026-08-28T10:00:00Z'))
    const again = foldSample(first, sample('2026-08-28T10:00:00Z'))
    expect(again.series.gpuFree).toEqual([40])
  })

  // A series the runtime does not publish keeps its own (possibly empty)
  // history rather than taking a zero the runtime never reported.
  it('leaves an unpublished series alone', () => {
    const first = foldSample(EMPTY_MACHINE, sample('2026-08-28T10:00:00Z'))
    expect(first.series.running).toEqual([])
    expect(first.series.power).toEqual([])
    const withPower = foldSample(first, sample('2026-08-28T10:00:05Z', { gpu_power_draw_watts: 60 }))
    expect(withPower.series.power).toEqual([60])
  })

  it('keeps the window from growing without end', () => {
    let held = EMPTY_MACHINE
    for (let index = 0; index < WINDOW + 20; index += 1) {
      held = foldSample(held, sample(`2026-08-28T10:${String(index).padStart(2, '0')}:00Z`, { gpu_memory_free: index }))
    }
    expect(held.series.gpuFree).toHaveLength(WINDOW)
    expect(held.series.gpuFree[WINDOW - 1]).toBe(WINDOW + 19)
  })
})

describe('the telemetry store', () => {
  beforeEach(() => resetTelemetry())

  it('builds one history per machine and tells its readers', () => {
    let told = 0
    const stop = subscribeTelemetry(() => {
      told += 1
    })
    recordTelemetry('local', sample('2026-08-28T10:00:00Z', { gpu_memory_free: 5 }))
    recordTelemetry('peer-1', sample('2026-08-28T10:00:00Z', { gpu_memory_free: 9 }))
    expect(told).toBe(2)
    expect(telemetryRevision()).toBe(2)
    expect(machineSeries('local').series.gpuFree).toEqual([5])
    expect(machineSeries('peer-1').series.gpuFree).toEqual([9])
    stop()
    recordTelemetry('local', sample('2026-08-28T10:00:05Z'))
    expect(told).toBe(2)
  })

  // A Spark that stops answering keeps every sample it sent: the last reading
  // of a machine that went quiet is still the last reading of that machine.
  it('keeps the samples of a machine that stopped answering', () => {
    recordTelemetry('peer-1', sample('2026-08-28T10:00:00Z', { gpu_memory_free: 7 }))
    recordSilence('peer-1')
    expect(machineSeries('peer-1').answering).toBe(false)
    expect(machineSeries('peer-1').series.gpuFree).toEqual([7])
    expect(machineSeries('peer-1').latest?.gpu_memory_free).toBe(7)
  })

  it('says nothing about a machine it has never heard from', () => {
    expect(machineSeries('peer-9')).toBe(EMPTY_MACHINE)
    recordSilence('peer-9')
    expect(telemetryRevision()).toBe(0)
  })

  it('forgets a machine this console no longer holds', () => {
    recordTelemetry('local', sample('2026-08-28T10:00:00Z'))
    recordTelemetry('peer-1', sample('2026-08-28T10:00:00Z'))
    forgetMachines(['local'])
    expect(machineSeries('peer-1')).toBe(EMPTY_MACHINE)
    expect(machineSeries('local').series.gpuFree).toHaveLength(1)
  })
})

// One row of the fleet table, with only the fields this list reads set.
const row = (nodeID: string, displayName: string, consoleURL: string, isSelf = false): FleetRow => ({
  nodeID,
  displayName,
  isSelf,
  role: isSelf ? 'controller' : 'member',
  consoleURL,
  installedModels: [],
  status: { word: 'ready', dot: 'on' },
  answering: true,
  power: { mode: 'full', failure: '' },
})

describe('which Sparks the meters are drawn for', () => {
  // The fleet in the CLAUDE.md lab: three machines, and only one of them is a
  // peer this console holds a key for. All three get a section; the third one
  // gets no key, which is what its section then says.
  const three: FleetRow[] = [
    row('node-lead', 'spark-f1cc', 'http://spark-f1cc.local:7070', true),
    row('node-loft', 'spark-a393', 'http://spark-a393.local:7070'),
    row('node-msi', 'edgexpert-2051', 'http://edgexpert-2051.local:7070'),
  ]
  const peer = { id: 'peer-1', name: 'spark-a393', base_url: 'http://spark-a393.local:7070' }

  it('lists every Spark in the fleet, this one first', () => {
    expect(monitorMachines(three, [peer], 'attic')).toEqual([
      { key: LOCAL_MACHINE, name: 'spark-f1cc' },
      { key: 'peer-1', name: 'spark-a393' },
      { key: '', name: 'edgexpert-2051' },
    ])
  })

  // The order the fleet reports is not the order the meters draw: this
  // machine leads, wherever the summary put it.
  it('puts this machine first whatever the fleet order is', () => {
    const reversed = [three[2], three[1], three[0]]
    expect(monitorMachines(reversed, [peer], 'attic').map(machine => machine.name))
      .toEqual(['spark-f1cc', 'edgexpert-2051', 'spark-a393'])
  })

  // A Spark that leads no fleet has no row of its own, so the list is made
  // from the name this console knows for itself.
  it('draws this machine alone when there is no fleet', () => {
    expect(monitorMachines([], [], 'attic')).toEqual([{ key: LOCAL_MACHINE, name: 'attic' }])
  })

  // A Spark added by address has a row and a key, so its meters are readable
  // even though it never joined the fleet.
  it('reads a Spark added by address through its own key', () => {
    const added = row('peer-1', 'spark-a393', 'http://spark-a393.local:7070')
    expect(monitorMachines([added], [peer], 'attic')).toEqual([
      { key: LOCAL_MACHINE, name: 'attic' },
      { key: 'peer-1', name: 'spark-a393' },
    ])
  })

  // The console URL is the key both sides meet on, so a trailing slash or a
  // different case must not lose the key.
  it('matches a peer to its row however the URL was written', () => {
    const loud = { ...peer, base_url: 'http://SPARK-A393.local:7070/' }
    expect(monitorMachines(three, [loud], 'attic')[1]).toEqual({ key: 'peer-1', name: 'spark-a393' })
  })
})
