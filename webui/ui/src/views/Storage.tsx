import { useEffect, useState } from 'react'
import { api, idempotency, formatBytes, type StorageInfo } from '../api'
import type { AppState } from '../App'
import { logoFor, ownerName, readableWeights } from '../catalog'
import { confirmBox, noticeBox } from '../confirm'

type Artifact = StorageInfo['artifacts'][number]
interface ModelGroup {
  recipeID: string
  artifacts: Artifact[]
  bytes: number
}

export default function Storage({ recipes, models, openDeployment, refreshModelsAndJobs }: AppState) {
  const [info, setInfo] = useState<StorageInfo | null>(null)
  const [error, setError] = useState('')

  const load = () =>
    api<StorageInfo>('/api/v1/storage')
      .then(setInfo)
      .catch(problem => setError(problem instanceof Error ? problem.message : 'Could not read storage'))
  useEffect(() => {
    load()
  }, [])

  // Reclaiming space is this screen's whole job, so every row acts in place.
  // Files of an installed model leave through the same uninstall flow as the
  // Models tab; leftover downloads can be deleted directly.
  const uninstall = async (recipeID: string) => {
    const selected = recipes.find(recipe => recipe.id === recipeID)
    if (!selected) return
    const serving = models.find(model => model.recipe_id === recipeID)?.active
    const { ok, checked } = await confirmBox({
      title: `Uninstall ${selected.display_name}?`,
      body: serving
        ? 'It is currently serving and will be stopped first. The runtime and configuration are removed.'
        : 'The runtime and configuration are removed.',
      confirmLabel: 'Uninstall',
      danger: true,
      checkbox: {
        label: `Also delete ${formatBytes(selected.artifact_bytes)} of downloaded model files`,
        note: 'Keeping them makes a future reinstall much faster.',
      },
    })
    if (!ok) return
    try {
      const result = await api<{ job: { id: string } }>(`/api/v1/models/${recipeID}`, {
        method: 'DELETE',
        headers: idempotency(),
        body: JSON.stringify({
          remove_artifacts: checked,
          expected_reclaim_bytes: checked ? selected.artifact_bytes : 0,
        }),
      })
      openDeployment(result.job.id)
      refreshModelsAndJobs()
    } catch (problem) {
      noticeBox('Could not uninstall', problem instanceof Error ? problem.message : undefined)
    }
  }

  const deleteArtifacts = async (title: string, artifacts: Artifact[], bytes: number) => {
    const { ok } = await confirmBox({
      title: `Delete the ${title} files?`,
      body: `Frees ${formatBytes(bytes)}. A future install downloads them again.`,
      confirmLabel: 'Delete files',
      danger: true,
    })
    if (!ok) return
    try {
      for (const artifact of artifacts) {
        await api('/api/v1/storage/artifacts', {
          method: 'DELETE',
          body: JSON.stringify({ repository: artifact.repository, revision: artifact.revision }),
        })
      }
    } catch (problem) {
      noticeBox('Could not delete the files', problem instanceof Error ? problem.message : undefined)
    }
    load()
  }

  if (error) return <div className="empty">{error}</div>
  if (!info) return <div className="empty">Reading storage…</div>

  const total = info.storage_total || 1
  const managed = Math.min(info.total_managed_bytes, total)
  const free = Math.min(info.storage_available, total)
  const otherUsed = Math.max(total - managed - free, 0)
  const pct = (bytes: number) => `${Math.max((bytes / total) * 100, 0.4)}%`
  const recipeName = (id: string) => recipes.find(recipe => recipe.id === id)?.display_name ?? id

  return (
    <div className="stack">
      <div className="section-head">
        <span className="muted">{formatBytes(info.storage_available)} free of {formatBytes(info.storage_total)}</span>
      </div>
      <section className="card">
        <div className="disk-bar" role="img" aria-label={`Models use ${formatBytes(managed)}, other data ${formatBytes(otherUsed)}, free ${formatBytes(free)}`}>
          <span style={{ width: pct(managed), background: 'var(--green)', marginRight: 2 }} />
          <span style={{ width: pct(otherUsed), background: 'var(--line-strong)', marginRight: 2 }} />
          <span style={{ width: pct(free), background: 'var(--surface-2)' }} />
        </div>
        <div className="disk-legend">
          <span><i style={{ background: 'var(--green)' }} />Models, caches &amp; runtimes · {formatBytes(managed)}</span>
          <span><i style={{ background: 'var(--line-strong)' }} />Everything else · {formatBytes(otherUsed)}</span>
          <span><i style={{ background: 'var(--surface-2)', border: '1px solid var(--line-strong)' }} />Free · {formatBytes(free)}</span>
        </div>
      </section>

      <section className="card">
        <div className="section-head" style={{ marginBottom: 4 }}>
          <h2 style={{ fontSize: 16 }}>Downloaded models</h2>
          <span className="muted">Uninstall a model or delete leftover downloads to reclaim space</span>
        </div>
        {(() => {
          // One row per model: people think in models, not weight repositories.
          // Files with no owning recipe stay as their own rows below.
          const groups: ModelGroup[] = []
          const orphans: Artifact[] = []
          for (const artifact of info.artifacts) {
            const recipeID = artifact.recipe_ids[0]
            if (!recipeID) {
              orphans.push(artifact)
              continue
            }
            const group = groups.find(item => item.recipeID === recipeID)
            if (group) {
              group.artifacts.push(artifact)
              group.bytes += artifact.bytes
            } else {
              groups.push({ recipeID, artifacts: [artifact], bytes: artifact.bytes })
            }
          }
          groups.sort((a, b) => b.bytes - a.bytes)
          return (
            <>
              {groups.map(group => {
                const primary = group.artifacts[0]
                const { quant } = readableWeights(primary.repository)
                const publisher = ownerName(primary.repository)
                const installed = models.some(model => model.recipe_id === group.recipeID)
                return (
                  <div className="storage-row" key={group.recipeID}>
                    <img src={logoFor([group.recipeID])} alt="" width="24" height="24" />
                    <div className="grow">
                      <strong>{recipeName(group.recipeID)}</strong>
                      <div className="faint" style={{ fontSize: 12 }}>
                        {quant ? `${quant} weights by ${publisher}` : `Weights by ${publisher}`}
                        {group.artifacts.length > 1 ? ` · ${group.artifacts.length} parts` : ''}
                      </div>
                    </div>
                    <span className="bytes">{formatBytes(group.bytes)}</span>
                    {installed ? (
                      <button className="quiet" onClick={() => uninstall(group.recipeID)}>Uninstall</button>
                    ) : (
                      <button className="quiet" onClick={() => deleteArtifacts(recipeName(group.recipeID), group.artifacts, group.bytes)}>Delete</button>
                    )}
                  </div>
                )
              })}
              {orphans.map(artifact => {
                const { name, quant } = readableWeights(artifact.repository)
                return (
                  <div className="storage-row" key={`${artifact.repository}@${artifact.revision}`}>
                    <img src={logoFor([])} alt="" width="24" height="24" />
                    <div className="grow">
                      <strong>{name}</strong>
                      <div className="faint" style={{ fontSize: 12 }}>
                        {quant ? `${quant} weights by ${ownerName(artifact.repository)}` : `Weights by ${ownerName(artifact.repository)}`}
                        {' · Not used by any current model'}
                      </div>
                    </div>
                    <span className="bytes">{formatBytes(artifact.bytes)}</span>
                    <button className="quiet" onClick={() => deleteArtifacts(name, [artifact], artifact.bytes)}>Delete</button>
                  </div>
                )
              })}
            </>
          )
        })()}
        {info.artifacts.length === 0 && <p className="muted">No model downloads yet.</p>}
      </section>

      {(info.images ?? []).length > 0 && (
        <section className="card">
          <div className="section-head" style={{ marginBottom: 4 }}>
            <h2 style={{ fontSize: 16 }}>Runtime images</h2>
            <span className="muted">Pulled by installs; removed with their last model</span>
          </div>
          {info.images.map(image => {
            const shortRef = image.reference.split('@')[0]
            const title = shortRef.includes('vllm') ? 'vLLM runtime' : shortRef
            const usedBy = image.recipe_ids.map(recipeName).join(', ')
            return (
              <div className="storage-row" key={image.reference}>
                <img src={logoFor(image.recipe_ids)} alt="" width="24" height="24" />
                <div className="grow">
                  <strong>{title}</strong>
                  <div className="faint" style={{ fontSize: 12 }}>
                    {usedBy
                      ? `The exact version pinned for ${usedBy}`
                      : 'Not used by any current model'}
                  </div>
                </div>
                <span className="bytes">{formatBytes(image.bytes)}</span>
              </div>
            )
          })}
        </section>
      )}

      <section className="card">
        <div className="section-head" style={{ marginBottom: 4 }}>
          <h2 style={{ fontSize: 16 }}>Manager data</h2>
        </div>
        {info.caches.map(cache => (
          <div className="storage-row" key={cache.recipe_id}>
            <div className="grow">Compilation cache · {recipeName(cache.recipe_id)}</div>
            <span className="bytes">{formatBytes(cache.bytes)}</span>
          </div>
        ))}
        <div className="storage-row">
          <div className="grow">State database</div>
          <span className="bytes">{formatBytes(info.database_bytes)}</span>
        </div>
        <div className="storage-row">
          <div className="grow"><span className="faint">Data directory</span></div>
          <code className="faint" style={{ fontSize: 12 }}>{info.data_dir}</code>
        </div>
      </section>
    </div>
  )
}
