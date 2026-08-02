import { Fragment, useEffect, useMemo, useRef, useState } from 'react'
import { api, idempotency, terminal, formatBytes, startTimeoutMinutes, type Job, type Preflight, type Recipe, type StorageInfo } from '../api'
import type { AppState } from '../App'
import { confirmBox, noticeBox } from '../confirm'
import { LOGOS, readableWeights } from '../catalog'

const USE: Record<string, string> = {
  'qwen36-35b-a3b-nvfp4-1s': 'Fast enough to become your default. Best all-rounder.',
  'qwen36-27b-nvfp4-1s': 'Flagship-level coding in a smaller footprint.',
  'laguna-s-2-1-nvfp4-dflash-1s': 'Built for long, independent agent runs.',
}
// Community-reported typical speeds on a DGX Spark, shown until this device
// measures its own number.
const REFERENCE_TPS: Record<string, number> = {
  'qwen36-35b-a3b-nvfp4-1s': 80,
  'qwen36-27b-nvfp4-1s': 33,
  'laguna-s-2-1-nvfp4-dflash-1s': 19.4,
}
const ORDER = ['qwen36-35b-a3b-nvfp4-1s', 'qwen36-27b-nvfp4-1s', 'laguna-s-2-1-nvfp4-dflash-1s']

interface ConfirmState {
  recipe: Recipe
  preflight: Preflight
  switchFrom?: string
}

