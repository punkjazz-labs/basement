import { stateCopy, operationCopy, type Job } from '../api'
import type { AppState } from '../App'

export default function Activity({ jobs, recipes, openDeployment }: AppState) {
  const name = (job: Job) => recipes.find(recipe => recipe.id === job.recipe_id)?.display_name ?? job.recipe_id
  const detail = (job: Job) => {
    const step = [...job.steps].reverse().find(item => item.state === 'running')
      ?? [...job.steps].reverse().find(item => item.state === 'completed')
    return step ? operationCopy[step.operation] ?? step.operation : 'Waiting for manager'
  }

  return (
    <div className="stack">
      <div className="section-head">
        <span className="spacer" />
        <a className="muted" href="/api/v1/diagnostics" style={{ fontSize: 12.5 }}>Download diagnostics</a>
      </div>
      {jobs.length === 0 && <div className="empty">Nothing has run yet.</div>}
      {jobs.map(job => (
        <button key={job.id} className="job-row" onClick={() => openDeployment(job.id)}>
          <div className="grow">
            <strong>{{ install: 'Install', start: 'Start', stop: 'Stop', remove: 'Remove', 'smoke-test': 'Test', benchmark: 'Measure' }[job.kind] ?? job.kind} {name(job)}</strong>
            <small>{detail(job)} · {new Date(job.updated_at).toLocaleString()}</small>
          </div>
          <span className={`job-state ${job.state}`}>{stateCopy[job.state] ?? job.state}</span>
        </button>
      ))}
    </div>
  )
}
