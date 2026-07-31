import { useEffect, useRef } from 'react'
import { api, terminal, stateCopy, operationCopy, type Job, type Recipe } from '../api'

const CHECKS = [
  'verify_architecture', 'verify_dgx_spark', 'verify_memory_capacity', 'verify_disk',
  'verify_port', 'verify_docker', 'verify_nvidia_runtime', 'verify_artifact_access',
]

interface Phase {
  title: string
  note: string
  states: string[]
  operations: string[]
}

function phasePlan(job: Job): Phase[] {
  if (job.kind === 'install') {
    return [
      { title: 'Check system', note: 'Hardware, memory, disk and access', states: ['queued', 'preflighting'], operations: CHECKS },
      { title: 'Prepare runtime', note: 'Pinned vLLM image', states: ['downloading_runtime'], operations: ['pull_image'] },
      { title: 'Download model', note: 'Resumable model files', states: ['downloading_models'], operations: ['download_artifact'] },
      { title: 'Configure service', note: 'Owned configuration and container', states: ['configuring'], operations: ['write_generated_config', 'create_container'] },
      { title: 'Start model', note: 'Safe memory reservation', states: ['checking_memory', 'starting', 'stopping'], operations: ['stop_container', 'verify_memory', 'start_container'] },
      { title: 'Verify endpoint', note: 'Health and real inference', states: ['verifying_health', 'verifying_inference'], operations: ['wait_http', 'verify_openai_inference'] },
    ]
  }
  if (job.kind === 'start') {
    return [
      { title: 'Reserve hardware', note: 'Stop the active model, check memory', states: ['queued', 'stopping', 'checking_memory'], operations: ['stop_container', 'verify_memory'] },
      { title: 'Start model', note: 'Launch the pinned runtime', states: ['starting'], operations: ['start_container'] },
      { title: 'Verify endpoint', note: 'Health and real inference', states: ['verifying_health', 'verifying_inference'], operations: ['wait_http', 'verify_openai_inference'] },
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

export default function DeploymentDialog({ job, recipes, onClose }: {
  job: Job | null
  recipes: Recipe[]
  onClose: () => void
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
  const recipe = recipes.find(item => item.id === job.recipe_id)
  const verb = { install: 'Deploy', start: 'Start', stop: 'Stop', remove: 'Remove', 'smoke-test': 'Test', benchmark: 'Measure' }[job.kind] ?? 'Manage'
  const current = [...job.steps].reverse().find(step => step.state === 'running')
    ?? [...job.steps].reverse().find(step => step.state === 'failed')

  const cancel = async () => {
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
            return (
              <li key={phase.title} className={status}>
                <i aria-hidden="true" />
                <div>
                  <strong>{phase.title}</strong>
                  <span>{phase.note}</span>
                </div>
                <b>{label}</b>
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
          <span className="note">Closing this window does not stop the deployment.</span>
          {!terminal(job.state) && <button className="danger" onClick={cancel}>Cancel deployment</button>}
          <button className="ghost" onClick={onClose}>Close</button>
        </div>
      </div>
    </dialog>
  )
}
