import { useEffect, useRef, useState } from 'react'
import { api, formatBytes, terminal, stateCopy, operationCopy, type Job, type Recipe, type Step } from '../api'
import { confirmBox, noticeBox } from '../confirm'

const CHECKS = [
  'verify_architecture', 'verify_dgx_spark', 'verify_memory_capacity', 'verify_disk',
  'verify_port', 'verify_docker', 'verify_nvidia_runtime', 'verify_artifact_access',
]

interface Phase {
  title: string
  note: string
  // activeNote replaces note while the phase runs — the honest time
  // expectation, so a long wait reads as normal instead of stuck.
  activeNote?: string
  states: string[]
  operations: string[]
}

const FIRST_START_NOTE = 'Loading the model into memory — the first start typically takes 5–15 minutes, never more than 20. Later starts are much faster.'

function phasePlan(job: Job): Phase[] {
  if (job.kind === 'install') {
    return [
      { title: 'Check system', note: 'Hardware, memory, disk and access', states: ['queued', 'preflighting'], operations: CHECKS },
      { title: 'Prepare runtime', note: 'Pinned vLLM image', states: ['downloading_runtime'], operations: ['pull_image'] },
      { title: 'Download model', note: 'Resumable model files', states: ['downloading_models'], operations: ['download_artifact'] },
      { title: 'Configure service', note: 'Owned configuration and container', states: ['configuring'], operations: ['write_generated_config', 'create_container'] },
      { title: 'Start model', note: 'Safe memory reservation', activeNote: FIRST_START_NOTE, states: ['checking_memory', 'starting', 'stopping'], operations: ['stop_container', 'verify_memory', 'start_container'] },
      { title: 'Verify endpoint', note: 'Health and real inference', activeNote: FIRST_START_NOTE, states: ['verifying_health', 'verifying_inference'], operations: ['wait_http', 'verify_openai_inference'] },
    ]
  }
  if (job.kind === 'start') {
    return [
      { title: 'Reserve hardware', note: 'Stop the active model, check memory', states: ['queued', 'stopping', 'checking_memory'], operations: ['stop_container', 'verify_memory'] },
      { title: 'Start model', note: 'Launch the pinned runtime', states: ['starting'], operations: ['start_container'] },
      { title: 'Verify endpoint', note: 'Health and real inference', activeNote: 'Waiting for the model to load and answer — takes a few minutes when memory is cold.', states: ['verifying_health', 'verifying_inference'], operations: ['wait_http', 'verify_openai_inference'] },
    ]
  }
  if (job.kind === 'remove') {
    return [
      { title: 'Stop model', note: 'End the running service', states: ['queued', 'stopping'], operations: ['stop_container'] },
      { title: 'Remove runtime', note: 'Delete owned container state', states: ['removing'], operations: ['remove_container'] },
      { title: 'Reclaim storage', note: 'Delete only unshared model files', states: ['removing'], operations: ['remove_artifact_if_unshared'] },
    ]
  }
  if (job.kind === 'smoke-test') {
    return [
      { title: 'Check endpoint', note: 'Wait for a healthy response', states: ['queued', 'verifying_health'], operations: ['wait_http'] },
      { title: 'Run inference', note: 'Require a non-empty model response', states: ['verifying_inference'], operations: ['verify_openai_inference'] },
    ]
  }
  if (job.kind === 'benchmark') {
    return [
      { title: 'Check endpoint', note: 'Wait for a healthy response', states: ['queued', 'verifying_health'], operations: ['wait_http'] },
      { title: 'Measure speed', note: 'Timed generation on this Spark', states: ['benchmarking'], operations: ['measure_throughput'] },
    ]
  }
  return [{ title: 'Stop model', note: 'End the running service', states: ['queued', 'stopping'], operations: ['stop_container'] }]
}

