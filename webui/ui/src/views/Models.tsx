import { Fragment, useEffect, useMemo, useRef, useState } from 'react'
import {
  api, idempotency, terminal, formatBytes, formatTokens, runtimeLabel, startTimeoutMinutes, modelStateWord, peerModelList,
  updatePlan,
  type InstalledModel, type Job, type Peer, type Preflight, type Recipe, type StorageInfo, type PeerSummary,
  type TokenUsage,
} from '../api'
import type { AppState } from '../App'
import { confirmBox, noticeBox } from '../confirm'
import { LOGOS, RECOMMENDED_ID, readableWeights, sortCatalog } from '../catalog'

const USE: Record<string, string> = {
  'qwen36-35b-a3b-nvfp4-1s': 'Fast enough to become your default. Best all-rounder.',
  'qwen36-27b-nvfp4-1s': 'Flagship-level coding in a smaller footprint.',
  'laguna-s-2-1-nvfp4-dflash-1s': 'Built for long, independent agent runs.',
  'nemotron-omni-30b-a3b-nvfp4-1s': "NVIDIA's own reasoning model, tuned for this hardware.",
  'qwen35-122b-a10b-nvfp4-1s': 'The biggest model a single Spark can hold.',
  'deepseek-v4-flash-0731-2s': 'The flagship run. Needs two Sparks linked together.',
}
// Community-reported typical speeds on a DGX Spark, shown until this device
// measures its own number. Each figure traces to a corroborated measurement
// recorded in docs/MODEL-CANDIDATES-2026-08.md.
const REFERENCE_TPS: Record<string, number> = {
  'qwen36-35b-a3b-nvfp4-1s': 80,
  'qwen36-27b-nvfp4-1s': 33,
  'laguna-s-2-1-nvfp4-dflash-1s': 19.4,
  'nemotron-omni-30b-a3b-nvfp4-1s': 57, // 56.94 median, dev.classmethod.jp, Omni NVFP4
  'qwen35-122b-a10b-nvfp4-1s': 28, // 28.3 corroborated baseline, ice-ice-bear bench + NVIDIA forum 365639
  'deepseek-v4-flash-0731-2s': 68, // 67.58 measured on 2x Spark vLLM NVFP4; inside the forum's 42-76 range
}
interface ConfirmState {
  recipe: Recipe
  preflight: Preflight
  switchFrom?: string
}

// Where an install is about to run. A recipe that needs two Sparks always
// runs across both, so this choice only exists for one-Spark recipes.
type Placement = 'local' | 'peer'

