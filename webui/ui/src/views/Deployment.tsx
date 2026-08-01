import { useEffect, useRef, useState } from 'react'
import { api, formatBytes, terminal, stateCopy, operationCopy, type Job, type Recipe, type Step } from '../api'

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

// LiveProgress renders the running step's receipt as human progress: a byte
// bar for the model download, layer status for the image pull. Transfer rate
// is derived client-side from receipt deltas.
function LiveProgress({ step }: { step: Step }) {
  const receipt = (step.receipt ?? {}) as Record<string, unknown>
  const rateRef = useRef<{ key: string; at: number; bytes: number; rate: number } | null>(null)

  const asNumber = (value: unknown) => (typeof value === 'number' && Number.isFinite(value) ? value : 0)

  if (step.operation === 'download_artifact') {
    const done = asNumber(receipt.bytes_complete)
    const total = asNumber(receipt.bytes_total)
    if (total <= 0) return null
    const key = `${step.index}:${receipt.repository ?? ''}`
    const now = performance.now()
    const last = rateRef.current
    if (!last || last.key !== key || done < last.bytes) {
      rateRef.current = { key, at: now, bytes: done, rate: 0 }
    } else if (done > last.bytes && now > last.at) {
      const instant = ((done - last.bytes) / (now - last.at)) * 1000
      const smoothed = last.rate > 0 ? last.rate * 0.7 + instant * 0.3 : instant
      rateRef.current = { key, at: now, bytes: done, rate: smoothed }
    }
    const rate = rateRef.current?.rate ?? 0
    const remaining = rate > 0 ? (total - done) / rate : 0
    const percent = Math.min((done / total) * 100, 100)
    return (
      <div className="sub-progress">
        <div className="bar" role="progressbar" aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(percent)}>
          <span style={{ width: `${Math.max(percent, 0.5)}%` }} />
        </div>
        <span className="mono nums">
          {formatBytes(done)} of {formatBytes(total)} · {percent.toFixed(0)}%
          {rate > 1 && ` · ${formatBytes(rate)}/s`}
          {remaining > 1 && ` · about ${formatDuration(remaining)} left`}
        </span>
        {typeof receipt.file === 'string' && receipt.file && <span className="file">{receipt.file}</span>}
      </div>
    )
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
    const detail = (receipt.progress ?? {}) as Record<string, unknown>
    const current = asNumber(detail.current)
    const total = asNumber(detail.total)
    const status = typeof receipt.status === 'string' && receipt.status ? receipt.status : 'Pulling'
    const layer = typeof receipt.layer === 'string' && receipt.layer ? receipt.layer : ''
    return (
      <div className="sub-progress">
        {total > 0 && (
          <div className="bar" role="progressbar" aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round((current / total) * 100)}>
            <span style={{ width: `${Math.min((current / total) * 100, 100)}%` }} />
          </div>
        )}
        <span className="mono nums">
          {status}
          {layer && ` · layer ${layer}`}
          {total > 0 && ` · ${formatBytes(current)} of ${formatBytes(total)}`}
        </span>
      </div>
    )
  }

  return null
}

function formatDuration(seconds: number): string {
  const whole = Math.round(seconds)
  if (whole < 60) return `${whole}s`
  if (whole < 3600) return `${Math.floor(whole / 60)}m ${whole % 60}s`
  return `${Math.floor(whole / 3600)}h ${Math.floor((whole % 3600) / 60)}m`
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
  const current = [...job.steps].reverse().find(step => step.state === 'running')
    ?? [...job.steps].reverse().find(step => step.state === 'failed')

  const cancel = async () => {
    if (!window.confirm('Cancel this deployment?\n\nDownloads are resumable — installing again later picks up where this left off.')) return
    try {
      await api(`/api/v1/jobs/${encodeURIComponent(job.id)}/cancel`, { method: 'POST', body: '{}' })
    } catch (problem) {
      alert(problem instanceof Error ? problem.message : 'Cancel failed')
    }
  }

  return (
    <dialog ref={ref} onClose={onClose} aria-label={`${verb} ${recipe?.display_name ?? job.recipe_id}`}>
      <div className="dialog-pad">
        <div className="dialog-head">
          <div>
            <p className="kicker">Deployment</p>
            <h2>{verb} {recipe?.display_name ?? job.recipe_id}</h2>
          </div>
          <button className="dialog-close" onClick={onClose} aria-label="Close">×</button>
        </div>
        <div style={{ display: 'flex', gap: 12, alignItems: 'center', flexWrap: 'wrap' }}>
          <span className={`deployment-status ${job.state}`}>{stateCopy[job.state] ?? job.state}</span>
          <span className="muted">
            {current ? operationCopy[current.operation] ?? current.operation : terminal(job.state) ? '' : 'Waiting for manager'}
          </span>
          <code className="faint" style={{ marginLeft: 'auto', fontSize: 11 }}>{job.id.slice(0, 12)}</code>
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
        {job.error && <p className="error-text" role="alert">{job.error}</p>}
        <details>
          <summary className="muted">Technical receipts</summary>
          <ul className="receipts">
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
            {succeeded && (job.kind === 'install' || job.kind === 'start')
              ? `${recipe?.display_name ?? job.recipe_id} is live and serving on this Spark.`
              : 'Closing this window does not stop the deployment.'}
          </span>
          {!terminal(job.state) && <button className="danger" onClick={cancel}>Cancel deployment</button>}
          {succeeded && (job.kind === 'install' || job.kind === 'start') && (
            <button className="brand" onClick={onOpenPlayground}>Try it in the playground</button>
          )}
          <button className="ghost" onClick={onClose}>Close</button>
        </div>
      </div>
    </dialog>
  )
}
