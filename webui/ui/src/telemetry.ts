import type { Peer, Telemetry } from './api'
import { consoleKey } from './fleetInvite'
import type { FleetRow } from './fleetModels'

// The live meters, kept for as long as the console is open. App polls this
// Spark and every paired Spark; this file holds what they answered; Monitor
// draws it. The history used to be built inside Monitor itself, so the page
// opened empty and drew its first line minutes after the console did. Nothing
// about the samples changed: only where they are kept.

// Samples kept per series. This Spark is sampled every five seconds, a paired
// Spark every ten, so the window is a count and not a length of time.
export const WINDOW = 60

export interface Series {
  tps: number[]
  running: number[]
  waiting: number[]
  kv: number[]
  gpuFree: number[]
  power: number[]
  clock: number[]
  temperature: number[]
}

export const EMPTY_SERIES: Series = {
  tps: [], running: [], waiting: [], kv: [], gpuFree: [], power: [], clock: [], temperature: [],
}

// One machine's history: every series, the last sample whole, and whether
// that machine still answers.
export interface MachineSeries {
  series: Series
  // The last telemetry that machine sent, which is what the tiles read for a
  // figure that has no series. Null until it has sent one.
  latest: Telemetry | null
  // False for a machine that has stopped answering, and for one that has
  // never answered. The samples it did send stay, because the last reading of
  // a Spark that went quiet is still the last reading of that Spark.
  answering: boolean
  // The token total the last rate was measured against, with the server's own
  // timestamp for it.
  lastTotal: { at: number; total: number } | null
  // The stamp of the sample already folded in, so a poll that catches the
  // same server sample twice does not push a second point for it.
  lastSample: string
}

export const EMPTY_MACHINE: MachineSeries = {
  series: EMPTY_SERIES,
  latest: null,
  answering: false,
  lastTotal: null,
  lastSample: '',
}

// One sample folded into a machine's history. Pure, so the accumulation the
// whole page depends on is tested on its own.
export function foldSample(previous: MachineSeries, sample: Telemetry): MachineSeries {
  if (sample.sampled_at === previous.lastSample) {
    // The same reading again: the machine answered, so it is answering, and
    // there is nothing new to push.
    return previous.answering ? previous : { ...previous, answering: true }
  }
  const runtime = sample.active_model?.runtime_metrics
  const push = (values: number[], value: number) => [...values.slice(-(WINDOW - 1)), value]
  // Rate from the server's own sample timestamps: the client's polling
  // jitter must not distort tokens-per-second.
  let tps = -1
  let lastTotal = previous.lastTotal
  const total = runtime?.generation_tokens_total
  if (typeof total === 'number') {
    const at = Date.parse(sample.sampled_at)
    const last = previous.lastTotal
    if (last && at > last.at && total >= last.total) tps = ((total - last.total) / (at - last.at)) * 1000
    lastTotal = { at, total }
  } else {
    lastTotal = null
  }
  const held = previous.series
  return {
    series: {
      tps: tps >= 0 ? push(held.tps, tps) : held.tps,
      // A series the running runtime does not publish keeps its previous
      // (possibly empty) history, so the tile reads n/a instead of a zero
      // the runtime never reported.
      running: typeof runtime?.requests_running === 'number' ? push(held.running, runtime.requests_running) : held.running,
      waiting: typeof runtime?.requests_waiting === 'number' ? push(held.waiting, runtime.requests_waiting) : held.waiting,
      kv: typeof runtime?.kv_cache_usage === 'number' ? push(held.kv, runtime.kv_cache_usage * 100) : held.kv,
      gpuFree: push(held.gpuFree, sample.gpu_memory_free),
      power: sample.gpu_power_draw_watts > 0 ? push(held.power, sample.gpu_power_draw_watts) : held.power,
      clock: sample.gpu_clock_mhz > 0 ? push(held.clock, sample.gpu_clock_mhz) : held.clock,
      temperature: sample.gpu_temperature_c > 0 ? push(held.temperature, sample.gpu_temperature_c) : held.temperature,
    },
    latest: sample,
    answering: true,
    lastTotal,
    lastSample: sample.sampled_at,
  }
}

// ---- The store itself ---------------------------------------------------