// LiveProgress renders the running step's receipt as human progress. The
// layout follows the classic download pattern: one bar, then one line with
// what is transferring on the left and speed plus time remaining on the
// right. Transfer rate is derived client-side from receipt deltas.
function LiveProgress({ step }: { step: Step }) {
  const receipt = (step.receipt ?? {}) as Record<string, unknown>
  const rateRef = useRef<{ key: string; at: number; bytes: number; rate: number } | null>(null)

  const asNumber = (value: unknown) => (typeof value === 'number' && Number.isFinite(value) ? value : 0)

  // Smoothed bytes-per-second across receipt updates, reset when the
  // measured stream changes.
  const smoothedRate = (key: string, bytes: number) => {
    const now = performance.now()
    const last = rateRef.current
    if (!last || last.key !== key || bytes < last.bytes) {
      rateRef.current = { key, at: now, bytes, rate: 0 }
    } else if (bytes > last.bytes && now > last.at) {
      const instant = ((bytes - last.bytes) / (now - last.at)) * 1000
      rateRef.current = { key, at: now, bytes, rate: last.rate > 0 ? last.rate * 0.7 + instant * 0.3 : instant }
    }
    return rateRef.current?.rate ?? 0
  }

  const row = (percent: number | null, left: string, right?: string) => (
    <div className="sub-progress">
      {percent !== null && (
        <div className="bar" role="progressbar" aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(percent)}>
          <span style={{ width: `${Math.max(percent, 0.5)}%` }} />
        </div>
      )}
      <div className="stats">
        <span className="mono nums">{left}</span>
        {right && <span className="mono nums eta">{right}</span>}
      </div>
    </div>
  )

  const speedAndETA = (rate: number, remainingBytes: number) => {
    if (rate <= 1) return undefined
    const parts = [`${formatBytes(rate)}/s`]
    if (remainingBytes > 0) parts.push(formatETA(remainingBytes / rate))
    return parts.join(' · ')
  }

  // "4.2 of 71.9 GB" — one unit, chosen from the total, so the line stays
  // short enough to never wrap.
  const bytePair = (done: number, total: number) => {
    const units = ['B', 'KB', 'MB', 'GB', 'TB']
    let unit = 0
    let scale = 1
    while (total / scale >= 1000 && unit < units.length - 1) {
      scale *= 1000
      unit += 1
    }
    return `${(done / scale).toFixed(1)} of ${(total / scale).toFixed(1)} ${units[unit]}`
  }

  if (step.operation === 'download_artifact') {
    const done = asNumber(receipt.bytes_complete)
    const total = asNumber(receipt.bytes_total)
    if (total <= 0) return null
    const rate = smoothedRate(`artifact:${step.index}:${receipt.repository ?? ''}`, done)
    const file = typeof receipt.file === 'string' ? fileLabel(receipt.file) : ''
    const left = `${file ? `${file} · ` : ''}${bytePair(done, total)}`
    return row(Math.min((done / total) * 100, 100), left, speedAndETA(rate, total - done))
  }

  if (step.operation === 'wait_http') {
    const attempt = asNumber(receipt.attempt)
    if (attempt <= 0) return null
    return (
      <div className="sub-progress">
        <span className="mono nums">Health check #{attempt} — the model is still loading, this is normal.</span>
      </div>
    )
  }

  if (step.operation === 'pull_image') {
    const done = asNumber(receipt.bytes_complete)
    const total = asNumber(receipt.bytes_total)
    const layersDone = asNumber(receipt.layers_done)
    const layersTotal = asNumber(receipt.layers_total)
    const status = typeof receipt.status === 'string' ? receipt.status : ''
    if (status === 'Extracting' && layersTotal > 0) {
      return row(
        Math.min((layersDone / layersTotal) * 100, 100),
        `Unpacking layers · ${layersDone} of ${layersTotal}`,
      )
    }
    if (total > 0) {
      const rate = smoothedRate(`pull:${step.index}`, done)
      return row(
        Math.min((done / total) * 100, 100),
        `Runtime image · ${bytePair(done, total)}`,
        speedAndETA(rate, total - done),
      )
    }
    return row(null, 'Contacting the registry…')
  }

  return null
}

// fileLabel turns sharded artifact names like model-00003-of-00015.safetensors
// into "File 3 of 15"; anything else keeps its own name.
function fileLabel(name: string): string {
  const shard = name.match(/(\d+)-of-0*(\d+)/)
  if (shard) return `File ${parseInt(shard[1], 10)} of ${parseInt(shard[2], 10)}`
  return name
}

function formatDuration(seconds: number): string {
  const whole = Math.round(seconds)
  if (whole < 60) return `${whole}s`
  if (whole < 3600) return `${Math.floor(whole / 60)}m ${whole % 60}s`
  return `${Math.floor(whole / 3600)}h ${Math.floor((whole % 3600) / 60)}m`
}

