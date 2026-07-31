import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  api, setCSRF, terminal, formatBytes,
  type SystemInfo, type Recipe, type InstalledModel, type Job, type Telemetry, type UpdateInfo,
} from './api'
import Pairing from './views/Pairing'
import Models from './views/Models'
import Playground from './views/Playground'
import Connect from './views/Connect'
import Monitor from './views/Monitor'
import Storage from './views/Storage'
import Activity from './views/Activity'
import DeploymentDialog from './views/Deployment'

const TABS = ['Models', 'Playground', 'Connect', 'Monitor', 'Storage', 'Activity'] as const
type Tab = (typeof TABS)[number]

// Minimal 18px stroke icons, one per section.
function Icon({ name }: { name: Tab }) {
  const paths: Record<Tab, React.ReactNode> = {
    Models: <><rect x="3" y="3" width="7" height="7" rx="1.5" /><rect x="14" y="3" width="7" height="7" rx="1.5" /><rect x="3" y="14" width="7" height="7" rx="1.5" /><rect x="14" y="14" width="7" height="7" rx="1.5" /></>,
    Playground: <path d="M21 12a8 8 0 0 1-8 8H4l2.5-2.5A8 8 0 1 1 21 12z" />,
    Connect: <><path d="M9 12h6" /><path d="M8 7H6a5 5 0 0 0 0 10h2" /><path d="M16 7h2a5 5 0 0 1 0 10h-2" /></>,
    Monitor: <path d="M3 12h4l2-6 4 12 2-6h6" />,
    Storage: <><ellipse cx="12" cy="5.5" rx="8" ry="2.8" /><path d="M4 5.5V18c0 1.6 3.6 2.8 8 2.8s8-1.2 8-2.8V5.5" /><path d="M4 12c0 1.6 3.6 2.8 8 2.8s8-1.2 8-2.8" /></>,
    Activity: <><circle cx="12" cy="12" r="9" /><path d="M12 7v5l3.5 2" /></>,
  }
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      {paths[name]}
    </svg>
  )
}

export interface AppState {
  system: SystemInfo | null
  recipes: Recipe[]
  models: InstalledModel[]
  jobs: Job[]
  refresh: () => Promise<void>
  refreshModelsAndJobs: () => Promise<void>
  openDeployment: (jobID: string) => void
}

