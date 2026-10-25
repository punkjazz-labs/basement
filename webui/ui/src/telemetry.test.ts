import { beforeEach, describe, expect, it } from 'vitest'
import type { NodeInventory, Telemetry } from './api'
import type { FleetRow } from './fleetModels'
import {
  EMPTY_MACHINE, LOCAL_MACHINE, WINDOW, foldSample, forgetMachines, heartbeatKey, inventorySample, isHeartbeatKey,
  machineSeries, monitorMachines, recordHeartbeats, recordSilence, recordTelemetry, requestLine, resetTelemetry,
  subscribeTelemetry, telemetryRevision, withHeartbeatKeys,
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
    row('node-lead', 'spark-f7e0', 'http://spark-f7e0.local:7070', true),
    row('node-loft', 'spark-a5d6', 'http://spark-a5d6.local:7070'),
    row('node-msi', 'edgexpert-1c47', 'http://edgexpert-1c47.local:7070'),
  ]
  const peer = { id: 'peer-1', name: 'spark-a5d6', base_url: 'http://spark-a5d6.local:7070' }

  it('lists every Spark in the fleet, this one first', () => {
    expect(monitorMachines(three, [peer], 'attic')).toEqual([
      { id: 'node-lead', key: LOCAL_MACHINE, name: 'spark-f7e0' },
      { id: 'node-loft', key: 'peer-1', name: 'spark-a5d6' },
      { id: 'node-msi', key: '', name: 'edgexpert-1c47' },
    ])
  })

  // The peers table holds one machine at most, so a fleet of four has two
  // Sparks this console cannot read. They share the empty samples key, so
  // only the id keeps them apart, and the page is drawn from the id.
  it('gives two unreadable Sparks two identities', () => {
    const four = [...three, row('node-msi-b', 'edgexpert-41b8', 'http://edgexpert-41b8.local:7070')]
    const machines = monitorMachines(four, [peer], 'attic')
    expect(machines).toHaveLength(4)
    expect(machines.filter(machine => machine.key === '').map(machine => machine.name))
      .toEqual(['edgexpert-1c47', 'edgexpert-41b8'])
    expect(new Set(machines.map(machine => machine.id)).size).toBe(4)
  })

  // A row the fleet named with no id of its own still has to be told apart
  // from every other row, so it takes the console URL the table already
  // de-duplicates on.
  it('tells apart two Sparks that report no node id', () => {
    const nameless = [
      row('', 'edgexpert-1c47', 'http://edgexpert-1c47.local:7070'),
      row('', 'edgexpert-41b8', 'http://edgexpert-41b8.local:7070'),
    ]
    const machines = monitorMachines(nameless, [], 'attic')
    expect(new Set(machines.map(machine => machine.id)).size).toBe(3)
  })

  // The order the fleet reports is not the order the meters draw: this
  // machine leads, wherever the summary put it.
  it('puts this machine first whatever the fleet order is', () => {
    const reversed = [three[2], three[1], three[0]]
    expect(monitorMachines(reversed, [peer], 'attic').map(machine => machine.name))
      .toEqual(['spark-f7e0', 'edgexpert-1c47', 'spark-a5d6'])
  })

  // A Spark that leads no fleet has no row of its own, so the list is made
  // from the name this console knows for itself.
  it('draws this machine alone when there is no fleet', () => {
    expect(monitorMachines([], [], 'attic'))
      .toEqual([{ id: LOCAL_MACHINE, key: LOCAL_MACHINE, name: 'attic' }])
  })

  // A Spark added by address has a row and a key, so its meters are readable
  // even though it never joined the fleet.
  it('reads a Spark added by address through its own key', () => {
    const added = row('peer-1', 'spark-a5d6', 'http://spark-a5d6.local:7070')
    expect(monitorMachines([added], [peer], 'attic')).toEqual([
      { id: LOCAL_MACHINE, key: LOCAL_MACHINE, name: 'attic' },
      { id: 'peer-1', key: 'peer-1', name: 'spark-a5d6' },
    ])
  })

  // The console URL is the key both sides meet on, so a trailing slash or a
  // different case must not lose the key.
  it('matches a peer to its row however the URL was written', () => {
    const loud = { ...peer, base_url: 'http://SPARK-A5D6.local:7070/' }
    expect(monitorMachines(three, [loud], 'attic')[1])
      .toEqual({ id: 'node-loft', key: 'peer-1', name: 'spark-a5d6' })
  })
})