// formatETA speaks the way downloads do: never seconds-precise, always an
// approachable remaining time.
function formatETA(seconds: number): string {
  if (seconds < 60) return 'under a minute left'
  if (seconds < 3600) return `${Math.round(seconds / 60)} min left`
  const hours = Math.floor(seconds / 3600)
  const minutes = Math.round((seconds % 3600) / 60)
  return `${hours} h ${minutes} min left`
}

// Elapsed ticks up from when this step was first observed running, so even
// receipt-less steps (model start, health wait) visibly make progress.
function Elapsed({ stepKey }: { stepKey: string }) {
  const startRef = useRef<{ key: string; at: number }>({ key: stepKey, at: Date.now() })
  const [, setTick] = useState(0)
  if (startRef.current.key !== stepKey) startRef.current = { key: stepKey, at: Date.now() }
  useEffect(() => {
    const timer = setInterval(() => setTick(value => value + 1), 1000)
    return () => clearInterval(timer)
  }, [])
  const seconds = (Date.now() - startRef.current.at) / 1000
  if (seconds < 3) return null
  return <>{' · '}{formatDuration(seconds)}</>
}

function activePhaseIndex(job: Job, phases: Phase[]): number {
  if (terminal(job.state) && job.state !== 'failed' && job.state !== 'cancelled') return phases.length
  const failed = [...job.steps].reverse().find(step => step.state === 'failed')
  if (failed) {
    const index = phases.findIndex(phase => phase.operations.includes(failed.operation.replace(/^rollback_/, '')))
    if (index >= 0) return index
  }
  const index = phases.findIndex(phase => phase.states.includes(job.state))
  return index >= 0 ? index : 0
}

