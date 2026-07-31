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
    <>
      <a className="skip-link" href="#main">Skip to content</a>
      <header className="rail">
        <a className="rail-mark" href="/" aria-label="RunOnSpark Manager">
          <strong>RunOnSpark</strong>
          <span>manager</span>
        </a>
        <div className="rail-host">
          <code>{system?.hostname ?? '…'}</code>
          {system && system.memory_available_bytes > 0 && (
            <span>{formatBytes(system.memory_available_bytes)} RAM free</span>
          )}
        </div>
        <div className={`rail-live ${railClass}`} role="status" aria-live="polite">
          {update?.update_available && update.release_url && (
            <a className="rail-update" href={update.release_url} target="_blank" rel="noreferrer">
              Update <b>{update.latest_version}</b>
            </a>
          )}
          <i className="led" aria-hidden="true" />
          <span>{railLabel}</span>
          {liveTPS !== null && activeModel?.status === 'ready' && (
            <span className="rail-tps">
              {liveTPS.toFixed(1)} <small>tok/s</small>
            </span>
          )}
          {!connected && <span className="error-text">Disconnected</span>}
        </div>
      </header>
      <nav className="tabs" role="tablist" aria-label="Console sections">
        {TABS.map(name => (
          <button
            key={name}
            role="tab"
            aria-selected={tab === name}
            onClick={() => setTab(name)}
          >
            {name}
            {name === 'Activity' && runningJobs > 0 && <span className="badge">{runningJobs}</span>}
          </button>
        ))}
      </nav>
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
      <DeploymentDialog job={selectedJob} recipes={recipes} onClose={() => setSelectedJobID('')} />
    </>
  )
}