export default function Models({
  system, recipes, models, jobs, peers, refreshModelsAndJobs, openDeployment, openPlayground, openFleet,
}: AppState) {
  const [confirm, setConfirm] = useState<ConfirmState | null>(null)
  const [licence, setLicence] = useState(false)
  // Whether the install switches to the new model as soon as it is ready.
  // Only meaningful when another model is serving; defaults to the
  // historical behaviour so a single click still installs and serves.
  const [activate, setActivate] = useState(true)
  const [pending, setPending] = useState<Set<string>>(new Set())
  const [expanded, setExpanded] = useState('')
  const [storage, setStorage] = useState<StorageInfo | null>(null)
  const [tokens, setTokens] = useState<TokenUsage | null>(null)
  const [placement, setPlacement] = useState<Placement>('local')
  const [peerPreflight, setPeerPreflight] = useState<Preflight | null>(null)
  const [peerChecking, setPeerChecking] = useState(false)
  const [peerError, setPeerError] = useState('')
  const [peerSummary, setPeerSummary] = useState<PeerSummary | null>(null)
  // Recipes this console has asked the paired Spark to install during this
  // session. That Spark only lists a model once its install finishes, and
  // the fleet API has no remote job stream, so this is all the row can
  // honestly say in between. It clears as soon as the model shows up.
  const [delegated, setDelegated] = useState<Set<string>>(new Set())
  const dialogRef = useRef<HTMLDialogElement>(null)

  // Storage tells us which recipes already have model files on disk, so a
  // partially downloaded model can offer to resume instead of start over.
  useEffect(() => {
    api<StorageInfo>('/api/v1/storage').then(setStorage).catch(() => {})
  }, [jobs.length])

  // Usage totals only move while a model serves, and the manager samples the
  // runtime on its own slow tick, so this reads them at a similar pace
  // rather than with every render.
  useEffect(() => {
    let cancelled = false
    const read = () => {
      if (document.hidden) return
      api<TokenUsage>('/api/v1/tokens')
        .then(next => {
          if (!cancelled) setTokens(next)
        })
        .catch(() => {})
    }
    read()
    const timer = setInterval(read, 30000)
    return () => {
      cancelled = true
      clearInterval(timer)
    }
  }, [])

  // The fleet holds one other Spark at most today. Everything this view says
  // about a peer names that one machine, so with any other number it says
  // nothing rather than picking a Spark to speak for.
  const peer: Peer | undefined = peers.length === 1 ? peers[0] : undefined
  const peerID = peer?.id

  // The same merged read the Fleet tab polls, for the same reason: it is the
  // only source for what the other Spark has installed and is serving.
  useEffect(() => {
    if (!peerID) {
      setPeerSummary(null)
      return
    }
    let cancelled = false
    const sample = async () => {
      if (document.hidden) return
      try {
        const next = await api<PeerSummary>(`/api/v1/peers/${encodeURIComponent(peerID)}/summary`)
        if (!cancelled) setPeerSummary(next)
      } catch {
        if (!cancelled) setPeerSummary({ reachable: false })
      }
    }
    sample()
    const timer = setInterval(sample, 10000)
    return () => {
      cancelled = true
      clearInterval(timer)
    }
  }, [peerID])

  const peerModels = peerModelList(peerSummary)
  const peerInstalled = new Map(peerModels.map(model => [model.recipe_id, model]))

  // A delegated install has landed once the peer lists that model itself;
  // from then on its own status is the truthful thing to show. The map read
  // here is derived from this very summary, so it is always the fresh one.
  useEffect(() => {
    setDelegated(previous => {
      if (previous.size === 0) return previous
      const next = new Set([...previous].filter(id => !peerInstalled.has(id)))
      return next.size === previous.size ? previous : next
    })
  }, [peerSummary]) // eslint-disable-line react-hooks/exhaustive-deps

  const installed = useMemo(() => new Map(models.map(model => [model.recipe_id, model])), [models])
  const usage = useMemo(() => new Map((tokens?.models ?? []).map(item => [item.recipe_id, item])), [tokens])
  const sorted = useMemo(() => sortCatalog(recipes), [recipes])
  const detected = system?.hardware_scope.detected_spark_count ?? 0
  // Every Spark this console can install on: the one it runs on, plus the
  // ones paired on the Fleet tab. Pairing a second Spark is what makes a
  // two-Spark recipe installable; the distributed preflight still runs when
  // the install starts and can still refuse.
  const available = detected + peers.length
  const fitsOn = (recipe: Recipe) => available >= recipe.topology.spark_count
  // Short of exactly one machine, with none paired yet: something the user
  // can fix right now rather than a dead end.
  const pairable = (recipe: Recipe) =>
    !fitsOn(recipe) && peers.length === 0 && detected >= 1 && detected + 1 >= recipe.topology.spark_count
  const blockers = system?.blocking_conditions ?? []
  const activeOther = (id: string) => models.find(model => model.active && model.recipe_id !== id)
  const nameOf = (id?: string) => recipes.find(recipe => recipe.id === id)?.display_name ?? id ?? ''
  const downloadedBytes = (recipe: Recipe) =>
    storage?.artifacts.filter(a => a.recipe_ids.includes(recipe.id)).reduce((sum, a) => sum + a.bytes, 0) ?? 0
  // Label honesty for a not-installed model with files already on disk:
  // partial data resumes, a kept complete download reinstalls quickly. An
  // already-installed model whose recipe has since moved to a newer
  // version reads as an update, not a fresh install, whether or not it is
  // currently serving.
  const installVerb = (recipe: Recipe) => {
    const own = installed.get(recipe.id)
    if (own) return own.recipe_version < recipe.version ? 'Update' : 'Install'
    const bytes = downloadedBytes(recipe)
    if (bytes <= 0) return 'Install'
    return bytes >= recipe.artifact_bytes * 0.99 ? 'Reinstall' : 'Resume install'
  }
  // On the other Spark this console only knows what that Spark reports:
  // whether it has the model, and at which recipe version. Partial downloads
  // over there are its own business, so a resume is never claimed.
  const peerInstallVerb = (recipe: Recipe) => {
    const there = peerInstalled.get(recipe.id)
    return there && there.recipe_version < recipe.version ? 'Update' : 'Install'
  }
  const verbFor = (recipe: Recipe, where: Placement) =>
    where === 'peer' ? peerInstallVerb(recipe) : installVerb(recipe)
  // The licence was accepted when the first install of this recipe was
  // confirmed; a resume or reinstall never asks again.
  const licenceAccepted = (recipe: Recipe) =>
    jobs.some(job => job.recipe_id === recipe.id && job.kind === 'install')

  const setBusy = (id: string, busy: boolean) => {
    setPending(previous => {
      const next = new Set(previous)
      if (busy) next.add(id)
      else next.delete(id)
      return next
    })
  }

  const acceptJob = (result: { job: Job }) => {
    openDeployment(result.job.id)
    refreshModelsAndJobs()
  }

  const run = async (id: string, work: () => Promise<void>) => {
    if (pending.has(id)) return
    setBusy(id, true)
    try {
      await work()
    } catch (problem) {
      noticeBox('That did not work', problem instanceof Error ? problem.message : String(problem))
    } finally {
      setBusy(id, false)
    }
  }

  const startInstall = (recipe: Recipe) =>
    run(recipe.id, async () => {
      const preflight = await api<Preflight>(`/api/v1/preflight?recipe_id=${encodeURIComponent(recipe.id)}`)
      // switchFrom drives the "switch now vs later" choice below. It is
      // either a different model actively serving, or this same model
      // updating itself while it is the one actively serving — both stop
      // something before the new download can take over.
      const own = installed.get(recipe.id)
      const switchFrom = activeOther(recipe.id)?.recipe_id ?? (own?.active ? recipe.id : undefined)
      setConfirm({ recipe, preflight, switchFrom })
      setLicence(false)
      setActivate(true)
      setPlacement('local')
      setPeerPreflight(null)
      setPeerError('')
      setPeerChecking(false)
      requestAnimationFrame(() => dialogRef.current?.showModal())
    })

  // Every machine answers its own preflight, so choosing the other Spark
  // means asking that Spark before any number in the dialog can be trusted.
  const choosePlacement = (where: Placement) => {
    setPlacement(where)
    if (where === 'local' || !confirm || !peer || peerPreflight || peerChecking) return
    setPeerChecking(true)
    setPeerError('')
    const path = `/api/v1/peers/${encodeURIComponent(peer.id)}/preflight?recipe_id=${encodeURIComponent(confirm.recipe.id)}`
    api<Preflight>(path)
      .then(setPeerPreflight)
      .catch(problem => setPeerError(problem instanceof Error ? problem.message : `Could not reach ${peer.name}`))
      .finally(() => setPeerChecking(false))
  }

  // Which model has to give way, on the machine the install targets. Here it
  // was worked out when the dialog opened; on the other Spark it comes from
  // that Spark's own model list.
  const switchFromFor = (where: Placement): string | undefined => {
    if (!confirm) return undefined
    if (where === 'local') return confirm.switchFrom
    const other = peerModels.find(model => model.active && model.recipe_id !== confirm.recipe.id)
    if (other) return other.recipe_id
    return peerInstalled.get(confirm.recipe.id)?.active ? confirm.recipe.id : undefined
  }

  const confirmInstall = () =>
    confirm &&
    run(confirm.recipe.id, async () => {
      const recipe = confirm.recipe
      // The picker is only ever shown with a peer in hand; without one there
      // is nothing to delegate to, and quietly installing here instead would
      // not be what was asked for.
      if (placement === 'peer' && !peer) return
      const body = JSON.stringify({
        confirmed: true,
        accept_licence: true,
        activate: switchFromFor(placement) ? activate : true,
      })
      if (placement === 'peer' && peer) {
        await api<{ job?: Job }>(
          `/api/v1/peers/${encodeURIComponent(peer.id)}/models/${encodeURIComponent(recipe.id)}/install`,
          { method: 'POST', headers: idempotency(), body },
        )
        dialogRef.current?.close()
        setConfirm(null)
        setDelegated(previous => new Set(previous).add(recipe.id))
        noticeBox(
          `${peer.name} is installing ${recipe.display_name}`,
          `The job runs on that Spark, so its progress and logs live on its own console. This row shows the model as soon as ${peer.name} reports it.`,
        )
        return
      }
      const result = await api<{ job: Job }>(`/api/v1/models/${recipe.id}/install`, {
        method: 'POST',
        headers: idempotency(),
        body,
      })
      dialogRef.current?.close()
      setConfirm(null)
      acceptJob(result)
    })

  const simpleAction = (id: string, action: string) =>
    run(id, async () => {
      const result = await api<{ job: Job }>(`/api/v1/models/${id}/${action}`, {
        method: 'POST',
        headers: idempotency(),
        body: '{}',
      })
      acceptJob(result)
    })

  const startOrSwitch = async (recipe: Recipe) => {
    const active = activeOther(recipe.id)
    if (active) {
      const from = recipes.find(item => item.id === active.recipe_id)?.display_name ?? active.recipe_id
      const { ok } = await confirmBox({
        title: `Switch to ${recipe.display_name}?`,
        body: `${from} will stop. If the new model fails verification, basement restores the previous one.`,
        confirmLabel: 'Switch model',
      })
      if (!ok) return
    }
    simpleAction(recipe.id, 'start')
  }

  const remove = async (recipe: Recipe) => {
    const serving = installed.get(recipe.id)?.active
    const { ok, checked } = await confirmBox({
      title: `Uninstall ${recipe.display_name}?`,
      body: serving
        ? 'It is currently serving and will be stopped first. The runtime and configuration are removed.'
        : 'The runtime and configuration are removed.',
      confirmLabel: 'Uninstall',
      danger: true,
      checkbox: {
        label: `Also delete ${formatBytes(recipe.artifact_bytes)} of downloaded model files`,
        note: 'Keeping them makes a future reinstall much faster.',
      },
    })
    if (!ok) return
    run(recipe.id, async () => {
      const result = await api<{ job: Job }>(`/api/v1/models/${recipe.id}`, {
        method: 'DELETE',
        headers: idempotency(),
        body: JSON.stringify({
          remove_artifacts: checked,
          expected_reclaim_bytes: checked ? recipe.artifact_bytes : 0,
        }),
      })
      acceptJob(result)
    })
  }

  const preflightBlockers = (preflight: Preflight): string[] => {
    const list = preflight.checks.filter(check => !check.ok).map(check => check.error ?? check.operation)
    for (const [name, present] of Object.entries(preflight.secrets)) if (!present) list.push(`${name} is missing`)
    return list
  }
  // Two installs of different recipes are allowed to run at once; the
  // per-recipe guard already covers the same recipe, so this only needs to
  // know whether a different recipe has one in flight.
  const anotherInstallRunning = confirm
    ? jobs.some(job => job.kind === 'install' && job.recipe_id !== confirm.recipe.id && !terminal(job.state))
    : false
  const reclaimFrom = (preflight: Preflight) =>
    preflight.checks.find(check => !check.ok && check.operation === 'verify_disk')?.receipt?.reclaim_candidates

  const firstRun = models.length === 0
  const featured = firstRun ? sorted.find(recipe => recipe.id === RECOMMENDED_ID) : undefined
  const rows = featured ? sorted.filter(recipe => recipe.id !== featured.id) : sorted
  // Installed models are the user's own shelf; they always sit above the
  // remaining catalog, each group keeping the curated order.
  const installedRows = rows.filter(recipe => installed.has(recipe.id))
  const availableRows = rows.filter(recipe => !installed.has(recipe.id))

  const rowFor = (recipe: Recipe) => {
    const model = installed.get(recipe.id)
    // Only jobs that change what is running should lock the row. Smoke tests
    // and benchmarks run against a serving model — Open must stay available.
    const disruptive = new Set(['install', 'start', 'stop', 'remove'])
    const running = (kinds: (kind: string) => boolean) =>
      jobs.some(job => job.recipe_id === recipe.id && !terminal(job.state) && kinds(job.kind))
    const busy = pending.has(recipe.id) || running(kind => disruptive.has(kind))
    const measuring = running(kind => kind === 'benchmark' || kind === 'smoke-test')
    const isActive = Boolean(model?.active && model.status === 'ready')
    const fits = fitsOn(recipe)
    const canPair = pairable(recipe)
    // What the paired Spark says about this same model. Its own word for its
    // own state, plus the one thing this console knows that it cannot see
    // yet: an install this session asked it to run.
    const peerModel: InstalledModel | undefined = peerInstalled.get(recipe.id)
    const peerServing = Boolean(peerModel?.active && peerModel.status === 'ready')
    const peerWord = peerModel ? modelStateWord(peerModel) : delegated.has(recipe.id) ? 'Installing' : ''
    const peerBusy = peerWord === 'Installing' || peerWord === 'Starting' || peerWord === 'Switching'
    const localStatus = busy ? 'Working' : isActive ? (measuring ? 'Serving · measuring' : 'Serving') : model ? 'Installed' : 'Not installed'
    // A model that lives only on the other Spark reads as that Spark's
    // status; one that lives on both keeps this Spark's status in front.
    const statusText = !model && peerWord ? peerWord : localStatus
    const peerNote = peer && peerWord ? (!model ? `on ${peer.name}` : `${peerWord} on ${peer.name}`) : ''
    // Serving is serving, whichever Spark is doing it; the annotation says
    // which one.
    const dotClass = isActive || peerServing ? 'on' : busy || peerBusy ? 'busy' : ''
    const measured = model?.tokens_per_second
    const reference = REFERENCE_TPS[recipe.id]
    // Counting happens only while basement serves the model on this Spark,
    // so a model without a reading yet says so instead of showing zeros.
    const served = usage.get(recipe.id)
    const servedTotal = served ? served.prompt_tokens + served.generation_tokens : 0
    const updateAvailable = Boolean(model && model.recipe_version < recipe.version)
    const open = expanded === recipe.id
    const toggle = () => setExpanded(open ? '' : recipe.id)
    // Buttons act without toggling the row; empty space anywhere else in the
    // row — including inside the actions column — expands it.
    const act = (work: () => void) => (event: React.MouseEvent) => {
      event.stopPropagation()
      work()
    }
    return (
      <Fragment key={recipe.id}>
        <div
          className={`mrow ${open ? 'open' : ''}`}
          role="button"
          tabIndex={0}
          aria-expanded={open}
          onClick={toggle}
          onKeyDown={event => {
            if (event.target === event.currentTarget && (event.key === 'Enter' || event.key === ' ')) {
              event.preventDefault()
              toggle()
            }
          }}
        >
          <div className="m-id">
            <img src={LOGOS[recipe.id] ?? '/logos/nvidia.webp'} alt="" width="28" height="28" />
            <div>
              <div className="nm">{recipe.display_name} {recipe.id === RECOMMENDED_ID && <span className="tag">Recommended</span>}</div>
              <div className="use">{USE[recipe.id] ?? 'Local model for your Spark.'}</div>
            </div>
          </div>
          <div className="m-num">
            <span className="n">{measured ? measured.toFixed(1) : reference ? `~${reference}` : 'n/a'}<small>tok/s</small></span>
            <span className={`sub ${measured ? 'ok' : ''}`}>{measured ? 'measured here' : 'typical'}</span>
          </div>
          <div className="m-num">
            <span className="n">{formatBytes(recipe.artifact_bytes)}</span>
          </div>
          <div className="m-status">
            <span className={`sdot ${dotClass}`} aria-hidden="true" />
            <span>
              {statusText}
              {peerNote && <small className="peer-note">{peerNote}</small>}
            </span>
          </div>
          <div className="m-actions" onKeyDown={event => event.stopPropagation()}>
            {peer && peerWord && (
              <button
                className="ghost"
                onClick={act(() => window.open(peer.base_url, '_blank', 'noopener,noreferrer'))}
              >
                Open on {peer.name}
              </button>
            )}
            {!model && (fits || !canPair) && (
              <button className="primary" disabled={busy || !fits} onClick={act(() => startInstall(recipe))}>
                {busy ? 'Working' : fits ? installVerb(recipe) : 'Needs a Spark'}
              </button>
            )}
            {!model && !fits && canPair && (
              <button className="primary" onClick={act(openFleet)}>Pair a second Spark</button>
            )}
            {model && isActive && (
              <>
                <button className="ghost" disabled={busy} onClick={act(() => simpleAction(recipe.id, 'stop'))}>Stop</button>
                {updateAvailable && (
                  <button className="ghost" disabled={busy} onClick={act(() => startInstall(recipe))}>Update</button>
                )}
                <button className="primary" disabled={busy} onClick={act(openPlayground)}>Open</button>
              </>
            )}
            {model && !isActive && model.status !== 'recovering' && (
              <>
                {updateAvailable && (
                  <button className="ghost" disabled={busy} onClick={act(() => startInstall(recipe))}>Update</button>
                )}
                <button className="primary" disabled={busy} onClick={act(() => startOrSwitch(recipe))}>
                  {activeOther(recipe.id) ? 'Switch to' : 'Start'}
                </button>
              </>
            )}
            {model?.status === 'recovering' && <button className="ghost" disabled onClick={act(() => {})}>Recovering</button>}
          </div>
          <span className={`m-caret ${open ? 'open' : ''}`} aria-hidden="true">
            <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="M9 6l6 6-6 6" /></svg>
          </span>
        </div>
        {open && (
          <div className="mdetail">
            <div className="board">
              <div className="cell">
                <div className="l">Speed</div>
                <div className="v">{measured ? measured.toFixed(1) : reference ? `~${reference}` : 'n/a'} <small>tok/s</small></div>
                <div className={`q ${measured ? 'ok' : ''}`}>{measured ? 'measured on this Spark' : 'typical on a Spark'}</div>
              </div>
              <div className="cell">
                <div className="l">First token</div>
                <div className="v">{model?.time_to_first_token_ms ? model.time_to_first_token_ms : 'n/a'} <small>ms</small></div>
              </div>
              <div className="cell">
                <div className="l">Tokens served</div>
                <div className="v" title={served ? servedTotal.toLocaleString() : undefined}>
                  {served ? formatTokens(servedTotal) : 'n/a'}
                </div>
                <div className="q">{served ? 'since basement started counting' : 'no usage counted yet'}</div>
              </div>
              <div className="cell">
                <div className="l">Download</div>
                <div className="v">{formatBytes(recipe.artifact_bytes)}</div>
              </div>
              <div className="cell">
                <div className="l">Space needed</div>
                <div className="v">{formatBytes(recipe.required_bytes)}</div>
              </div>
            </div>
            <dl className="facts">
              <dt>Model by</dt><dd>{recipe.model_by || recipe.publisher}</dd>
              <dt>Released</dt><dd>{recipe.model_released || 'n/a'}</dd>
              <dt>Quantization</dt>
              <dd>{recipe.artifacts[0] ? readableWeights(recipe.artifacts[0].repository).quant ?? 'Original weights' : 'n/a'}</dd>
              <dt>Recipe by</dt><dd>{recipe.recipe_by || 'n/a'}</dd>
              <dt>Recipe version</dt><dd>v{recipe.version}</dd>
              {updateAvailable && model && (
                <>
                  <dt>Update</dt>
                  <dd className="update-line">v{model.recipe_version} installed, v{recipe.version} available</dd>
                </>
              )}
              {served && (
                <>
                  <dt>Prompt tokens</dt>
                  <dd title={served.prompt_tokens.toLocaleString()}>{formatTokens(served.prompt_tokens)}</dd>
                  <dt>Generated tokens</dt>
                  <dd title={served.generation_tokens.toLocaleString()}>{formatTokens(served.generation_tokens)}</dd>
                </>
              )}
              <dt>Model ID</dt><dd><code>{recipe.service.served_model_id}</code></dd>
              <dt>Runtime</dt><dd><code>{runtimeLabel(recipe.runtime.kind)} · pinned digest</code></dd>
              <dt>Source</dt><dd><a href={recipe.source.url} target="_blank" rel="noreferrer">{recipe.source.url} ↗</a></dd>
              {recipe.artifacts.map(artifact => (
                <Fragment key={artifact.role}>
                  <dt>{artifact.role === 'primary' ? 'Weights' : artifact.role === 'drafter' ? 'Draft weights' : artifact.role}</dt>
                  <dd>
                    <code>{artifact.repository}@{artifact.revision.slice(0, 12)}</code>{' '}
                    <a href={artifact.licence_url} target="_blank" rel="noreferrer">{artifact.licence} licence ↗</a>
                  </dd>
                </Fragment>
              ))}
            </dl>
            {model && (
              <div className="row-tools">
                {isActive && (
                  <>
                    <button className="ghost" disabled={busy} onClick={() => simpleAction(recipe.id, 'benchmark')}>Measure speed</button>
                    <button className="ghost" disabled={busy} onClick={() => simpleAction(recipe.id, 'smoke-test')}>Check health</button>
                  </>
                )}
                <button className="danger" disabled={busy} onClick={() => remove(recipe)}>Uninstall</button>
              </div>
            )}
          </div>
        )}
      </Fragment>
    )
  }

  return (
    <div className="stack">
      {blockers.length > 0 && (
        <div className="alert" role="alert">
          <strong>Setup needed before models can run</strong>
          <ul>{blockers.map(item => <li key={item}>{item}</li>)}</ul>
        </div>
      )}

      {featured && (
        <section className="hero" aria-label="Recommended model">
          <div className="hero-top">
            <img src={LOGOS[featured.id] ?? '/logos/nvidia.webp'} alt="" width="68" height="68" />
            <div className="hero-name">
              <p className="kicker">Recommended for your Spark</p>
              <h2>{featured.display_name}</h2>
              <p className="pub">{featured.model_by || featured.publisher}</p>
            </div>
            <div className="hero-get">
              {(() => {
                // Same lock the table rows use: a running install/start/stop/
                // remove for this recipe means the hero must say so too, not
                // offer a second Install.
                const heroBusy = pending.has(featured.id) ||
                  jobs.some(job => job.recipe_id === featured.id && !terminal(job.state) &&
                    ['install', 'start', 'stop', 'remove'].includes(job.kind))
                const heroFits = fitsOn(featured)
                if (!heroFits && pairable(featured)) {
                  return <button className="brand" onClick={openFleet}>Pair a second Spark</button>
                }
                return (
                  <button
                    className="brand"
                    disabled={heroBusy || !heroFits}
                    onClick={() => startInstall(featured)}
                  >
                    {!heroFits ? 'Needs a Spark' : heroBusy ? 'Working' : installVerb(featured)}
                  </button>
                )
              })()}
              <small>{formatBytes(featured.artifact_bytes)} download</small>
            </div>
          </div>
          <p className="hero-line">
            {USE[featured.id]}{' '}
            <span>Verified and pinned for a single Spark. Basement measures its real speed after install.</span>
          </p>
          <div className="hero-score">
            <div className="cell"><div className="l">Speed</div><div className="v">~{REFERENCE_TPS[featured.id]}</div><div className="u">tok/s · typical</div></div>
            <div className="cell"><div className="l">Download</div><div className="v">{formatBytes(featured.artifact_bytes)}</div><div className="u">one time</div></div>
            <div className="cell"><div className="l">Licence</div><div className="v">{featured.artifacts[0]?.licence ?? 'n/a'}</div><div className="u">open weights</div></div>
            <div className="cell"><div className="l">Runtime</div><div className="v">{runtimeLabel(featured.runtime.kind)}</div><div className="u">pinned digest</div></div>
          </div>
        </section>
      )}

      <div className="mtable">
        <div className="mthead" aria-hidden="true">
          <span>Model</span><span className="r">Speed</span><span className="r">Disk</span><span style={{ paddingLeft: 20 }}>Status</span><span />
        </div>
        {installedRows.length > 0 && availableRows.length > 0 ? (
          <>
            <div className="mgroup">On this Spark</div>
            {installedRows.map(rowFor)}
            <div className="mgroup">Available to install</div>
            {availableRows.map(rowFor)}
          </>
        ) : (
          rows.map(rowFor)
        )}
      </div>
      {/* Only shown once something has actually been counted. Basement
          counts a model's tokens while it serves it here, so an empty total
          means no serving has been sampled yet, not that nothing ran. */}
      {tokens && tokens.totals.prompt_tokens + tokens.totals.generation_tokens > 0 && (
        <div className="token-total">
          <span
            className="n"
            title={(tokens.totals.prompt_tokens + tokens.totals.generation_tokens).toLocaleString()}
          >
            {formatTokens(tokens.totals.prompt_tokens + tokens.totals.generation_tokens)}
          </span>
          <span>
            tokens served on this Spark since basement started counting
            <small title={tokens.totals.prompt_tokens.toLocaleString()}>
              {formatTokens(tokens.totals.prompt_tokens)} prompt
            </small>
            <small title={tokens.totals.generation_tokens.toLocaleString()}>
              {formatTokens(tokens.totals.generation_tokens)} generated
            </small>
          </span>
        </div>
      )}
      <p className="table-note">
        Speeds marked “typical” are community-reported for a DGX Spark; basement measures the real number after install.
        Click a row for weights, revisions and licences.
      </p>

      <dialog ref={dialogRef} onClose={() => setConfirm(null)} aria-label="Confirm installation">
        {confirm && (() => {
          const recipe = confirm.recipe
          const onPeer = placement === 'peer'
          // Every number in here belongs to the machine that will actually
          // run the install, which is why the other Spark answers its own
          // preflight before anything below is shown for it.
          const shown = onPeer ? peerPreflight : confirm.preflight
          const target = onPeer ? peerSummary?.system : system
          const machine = onPeer && peer ? peer.name : 'This Spark'
          const verb = verbFor(recipe, placement)
          const switchFrom = switchFromFor(placement)
          const licenceAlready = onPeer ? Boolean(peerPreflight?.licence_accepted) : licenceAccepted(recipe)
          // An update has a version already installed on the target machine
          // to name. Only this Spark's disk is readable from this console, so
          // a delegated update states the versions and leaves that Spark's
          // files to that Spark.
          const installedVersion = onPeer
            ? peerInstalled.get(recipe.id)?.recipe_version
            : installed.get(recipe.id)?.recipe_version
          const isUpdate = verb === 'Update' && installedVersion !== undefined
          const plan = isUpdate && !onPeer ? updatePlan(installedVersion, recipe, storage) : null
          // "container" only when the plan names no kind at all; with a kind,
          // runtimeLabel spells it the way its own project does and falls
          // back to the recipe's own word rather than renaming it.
          const runtimeWord = plan?.runtimeKind ? runtimeLabel(plan.runtimeKind) : 'container'
          // Nothing is fetched only when this Spark already has both the
          // weights and the runtime image this version pins. Anything less
          // than both, and the install does download something.
          const nothingToFetch = plan?.weightsPresent === true && plan?.imagePresent === true
          const weightsFormat =
            recipe.service.sglang?.quantization ??
            (recipe.artifacts[0] ? readableWeights(recipe.artifacts[0].repository).quant : undefined)
          const reclaim = shown ? reclaimFrom(shown) : undefined
          const close = () => dialogRef.current?.close()
          const foot = (confirmable: boolean) => (
            <div className="dialog-foot">
              <button type="button" className="ghost" onClick={close}>Cancel</button>
              <button type="button" className="primary" disabled={!confirmable} onClick={confirmInstall}>{verb}</button>
            </div>
          )
          return (
            <form method="dialog" className="dialog-pad" onSubmit={event => event.preventDefault()}>
              <div className="dialog-head">
                <div>
                  <p className="kicker">{verb === 'Install' ? 'Install model' : verb === 'Update' ? 'Update model' : verb}</p>
                  <h2>{recipe.display_name}</h2>
                </div>
                <button type="button" className="dialog-close" onClick={close} aria-label="Close">×</button>
              </div>
              {/* A model that needs two Sparks always runs across both, so
                  there is nothing to choose; a one-Spark model can run on
                  either machine in the fleet. */}
              {peer && recipe.topology.spark_count === 1 && (
                <div className="install-choice" role="radiogroup" aria-label="Run on">
                  <p className="kicker">Run on</p>
                  <label className="confirm-check">
                    <input
                      type="radio"
                      name="install-placement"
                      checked={!onPeer}
                      onChange={() => choosePlacement('local')}
                    />
                    <span>
                      This Spark
                      <small>{system?.hostname ?? 'the machine this console runs on'}</small>
                    </span>
                    <span className="row-check" />
                  </label>
                  <label className="confirm-check">
                    <input
                      type="radio"
                      name="install-placement"
                      checked={onPeer}
                      onChange={() => choosePlacement('peer')}
                    />
                    <span>
                      {peer.name}
                      <small>{peerSummary?.system?.hostname ?? peer.base_url}</small>
                    </span>
                    <span className="row-check" />
                  </label>
                </div>
              )}
              {onPeer && peerChecking ? (
                <>
                  <p className="muted" style={{ fontSize: 12.5 }}>Checking {machine} for space, memory and licence…</p>
                  {foot(false)}
                </>
              ) : onPeer && (peerError || !shown) ? (
                <>
                  <p className="error-text" role="alert">{peerError || `${machine} did not answer with a preflight.`}</p>
                  <p className="muted" style={{ fontSize: 12.5 }}>Pick this Spark to install here instead.</p>
                  {foot(false)}
                </>
              ) : shown && shown.ready ? (
                <>
                  {isUpdate ? (
                    // Every line here is either a version number, something
                    // this Spark reported about its own disk, or a setting
                    // the new recipe pins. Nothing is a changelog, because
                    // recipes carry no notes field and the recipe of the
                    // installed version is not kept (see updatePlan).
                    <div className="update-plan">
                      <dl className="facts">
                        <dt>Version</dt>
                        <dd>v{installedVersion} → v{recipe.version}</dd>
                        {plan ? (
                          <>
                            <dt>Weights</dt>
                            <dd>
                              {plan.weightsPresent === null
                                ? 'Reading what is already on this Spark.'
                                : plan.weightsPresent
                                  ? 'This version pins the same weights that are already on this Spark. They are reused, so there is no large download.'
                                  : `This version pins weights this Spark does not have yet. ${formatBytes(plan.bytesToFetch)} is downloaded.`}
                            </dd>
                            <dt>Runtime</dt>
                            <dd>
                              {plan.imagePresent === null
                                ? `Pinned ${runtimeWord} image, by digest.`
                                : plan.imagePresent
                                  ? `The ${runtimeWord} image this version pins is already on this Spark.`
                                  : `The ${runtimeWord} image this version pins is not on this Spark yet, so it is pulled.`}
                            </dd>
                          </>
                        ) : (
                          <>
                            <dt>Files</dt>
                            <dd>{machine} works out for itself which weights and which runtime image it already has.</dd>
                          </>
                        )}
                        {plan?.contextLength ? (
                          <>
                            <dt>Context length</dt>
                            <dd>{plan.contextLength.toLocaleString()} tokens</dd>
                          </>
                        ) : null}
                        <dt>Runs on</dt>
                        <dd>{recipe.topology.spark_count === 1 ? 'One Spark' : `${recipe.topology.spark_count} Sparks`}</dd>
                        {weightsFormat ? (
                          <>
                            <dt>Weights format</dt>
                            <dd>{weightsFormat}</dd>
                          </>
                        ) : null}
                      </dl>
                      <p className="faint">
                        Basement does not keep the recipe of the version already installed, so it cannot list
                        setting by setting what differs. These recipes carry no release notes, so there is no
                        changelog to show.
                      </p>
                    </div>
                  ) : (
                    <dl className="model-facts">
                      <div>
                        <dt>Download</dt>
                        <dd>
                          {verb === 'Resume install'
                            ? `${formatBytes(Math.max(recipe.artifact_bytes - downloadedBytes(recipe), 0))} to go`
                            : formatBytes(recipe.artifact_bytes)}
                        </dd>
                      </div>
                      <div><dt>Space needed</dt><dd>{formatBytes(recipe.required_bytes)}</dd></div>
                      <div><dt>Typical speed</dt><dd>{REFERENCE_TPS[recipe.id] ? `~${REFERENCE_TPS[recipe.id]} tok/s` : 'n/a'}</dd></div>
                    </dl>
                  )}
                  {/* Read from whichever machine will run this, and only when
                      that machine actually reported both numbers. */}
                  {target && target.storage_available_bytes > 0 && target.memory_available_bytes > 0 && (
                    <p className="muted" style={{ fontSize: 12.5 }}>
                      {machine} has {formatBytes(target.storage_available_bytes)} free on disk and{' '}
                      {formatBytes(target.memory_available_bytes)} memory free right now.
                    </p>
                  )}
                  {onPeer ? (
                    <p className="muted" style={{ fontSize: 12.5 }}>
                      {machine} runs this install itself, so the live progress is on that Spark's own console.
                      The first start loads the model into memory and can take up to {startTimeoutMinutes(recipe)} minutes.
                    </p>
                  ) : nothingToFetch ? (
                    <p className="muted" style={{ fontSize: 12.5 }}>
                      Nothing is downloaded, so this goes straight to starting the model. Loading it into
                      memory can take up to {startTimeoutMinutes(recipe)} minutes, with live progress the
                      whole way, and later starts are much faster.
                    </p>
                  ) : (
                    <p className="muted" style={{ fontSize: 12.5 }}>
                      After the download, the first start loads the model into memory. This can take up to{' '}
                      {startTimeoutMinutes(recipe)} minutes, with live progress the whole way, and later
                      starts are much faster. Cancelling is always safe: downloads resume where they left off.
                    </p>
                  )}
                  {!onPeer && anotherInstallRunning && (
                    <p className="muted" style={{ fontSize: 12.5 }}>Another download is running. Both continue, sharing bandwidth.</p>
                  )}
                  {switchFrom && (
                    <div className="install-choice" role="radiogroup" aria-label="After the download finishes">
                      {peer && recipe.topology.spark_count === 1 && <p className="kicker">After the download finishes</p>}
                      <label className="confirm-check">
                        <input
                          type="radio"
                          name="install-activate"
                          checked={activate}
                          onChange={() => setActivate(true)}
                        />
                        <span>
                          {switchFrom === recipe.id ? 'Update and switch now' : 'Download and switch now'}
                          <small>
                            {switchFrom === recipe.id
                              ? `This restarts ${recipe.display_name} on the new version. If it fails, basement restores the version that was running.`
                              : `This stops ${nameOf(switchFrom)} while ${recipe.display_name} starts.`}
                          </small>
                        </span>
                        <span className="row-check" />
                      </label>
                      <label className="confirm-check">
                        <input
                          type="radio"
                          name="install-activate"
                          checked={!activate}
                          onChange={() => setActivate(false)}
                        />
                        <span>
                          {switchFrom === recipe.id ? 'Update only' : 'Download only'}
                          <small>
                            {switchFrom === recipe.id
                              ? `${recipe.display_name} keeps serving the current version. Switch to the update later from the Models tab.`
                              : `${nameOf(switchFrom)} keeps serving. Start ${recipe.display_name} later from the Models tab.`}
                          </small>
                        </span>
                        <span className="row-check" />
                      </label>
                    </div>
                  )}
                  {onPeer && (
                    <p className="muted" style={{ fontSize: 12.5 }}>
                      {switchFrom
                        ? `Switching happens on ${machine}: it changes the model that Spark serves, not the one serving here.`
                        : `${machine} downloads and serves this model. What this Spark is serving does not change.`}
                    </p>
                  )}
                  <a href={recipe.artifacts[0].licence_url} target="_blank" rel="noreferrer">
                    Read the {recipe.artifacts[0].licence} licence ↗
                  </a>
                  {licenceAlready ? (
                    <p className="muted" style={{ fontSize: 12.5, margin: 0 }}>
                      {onPeer
                        ? `Licence already accepted on ${machine}.`
                        : 'Licence already accepted with the first install of this model.'}
                    </p>
                  ) : (
                    <label style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
                      <input type="checkbox" checked={licence} onChange={event => setLicence(event.target.checked)} />
                      I accept the model licence
                    </label>
                  )}
                  {foot(licenceAlready || licence)}
                </>
              ) : shown ? (
                <>
                  <p className="error-text" role="alert">{machine} is not ready for {recipe.display_name} yet:</p>
                  <ul>{preflightBlockers(shown).map(item => <li key={item}>{item}</li>)}</ul>
                  {Array.isArray(reclaim) && reclaim.length > 0 && (
                    <div className="alert">
                      <strong>Free up space</strong>
                      <ul>
                        {reclaim.map(candidate => (
                          <li key={candidate.recipe_id}>
                            Removing {candidate.display_name} reclaims {formatBytes(candidate.bytes)}
                            {candidate.active ? ' (currently active)' : ''}
                          </li>
                        ))}
                      </ul>
                    </div>
                  )}
                  <div className="dialog-foot">
                    {onPeer && <span className="note">Pick this Spark to install here instead.</span>}
                    <button type="button" className="ghost" onClick={close}>Close</button>
                  </div>
                </>
              ) : null}
            </form>
          )
        })()}
      </dialog>
    </div>
  )
}
