import { Fragment, useMemo, useRef, useState } from 'react'
import { api, idempotency, terminal, formatBytes, type Job, type Preflight, type Recipe } from '../api'
import type { AppState } from '../App'

// Family identity: official publisher logos, embedded so the console works offline.
const LOGOS: Record<string, string> = {
  'qwen36-35b-a3b-nvfp4-1s': '/logos/qwen.webp',
  'qwen36-27b-nvfp4-1s': '/logos/qwen.webp',
  'laguna-s-2-1-nvfp4-dflash-1s': '/logos/poolside.webp',
}
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
  const [pending, setPending] = useState<Set<string>>(new Set())
  const [expanded, setExpanded] = useState('')
  const dialogRef = useRef<HTMLDialogElement>(null)

  const installed = useMemo(() => new Map(models.map(model => [model.recipe_id, model])), [models])
  const sorted = useMemo(
    () => [...recipes].sort((a, b) => ORDER.indexOf(a.id) - ORDER.indexOf(b.id)),
    [recipes],
  )
  const detected = system?.hardware_scope.detected_spark_count ?? 0
  const blockers = system?.blocking_conditions ?? []
  const activeOther = (id: string) => models.find(model => model.active && model.recipe_id !== id)

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
      alert(problem instanceof Error ? problem.message : String(problem))
    } finally {
      setBusy(id, false)
    }
  }

  const startInstall = (recipe: Recipe) =>
    run(recipe.id, async () => {
      const preflight = await api<Preflight>(`/api/v1/preflight?recipe_id=${encodeURIComponent(recipe.id)}`)
      setConfirm({ recipe, preflight, switchFrom: activeOther(recipe.id)?.recipe_id })
      setLicence(false)
      requestAnimationFrame(() => dialogRef.current?.showModal())
    })

  const confirmInstall = () =>
    confirm &&
    run(confirm.recipe.id, async () => {
      const result = await api<{ job: Job }>(`/api/v1/models/${confirm.recipe.id}/install`, {
        method: 'POST',
        headers: idempotency(),
        body: JSON.stringify({ confirmed: true, accept_licence: true }),
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

  const startOrSwitch = (recipe: Recipe) => {
    const active = activeOther(recipe.id)
    if (active) {
      const from = recipes.find(item => item.id === active.recipe_id)?.display_name ?? active.recipe_id
      if (!window.confirm(`Switch to ${recipe.display_name}?\n\n${from} will stop. If the new model fails verification, RunOnSpark restores the previous one.`)) return
    }
    simpleAction(recipe.id, 'start')
  }

  const remove = (recipe: Recipe) => {
    if (!window.confirm(`Remove the ${recipe.display_name} runtime and configuration?`)) return
    const removeArtifacts = window.confirm(
      `Also delete ${formatBytes(recipe.artifact_bytes)} of downloaded model data? Cancel keeps the download for a fast reinstall.`,
    )
    run(recipe.id, async () => {
      const result = await api<{ job: Job }>(`/api/v1/models/${recipe.id}`, {
        method: 'DELETE',
        headers: idempotency(),
        body: JSON.stringify({
          remove_artifacts: removeArtifacts,
          expected_reclaim_bytes: removeArtifacts ? recipe.artifact_bytes : 0,
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

  const rowFor = (recipe: Recipe) => {
    const model = installed.get(recipe.id)
    const busy = pending.has(recipe.id) || jobs.some(job => job.recipe_id === recipe.id && !terminal(job.state))
    const isActive = Boolean(model?.active && model.status === 'ready')
    const fits = detected >= recipe.topology.spark_count
    const statusText = busy ? 'Working' : isActive ? 'Serving' : model ? 'Installed' : 'Not installed'
    const measured = model?.tokens_per_second
    const reference = REFERENCE_TPS[recipe.id]
    const open = expanded === recipe.id
    const toggle = () => setExpanded(open ? '' : recipe.id)
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
            <span className="n">{measured ? measured.toFixed(1) : reference ? `~${reference}` : '—'}<small>tok/s</small></span>
            <span className={`sub ${measured ? 'ok' : ''}`}>{measured ? 'measured here' : 'typical'}</span>
          </div>
          <div className="m-num">
            <span className="n">{formatBytes(recipe.artifact_bytes)}</span>
          </div>
          <div className="m-status">
            <span className={`sdot ${isActive ? 'on' : busy ? 'busy' : ''}`} aria-hidden="true" />
            {statusText}
          </div>
          <div className="m-actions" onClick={event => event.stopPropagation()} onKeyDown={event => event.stopPropagation()}>
            {!model && (
              <button className="primary" disabled={busy || !fits} onClick={() => startInstall(recipe)}>
                {busy ? 'Working' : fits ? 'Install' : 'Needs a Spark'}
              </button>
            )}
            {model && isActive && (
              <>
                <button className="quiet" disabled={busy} onClick={() => simpleAction(recipe.id, 'smoke-test')}>Test</button>
                <button className="ghost" disabled={busy} onClick={() => simpleAction(recipe.id, 'stop')}>Stop</button>
                <button className="primary" disabled={busy} onClick={openPlayground}>Open</button>
              </>
            )}
            {model && !isActive && model.status !== 'recovering' && (
              <>
                <button className="quiet" disabled={busy} onClick={() => remove(recipe)}>Remove</button>
                <button className="primary" disabled={busy} onClick={() => startOrSwitch(recipe)}>
                  {activeOther(recipe.id) ? 'Switch to' : 'Start'}
                </button>
              </>
            )}
            {model?.status === 'recovering' && <button className="ghost" disabled>Recovering</button>}
          </div>
        </div>
        {open && (
          <div className="mdetail">
            <div className="board">
              <div className="cell">
                <div className="l">Speed</div>
                <div className="v">{measured ? measured.toFixed(1) : reference ? `~${reference}` : '—'} <small>tok/s</small></div>
                <div className={`q ${measured ? 'ok' : ''}`}>{measured ? 'measured on this Spark' : 'typical on a Spark'}</div>
              </div>
              <div className="cell">
                <div className="l">First token</div>
                <div className="v">{model?.time_to_first_token_ms ? model.time_to_first_token_ms : '—'} <small>ms</small></div>
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
              <dt>Publisher</dt><dd>{recipe.publisher}</dd>
              <dt>Model ID</dt><dd><code>{recipe.service.served_model_id}</code></dd>
              <dt>Runtime</dt><dd><code>vLLM · pinned digest</code></dd>
              <dt>Source</dt><dd><a href={recipe.source.url} target="_blank" rel="noreferrer">{recipe.source.url} ↗</a></dd>
              {recipe.artifacts.map(artifact => (
                <Fragment key={artifact.role}>
                  <dt>{artifact.role}</dt>
                  <dd>
                    <code>{artifact.repository}@{artifact.revision.slice(0, 12)}</code>{' '}
                    <a href={artifact.licence_url} target="_blank" rel="noreferrer">{artifact.licence} licence ↗</a>
                  </dd>
                </Fragment>
              ))}
            </dl>
            {model && isActive && (
              <div className="row-tools">
                <button className="ghost" disabled={busy} onClick={() => simpleAction(recipe.id, 'benchmark')}>Measure speed</button>
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
              <p className="pub">{featured.publisher}</p>
            </div>
            <div className="hero-get">
              <button
                className="brand"
                disabled={pending.has(featured.id) || detected < featured.topology.spark_count}
                onClick={() => startInstall(featured)}
              >
                {detected >= featured.topology.spark_count ? 'Install' : 'Needs a Spark'}
              </button>
              <small>{formatBytes(featured.artifact_bytes)} download</small>
            </div>
          </div>
          <p className="hero-line">
            {USE[featured.id]}{' '}
            <span>Verified and pinned for a single Spark — RunOnSpark measures its real speed after install.</span>
          </p>
          <div className="hero-score">
            <div className="cell"><div className="l">Speed</div><div className="v">~{REFERENCE_TPS[featured.id]}</div><div className="u">tok/s · typical</div></div>
            <div className="cell"><div className="l">Download</div><div className="v">{formatBytes(featured.artifact_bytes)}</div><div className="u">one time</div></div>
            <div className="cell"><div className="l">Licence</div><div className="v">{featured.artifacts[0]?.licence ?? '—'}</div><div className="u">open weights</div></div>
            <div className="cell"><div className="l">Runtime</div><div className="v">vLLM</div><div className="u">pinned digest</div></div>
          </div>
        </section>
      )}

      <div className="mtable">
        <div className="mthead" aria-hidden="true">
          <span>Model</span><span className="r">Speed</span><span className="r">Disk</span><span style={{ paddingLeft: 20 }}>Status</span><span />
        </div>
        {rows.map(rowFor)}
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
                <p className="kicker">Install model</p>
                <h2>{confirm.recipe.display_name}</h2>
              </div>
              <button type="button" className="dialog-close" onClick={() => dialogRef.current?.close()} aria-label="Close">×</button>
            </div>
            {confirm.preflight.ready ? (
              <>
                <dl className="model-facts">
                  <div><dt>Download</dt><dd>{formatBytes(confirm.recipe.artifact_bytes)}</dd></div>
                  <div><dt>Space needed</dt><dd>{formatBytes(confirm.recipe.required_bytes)}</dd></div>
                  <div><dt>RAM kept free</dt><dd>{formatBytes(confirm.recipe.requirements.per_node_memory_reserve_bytes)}</dd></div>
                </dl>
                {confirm.switchFrom && (
                  <p className="muted">
                    <strong>{recipes.find(item => item.id === confirm.switchFrom)?.display_name} stops after the download.</strong>{' '}
                    If this model fails verification, RunOnSpark restores it.
                  </p>
                )}
                <a href={confirm.recipe.artifacts[0].licence_url} target="_blank" rel="noreferrer">
                  Read the {confirm.recipe.artifacts[0].licence} licence ↗
                </a>
                <label style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
                  <input type="checkbox" checked={licence} onChange={event => setLicence(event.target.checked)} />
                  I accept the model licence
                </label>
                <div className="dialog-foot">
                  <button type="button" className="ghost" onClick={() => dialogRef.current?.close()}>Cancel</button>
                  <button type="button" className="primary" disabled={!licence} onClick={confirmInstall}>
                    Install
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