export default function DeploymentDialog({ job, recipes, onClose, onOpenPlayground }: {
  job: Job | null
  recipes: Recipe[]
  onClose: () => void
  onOpenPlayground: () => void
}) {
  const ref = useRef<HTMLDialogElement>(null)

  useEffect(() => {
    const dialog = ref.current
    if (!dialog) return
    if (job && !dialog.open) dialog.showModal()
    if (!job && dialog.open) dialog.close()
  }, [job])

  if (!job) return <dialog ref={ref} onClose={onClose} />

  const phases = phasePlan(job)
  const activeIndex = activePhaseIndex(job, phases)
  const succeeded = terminal(job.state) && job.state !== 'failed' && job.state !== 'cancelled'
  const recipe = recipes.find(item => item.id === job.recipe_id)
  const verb = { install: 'Deploy', start: 'Start', stop: 'Stop', remove: 'Remove', 'smoke-test': 'Test', benchmark: 'Measure' }[job.kind] ?? 'Manage'
  // A benchmark or smoke test is not a deployment; every piece of copy in
  // this dialog follows the job's own noun.
  const noun = { install: 'deployment', start: 'start', stop: 'stop', remove: 'removal', 'smoke-test': 'test', benchmark: 'measurement' }[job.kind] ?? 'job'
  const kicker = { 'smoke-test': 'Model test', benchmark: 'Speed measurement' }[job.kind] ?? 'Deployment'
  const current = [...job.steps].reverse().find(step => step.state === 'running')
    ?? [...job.steps].reverse().find(step => step.state === 'failed')
  const benchReceipt = succeeded && job.kind === 'benchmark'
    ? [...job.steps].reverse().find(step => step.operation === 'measure_throughput' && step.state === 'completed')?.receipt
    : undefined

  const cancel = async () => {
    const { ok } = await confirmBox({
      title: `Cancel this ${noun}?`,
      body: job.kind === 'install'
        ? 'Downloads are resumable — installing again later picks up where this left off.'
        : undefined,
      confirmLabel: `Cancel ${noun}`,
      cancelLabel: 'Keep going',
      danger: true,
    })
    if (!ok) return
    try {
      await api(`/api/v1/jobs/${encodeURIComponent(job.id)}/cancel`, { method: 'POST', body: '{}' })
    } catch (problem) {
      noticeBox('Could not cancel', problem instanceof Error ? problem.message : undefined)
    }
  }

  return (
    <dialog ref={ref} onClose={onClose} aria-label={`${verb} ${recipe?.display_name ?? job.recipe_id}`}>
      <div className="dialog-pad">
        <div className="dialog-head">
          <div>
            <p className="kicker">{kicker}</p>
            <h2>{verb} {recipe?.display_name ?? job.recipe_id}</h2>
          </div>
          <button className="dialog-close" onClick={onClose} aria-label="Close">×</button>
        </div>
        <div style={{ display: 'flex', gap: 12, alignItems: 'center' }}>
          <span className={`deployment-status ${job.state}`}>{stateCopy[job.state] ?? job.state}</span>
        </div>
        <ol className="phase-list">
          {phases.map((phase, index) => {
            let status = index < activeIndex ? 'complete' : index === activeIndex ? 'active' : 'pending'
            if (job.state === 'failed' && index === activeIndex) status = 'failed'
            if (job.state === 'cancelled' && index === activeIndex) status = 'cancelled'
            if (activeIndex === phases.length) status = 'complete'
            const label = { complete: 'Complete', active: 'In progress', failed: 'Failed', cancelled: 'Cancelled', pending: 'Waiting' }[status]
            const showsProgress = status === 'active' && current && phase.operations.includes(current.operation.replace(/^rollback_/, ''))
            return (
              <li key={phase.title} className={status}>
                <i aria-hidden="true" />
                <div>
                  <strong>{phase.title}</strong>
                  <span>{status === 'active' && phase.activeNote ? phase.activeNote : phase.note}</span>
                  {showsProgress && <LiveProgress step={current} />}
                </div>
                <b>
                  {label}
                  {showsProgress && <Elapsed stepKey={`${job.id}:${current.index}:${current.operation}`} />}
                </b>
              </li>
            )
          })}
        </ol>
        {job.state === 'cancelled' ? (
          <p className="muted" role="status">
            {job.kind === 'install'
              ? 'Cancelled. Everything downloaded so far is kept — installing again resumes where this left off.'
              : `The ${noun} was cancelled.`}
          </p>
        ) : (
          job.error && (
            <div className="error-note" role="alert">
              <strong>This {noun} stopped before finishing.</strong>
              <p>{job.error}</p>
            </div>
          )
        )}
        {benchReceipt && (
          <div className="dialog-result" role="status">
            <div className="cell">
              <div className="l">Generation speed</div>
              <div className="v">{typeof benchReceipt.tokens_per_second === 'number' ? benchReceipt.tokens_per_second.toFixed(1) : '—'} <small>tok/s</small></div>
            </div>
            <div className="cell">
              <div className="l">First token</div>
              <div className="v">{typeof benchReceipt.time_to_first_token_ms === 'number' ? Math.round(benchReceipt.time_to_first_token_ms) : '—'} <small>ms</small></div>
            </div>
            <div className="cell">
              <div className="l">Sample</div>
              <div className="v">{typeof benchReceipt.completion_tokens === 'number' ? benchReceipt.completion_tokens : '—'} <small>tokens</small></div>
            </div>
          </div>
        )}
        <details>
          <summary className="muted">Technical receipts</summary>
          <ul className="receipts">
            <li className="faint">Job reference <code>{job.id}</code></li>
            {job.steps.map(step => (
              <li key={step.index}>
                <strong style={{ fontSize: 13 }}>{operationCopy[step.operation] ?? step.operation}</strong>{' '}
                <span className="faint">{step.state}</span>
                <pre>{step.receipt && Object.keys(step.receipt).length ? JSON.stringify(step.receipt, null, 2) : 'No receipt yet'}</pre>
              </li>
            ))}
            {job.steps.length === 0 && <li className="faint">The first persisted step will appear here.</li>}
          </ul>
        </details>
        <div className="dialog-foot">
          <span className="note">
            {!terminal(job.state)
              ? `Closing this window does not stop the ${noun}.`
              : succeeded
                ? {
                    install: `${recipe?.display_name ?? job.recipe_id} is live and serving on this Spark.`,
                    start: `${recipe?.display_name ?? job.recipe_id} is live and serving on this Spark.`,
                    benchmark: 'Measured with a real request on this Spark.',
                    'smoke-test': 'The model answered a real inference request.',
                    stop: 'The model has stopped.',
                    remove: 'The model has been removed.',
                  }[job.kind] ?? 'Finished.'
                : ''}
          </span>
          {!terminal(job.state) && <button className="danger" onClick={cancel}>Cancel {noun}</button>}
          {succeeded && (job.kind === 'install' || job.kind === 'start') && (
            <button className="brand" onClick={onOpenPlayground}>Try it in the playground</button>
          )}
          <button className="ghost" onClick={onClose}>Close</button>
        </div>
      </div>
    </dialog>
  )
}
