import { useCallback, useEffect, useMemo, useReducer, useRef, useState } from 'react'
import {
  api, idempotency, setCSRF, terminal, formatBytes, OfflineError,
  type FleetDeploymentView, type SystemInfo, type Recipe, type InstalledModel, type Job, type Peer,
  type Telemetry, type UpdateInfo,
} from './api'
import Pairing from './views/Pairing'
import Models from './views/Models'
import Roles from './views/Roles'
import Playground from './views/Playground'
import Generate from './views/Generate'
import Redactor from './views/Redactor'
import Connect from './views/Connect'
import Monitor from './views/Monitor'
import Fleet, { FleetInvitationPrompt } from './views/Fleet'
import Storage from './views/Storage'
import Activity from './views/Activity'
import ManagerUpdateDialog, { ManagerUpdateSidebar } from './views/ManagerUpdate'
import DeploymentDialog from './views/Deployment'
import { ConfirmHost } from './confirm'
import { initialManagerUpdateDialogState, managerUpdateDialogReducer } from './managerUpdate'
import { servingChatModels } from './council'

const TABS = ['Models', 'Roles', 'Playground', 'Generate', 'Redactor', 'Connect', 'Monitor', 'Fleet', 'Storage', 'Activity'] as const
type Tab = (typeof TABS)[number]

// Redactor has no line here on purpose: its own bar names the open document,
// and nothing above it needs to repeat what the screen already shows.
const DESC: Partial<Record<Tab, string>> = {
  Roles: 'Endpoints that stay the same while you change the model.',
  Connect: 'Endpoint, keys and snippets.',
  Monitor: 'Live GPU health and serving metrics.',
}

export interface AppState {
  system: SystemInfo | null
  recipes: Recipe[]
  models: InstalledModel[]
  jobs: Job[]
  // Every Spark added to the fleet. Models needs this as much as Fleet does:
  // how many Sparks are available decides which recipes can be installed at
  // all, and a paired Spark is a place a one-Spark model can run.
  peers: Peer[]
  refresh: () => Promise<void>
  refreshModelsAndJobs: () => Promise<void>
  refreshPeers: () => Promise<void>
  openDeployment: (jobID: string) => void
  // Progress for work this console asked another Spark to do. The job row
  // lives on that Spark, so it is handed over whole with the placement that
  // owns it, and the placement is what the progress window then follows.
  openFleetDeployment: (deploymentID: string, job: Job) => void
  openPlayground: () => void
  openGenerate: () => void
  openFleet: () => void
}