export default function App() {
  const [authed, setAuthed] = useState<boolean | null>(null)
  const [tab, setTab] = useState<Tab>('Models')
  const [system, setSystem] = useState<SystemInfo | null>(null)
  const [recipes, setRecipes] = useState<Recipe[]>([])
  const [models, setModels] = useState<InstalledModel[]>([])
  const [jobs, setJobs] = useState<Job[]>([])
  const [connected, setConnected] = useState(true)
  const [telemetry, setTelemetry] = useState<Telemetry | null>(null)
  const [update, setUpdate] = useState<UpdateInfo | null>(null)
  const [selectedJobID, setSelectedJobID] = useState('')
  const streams = useRef(new Map<string, EventSource>())
  const tokenRate = useRef<{ at: number; total: number } | null>(null)
  const [liveTPS, setLiveTPS] = useState<number | null>(null)

  const refreshModelsAndJobs = useCallback(async () => {
    try {
      const [nextModels, nextJobs] = await Promise.all([
        api<InstalledModel[]>('/api/v1/models'),
        api<Job[]>('/api/v1/jobs'),
      ])
      setModels(nextModels)
      setJobs(nextJobs)
    } catch {
      /* the next full refresh will surface connection loss */
    }
  }, [])

  const refresh = useCallback(async () => {
    try {
      const [nextSystem, nextRecipes, nextModels, nextJobs] = await Promise.all([
        api<SystemInfo>('/api/v1/system'),
        api<Recipe[]>('/api/v1/recipes'),
        api<InstalledModel[]>('/api/v1/models'),
        api<Job[]>('/api/v1/jobs'),
      ])
      setSystem(nextSystem)
      setRecipes(nextRecipes)
      setModels(nextModels)
      setJobs(nextJobs)
      setConnected(true)
    } catch {
      setConnected(false)
    }
  }, [])

  // Boot: check the session, then load everything.
  useEffect(() => {
    ;(async () => {
      try {
        const status = await api<{ authenticated: boolean; csrf_token: string }>('/api/v1/auth/status')
        if (!status.authenticated) {
          setAuthed(false)
          return
        }
        setCSRF(status.csrf_token)
        setAuthed(true)
      } catch {
        setAuthed(false)
      }
    })()
  }, [])

  useEffect(() => {
    if (!authed) return
    refresh()
    api<UpdateInfo>('/api/v1/update').then(setUpdate).catch(() => {})
  }, [authed, refresh])

  // Keep an SSE stream open per non-terminal job so progress is live.
  useEffect(() => {
    if (!authed) return
    const active = new Set(jobs.filter(job => !terminal(job.state)).map(job => job.id))
    for (const [id, stream] of streams.current) {
      if (!active.has(id)) {
        stream.close()
        streams.current.delete(id)
      }
    }
    for (const id of active) {
      if (streams.current.has(id)) continue
      const stream = new EventSource(`/api/v1/jobs/${encodeURIComponent(id)}/events`)
      streams.current.set(id, stream)
      stream.addEventListener('job', event => {
        const job = JSON.parse((event as MessageEvent).data) as Job
        setJobs(previous => {
          const index = previous.findIndex(item => item.id === job.id)
          if (index === -1) return [job, ...previous]
          const next = [...previous]
          next[index] = job
          return next
        })
        if (terminal(job.state)) {
          stream.close()
          streams.current.delete(job.id)
          refreshModelsAndJobs()
        }
      })
    }
  }, [jobs, authed, refreshModelsAndJobs])
  useEffect(() => () => {
    for (const stream of streams.current.values()) stream.close()
  }, [])

  // The rail's live pulse: light telemetry poll while the tab is visible.
  useEffect(() => {
    if (!authed) return
    let cancelled = false
    const sample = async () => {
      if (document.hidden) return
      try {
        const next = await api<Telemetry>('/api/v1/telemetry')
        if (cancelled) return
        setTelemetry(next)
        const total = next.active_model?.vllm?.generation_tokens_total
        if (typeof total === 'number') {
          const now = Date.now()
          const last = tokenRate.current
          if (last && now > last.at && total >= last.total) {
            setLiveTPS(((total - last.total) / (now - last.at)) * 1000)
          }
          tokenRate.current = { at: now, total }
        } else {
          tokenRate.current = null
          setLiveTPS(null)
        }
      } catch {
        /* rail simply shows no live numbers */
      }
    }
    sample()
    const timer = setInterval(sample, 5000)
    return () => {
      cancelled = true
      clearInterval(timer)
    }
  }, [authed])

  const activeModel = useMemo(() => models.find(model => model.active), [models])
  const activeRecipe = useMemo(
    () => recipes.find(recipe => recipe.id === activeModel?.recipe_id),
    [recipes, activeModel],
  )
  const working = jobs.some(job => !terminal(job.state))
  const failedRecently = !working && jobs[0]?.state === 'failed'
  const railClass = working ? 'working' : activeModel?.status === 'ready' ? 'serving' : failedRecently ? 'failed' : ''
  const railLabel = working
    ? 'Working'
    : activeModel?.status === 'ready'
      ? activeRecipe?.display_name ?? activeModel.recipe_id
      : 'Idle'

  const runningJobs = jobs.filter(job => !terminal(job.state)).length

  if (authed === null) return null
  if (!authed) return <Pairing onPaired={() => setAuthed(true)} />

  const state: AppState = {
    system, recipes, models, jobs, refresh, refreshModelsAndJobs,
    openDeployment: id => setSelectedJobID(id),
  }
  const selectedJob = jobs.find(job => job.id === selectedJobID) ?? null

  return (
    <div className="shell">
      <a className="skip-link" href="#main">Skip to content</a>
      <aside className="side">
        <a className="side-mark" href="/" aria-label="RunOnSpark Manager">
          <span className="dot" aria-hidden="true" />
          <span>
            <strong>RunOnSpark</strong>
            <small>DGX Spark manager</small>
          </span>
        </a>
        <nav aria-label="Console sections">
          {TABS.map(name => (
            <button key={name} aria-current={tab === name} onClick={() => setTab(name)}>
              <Icon name={name} />
              {name}
              {name === 'Activity' && runningJobs > 0 && <span className="badge">{runningJobs}</span>}
            </button>
          ))}
        </nav>
        <div className="side-foot">
          <code>{system?.hostname ?? '…'}</code>
          {system && system.memory_available_bytes > 0 && (
            <span>{formatBytes(system.memory_available_bytes)} RAM free</span>
          )}
          <span>manager {system?.manager_version ?? ''}</span>
          {update?.update_available && update.release_url && (
            <a className="side-update" href={update.release_url} target="_blank" rel="noreferrer">
              Update {update.latest_version} available
            </a>
          )}
        </div>
      </aside>
      <div className="content">
        <header className="content-head">
          <h1>{tab}</h1>
          <div className={`live-chip ${railClass}`} role="status" aria-live="polite">
            <i className="led" aria-hidden="true" />
            <span>{railLabel}</span>
            {liveTPS !== null && activeModel?.status === 'ready' && (
              <span className="tps">{liveTPS.toFixed(1)} <small>tok/s</small></span>
            )}
            {!connected && <span className="error-text">Disconnected</span>}
          </div>
        </header>
        <main id="main">
          {tab === 'Models' && <Models {...state} />}
          {tab === 'Playground' && (
            <Playground
              ready={activeModel?.status === 'ready'}
              modelID={activeRecipe?.service.served_model_id}
              modelName={activeRecipe?.display_name}
            />
          )}
          {tab === 'Connect' && <Connect activeModelID={activeRecipe?.service.served_model_id} />}
          {tab === 'Monitor' && <Monitor telemetry={telemetry} activeName={activeRecipe?.display_name} />}
          {tab === 'Storage' && <Storage {...state} />}
          {tab === 'Activity' && <Activity {...state} />}
        </main>
      </div>
      <DeploymentDialog job={selectedJob} recipes={recipes} onClose={() => setSelectedJobID('')} />
    </div>
  )
}