// The key this Spark's own history is kept under. Every other key is a peer
// id, which is what the console already names a paired Spark by.
export const LOCAL_MACHINE = 'local'

const machines = new Map<string, MachineSeries>()
const listeners = new Set<() => void>()
// What subscribers compare. The map is mutable, so a count of the changes is
// the snapshot React can hold on to.
let revision = 0

const announce = () => {
  revision += 1
  for (const listener of listeners) listener()
}

export function recordTelemetry(key: string, sample: Telemetry): void {
  const previous = machines.get(key) ?? EMPTY_MACHINE
  const next = foldSample(previous, sample)
  if (next === previous) return
  machines.set(key, next)
  announce()
}

// A machine that did not answer this round. Its samples stay: what it last
// reported is still the last thing it reported, and the page says the machine
// is quiet rather than dropping the history and reading as a fresh start.
export function recordSilence(key: string): void {
  const held = machines.get(key)
  if (held === undefined || !held.answering) return
  machines.set(key, { ...held, answering: false })
  announce()
}

export const machineSeries = (key: string): MachineSeries => machines.get(key) ?? EMPTY_MACHINE

// Every machine that is no longer on the fleet's list loses its history: a
// Spark that was removed from this console has nothing left to draw.
export function forgetMachines(keep: readonly string[]): void {
  const kept = new Set(keep)
  let dropped = false
  for (const key of [...machines.keys()]) {
    if (kept.has(key)) continue
    machines.delete(key)
    dropped = true
  }
  if (dropped) announce()
}

// ---- Which Sparks the meters are drawn for ------------------------------

// One Spark, as the Monitor tab draws it.
export interface MonitorMachine {
  // What tells this machine apart from every other one on the list, whatever
  // this console can read of it: the fleet's own id for it, or the console
  // URL the fleet holds it under where it has no id. Two Sparks never share
  // one, which is why the page is keyed on this and not on the key below.
  id: string
  // The key its samples are kept under: this Spark, or the id of a paired
  // Spark. Empty for a Spark whose meters this console cannot read at all,
  // which is a machine with a section and no chart rather than no section.
  key: string
  name: string
}

// Every Spark this console knows, in the order the meters draw them: this
// machine first, then the fleet in the order the fleet reports it. The rows
// are the ones the Sparks page renders, so a machine with a row and a power
// switch there is never missing here.
//
// A Spark is readable only where this console holds its own way in: itself,
// and a Spark it has an API key for. The peers table holds one machine at
// most (ADR 0005), so a third Spark that joined the fleet by invitation has
// no key here and is listed without one.
//
// One state this list cannot resolve: a member console opened at an address
// the fleet did not pin. isLocalNode then has only the browser's own origin
// to match against the console URL the fleet stored, so this machine can be
// drawn twice, once as itself and once as a Spark with no key. The Sparks
// page reads self the same way (membershipRows), so pinning that origin is
// one fix for both pages rather than something this list can do alone.
export function monitorMachines(
  sparks: readonly FleetRow[],
  peers: readonly Peer[],
  localName: string,
): MonitorMachine[] {
  const keyed = new Map(peers.map(peer => [consoleKey(peer.base_url), peer.id]))
  // A row the fleet named is told apart by its node id; a row with none is
  // told apart by the console URL the table already de-duplicates on, so
  // every machine on the list carries something of its own.
  const idOf = (spark: FleetRow) => spark.nodeID || consoleKey(spark.consoleURL)
  const own = sparks.find(spark => spark.isSelf)
  const machines: MonitorMachine[] = [{
    id: own ? idOf(own) : LOCAL_MACHINE,
    key: LOCAL_MACHINE,
    name: own?.displayName || localName,
  }]
  for (const spark of sparks) {
    if (spark.isSelf) continue
    machines.push({
      id: idOf(spark),
      key: keyed.get(consoleKey(spark.consoleURL)) ?? '',
      name: spark.displayName,
    })
  }
  return machines
}

export function subscribeTelemetry(listener: () => void): () => void {
  listeners.add(listener)
  return () => {
    listeners.delete(listener)
  }
}

export const telemetryRevision = (): number => revision

// For the tests only: one suite must not inherit another's samples.
export function resetTelemetry(): void {
  machines.clear()
  revision = 0
}
