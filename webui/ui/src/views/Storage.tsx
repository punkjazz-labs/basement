import { useEffect, useState } from 'react'
import { api, formatBytes, type StorageInfo } from '../api'
import type { AppState } from '../App'

export default function Storage({ recipes }: AppState) {
  const [info, setInfo] = useState<StorageInfo | null>(null)
  const [error, setError] = useState('')

  useEffect(() => {
    api<StorageInfo>('/api/v1/storage')
      .then(setInfo)
      .catch(problem => setError(problem instanceof Error ? problem.message : 'Could not read storage'))
  }, [])

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
          <span className="muted">Remove models from the Models tab to reclaim space</span>
        </div>
        {info.artifacts.map(artifact => (
          <div className="storage-row" key={`${artifact.repository}@${artifact.revision}`}>
            <div className="grow">
              <code>{artifact.repository}</code>
              <div className="faint" style={{ fontSize: 12 }}>
                {artifact.recipe_ids.length
                  ? `Used by ${artifact.recipe_ids.map(recipeName).join(', ')}`
                  : 'Not referenced by any current recipe'}
                {' · '}
                <span className="mono">{artifact.revision.slice(0, 12)}</span>
              </div>
            </div>
            <span className="bytes">{formatBytes(artifact.bytes)}</span>
          </div>
        ))}
        {info.artifacts.length === 0 && <p className="muted">No model downloads yet.</p>}
      </section>

      {(info.images ?? []).length > 0 && (
        <section className="card">
          <div className="section-head" style={{ marginBottom: 4 }}>
            <h2 style={{ fontSize: 16 }}>Runtime images</h2>
            <span className="muted">Pulled by installs; removed with their last model</span>
          </div>
          {info.images.map(image => (
            <div className="storage-row" key={image.reference}>
              <div className="grow">
                <code>{image.reference.split('@')[0]}</code>
                <div className="faint" style={{ fontSize: 12 }}>
                  {image.recipe_ids.length ? `Used by ${image.recipe_ids.map(recipeName).join(', ')}` : 'Not referenced by any current recipe'}
                  {' · '}
                  <span className="mono">{(image.reference.split('@')[1] ?? '').slice(0, 19)}</span>
                </div>
              </div>
              <span className="bytes">{formatBytes(image.bytes)}</span>
            </div>
          ))}
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