export default function App() {
  const [authed, setAuthed] = useState<boolean | null>(null)
  const [tab, setTab] = useState<Tab>('Models')
  const [system, setSystem] = useState<SystemInfo | null>(null)
  const [recipes, setRecipes] = useState<Recipe[]>([])
  const [models, setModels] = useState<InstalledModel[]>([])
  const [jobs, setJobs] = useState<Job[]>([])
  const [peers, setPeers] = useState<Peer[]>([])
  const [connected, setConnected] = useState(true)
  const [telemetry, setTelemetry] = useState<Telemetry | null>(null)
  const [update, setUpdate] = useState<UpdateInfo | null>(null)
  const [updateDialog, dispatchUpdateDialog] = useReducer(
    managerUpdateDialogReducer,
    initialManagerUpdateDialogState,
  )
  const [selectedJobID, setSelectedJobID] = useState('')
  // The one job on another Spark this console is watching, and the placement
  // it was started through. Only one at a time, because only one progress
  // window is ever open.
  const [remoteJob, setRemoteJob] = useState<{ deploymentID: string; job: Job } | null>(null)
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

  const refreshPeers = useCallback(async () => {
    const next = await api<Peer[]>('/api/v1/peers')
    setPeers(next)
  }, [])

  const refresh = useCallback(async () => {
    try {
      const [nextSystem, nextRecipes, nextModels, nextJobs, nextPeers] = await Promise.all([
        api<SystemInfo>('/api/v1/system'),
        api<Recipe[]>('/api/v1/recipes'),
        api<InstalledModel[]>('/api/v1/models'),
        api<Job[]>('/api/v1/jobs'),
        api<Peer[]>('/api/v1/peers'),
      ])
      setSystem(nextSystem)
      setRecipes(nextRecipes)
      setModels(nextModels)
      setJobs(nextJobs)
      setPeers(nextPeers)
      setConnected(true)
    } catch (problem) {
      // A real HTTP error still means this Spark answered; only a fetch
      // that never got a response means the rail should read Disconnected.
      if (problem instanceof OfflineError) setConnected(false)
    }
  }, [])

  const refreshAfterManagerUpdate = useCallback(() => {
    void refresh()
    // The cached answer necessarily predates the update that just applied.
    void api<UpdateInfo>('/api/v1/update?refresh=1').then(setUpdate).catch(() => {})
  }, [refresh])

  const openManagerUpdate = useCallback(() => {
    dispatchUpdateDialog({ type: 'open_from_sidebar' })
  }, [])

  const closeManagerUpdate = useCallback(() => {
    dispatchUpdateDialog({ type: 'close' })
  }, [])

  const setManagerUpdateReconnecting = useCallback((value: boolean) => {
    dispatchUpdateDialog({ type: 'reconnecting', value })
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
    // A console left open should still learn about a new release: re-ask
    // hourly rather than only at page load. The manager caches upstream, so
    // this costs at most one GitHub call an hour per machine.
    const checkUpdate = () => api<UpdateInfo>('/api/v1/update').then(setUpdate).catch(() => {})
    checkUpdate()
    const timer = setInterval(checkUpdate, 60 * 60 * 1000)
    return () => clearInterval(timer)
  }, [authed, refresh])

  // The manager refreshes its recipe catalog from the signed remote index in
  // the background every few hours (spec 04); this light poll is what lets
  // an already-open console notice a "Recipe updated" row without the user
  // reloading the page. Silent on failure, like every other poll here — the
  // next successful tick just catches up.
  useEffect(() => {
    if (!authed) return
    let cancelled = false
    const poll = () => {
      if (document.hidden) return
      api<Recipe[]>('/api/v1/recipes').then(next => {
        if (!cancelled) setRecipes(next)
      }).catch(() => {})
    }
    const timer = setInterval(poll, 5 * 60 * 1000)
    return () => {
      cancelled = true
      clearInterval(timer)
    }
  }, [authed])

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

  // The same wiring for a job on another Spark, over the placement stream
  // instead of the local job stream. That stream carries the whole placement,
  // so the job inside it is what the progress window reads, and the stream
  // closes on the same terminal states the local one does.
  const watchedDeploymentID = remoteJob?.deploymentID
  useEffect(() => {
    if (!authed || !watchedDeploymentID) return
    const path = `/api/v1/fleet/deployments/${encodeURIComponent(watchedDeploymentID)}/events`
    const stream = new EventSource(path)
    stream.addEventListener('deployment', event => {
      const view = JSON.parse((event as MessageEvent).data) as FleetDeploymentView
      const job = view.job
      if (!job) return
      setRemoteJob(previous =>
        previous?.deploymentID === watchedDeploymentID ? { deploymentID: watchedDeploymentID, job } : previous,
      )
      if (terminal(job.state)) stream.close()
    })
    return () => stream.close()
  }, [authed, watchedDeploymentID])

  // A job on another Spark is cancelled through the placement that owns it;
  // this console's own /api/v1/jobs knows nothing about it.
  const cancelRemoteJob = useCallback(async () => {
    if (!remoteJob) return
    await api(`/api/v1/fleet/deployments/${encodeURIComponent(remoteJob.deploymentID)}/cancel`, {
      method: 'POST',
      headers: idempotency(),
      body: '{}',
    })
  }, [remoteJob])

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
        const total = next.active_model?.runtime_metrics?.generation_tokens_total
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
  const activeMedia = Boolean(activeModel?.status === 'ready' && activeRecipe?.media_generation)
  // Every text model answering right now, which is all the playground needs
  // to know to decide whether a council is on offer. One model or none and
  // it behaves exactly as it always has.
  const chatModels = useMemo(
    () => servingChatModels(models.map(model => {
      const recipe = recipes.find(item => item.id === model.recipe_id)
      return {
        serving: model.active && model.status === 'ready',
        id: recipe?.service.served_model_id,
        name: recipe?.display_name,
        media: Boolean(recipe?.media_generation),
      }
    })),
    [models, recipes],
  )
  const visibleTabs = TABS.filter(name =>
    name === 'Generate' ? activeMedia : name === 'Playground' ? !activeMedia : true,
  )

  useEffect(() => {
    if (tab === 'Generate' && !activeMedia) setTab('Models')
    if (tab === 'Playground' && activeMedia) setTab('Generate')
  }, [tab, activeMedia])

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
    system, recipes, models, jobs, peers, refresh, refreshModelsAndJobs, refreshPeers,
    openDeployment: id => {
      setRemoteJob(null)
      setSelectedJobID(id)
    },
    openFleetDeployment: (deploymentID, job) => {
      setSelectedJobID('')
      setRemoteJob({ deploymentID, job })
    },
    openPlayground: () => setTab('Playground'),
    openGenerate: () => setTab('Generate'),
    openFleet: () => setTab('Fleet'),
  }
  // One progress window, whichever Spark is doing the work. A job on another
  // Spark serves that Spark's endpoint, so this console's own playground and
  // generate tabs are not where it finishes.
  const selectedJob = jobs.find(job => job.id === selectedJobID) ?? remoteJob?.job ?? null
  const showingRemoteJob = remoteJob !== null && selectedJob === remoteJob.job

  return (
    <div className="shell">
      <a className="skip-link" href="#main">Skip to content</a>
      <aside className="side">
        <a className="side-mark" href="/" aria-label="basement">
          <strong>basement</strong>
        </a>
        <nav aria-label="Console sections">
          {visibleTabs.map(name => (
            <button key={name} aria-current={tab === name} onClick={() => setTab(name)}>
              {name}
              {name === 'Activity' && runningJobs > 0 && <span className="badge">{runningJobs}</span>}
            </button>
          ))}
        </nav>
        <div className="side-foot">
          <div className={`foot-live ${railClass}`} role="status" aria-live="polite">
            <i className="led" aria-hidden="true" />
            <span>{railLabel}</span>
            {liveTPS !== null && activeModel?.status === 'ready' && !activeMedia && (
              <span className="tps">{liveTPS.toFixed(1)} tok/s</span>
            )}
          </div>
          <span>{system?.hostname ?? '…'}
            {system && system.memory_available_bytes > 0 && ` · ${formatBytes(system.memory_available_bytes)} free`}
          </span>
          <ManagerUpdateSidebar info={update} managerVersion={system?.manager_version} onOpen={openManagerUpdate} />
        </div>
      </aside>
      <div className="content">
        <header className="content-head">
          <div className="head-row">
            <h1>{tab}</h1>
            {!connected && !updateDialog.reconnecting && <span className="offline" role="status">Disconnected</span>}
          </div>
          {DESC[tab] && <p className="desc">{DESC[tab]}</p>}
        </header>
        <main id="main" className={tab === 'Generate' ? 'wide-generate' : undefined}>
          {tab === 'Models' && <Models {...state} />}
          {tab === 'Roles' && <Roles {...state} />}
          {/* The playground stays mounted so switching tabs never wipes the
              conversation and a streaming reply keeps flowing. */}
          <div style={{ display: tab === 'Playground' ? 'contents' : 'none' }}>
            <Playground
              ready={activeModel?.status === 'ready'}
              modelID={activeRecipe?.service.served_model_id}
              modelName={activeRecipe?.display_name}
              recipeID={activeRecipe?.id}
              chatModels={chatModels}
            />
          </div>
          {tab === 'Generate' && activeRecipe?.media_generation && (
            <Generate recipe={activeRecipe} recipes={recipes} />
          )}
          {/* The redactor stays mounted too: the open document, its findings
              and everything hidden by hand live in this session only, so a
              trip to another tab must not throw them away. */}
          <div style={{ display: tab === 'Redactor' ? 'contents' : 'none' }}>
            <Redactor />
          </div>
          {tab === 'Connect' && <Connect activeModelID={activeRecipe?.service.served_model_id} />}
          {tab === 'Monitor' && <Monitor telemetry={telemetry} activeName={activeRecipe?.display_name} />}
          {tab === 'Fleet' && <Fleet {...state} liveTPS={liveTPS} />}
          {tab === 'Storage' && <Storage {...state} />}
          {tab === 'Activity' && <Activity {...state} />}
        </main>
      </div>
      <ManagerUpdateDialog
        open={updateDialog.open}
        reconnecting={updateDialog.reconnecting}
        info={update}
        onInfoChange={setUpdate}
        onReconnectingChange={setManagerUpdateReconnecting}
        onManagerReady={refreshAfterManagerUpdate}
        onClose={closeManagerUpdate}
        onOpenModels={() => {
          closeManagerUpdate()
          setTab('Models')
        }}
        onOpenActivity={() => {
          closeManagerUpdate()
          setTab('Activity')
        }}
        onOpenGeneration={() => {
          closeManagerUpdate()
          setTab('Generate')
        }}
      />
      <DeploymentDialog
        job={selectedJob}
        recipes={recipes}
        onCancel={showingRemoteJob ? cancelRemoteJob : undefined}
        onClose={() => {
          setSelectedJobID('')
          setRemoteJob(null)
        }}
        onOpenPlayground={showingRemoteJob ? undefined : () => {
          setSelectedJobID('')
          setTab('Playground')
        }}
        onOpenGenerate={showingRemoteJob ? undefined : () => {
          setSelectedJobID('')
          refreshModelsAndJobs().then(() => setTab('Generate'))
        }}
      />
      {/* A Spark asking to adopt this one is answered wherever the owner
          happens to be in the console, not only on the Fleet tab. */}
      <FleetInvitationPrompt onAnswered={refresh} />
      <ConfirmHost />
    </div>
  )
}
