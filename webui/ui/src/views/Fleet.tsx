import { Fragment, useEffect, useRef, useState } from 'react'
import { api, formatBytes, type Peer, type PeerSummary } from '../api'
import type { AppState } from '../App'
import { confirmBox, noticeBox } from '../confirm'
import { logoFor } from '../catalog'

interface FleetProps extends AppState {
  liveTPS: number | null
}

interface AddForm {
  name: string
  base_url: string
  api_key: string
}

const EMPTY_FORM: AddForm = { name: '', base_url: '', api_key: '' }

export default function Fleet({ system, recipes, models, liveTPS }: FleetProps) {
  const [peers, setPeers] = useState<Peer[]>([])
  const [summaries, setSummaries] = useState<Record<string, PeerSummary>>({})
  const [error, setError] = useState('')
  const [expanded, setExpanded] = useState('')
  const [form, setForm] = useState<AddForm>(EMPTY_FORM)
  const [formError, setFormError] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const dialogRef = useRef<HTMLDialogElement>(null)

  const load = () =>
    api<Peer[]>('/api/v1/peers')
      .then(list => {
        setPeers(list)
        setError('')
      })
      .catch(problem => setError(problem instanceof Error ? problem.message : 'Could not read the fleet'))
  useEffect(() => {
    load()
  }, [])

  // Poll every peer's merged summary while this tab is mounted (it unmounts
  // with every other tab switch, which stops the polling for free) and
  // skip a round while the tab is hidden, matching the rail's telemetry poll.
  useEffect(() => {
    if (peers.length === 0) return
    let cancelled = false
    const sample = async () => {
      if (document.hidden) return
      const next: Record<string, PeerSummary> = {}
      await Promise.all(
        peers.map(async peer => {
          try {
            next[peer.id] = await api<PeerSummary>(`/api/v1/peers/${encodeURIComponent(peer.id)}/summary`)
          } catch {
            next[peer.id] = { reachable: false }
          }
        }),
      )
      if (!cancelled) setSummaries(next)
    }
    sample()
    const timer = setInterval(sample, 10000)
    return () => {
      cancelled = true
      clearInterval(timer)
    }
  }, [peers])

  const openAdd = () => {
    setForm(EMPTY_FORM)
    setFormError('')
    dialogRef.current?.showModal()
  }

  const submit = async (event: React.FormEvent) => {
    event.preventDefault()
    setSubmitting(true)
    setFormError('')
    try {
      await api('/api/v1/peers', {
        method: 'POST',
        body: JSON.stringify({ name: form.name, base_url: form.base_url, api_key: form.api_key }),
      })
      dialogRef.current?.close()
      load()
    } catch (problem) {
      setFormError(problem instanceof Error ? problem.message : 'Could not add that Spark')
    } finally {
      setSubmitting(false)
    }
  }

  const remove = async (peer: Peer) => {
    const { ok } = await confirmBox({
      title: `Remove “${peer.name}” from the fleet?`,
      body: 'RunOnSpark stops polling it. Nothing changes on that Spark itself.',
      confirmLabel: 'Remove',
      danger: true,
    })
    if (!ok) return
    try {
      await api(`/api/v1/peers/${encodeURIComponent(peer.id)}`, { method: 'DELETE', body: '{}' })
      setExpanded('')
      load()
    } catch (problem) {
      noticeBox('Could not remove that Spark', problem instanceof Error ? problem.message : undefined)
    }
  }

  const thisModel = models.find(model => model.active && model.status === 'ready')
  const thisRecipe = recipes.find(recipe => recipe.id === thisModel?.recipe_id)

  if (error) return <div className="empty">{error}</div>

  return (
    <div className="stack">
      <div className="section-head">
        <span className="spacer" />
        <button className="primary" onClick={openAdd}>Add a Spark</button>
      </div>

      {peers.length === 0 ? (
        <div className="empty">One Spark here. Add another to see your whole fleet on one screen.</div>
      ) : (
        <div className="mtable fleet">
          <div className="mthead" aria-hidden="true">
            <span>Spark</span><span>Serving</span><span className="r">Memory free</span><span className="r">Version</span><span style={{ paddingLeft: 20 }}>Status</span><span /><span />
          </div>

          <div className="mrow">
            <div className="m-id"><div><div className="nm">This Spark</div><div className="use">{system?.hostname ?? 'n/a'}</div></div></div>
            <div className="m-id">
              {thisRecipe ? (
                <>
                  <img src={logoFor([thisRecipe.id])} alt="" width="24" height="24" />
                  <div className="nm" style={{ fontSize: 13 }}>{thisRecipe.display_name}</div>
                </>
              ) : (
                <span className="faint">Idle</span>
              )}
            </div>
            <div className="m-num"><span className="n">{formatBytes(system?.memory_available_bytes)}</span></div>
            <div className="m-num"><span className="n">{system?.manager_version || 'n/a'}</span></div>
            <div className="m-status">
              <span className={`sdot ${thisModel ? 'on' : ''}`} aria-hidden="true" />
              {thisModel ? 'Serving' : 'Idle'}
              {thisModel && liveTPS !== null && <span className="faint" style={{ marginLeft: 4 }}>{liveTPS.toFixed(1)} tok/s</span>}
            </div>
            <div className="m-actions" />
            <span />
          </div>

          {peers.map(peer => {
            const summary = summaries[peer.id]
            const reachable = summary?.reachable ?? false
            const peerModels = summary?.system?.installed_models ?? []
            const peerActive = peerModels.find(model => model.active && model.status === 'ready')
            const peerRecipe = recipes.find(recipe => recipe.id === peerActive?.recipe_id)
            const open = expanded === peer.id
            const toggle = () => setExpanded(open ? '' : peer.id)
            const statusLabel = !summary ? 'Checking' : !reachable ? 'Unreachable' : peerActive ? 'Serving' : 'Idle'
            const dotClass = !summary ? '' : !reachable ? 'fail' : peerActive ? 'on' : ''
            return (
              <Fragment key={peer.id}>
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
                  <div className="m-id"><div><div className="nm">{peer.name}</div><div className="use">{summary?.system?.hostname ?? 'n/a'}</div></div></div>
                  <div className="m-id">
                    {peerRecipe ? (
                      <>
                        <img src={logoFor([peerRecipe.id])} alt="" width="24" height="24" />
                        <div className="nm" style={{ fontSize: 13 }}>{peerRecipe.display_name}</div>
                      </>
                    ) : (
                      <span className="faint">{reachable ? 'Idle' : 'n/a'}</span>
                    )}
                  </div>
                  <div className="m-num"><span className="n">{reachable ? formatBytes(summary?.system?.memory_available_bytes) : 'n/a'}</span></div>
                  <div className="m-num"><span className="n">{reachable ? (summary?.system?.manager_version || 'n/a') : 'n/a'}</span></div>
                  <div className="m-status">
                    <span className={`sdot ${dotClass}`} aria-hidden="true" />
                    {statusLabel}
                  </div>
                  <div className="m-actions" onKeyDown={event => event.stopPropagation()}>
                    <button
                      className="ghost"
                      onClick={event => {
                        event.stopPropagation()
                        window.open(peer.base_url, '_blank', 'noopener,noreferrer')
                      }}
                    >
                      Open console
                    </button>
                  </div>
                  <span className={`m-caret ${open ? 'open' : ''}`} aria-hidden="true">
                    <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="M9 6l6 6-6 6" /></svg>
                  </span>
                </div>
                {open && (
                  <div className="mdetail">
                    <dl className="facts">
                      <dt>Address</dt><dd><code>{peer.base_url}</code></dd>
                    </dl>
                    <div className="row-tools">
                      <button className="danger" onClick={() => remove(peer)}>Remove</button>
                    </div>
                  </div>
                )}
              </Fragment>
            )
          })}
        </div>
      )}

      <dialog ref={dialogRef} onClose={() => setFormError('')} aria-label="Add a Spark">
        <form className="dialog-pad" onSubmit={submit}>
          <div className="dialog-head">
            <div>
              <p className="kicker">Fleet</p>
              <h2>Add a Spark</h2>
            </div>
            <button type="button" className="dialog-close" onClick={() => dialogRef.current?.close()} aria-label="Close">×</button>
          </div>
          <label className="field">
            <span>Name</span>
            <input type="text" required maxLength={64} value={form.name} onChange={event => setForm({ ...form, name: event.target.value })} />
          </label>
          <label className="field">
            <span>URL</span>
            <input
              type="text"
              required
              placeholder="http://edgexpert-beta.local:7070"
              value={form.base_url}
              onChange={event => setForm({ ...form, base_url: event.target.value })}
            />
          </label>
          <label className="field">
            <span>API key</span>
            <input type="password" required value={form.api_key} onChange={event => setForm({ ...form, api_key: event.target.value })} />
          </label>
          <p className="faint" style={{ fontSize: 12.5, margin: 0 }}>
            Generate an API key on that Spark's Connect tab, then paste it here.
          </p>
          {formError && <p className="error-text" role="alert" style={{ margin: 0 }}>{formError}</p>}
          <div className="dialog-foot">
            <button type="button" className="ghost" onClick={() => dialogRef.current?.close()}>Cancel</button>
            <button type="submit" className="primary" disabled={submitting}>{submitting ? 'Adding' : 'Add a Spark'}</button>
          </div>
        </form>
      </dialog>
    </div>
  )
}
