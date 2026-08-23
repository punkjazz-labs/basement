import { useEffect, useRef, useState } from 'react'
import { api, benchmarkReceipt, formatBytes, runtimeLabel, terminal, startTimeoutMinutes, stateCopy, stepCopy, stepElapsedSeconds, stepOperation, type Job, type Recipe, type Step } from '../api'
import { confirmBox, noticeBox } from '../confirm'

// verify_fabric is the two Sparks meeting over the cable and verify_peer_node
// is the second Spark checking itself, both planned only for a two-Spark job;
// on every other job they simply never appear.
const CHECKS = [
  'verify_architecture', 'verify_dgx_spark', 'verify_memory_capacity', 'verify_disk',
  'verify_port', 'verify_docker', 'verify_nvidia_runtime', 'verify_artifact_access',
  'verify_fabric', 'verify_peer_node',
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

function firstStartNote(recipe?: Recipe): string {
  return `Loading into memory. The first start can take up to ${startTimeoutMinutes(recipe)} minutes.`
}

function phasePlan(job: Job, recipe?: Recipe): Phase[] {
  const mediaVerification: Phase = recipe?.media_generation
    ? {
        title: 'Verify generation',
        note: 'Health and a real generation',
        states: ['verifying_health', 'verifying_generation'],
        operations: ['wait_http', 'verify_media_generation'],
      }
    : {
        title: 'Verify endpoint',
        note: 'Health and real inference',
        states: ['verifying_health', 'verifying_inference'],
        operations: ['wait_http', 'verify_openai_inference'],
      }
  if (job.kind === 'install') {
    const firstStart = firstStartNote(recipe)
    return [
      { title: 'Check system', note: 'Hardware, memory, disk and access', states: ['queued', 'preflighting'], operations: CHECKS },
      { title: 'Prepare runtime', note: `Pinned ${runtimeLabel(recipe?.runtime.kind)} image`, states: ['downloading_runtime'], operations: ['pull_image'] },
      { title: 'Download model', note: 'Resumable model files', states: ['downloading_models'], operations: ['download_artifact'] },
      { title: 'Configure service', note: 'Owned configuration and container', states: ['configuring'], operations: ['write_generated_config', 'create_container'] },
      { title: 'Start model', note: 'Safe memory reservation', activeNote: firstStart, states: ['checking_memory', 'starting', 'stopping'], operations: ['stop_container', 'verify_memory', 'start_container'] },
      { ...mediaVerification, activeNote: firstStart },
    ]
  }
  if (job.kind === 'start') {
    return [
      { title: 'Reserve hardware', note: 'Stop the active model, check memory', states: ['queued', 'stopping', 'checking_memory'], operations: ['stop_container', 'verify_memory'] },
      { title: 'Start model', note: 'Launch the pinned runtime', states: ['starting'], operations: ['start_container'] },
      {
        ...mediaVerification,
        activeNote: recipe?.media_generation
          ? 'Waiting for the model to load and verify.'
          : 'Waiting for the model to load and answer.',
      },
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
      recipe?.media_generation
        ? { title: 'Run generation', note: 'Require a completed media file', states: ['verifying_generation'], operations: ['verify_media_generation'] }
        : { title: 'Run inference', note: 'Require a non-empty model response', states: ['verifying_inference'], operations: ['verify_openai_inference'] },
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
  const operation = stepOperation(step.operation)
  const rateRef = useRef<{ key: string; at: number; bytes: number; rate: number; since: number } | null>(null)

  const asNumber = (value: unknown) => (typeof value === 'number' && Number.isFinite(value) ? value : 0)

  // Both Sparks of a two-Spark job run the same step, so a bar with no node
  // on it cannot say which machine is working. Shown only when the receipt
  // carries the node itself.
  const node = (() => {
    const name = typeof receipt.node === 'string' ? receipt.node : ''
    if (!name) return null
    return receipt.node_role === 'worker' ? `${name} · second Spark` : `${name} · this Spark`
  })()

  // Smoothed bytes-per-second across receipt updates, reset when the
  // measured stream changes. A resumed download first sweeps the bytes
  // already on disk, which looks like an absurd transfer rate; the warm-up
  // window ignores those readings so the ETA never lies low.
  const WARMUP_MS = 6000
  const smoothedRate = (key: string, bytes: number) => {
    const now = performance.now()
    const last = rateRef.current
    if (!last || last.key !== key || bytes < last.bytes) {
      rateRef.current = { key, at: now, bytes, rate: 0, since: now }
      return 0
    }
    if (now - last.since < WARMUP_MS) {
      rateRef.current = { ...last, at: now, bytes }
      return 0
    }
    if (bytes > last.bytes && now > last.at) {
      const instant = ((bytes - last.bytes) / (now - last.at)) * 1000
      rateRef.current = { ...last, at: now, bytes, rate: last.rate > 0 ? last.rate * 0.7 + instant * 0.3 : instant }
    }
    return rateRef.current?.rate ?? 0
  }

  const row = (percent: number | null, left: string, right?: string) => (
    <div className="sub-progress">
      {node && <span className="file">{node}</span>}
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
    if (rate <= 1) return 'calculating time left'
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

  if (operation === 'download_artifact') {
    const done = asNumber(receipt.bytes_complete)
    const total = asNumber(receipt.bytes_total)
    if (total <= 0) return null
    const percent = Math.min((done / total) * 100, 100)
    // Resume verification reads existing files at disk speed; those bytes
    // must never enter the transfer-rate average.
    if (receipt.checking_existing === true) {
      rateRef.current = null
      return row(percent, bytePair(done, total), 'checking files already on disk')
    }
    const rate = smoothedRate(`artifact:${step.index}:${receipt.repository ?? ''}`, done)
    return row(percent, bytePair(done, total), speedAndETA(rate, total - done))
  }

  if (operation === 'wait_http') {
    const attempt = asNumber(receipt.attempt)
    if (attempt <= 0) return null
    return (
      <div className="sub-progress">
        {node && <span className="file">{node}</span>}
        <span className="mono nums">Health check #{attempt}. Still loading.</span>
      </div>
    )
  }

  if (operation === 'pull_image') {
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
        bytePair(done, total),
        speedAndETA(rate, total - done),
      )
    }
    return row(null, 'Contacting the registry…')
  }

  return null
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
  if (seconds < 60) return 'almost done'
  if (seconds < 3600) return `${Math.round(seconds / 60)} min left`
  const hours = Math.floor(seconds / 3600)
  const minutes = Math.round((seconds % 3600) / 60)
  return `${hours} h ${minutes} min left`
}

// Elapsed ticks up from the step's own started_at (the server's clock), so
// even receipt-less steps (model start, health wait) visibly make progress
// — and so closing and reopening the deployment dialog mid-step keeps
// counting from the real beginning instead of restarting at zero. A step
// that has already finished reports its fixed final duration and stops
// ticking.
function Elapsed({ step }: { step: Step }) {
  const [, setTick] = useState(0)
  const running = !step.completed_at
  useEffect(() => {
    if (!running) return
    const timer = setInterval(() => setTick(value => value + 1), 1000)
    return () => clearInterval(timer)
  }, [running])
  const seconds = stepElapsedSeconds(step, Date.now())
  if (seconds === null || seconds < 3) return null
  return <>{' · '}{formatDuration(seconds)}</>
}

function activePhaseIndex(job: Job, phases: Phase[]): number {
  if (terminal(job.state) && job.state !== 'failed' && job.state !== 'cancelled') return phases.length
  const failed = [...job.steps].reverse().find(step => step.state === 'failed')
  if (failed) {
    const index = phases.findIndex(phase => phase.operations.includes(stepOperation(failed.operation)))
    if (index >= 0) return index
  }
  const index = phases.findIndex(phase => phase.states.includes(job.state))
  return index >= 0 ? index : 0
}

export default function DeploymentDialog({ job, recipes, onClose, onOpenPlayground, onOpenGenerate }: {
  job: Job | null
  recipes: Recipe[]
  onClose: () => void
  onOpenPlayground: () => void
  onOpenGenerate: () => void
}) {
  const ref = useRef<HTMLDialogElement>(null)

  useEffect(() => {
    const dialog = ref.current
    if (!dialog) return
    if (job && !dialog.open) dialog.showModal()
    if (!job && dialog.open) dialog.close()
  }, [job])

  if (!job) return <dialog ref={ref} onClose={onClose} />

  const recipe = recipes.find(item => item.id === job.recipe_id)
  const isMedia = Boolean(recipe?.media_generation)
  const mediaReady = !isMedia || job.steps.some(step =>
    stepOperation(step.operation) === 'verify_media_generation' && step.state === 'completed',
  )
  const phases = phasePlan(job, recipe)
  const activeIndex = activePhaseIndex(job, phases)
  const succeeded = terminal(job.state) && job.state !== 'failed' && job.state !== 'cancelled'
  const verb = { install: 'Deploy', start: 'Start', stop: 'Stop', remove: 'Remove', 'smoke-test': 'Test', benchmark: 'Measure' }[job.kind] ?? 'Manage'
  // A benchmark or smoke test is not a deployment; every piece of copy in
  // this dialog follows the job's own noun.
  const noun = { install: 'deployment', start: 'start', stop: 'stop', remove: 'removal', 'smoke-test': 'test', benchmark: 'measurement' }[job.kind] ?? 'job'
  const kicker = { 'smoke-test': 'Model test', benchmark: 'Speed measurement' }[job.kind] ?? 'Deployment'
  const current = [...job.steps].reverse().find(step => step.state === 'running')
    ?? [...job.steps].reverse().find(step => step.state === 'failed')
  const benchReceipt = benchmarkReceipt(job)

  const cancel = async () => {
    const { ok } = await confirmBox({
      title: `Cancel this ${noun}?`,
      body: job.kind === 'install'
        ? 'Downloads are resumable and resume later.'
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
            const showsProgress = status === 'active' && current && phase.operations.includes(stepOperation(current.operation))
            // Transfers estimate their own remaining time; elapsed time is
            // only for steps with no ETA, where it proves liveness.
            const showsElapsed = showsProgress && current && !['download_artifact', 'pull_image'].includes(stepOperation(current.operation))
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
                  {showsElapsed && <Elapsed step={current} />}
                </b>
              </li>
            )
          })}
        </ol>
        {job.state === 'cancelled' ? (
          <p className="muted" role="status">
            {job.kind === 'install'
              ? 'Cancelled. Downloads are kept; installing again resumes.'
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
              <div className="v">{typeof benchReceipt.tokens_per_second === 'number' ? benchReceipt.tokens_per_second.toFixed(1) : 'n/a'} <small>tok/s</small></div>
            </div>
            <div className="cell">
              <div className="l">First token</div>
              <div className="v">{typeof benchReceipt.time_to_first_token_ms === 'number' ? Math.round(benchReceipt.time_to_first_token_ms) : 'n/a'} <small>ms</small></div>
            </div>
            <div className="cell">
              <div className="l">Sample</div>
              <div className="v">{typeof benchReceipt.completion_tokens === 'number' ? benchReceipt.completion_tokens : 'n/a'} <small>tokens</small></div>
            </div>
          </div>
        )}
        <details>
          <summary className="muted">Technical receipts</summary>
          <ul className="receipts">
            <li className="faint">Job reference <code>{job.id}</code></li>
            {job.steps.map(step => (
              <li key={step.index}>
                <strong style={{ fontSize: 13 }}>{stepCopy(step.operation)}</strong>{' '}
                <span className="faint">{step.state}</span>
                <pre>{step.receipt && Object.keys(step.receipt).length ? JSON.stringify(step.receipt, null, 2) : 'No receipt yet'}</pre>
              </li>
            ))}
            {job.steps.length === 0 && <li className="faint">No steps yet.</li>}
          </ul>
        </details>
        <div className="dialog-foot">
          <span className="note">
            {!terminal(job.state)
              ? `Closing this window does not stop the ${noun}.`
              : succeeded
                ? {
                    install: `${recipe?.display_name ?? job.recipe_id} is live and serving.`,
                    start: `${recipe?.display_name ?? job.recipe_id} is live and serving.`,
                    benchmark: 'Measured with a real request.',
                    'smoke-test': isMedia
                      ? 'The model completed a real generation.'
                      : 'The model answered a real inference request.',
                    stop: 'The model has stopped.',
                    remove: 'The model has been removed.',
                  }[job.kind] ?? 'Finished.'
                : ''}
          </span>
          {!terminal(job.state) && <button className="danger" onClick={cancel}>Cancel {noun}</button>}
          {succeeded && mediaReady && (job.kind === 'install' || job.kind === 'start') && (
            <button className="brand" onClick={isMedia ? onOpenGenerate : onOpenPlayground}>
              {isMedia ? 'Generate video' : 'Try it in the playground'}
            </button>
          )}
          <button className="ghost" onClick={onClose}>Close</button>
        </div>
      </div>
    </dialog>
  )
}
