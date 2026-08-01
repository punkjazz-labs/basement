import { useEffect, useRef, useState } from 'react'
import { formatBytes, type Telemetry } from '../api'

const WINDOW = 60 // samples kept per series (~3 minutes at 3s)

interface Series {
  tps: number[]
  running: number[]
  waiting: number[]
  kv: number[]
  gpuFree: number[]
  power: number[]
  clock: number[]
  temperature: number[]
}

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

export default function Monitor({ telemetry, activeName }: { telemetry: Telemetry | null; activeName?: string }) {
  const [series, setSeries] = useState<Series>({ tps: [], running: [], waiting: [], kv: [], gpuFree: [], power: [], clock: [], temperature: [] })
  const lastTotal = useRef<{ at: number; total: number } | null>(null)
  const lastSample = useRef('')

  useEffect(() => {
    if (!telemetry || telemetry.sampled_at === lastSample.current) return
    lastSample.current = telemetry.sampled_at
    const vllm = telemetry.active_model?.vllm
    const push = (values: number[], value: number) => [...values.slice(-(WINDOW - 1)), value]
    // Rate from the server's own sample timestamps: the client's polling
    // jitter must not distort tokens-per-second.
    let tps = -1
    const total = vllm?.generation_tokens_total
    if (typeof total === 'number') {
      const at = Date.parse(telemetry.sampled_at)
      const last = lastTotal.current
      if (last && at > last.at && total >= last.total) tps = ((total - last.total) / (at - last.at)) * 1000
      lastTotal.current = { at, total }
    } else {
      lastTotal.current = null
    }
    setSeries(previous => ({
      tps: tps >= 0 ? push(previous.tps, tps) : previous.tps,
      running: vllm ? push(previous.running, vllm.requests_running ?? 0) : previous.running,
      waiting: vllm ? push(previous.waiting, vllm.requests_waiting ?? 0) : previous.waiting,
      kv: vllm ? push(previous.kv, (vllm.kv_cache_usage ?? 0) * 100) : previous.kv,
      gpuFree: push(previous.gpuFree, telemetry.gpu_memory_free),
      power: telemetry.gpu_power_draw_watts > 0 ? push(previous.power, telemetry.gpu_power_draw_watts) : previous.power,
      clock: telemetry.gpu_clock_mhz > 0 ? push(previous.clock, telemetry.gpu_clock_mhz) : previous.clock,
      temperature: telemetry.gpu_temperature_c > 0 ? push(previous.temperature, telemetry.gpu_temperature_c) : previous.temperature,
    }))
  }, [telemetry])

  if (!telemetry) {
    return <div className="empty">Waiting for the first telemetry sample…</div>
  }

  const latest = (values: number[]) => (values.length ? values[values.length - 1] : undefined)
  const serving = Boolean(telemetry.active_model)

  return (
    <div className="stack">
      <div className="section-head">
        <span className="muted">
          {serving ? `${activeName ?? telemetry.active_model?.recipe_id} · sampled every few seconds` : 'No model serving. System metrics only.'}
        </span>
      </div>
      <div className="tiles">
        {serving && (
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
        {telemetry.gpu_memory_total > 0 && (
          <Tile
            label="GPU memory free"
            value={formatBytes(telemetry.gpu_memory_free)}
            points={series.gpuFree}
            max={telemetry.gpu_memory_total}
          />
        )}
        {telemetry.memory_total > 0 && (
          <Tile label="System RAM free" value={formatBytes(telemetry.memory_available)} />
        )}
        <Tile label="Disk free" value={formatBytes(telemetry.storage_available)} />
      </div>
      {!serving && (
        <p className="faint">Start a model to see live generation speed, request queue, and KV-cache pressure here.</p>
      )}
    </div>
  )
}
