import { useState } from 'react'
import { stateCopy, type Job } from '../api'
import type { AppState } from '../App'

const KIND: Record<string, string> = {
  install: 'Install',
  start: 'Start',
  stop: 'Stop',
  remove: 'Uninstall',
  'smoke-test': 'Health check',
  benchmark: 'Speed measurement',
}
const PAGE = 25

export default function Activity({ jobs, recipes, openDeployment }: AppState) {
  const [shown, setShown] = useState(PAGE)
  const name = (job: Job) => recipes.find(recipe => recipe.id === job.recipe_id)?.display_name ?? job.recipe_id
  const dayLabel = (iso: string) => {
    const date = new Date(iso)
    const today = new Date()
    const yesterday = new Date()
    yesterday.setDate(today.getDate() - 1)
    if (date.toDateString() === today.toDateString()) return 'Today'
    if (date.toDateString() === yesterday.toDateString()) return 'Yesterday'
    return date.toLocaleDateString(undefined, { day: 'numeric', month: 'long', year: 'numeric' })
  }

  // One compact line per job, grouped under day headers, newest first. The
  // phases and receipts live one click away in the job dialog.
  const visible = jobs.slice(0, shown)
  const groups: { label: string; items: Job[] }[] = []
  for (const job of visible) {
    const label = dayLabel(job.updated_at)
    const last = groups[groups.length - 1]
    if (last && last.label === label) last.items.push(job)
    else groups.push({ label, items: [job] })
  }

  return (
    <div className="stack">
      <div className="section-head">
        <span className="spacer" />
        <a className="muted" href="/api/v1/diagnostics" style={{ fontSize: 12.5 }}>Download diagnostics</a>
      </div>
      {jobs.length === 0 && <div className="empty">Nothing has run yet.</div>}
      {groups.map(group => (
        <section className="card job-day" key={group.label}>
          <div className="day-label">{group.label}</div>
          {group.items.map(job => (
            <button key={job.id} className="job-line" onClick={() => openDeployment(job.id)}>
              <strong>{KIND[job.kind] ?? job.kind} {name(job)}</strong>
              <span className="spacer" />
              <span className="when">
                {new Date(job.updated_at).toLocaleTimeString(undefined, { hour: 'numeric', minute: '2-digit' })}
              </span>
              <span className={`job-state ${job.state}`}>{stateCopy[job.state] ?? job.state}</span>
            </button>
          ))}
        </section>
      ))}
      {jobs.length > shown && (
        <button className="ghost" style={{ justifySelf: 'center' }} onClick={() => setShown(shown + PAGE)}>
          Show earlier activity
        </button>
      )}
    </div>
  )
}