// A box this console holds no key for still reports itself in every
// heartbeat, so its meters are read from there.
describe('boxes read from their heartbeat', () => {
  beforeEach(() => resetTelemetry())

  const beat = (at: string, over: Partial<NodeInventory> = {}): NodeInventory => ({
    hostname: 'edgexpert-41b8', product_name: 'EdgeXpert', dgx_spark: true,
    memory_total_bytes: 128, memory_available_bytes: 10,
    storage_total_bytes: 1900, storage_available_bytes: 1200,
    gpu_memory_total_bytes: 128, gpu_memory_free_bytes: 10,
    gpu_power_draw_watts: 74, gpu_clock_mhz: 2380, gpu_temperature_c: 66,
    observed_at: at,
    ...over,
  })

  const box = (answering: boolean, inventory?: NodeInventory) => ({
    nodeID: 'node-41b8', isSelf: false, answering, inventory,
  })

  it('turns one heartbeat into one sample', () => {
    expect(inventorySample(beat('2026-10-25T10:00:00Z'))).toEqual({
      sampled_at: '2026-10-25T10:00:00Z',
      memory_total: 128, memory_available: 10,
      gpu_memory_total: 128, gpu_memory_free: 10,
      gpu_power_draw_watts: 74, gpu_clock_mhz: 2380, gpu_temperature_c: 66,
      storage_total: 1900, storage_available: 1200,
    })
  })

  // A heartbeat with no stamp cannot be told apart from the last one.
  it('uses no heartbeat it cannot date', () => {
    expect(inventorySample(beat('', { observed_at: undefined }))).toBeNull()
  })

  // The controller's receipt time names the heartbeat when it has one.
  it('stamps a sample with the time the heartbeat arrived', () => {
    expect(inventorySample(beat('2026-10-25T10:00:00Z'), '2026-10-25T10:00:01Z')?.sampled_at)
      .toBe('2026-10-25T10:00:01Z')
  })

  it('grows a point for each heartbeat that arrives, even with the same reading stamp', () => {
    recordHeartbeats([{ ...box(true, beat('2026-10-25T10:00:00Z')), heartbeatAt: '2026-10-25T10:00:01Z' }])
    recordHeartbeats([{ ...box(true, beat('2026-10-25T10:00:00Z')), heartbeatAt: '2026-10-25T10:00:06Z' }])
    expect(machineSeries(heartbeatKey('node-41b8')).series.power).toEqual([74, 74])
  })

  // A meter the heartbeat left out is unread, and draws no point.
  it('reads an absent GPU meter as unread', () => {
    const next = foldSample(EMPTY_MACHINE, inventorySample(beat('2026-10-25T10:00:00Z', { gpu_temperature_c: undefined }))!)
    expect(next.series.temperature).toEqual([])
    expect(next.series.power).toEqual([74])
  })

  it('grows one point per new heartbeat and none for the same one twice', () => {
    recordHeartbeats([box(true, beat('2026-10-25T10:00:00Z'))])
    recordHeartbeats([box(true, beat('2026-10-25T10:00:00Z'))])
    recordHeartbeats([box(true, beat('2026-10-25T10:00:05Z', { memory_available_bytes: 9 }))])
    const held = machineSeries(heartbeatKey('node-41b8'))
    expect(held.series.memFree).toEqual([10, 9])
    expect(held.series.clock).toEqual([2380, 2380])
    expect(held.answering).toBe(true)
  })

  // A box gone quiet keeps its last samples and says it is quiet.
  it('keeps the samples of a box that stopped answering', () => {
    recordHeartbeats([box(true, beat('2026-10-25T10:00:00Z'))])
    recordHeartbeats([box(false, beat('2026-10-25T10:00:00Z'))])
    const held = machineSeries(heartbeatKey('node-41b8'))
    expect(held.answering).toBe(false)
    expect(held.series.power).toEqual([74])
  })

  // A box already quiet when the console opened still has a last reading,
  // shown as the last reading rather than as one still to come.
  it('shows the last reading of a box that was quiet before the console opened', () => {
    recordHeartbeats([box(false, beat('2026-10-25T10:00:00Z'))])
    recordHeartbeats([box(false, beat('2026-10-25T10:00:00Z'))])
    const held = machineSeries(heartbeatKey('node-41b8'))
    expect(held.latest?.gpu_power_draw_watts).toBe(74)
    expect(held.answering).toBe(false)
    expect(held.series.power).toEqual([74])
  })

  it('never reads this box from its heartbeat', () => {
    recordHeartbeats([{ ...box(true, beat('2026-10-25T10:00:00Z')), isSelf: true }])
    expect(machineSeries(heartbeatKey('node-41b8'))).toBe(EMPTY_MACHINE)
  })

  // The peers poll prunes only peer keys, and the fleet poll only heartbeat
  // keys, so neither throws away the other's history.
  it('lets each poll prune only its own keys', () => {
    recordTelemetry('peer-1', sample('2026-08-28T10:00:00Z'))
    recordHeartbeats([box(true, beat('2026-10-25T10:00:00Z'))])
    forgetMachines([LOCAL_MACHINE], isHeartbeatKey)
    expect(machineSeries(heartbeatKey('node-41b8'))).not.toBe(EMPTY_MACHINE)
    expect(machineSeries('peer-1')).toBe(EMPTY_MACHINE)
    forgetMachines([], key => !isHeartbeatKey(key))
    expect(machineSeries(heartbeatKey('node-41b8'))).toBe(EMPTY_MACHINE)
  })

  it('gives every fleet box with no key a heartbeat key', () => {
    const fleet = [
      row('node-lead', 'spark-f7e0', 'http://spark-f7e0.local:7070', true),
      row('node-41b8', 'edgexpert-41b8', 'http://edgexpert-41b8.local:7070'),
      { ...row('peer-9', 'shed', 'http://shed.local:7070'), legacyPeerOnly: true },
    ]
    const machines = withHeartbeatKeys(monitorMachines(fleet, [], 'attic'), fleet)
    expect(machines.map(machine => machine.key)).toEqual([LOCAL_MACHINE, 'node:node-41b8', ''])
  })
})

// The serving card's live line says only what the runtime counts.
describe('the requests a serving card names', () => {
  it('says what is in flight and queued, as the runtime counts it', () => {
    expect(requestLine({ requests_running: 2, requests_waiting: 0 }, '71.0M tokens served')).toEqual(['2 in flight', '0 queued'])
  })

  it('says only the counter a runtime publishes', () => {
    expect(requestLine({ requests_running: 1 })).toEqual(['1 in flight'])
  })

  it('falls back to what was counted here when the runtime reports no requests', () => {
    expect(requestLine(undefined, '71.0M tokens served')).toEqual(['71.0M tokens served'])
    expect(requestLine({ kv_cache_usage: 0.2 })).toEqual([])
  })
})
