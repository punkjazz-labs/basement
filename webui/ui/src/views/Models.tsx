import { useMemo, useRef, useState } from 'react'
import { api, idempotency, terminal, formatBytes, type Job, type Preflight, type Recipe } from '../api'
import type { AppState } from '../App'

// Family identity: official publisher logos, embedded so the console works offline.
const LOGOS: Record<string, string> = {
  'qwen36-35b-a3b-nvfp4-1s': '/logos/qwen.webp',
  'qwen36-27b-nvfp4-1s': '/logos/qwen.webp',
  'laguna-s-2-1-nvfp4-dflash-1s': '/logos/poolside.webp',
}
const USE: Record<string, string> = {
  'qwen36-35b-a3b-nvfp4-1s': 'Best all-rounder',
  'qwen36-27b-nvfp4-1s': 'Coding',
  'laguna-s-2-1-nvfp4-dflash-1s': 'Agentic work',
}
const ORDER = ['qwen36-35b-a3b-nvfp4-1s', 'qwen36-27b-nvfp4-1s', 'laguna-s-2-1-nvfp4-dflash-1s']

interface ConfirmState {
  recipe: Recipe
  preflight: Preflight
  switchFrom?: string
}

export default function Models({ system, recipes, models, jobs, refreshModelsAndJobs, openDeployment }: AppState) {
  const [confirm, setConfirm] = useState<ConfirmState | null>(null)
  const [licence, setLicence] = useState(false)
  const [pending, setPending] = useState<Set<string>>(new Set())
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

  return (
    <div className="stack">
      {blockers.length > 0 && (
        <div className="alert" role="alert">
          <strong>Setup needed before models can run</strong>
          <ul>{blockers.map(item => <li key={item}>{item}</li>)}</ul>
        </div>
      )}
      <div className="section-head">
        <h2>Models</h2>
        <span className="muted">
          {detected ? 'Matched to this Spark' : 'Connect a DGX Spark to unlock deployments'}
        </span>
      </div>
      {sorted.map(recipe => {
        const model = installed.get(recipe.id)
        const busy = pending.has(recipe.id) || jobs.some(job => job.recipe_id === recipe.id && !terminal(job.state))
        const isActive = Boolean(model?.active && model.status === 'ready')
        const fits = detected >= recipe.topology.spark_count
        const statusText = busy ? 'Working' : model ? model.status : 'Not installed'
        return (
          <article key={recipe.id} className={`model-row ${isActive ? 'active-model' : ''}`}>
            <img className="model-logo" src={LOGOS[recipe.id] ?? '/logos/nvidia.webp'} alt="" width="44" height="44" />
            <div className="model-head">
              <div className="model-title">
                <h3>{recipe.display_name}</h3>
                {recipe.id === ORDER[0] && <span className="pill reco">Recommended</span>}
                <span className="pill">{recipe.publisher}</span>
              </div>
              <p className="model-sub">{USE[recipe.id] ?? 'Local model'} · {recipe.service.served_model_id}</p>
              <dl className="model-facts">
                <div>
                  <dt>Speed on this Spark</dt>
                  <dd>
                    {model?.tokens_per_second
                      ? <>{model.tokens_per_second.toFixed(1)} <small>tok/s measured</small></>
                      : <small>not measured yet</small>}
                  </dd>
                </div>
                {model?.time_to_first_token_ms ? (
                  <div>
                    <dt>First token</dt>
                    <dd>{model.time_to_first_token_ms} <small>ms</small></dd>
                  </div>
                ) : null}
                <div>
                  <dt>Disk</dt>
                  <dd>{formatBytes(recipe.required_bytes)}</dd>
                </div>
              </dl>
            </div>
            <div className="model-side">
              <span className={`model-status ${isActive ? 'on' : busy ? 'busy' : ''}`}>
                <i aria-hidden="true" />
                {statusText}
              </span>
              <div className="model-actions">
                {!model && (
                  <button className="primary" disabled={busy || !fits} onClick={() => startInstall(recipe)}>
                    {busy ? 'Working' : fits ? 'Install' : 'Needs a Spark'}
                  </button>
                )}
                {model && isActive && (
                  <>
                    <button className="ghost" disabled={busy} onClick={() => simpleAction(recipe.id, 'stop')}>Stop</button>
                    <button className="quiet" disabled={busy} onClick={() => simpleAction(recipe.id, 'smoke-test')}>Test</button>
                    <button className="quiet" disabled={busy} onClick={() => simpleAction(recipe.id, 'benchmark')}>Measure speed</button>
                  </>
                )}
                {model && !isActive && model.status !== 'recovering' && (
                  <>
                    <button className="primary" disabled={busy} onClick={() => startOrSwitch(recipe)}>
                      {activeOther(recipe.id) ? 'Switch to' : 'Start'}
                    </button>
                    <button className="danger" disabled={busy} onClick={() => remove(recipe)}>Remove</button>
                  </>
                )}
                {model?.status === 'recovering' && <button className="ghost" disabled>Recovering</button>}
              </div>
            </div>
            <details className="model-more">
              <summary>Details</summary>
              <div className="detail-grid">
                <div><span>Runtime</span><code>vLLM · pinned digest</code></div>
                <div><span>Source</span><a href={recipe.source.url} target="_blank" rel="noreferrer">{recipe.source.url}</a></div>
                {recipe.artifacts.map(artifact => (
                  <div key={artifact.role}>
                    <span>{artifact.role}</span>
                    <code>{artifact.repository}@{artifact.revision.slice(0, 12)}</code>
                    <a href={artifact.licence_url} target="_blank" rel="noreferrer">{artifact.licence} licence</a>
                  </div>
                ))}
              </div>
            </details>
          </article>
        )
      })}

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