export default function Models({ system, recipes, models, jobs, refreshModelsAndJobs, openDeployment, openPlayground }: AppState) {
  const [confirm, setConfirm] = useState<ConfirmState | null>(null)
  const [licence, setLicence] = useState(false)
  // Whether the install switches to the new model as soon as it is ready.
  // Only meaningful when another model is serving; defaults to the
  // historical behaviour so a single click still installs and serves.
  const [activate, setActivate] = useState(true)
  const [pending, setPending] = useState<Set<string>>(new Set())
  const [expanded, setExpanded] = useState('')
  const [storage, setStorage] = useState<StorageInfo | null>(null)
  const dialogRef = useRef<HTMLDialogElement>(null)

  // Storage tells us which recipes already have model files on disk, so a
  // partially downloaded model can offer to resume instead of start over.
  useEffect(() => {
    api<StorageInfo>('/api/v1/storage').then(setStorage).catch(() => {})
  }, [jobs.length])

  const installed = useMemo(() => new Map(models.map(model => [model.recipe_id, model])), [models])
  const sorted = useMemo(
    () => [...recipes].sort((a, b) => ORDER.indexOf(a.id) - ORDER.indexOf(b.id)),
    [recipes],
  )
  const detected = system?.hardware_scope.detected_spark_count ?? 0
  const blockers = system?.blocking_conditions ?? []
  const activeOther = (id: string) => models.find(model => model.active && model.recipe_id !== id)
  const downloadedBytes = (recipe: Recipe) =>
    storage?.artifacts.filter(a => a.recipe_ids.includes(recipe.id)).reduce((sum, a) => sum + a.bytes, 0) ?? 0
  // Label honesty for a not-installed model with files already on disk:
  // partial data resumes, a kept complete download reinstalls quickly.
  const installVerb = (recipe: Recipe) => {
    if (installed.has(recipe.id)) return 'Install'
    const bytes = downloadedBytes(recipe)
    if (bytes <= 0) return 'Install'
    return bytes >= recipe.artifact_bytes * 0.99 ? 'Reinstall' : 'Resume install'
  }
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
      setConfirm({ recipe, preflight, switchFrom: activeOther(recipe.id)?.recipe_id })
      setLicence(licenceAccepted(recipe))
      setActivate(true)
      requestAnimationFrame(() => dialogRef.current?.showModal())
    })

  const confirmInstall = () =>
    confirm &&
    run(confirm.recipe.id, async () => {
      const result = await api<{ job: Job }>(`/api/v1/models/${confirm.recipe.id}/install`, {
        method: 'POST',
        headers: idempotency(),
        body: JSON.stringify({ confirmed: true, accept_licence: true, activate: confirm.switchFrom ? activate : true }),
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
        body: `${from} will stop. If the new model fails verification, RunOnSpark restores the previous one.`,
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
  const reclaimCandidates = confirm?.preflight.checks
    .find(check => !check.ok && check.operation === 'verify_disk')
    ?.receipt?.reclaim_candidates

  const firstRun = models.length === 0
  const featured = firstRun ? sorted.find(recipe => recipe.id === ORDER[0]) : undefined
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
    const fits = detected >= recipe.topology.spark_count
    const statusText = busy ? 'Working' : isActive ? (measuring ? 'Serving · measuring' : 'Serving') : model ? 'Installed' : 'Not installed'
    const measured = model?.tokens_per_second
    const reference = REFERENCE_TPS[recipe.id]
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
              <div className="nm">{recipe.display_name} {recipe.id === ORDER[0] && <span className="tag">Recommended</span>}</div>
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
            <span className={`sdot ${isActive ? 'on' : busy ? 'busy' : ''}`} aria-hidden="true" />
            {statusText}
          </div>
          <div className="m-actions" onKeyDown={event => event.stopPropagation()}>
            {!model && (
              <button className="primary" disabled={busy || !fits} onClick={act(() => startInstall(recipe))}>
                {busy ? 'Working' : fits ? installVerb(recipe) : 'Needs a Spark'}
              </button>
            )}
            {model && isActive && (
              <>
                <button className="ghost" disabled={busy} onClick={act(() => simpleAction(recipe.id, 'stop'))}>Stop</button>
                <button className="primary" disabled={busy} onClick={act(openPlayground)}>Open</button>
              </>
            )}
            {model && !isActive && model.status !== 'recovering' && (
              <button className="primary" disabled={busy} onClick={act(() => startOrSwitch(recipe))}>
                {activeOther(recipe.id) ? 'Switch to' : 'Start'}
              </button>
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
              <dt>Model ID</dt><dd><code>{recipe.service.served_model_id}</code></dd>
              <dt>Runtime</dt><dd><code>vLLM · pinned digest</code></dd>
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
              <button
                className="brand"
                disabled={pending.has(featured.id) || detected < featured.topology.spark_count}
                onClick={() => startInstall(featured)}
              >
                {detected >= featured.topology.spark_count ? installVerb(featured) : 'Needs a Spark'}
              </button>
              <small>{formatBytes(featured.artifact_bytes)} download</small>
            </div>
          </div>
          <p className="hero-line">
            {USE[featured.id]}{' '}
            <span>Verified and pinned for a single Spark. RunOnSpark measures its real speed after install.</span>
          </p>
          <div className="hero-score">
            <div className="cell"><div className="l">Speed</div><div className="v">~{REFERENCE_TPS[featured.id]}</div><div className="u">tok/s · typical</div></div>
            <div className="cell"><div className="l">Download</div><div className="v">{formatBytes(featured.artifact_bytes)}</div><div className="u">one time</div></div>
            <div className="cell"><div className="l">Licence</div><div className="v">{featured.artifacts[0]?.licence ?? 'n/a'}</div><div className="u">open weights</div></div>
            <div className="cell"><div className="l">Runtime</div><div className="v">vLLM</div><div className="u">pinned digest</div></div>
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
      <p className="table-note">
        Speeds marked “typical” are community-reported for a DGX Spark; RunOnSpark measures the real number after install.
        Click a row for weights, revisions and licences.
      </p>

      <dialog ref={dialogRef} onClose={() => setConfirm(null)} aria-label="Confirm installation">
        {confirm && (
          <form method="dialog" className="dialog-pad" onSubmit={event => event.preventDefault()}>
            <div className="dialog-head">
              <div>
                <p className="kicker">{installVerb(confirm.recipe) === 'Install' ? 'Install model' : installVerb(confirm.recipe)}</p>
                <h2>{confirm.recipe.display_name}</h2>
              </div>
              <button type="button" className="dialog-close" onClick={() => dialogRef.current?.close()} aria-label="Close">×</button>
            </div>
            {confirm.preflight.ready ? (
              <>
                <dl className="model-facts">
                  <div>
                    <dt>Download</dt>
                    <dd>
                      {installVerb(confirm.recipe) === 'Resume install'
                        ? `${formatBytes(Math.max(confirm.recipe.artifact_bytes - downloadedBytes(confirm.recipe), 0))} to go`
                        : formatBytes(confirm.recipe.artifact_bytes)}
                    </dd>
                  </div>
                  <div><dt>Space needed</dt><dd>{formatBytes(confirm.recipe.required_bytes)}</dd></div>
                  <div><dt>Typical speed</dt><dd>{REFERENCE_TPS[confirm.recipe.id] ? `~${REFERENCE_TPS[confirm.recipe.id]} tok/s` : 'n/a'}</dd></div>
                </dl>
                <p className="muted" style={{ fontSize: 12.5 }}>
                  After the download, the first start loads the model into memory. This can take up to{' '}
                  {startTimeoutMinutes(confirm.recipe)} minutes, with live progress the whole way, and later
                  starts are much faster. Cancelling is always safe: downloads resume where they left off.
                </p>
                {confirm.switchFrom && (
                  <div className="install-choice" role="radiogroup" aria-label="After the download finishes">
                    <label className="confirm-check">
                      <input
                        type="radio"
                        name="install-activate"
                        checked={activate}
                        onChange={() => setActivate(true)}
                      />
                      <span>
                        Download and switch now
                        <small>
                          This stops {recipes.find(item => item.id === confirm.switchFrom)?.display_name} while {confirm.recipe.display_name} starts.
                        </small>
                      </span>
                    </label>
                    <label className="confirm-check">
                      <input
                        type="radio"
                        name="install-activate"
                        checked={!activate}
                        onChange={() => setActivate(false)}
                      />
                      <span>
                        Download only
                        <small>
                          {recipes.find(item => item.id === confirm.switchFrom)?.display_name} keeps serving. Start {confirm.recipe.display_name} later from the Models tab.
                        </small>
                      </span>
                    </label>
                  </div>
                )}
                <a href={confirm.recipe.artifacts[0].licence_url} target="_blank" rel="noreferrer">
                  Read the {confirm.recipe.artifacts[0].licence} licence ↗
                </a>
                {licenceAccepted(confirm.recipe) ? (
                  <p className="muted" style={{ fontSize: 12.5, margin: 0 }}>
                    Licence already accepted with the first install of this model.
                  </p>
                ) : (
                  <label style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
                    <input type="checkbox" checked={licence} onChange={event => setLicence(event.target.checked)} />
                    I accept the model licence
                  </label>
                )}
                <div className="dialog-foot">
                  <button type="button" className="ghost" onClick={() => dialogRef.current?.close()}>Cancel</button>
                  <button type="button" className="primary" disabled={!licence} onClick={confirmInstall}>
                    {installVerb(confirm.recipe)}
                  </button>
                </div>
              </>
            ) : (
              <>
                <p className="error-text" role="alert">This Spark is not ready for {confirm.recipe.display_name} yet:</p>
                <ul>{preflightBlockers(confirm.preflight).map(item => <li key={item}>{item}</li>)}</ul>
                {Array.isArray(reclaimCandidates) && reclaimCandidates.length > 0 && (
                  <div className="alert">
                    <strong>Free up space</strong>
                    <ul>
                      {reclaimCandidates.map(candidate => (
                        <li key={candidate.recipe_id}>
                          Removing {candidate.display_name} reclaims {formatBytes(candidate.bytes)}
                          {candidate.active ? ' (currently active)' : ''}
                        </li>
                      ))}
                    </ul>
                  </div>
                )}
                <div className="dialog-foot">
                  <button type="button" className="ghost" onClick={() => dialogRef.current?.close()}>Close</button>
                </div>
              </>
            )}
          </form>
        )}
      </dialog>
    </div>
  )
}
