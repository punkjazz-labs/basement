import { Fragment, useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  adoptedName, api, bareHost, copyText, formatBytes, rankCandidates,
  type AdoptStatus, type FleetCandidate, type FleetInvitation, type FleetInviteProgress,
  type FleetSummary, type Peer, type PeerSummary,
} from '../api'
import type { AppState } from '../App'
import { confirmBox, noticeBox } from '../confirm'
import { logoFor } from '../catalog'
import { FORM_IGNORED_BY_MANAGERS, IGNORED_BY_MANAGERS } from '../fields'
import {
  fleetInvitations, fleetNodeFor, fleetSize, fleetStatusNote, fleetSummary, foundLine, foundSparks,
  invitationBody, invitationTitle, inviteBody, inviteName, inviteOutcome, inviteSettled, inviteTitle,
  inviteWaitLine, isFleetInviteProgress, joinedBadge, joinedFacts, localRoleLine, peerRoleLine,
  readIgnored, rememberIgnored, shouldSweepForSparks, sparkSubline,
  INVITATION_POLL_MS, INVITE_POLL_MS,
} from '../fleetInvite'

interface FleetProps extends AppState {
  liveTPS: number | null
}

interface AddForm {
  name: string
  base_url: string
  api_key: string
}

// One dialog, six states. The first two belong to the network sweep, the
// rest to the single machine being set up over SSH.
type FindStage = 'scanning' | 'results' | 'credentials' | 'progress' | 'failed' | 'done'

const EMPTY_FORM: AddForm = { name: '', base_url: '', api_key: '' }
// The same one-line install the website hands out. Nothing here is generated
// per machine, so it can be copied straight into the other Spark's terminal.
const INSTALL_COMMAND = 'curl -fsSL basement.punkjazz.ai/install.sh | sh'

// The adoption steps arrive with their own state; these only translate it
// into the phase list the Activity view already uses. An unknown state stays
// pending on screen and keeps its own word.
const STEP_CLASS: Record<string, string> = { done: 'complete', running: 'active', failed: 'failed', pending: 'pending' }
const STEP_WORD: Record<string, string> = { done: 'Done', running: 'Working', failed: 'Failed', pending: 'Waiting' }

export default function Fleet({ system, recipes, models, peers, refreshPeers, liveTPS }: FleetProps) {
  const [summaries, setSummaries] = useState<Record<string, PeerSummary>>({})
  const [expanded, setExpanded] = useState('')
  const [form, setForm] = useState<AddForm>(EMPTY_FORM)
  const [formError, setFormError] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [copied, setCopied] = useState<'' | 'done' | 'failed'>('')
  const dialogRef = useRef<HTMLDialogElement>(null)
  // A <dialog>'s children stay in the document while it is shut, so without
  // these flags the secret-shaped fields below would be mounted for as long
  // as this tab is, and would be torn out of the page on every tab switch.
  // Password managers read that as a login form appearing and vanishing.
  // Rendering only while the dialog is open keeps them out of the document
  // except when the user is actually filling them in.
  const [addOpen, setAddOpen] = useState(false)
  const [findOpen, setFindOpen] = useState(false)

  const [stage, setStage] = useState<FindStage>('scanning')
  const [candidates, setCandidates] = useState<FleetCandidate[]>([])
  const [scanError, setScanError] = useState('')
  const [target, setTarget] = useState<FleetCandidate | null>(null)
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [adoptError, setAdoptError] = useState('')
  const [starting, setStarting] = useState(false)
  const [status, setStatus] = useState<AdoptStatus | null>(null)
  const [tokenCopied, setTokenCopied] = useState<'' | 'done' | 'failed'>('')
  const findRef = useRef<HTMLDialogElement>(null)
  // A sweep cannot be cancelled once it is out, so its answer is dropped if
  // the dialog moved on while it was in flight.
  const scanToken = useRef(0)

  // Adding a Spark in two clicks (ADR 0019). undefined means the membership
  // summary has not answered yet, which is not the same as no fleet: the
  // quiet sweep waits for it rather than sweeping on a guess.
  const [membership, setMembership] = useState<FleetSummary | null | undefined>(undefined)
  const [swept, setSwept] = useState<FleetCandidate[]>([])
  const [ignored, setIgnored] = useState<string[]>(() => readIgnored(sessionStorage))
  const [addTarget, setAddTarget] = useState<{ consoleURL: string; name: string } | null>(null)
  const [attempt, setAttempt] = useState<FleetInviteProgress | null>(null)
  const [inviteError, setInviteError] = useState('')
  const [inviteOpen, setInviteOpen] = useState(false)
  const inviteRef = useRef<HTMLDialogElement>(null)
  const sweptOnce = useRef(false)

  // The password is only ever in this component's state and in the one
  // request that carries it. Leaving the view drops it.
  useEffect(() => () => setPassword(''), [])

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

  // Which Spark leads this fleet and which ones are really in it. Every row
  // reads its own words from here, so nothing on screen guesses a role.
  const refreshMembership = useCallback(async () => {
    try {
      setMembership(fleetSummary(await api<unknown>('/api/v1/fleet')))
    } catch {
      setMembership(null)
    }
  }, [])

  useEffect(() => {
    void refreshMembership()
  }, [refreshMembership])

  // One quiet sweep per visit to this screen, and only while the fleet is
  // small enough that a person is still assembling it. It reuses the same
  // discovery the Find dialog runs and says nothing when it finds nothing.
  useEffect(() => {
    if (membership === undefined || sweptOnce.current) return
    if (!shouldSweepForSparks(fleetSize(membership, peers))) return
    sweptOnce.current = true
    let cancelled = false
    void (async () => {
      try {
        const found = await api<{ candidates: FleetCandidate[] }>('/api/v1/fleet/discover', {
          method: 'POST',
          body: '{}',
        })
        if (!cancelled) setSwept(found.candidates ?? [])
      } catch {
        /* a quiet sweep that fails stays quiet */
      }
    })()
    return () => {
      cancelled = true
    }
  }, [membership, peers])

  const found = useMemo(
    () => foundSparks(swept, peers, membership ?? null, ignored, window.location.origin),
    [swept, peers, membership, ignored],
  )

  // The tab is opened from the click itself: a browser blocks a window opened
  // after an await, and the dialog promises that tab is already there.
  const addToFleet = (consoleURL: string, name: string, displayName = '') => {
    window.open(consoleURL, '_blank', 'noopener,noreferrer')
    setAddTarget({ consoleURL, name })
    setAttempt(null)
    setInviteError('')
    setInviteOpen(true)
    inviteRef.current?.showModal()
    void (async () => {
      try {
        const answer = await api<unknown>('/api/v1/fleet/invite', {
          method: 'POST',
          body: JSON.stringify({ console_url: consoleURL, display_name: displayName }),
        })
        if (isFleetInviteProgress(answer)) setAttempt(answer)
      } catch (problem) {
        setInviteError(problem instanceof Error ? problem.message : 'Could not ask that Spark to join')
      }
    })()
  }

  // Reading the status is what advances the addition, including running the
  // adoption once the owner has approved it, so this poll is the flow rather
  // than a view of it. It stops the moment the attempt has an answer.
  useEffect(() => {
    if (!inviteOpen || !addTarget || inviteError) return
    if (attempt && inviteSettled(attempt.state)) return
    let cancelled = false
    const poll = async () => {
      try {
        const answer = await api<unknown>(
          `/api/v1/fleet/invite/status?console_url=${encodeURIComponent(addTarget.consoleURL)}`,
        )
        if (cancelled || !isFleetInviteProgress(answer)) return
        setAttempt(answer)
        if (answer.state === 'done') {
          await refreshPeers()
          await refreshMembership()
        }
      } catch {
        /* the next tick asks again */
      }
    }
    const timer = setInterval(poll, INVITE_POLL_MS)
    return () => {
      cancelled = true
      clearInterval(timer)
    }
  }, [inviteOpen, addTarget, inviteError, attempt, refreshPeers, refreshMembership])

  const closeInvite = () => {
    setInviteOpen(false)
    setAddTarget(null)
    setAttempt(null)
    setInviteError('')
  }

  const openAdd = (prefill?: Partial<AddForm>) => {
    setForm({ ...EMPTY_FORM, ...prefill })
    setFormError('')
    setCopied('')
    setAddOpen(true)
    dialogRef.current?.showModal()
  }

  // The sweep blocks for as long as it takes; there is nothing to show in the
  // meantime except that it is running.
  const scan = useCallback(async () => {
    const token = ++scanToken.current
    setStage('scanning')
    setScanError('')
    setCandidates([])
    try {
      const found = await api<{ candidates: FleetCandidate[] }>('/api/v1/fleet/discover', {
        method: 'POST',
        body: '{}',
      })
      if (scanToken.current !== token) return
      setCandidates(rankCandidates(found.candidates ?? []))
    } catch (problem) {
      if (scanToken.current !== token) return
      setScanError(problem instanceof Error ? problem.message : 'The scan did not finish')
    }
    setStage('results')
  }, [])

  const openFind = async () => {
    scanToken.current += 1
    setStage('scanning')
    setTarget(null)
    setUsername('')
    setPassword('')
    setAdoptError('')
    setStatus(null)
    setTokenCopied('')
    setFindOpen(true)
    findRef.current?.showModal()
    // A setup started earlier and still running owns this dialog: show it
    // instead of a scan whose results could not be acted on anyway.
    try {
      const current = await api<AdoptStatus>('/api/v1/fleet/adopt/status')
      if (current.state === 'running') {
        setStatus(current)
        setStage('progress')
        return
      }
    } catch {
      /* no run to resume is the ordinary case */
    }
    scan()
  }

  const pair = (candidate: FleetCandidate) => {
    findRef.current?.close()
    openAdd({ name: candidate.name, base_url: candidate.basement?.base_url ?? '' })
  }

  const setUp = (candidate: FleetCandidate) => {
    setTarget(candidate)
    setPassword('')
    setAdoptError('')
    setStage('credentials')
  }

  const startAdopt = async (event: React.FormEvent) => {
    event.preventDefault()
    if (!target) return
    setStarting(true)
    setAdoptError('')
    try {
      // The 202 carries the first status snapshot, so the step list is on
      // screen before the first poll comes back.
      const snapshot = await api<AdoptStatus>('/api/v1/fleet/adopt', {
        method: 'POST',
        body: JSON.stringify({ address: bareHost(target.address), username, password }),
      })
      setStatus(snapshot)
      setStage('progress')
    } catch (problem) {
      setAdoptError(problem instanceof Error ? problem.message : 'Could not start the setup')
    } finally {
      // Whatever happened, the password has left this component. A retry
      // asks for it again.
      setPassword('')
      setStarting(false)
    }
  }

  // Poll only while a run is in flight. Reaching a result or an error moves
  // the dialog on, which is also what stops this.
  useEffect(() => {
    if (stage !== 'progress') return
    let cancelled = false
    const poll = async () => {
      try {
        const next = await api<AdoptStatus>('/api/v1/fleet/adopt/status')
        if (cancelled) return
        setStatus(next)
        if (next.state === 'succeeded') {
          await refreshPeers()
          if (!cancelled) setStage('done')
          return
        }
        // Only succeeded and failed are terminal. An idle answer means the
        // manager has not recorded the run yet, so ask again.
        if (next.state === 'failed') setStage('failed')
      } catch {
        /* the next tick asks again */
      }
    }
    poll()
    const timer = setInterval(poll, 2000)
    return () => {
      cancelled = true
      clearInterval(timer)
    }
  }, [stage, refreshPeers])

  const copyToken = async () => {
    const token = status?.result?.owner_pairing_token
    if (!token) return
    try {
      await copyText(token)
      setTokenCopied('done')
      setTimeout(() => setTokenCopied(''), 1600)
    } catch {
      // Say so rather than looking like it worked: the token is on screen
      // and can still be selected by hand.
      setTokenCopied('failed')
      setTimeout(() => setTokenCopied(''), 2600)
    }
  }

  const copyCommand = async () => {
    try {
      await copyText(INSTALL_COMMAND)
      setCopied('done')
      setTimeout(() => setCopied(''), 1600)
    } catch {
      setCopied('failed')
      setTimeout(() => setCopied(''), 2600)
    }
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
      await refreshPeers()
    } catch (problem) {
      setFormError(problem instanceof Error ? problem.message : 'Could not add that Spark')
    } finally {
      setSubmitting(false)
    }
  }

  const remove = async (peer: Peer) => {
    const { ok } = await confirmBox({
      title: `Remove “${peer.name}” from the fleet?`,
      body: 'Basement stops polling it. Nothing changes on that Spark itself.',
      confirmLabel: 'Remove',
      danger: true,
    })
    if (!ok) return
    try {
      await api(`/api/v1/peers/${encodeURIComponent(peer.id)}`, { method: 'DELETE', body: '{}' })
      setExpanded('')
      await refreshPeers()
    } catch (problem) {
      noticeBox('Could not remove that Spark', problem instanceof Error ? problem.message : undefined)
    }
  }

  // Closing puts the dialog back to its first state, unless a setup is under
  // way: that one keeps polling with the dialog shut, so the fleet table
  // still gains the Spark the moment the run lands.
  const closeFind = () => {
    scanToken.current += 1
    setFindOpen(false)
    setUsername('')
    setPassword('')
    setCandidates([])
    setScanError('')
    if (stage === 'scanning' || stage === 'results' || stage === 'credentials') {
      setAdoptError('')
      setStage('scanning')
    }
  }

  const thisModel = models.find(model => model.active && model.status === 'ready')
  const thisRecipe = recipes.find(recipe => recipe.id === thisModel?.recipe_id)
  const adoptSteps = status?.steps ?? []
  const failedStep = adoptSteps.find(step => step.state === 'failed')
  const progress = status?.progress ?? []
  const latestProgress = progress.length > 0 ? progress[progress.length - 1] : ''
  const setupHost = status?.address || (target ? bareHost(target.address) : '')
  const result = status?.result
  const newName = adoptedName(result) || target?.name || 'Your second Spark'
  // The Spark being added calls itself something; until it has answered, the
  // row or the sweep's own label stands in.
  const targetName = inviteName(attempt, addTarget?.name ?? '')
  const inviteState = attempt?.state ?? 'waiting'
  // Only claimed when the catalog actually carries a two-Spark recipe.
  const hasTwoSparkRecipe = recipes.some(recipe => recipe.topology.spark_count === 2)

  return (
    <div className="stack">
      {peers.length === 0 ? (
        // One peer is what basement supports today, so both ways in live
        // here and both disappear once a second Spark exists.
        <div className="empty">
          <p>One Spark here. Add another to see your whole fleet on one screen.</p>
          <div className="empty-actions">
            <button className="primary" onClick={openFind}>Find a second Spark</button>
            <button className="quiet" onClick={() => openAdd()}>Add by address</button>
          </div>
        </div>
      ) : (
        <div className="mtable fleet">
          <div className="mthead" aria-hidden="true">
            <span>Spark</span><span>Serving</span><span className="r">Memory free</span><span className="r">Version</span><span style={{ paddingLeft: 20 }}>Status</span><span /><span />
          </div>

          <div className="mrow">
            <div className="m-id">
              <div>
                <div className="nm">This Spark</div>
                <div className="use">{sparkSubline(system?.hostname ?? '', localRoleLine(membership ?? null))}</div>
              </div>
            </div>
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
            // A peer this console only polls over its API and a peer that is
            // really in the fleet are the same row; only membership decides
            // whether it can still be added.
            const memberNode = fleetNodeFor(membership ?? null, peer.base_url)
            const fleetNote = fleetStatusNote(memberNode)
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
                  <div className="m-id">
                    <div>
                      <div className="nm">{peer.name}</div>
                      <div className="use">
                        {sparkSubline(summary?.system?.hostname ?? '', peerRoleLine(membership ?? null, memberNode))}
                      </div>
                    </div>
                  </div>
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
                    <span>
                      {statusLabel}
                      {fleetNote && <span className="peer-note">{fleetNote}</span>}
                    </span>
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
                      {/* One verb for every Spark that is not in the fleet
                          yet, whichever way it was added to this console. */}
                      {!fleetNote && (
                        <button className="primary" onClick={() => addToFleet(peer.base_url, peer.name, peer.name)}>
                          Add to fleet
                        </button>
                      )}
                      <button className="danger" onClick={() => remove(peer)}>Remove</button>
                    </div>
                  </div>
                )}
              </Fragment>
            )
          })}
        </div>
      )}

      {/* A machine already running basement that this fleet has never met.
          Ignoring it says not now, for this session only. */}
      {found.map(spark => (
        <div key={spark.consoleURL} className="found">
          <div className="grow">
            <strong>{spark.name} is on this network</strong>
            <span>{foundLine(spark)}</span>
          </div>
          <button className="primary" onClick={() => addToFleet(spark.consoleURL, spark.name)}>Add to fleet</button>
          <button className="quiet" onClick={() => setIgnored(rememberIgnored(sessionStorage, spark.consoleURL))}>
            Ignore
          </button>
        </div>
      ))}

      <dialog
        ref={dialogRef}
        onClose={() => {
          setFormError('')
          setAddOpen(false)
          setForm(EMPTY_FORM)
        }}
        aria-label="Add a Spark"
      >
        {/* Three steps in one dialog, because the first two happen on the
            other machine and the third is the form that was always here. */}
        {addOpen && (
        <form className="dialog-pad" onSubmit={submit} {...FORM_IGNORED_BY_MANAGERS}>
          <div className="dialog-head">
            <div>
              <p className="kicker">Fleet</p>
              <h2>Add a Spark</h2>
            </div>
            <button type="button" className="dialog-close" onClick={() => dialogRef.current?.close()} aria-label="Close">×</button>
          </div>
          <ol className="steps">
            <li>
              <strong>Install basement on the other Spark</strong>
              <p>Run this in a terminal on that machine.</p>
              <div className="snippet">
                <button type="button" className="ghost copy" onClick={copyCommand}>
                  {copied === 'done' ? 'Copied' : copied === 'failed' ? 'Select it by hand' : 'Copy'}
                </button>
                <pre><code>{INSTALL_COMMAND}</code></pre>
              </div>
            </li>
            <li>
              <strong>Generate an API key on that Spark's Connect tab</strong>
              <p>Open that console, create a key, and copy it. It is only shown once.</p>
            </li>
            <li>
              <strong>Add it here</strong>
              <label className="field">
                <span>Name</span>
                <input
                  type="text"
                  name="spark-label"
                  id="spark-label"
                  required
                  maxLength={64}
                  value={form.name}
                  onChange={event => setForm({ ...form, name: event.target.value })}
                  {...IGNORED_BY_MANAGERS}
                />
              </label>
              <label className="field">
                <span>URL</span>
                <input
                  type="text"
                  name="spark-address"
                  id="spark-address"
                  required
                  placeholder="http://edgexpert-beta.local:7070"
                  value={form.base_url}
                  onChange={event => setForm({ ...form, base_url: event.target.value })}
                  {...IGNORED_BY_MANAGERS}
                />
              </label>
              {/* Not masked: this key was shown in the clear on the other
                  Spark's Connect tab a moment ago, it is pasted once and
                  never read back here, and masking it only hid a bad paste
                  until the request failed. */}
              <label className="field">
                <span>API key</span>
                <input
                  type="text"
                  name="spark-access-token"
                  id="spark-access-token"
                  required
                  spellCheck={false}
                  value={form.api_key}
                  onChange={event => setForm({ ...form, api_key: event.target.value })}
                  {...IGNORED_BY_MANAGERS}
                />
              </label>
              <p className="faint" style={{ fontSize: 12.5, margin: 0 }}>
                Basement calls that Spark with this URL and key before it saves anything.
              </p>
            </li>
          </ol>
          {formError && <p className="error-text" role="alert" style={{ margin: 0 }}>{formError}</p>}
          <div className="dialog-foot">
            <button type="button" className="ghost" onClick={() => dialogRef.current?.close()}>Cancel</button>
            <button type="submit" className="primary" disabled={submitting}>{submitting ? 'Adding' : 'Add a Spark'}</button>
          </div>
        </form>
        )}
      </dialog>

      <dialog ref={findRef} onClose={closeFind} aria-label="Find a second Spark">
        {findOpen && (
        <div className="dialog-pad">
          <div className="dialog-head">
            <div>
              <p className="kicker">Fleet</p>
              <h2>
                {stage === 'credentials' ? 'Set up this Spark'
                  : stage === 'progress' || stage === 'failed' ? 'Setting up this Spark'
                    : stage === 'done' ? 'Second Spark added'
                      : 'Find a second Spark'}
              </h2>
            </div>
            <button type="button" className="dialog-close" onClick={() => findRef.current?.close()} aria-label="Close">×</button>
          </div>

          {stage === 'scanning' && (
            <>
              <p className="thinking">Looking for Sparks on your network</p>
              <p className="faint dialog-note">This takes up to ten seconds.</p>
              <div className="dialog-foot">
                <button type="button" className="ghost" onClick={() => findRef.current?.close()}>Cancel</button>
              </div>
            </>
          )}

          {stage === 'results' && (
            <>
              {scanError ? (
                <>
                  <div className="error-note">
                    <strong>The scan did not finish</strong>
                    <p>{scanError}</p>
                  </div>
                  <p className="faint dialog-note">You can scan again, or add a Spark by address.</p>
                </>
              ) : candidates.length === 0 ? (
                <>
                  <div className="empty">Nothing answered on this network.</div>
                  <p className="faint dialog-note">
                    A Spark that is off, on another network, or blocking this scan will not show up here. You can still add one by address.
                  </p>
                </>
              ) : (
                <>
                  <p className="muted dialog-note">
                    These machines answered. Each label is what the scan could tell from outside, not a promise.
                  </p>
                  <div className="cand-list">
                    {candidates.map(candidate => (
                      <div key={candidate.address} className={`cand-row ${!candidate.basement && !candidate.gb10_hint ? 'plain' : ''}`}>
                        <div className="grow">
                          <div className="nm">
                            {candidate.name || candidate.address}
                            {candidate.basement ? (
                              <span className="tag">Running basement</span>
                            ) : candidate.gb10_hint ? (
                              <span className="tag quiet">Likely a GB10</span>
                            ) : (
                              <span className="tag quiet">Host on your network</span>
                            )}
                          </div>
                          <div className="use">{candidate.address}</div>
                        </div>
                        {candidate.basement ? (
                          <button type="button" className="primary" onClick={() => pair(candidate)}>Pair</button>
                        ) : (
                          <button
                            type="button"
                            className={candidate.gb10_hint ? 'primary' : 'ghost'}
                            onClick={() => setUp(candidate)}
                          >
                            Set up
                          </button>
                        )}
                      </div>
                    ))}
                  </div>
                </>
              )}
              <div className="dialog-foot">
                <button
                  type="button"
                  className="quiet lead"
                  onClick={() => {
                    findRef.current?.close()
                    openAdd()
                  }}
                >
                  Add by address
                </button>
                <button type="button" className="ghost" onClick={scan}>Scan again</button>
                <button type="button" className="ghost" onClick={() => findRef.current?.close()}>Cancel</button>
              </div>
            </>
          )}

          {/* This really is a login pair, so a manager offering to save it
              is not a misfire in principle. It is still wrong here: the
              credential belongs to the SSH account on the other Spark, and a
              manager would file it against this console's own web address,
              where it would be offered as the console's login and never fit.
              Basement keeps no copy either, so both fields opt out and a
              retry asks again. */}
          {stage === 'credentials' && (
            <form className="dialog-form" onSubmit={startAdopt} {...FORM_IGNORED_BY_MANAGERS}>
              <p className="muted dialog-note">
                Basement installs itself on that Spark over SSH from this one. Your password goes to that machine only. It is not stored here, and no password manager is asked to keep it.
              </p>
              <label className="field">
                <span>Address</span>
                <input
                  type="text"
                  name="ssh-host"
                  id="ssh-host"
                  readOnly
                  value={target ? bareHost(target.address) : ''}
                  {...IGNORED_BY_MANAGERS}
                />
                <small className="faint">Just the host. Basement brings its own port.</small>
              </label>
              <label className="field">
                <span>Username on that Spark</span>
                <input
                  type="text"
                  name="ssh-account"
                  id="ssh-account"
                  required
                  spellCheck={false}
                  value={username}
                  onChange={event => setUsername(event.target.value)}
                  {...IGNORED_BY_MANAGERS}
                />
              </label>
              <label className="field">
                <span>Password</span>
                <input
                  type="password"
                  name="ssh-secret"
                  id="ssh-secret"
                  required
                  value={password}
                  onChange={event => setPassword(event.target.value)}
                  {...IGNORED_BY_MANAGERS}
                />
              </label>
              {adoptError && <p className="error-text dialog-note" role="alert">{adoptError}</p>}
              <div className="dialog-foot">
                <button type="button" className="ghost" onClick={() => setStage('results')}>Back</button>
                <button type="submit" className="primary" disabled={starting}>{starting ? 'Starting' : 'Start'}</button>
              </div>
            </form>
          )}

          {(stage === 'progress' || stage === 'failed') && (
            <>
              {setupHost && <p className="faint dialog-note">{setupHost}</p>}
              {adoptSteps.length === 0 ? (
                <p className="thinking">Starting</p>
              ) : (
                <ol className="phase-list">
                  {adoptSteps.map(step => {
                    // A step's own detail wins; the running one borrows the
                    // latest progress line when it has nothing of its own.
                    const line = step.detail || (step.state === 'running' ? latestProgress : '')
                    return (
                      <li key={step.key} className={STEP_CLASS[step.state] ?? 'pending'}>
                        <i aria-hidden="true" />
                        <div>
                          <strong>{step.label}</strong>
                          {line && <span>{line}</span>}
                        </div>
                        <b>{STEP_WORD[step.state] ?? step.state}</b>
                      </li>
                    )
                  })}
                </ol>
              )}
              {/* The failed step already carries its sentence; only say it
                  once. */}
              {stage === 'failed' && status?.error && !failedStep?.detail && (
                <div className="error-note">
                  <strong>Setup stopped</strong>
                  <p>{status.error}</p>
                </div>
              )}
              <div className="dialog-foot">
                {stage === 'progress' && <span className="note">This keeps running if you close the dialog.</span>}
                <button type="button" className="ghost" onClick={() => findRef.current?.close()}>Close</button>
                {stage === 'failed' && target && (
                  <button type="button" className="primary" onClick={() => setStage('credentials')}>Retry</button>
                )}
              </div>
            </>
          )}

          {stage === 'done' && (
            <>
              <p className="done-line">{newName} is part of your basement now.</p>
              {result?.owner_pairing_token && (
                <>
                  <p className="muted dialog-note">
                    Its console will ask for this pairing token the first time you open it. Type it in there.
                  </p>
                  <div className="snippet token">
                    <button type="button" className="ghost copy" onClick={copyToken}>
                      {tokenCopied === 'done' ? 'Copied' : tokenCopied === 'failed' ? 'Select it by hand' : 'Copy'}
                    </button>
                    <pre><code>{result.owner_pairing_token}</code></pre>
                  </div>
                  <p className="faint dialog-note">It stays valid after that, so keep it like a password.</p>
                </>
              )}
              <p className="muted dialog-note">The fleet table shows what it is serving.</p>
              {hasTwoSparkRecipe && (
                <p className="faint dialog-note">Models that need two Sparks can be installed now. They are on the Models tab.</p>
              )}
              <div className="dialog-foot">
                <button type="button" className="ghost" onClick={() => findRef.current?.close()}>Done</button>
                {result?.owner_pairing_url && (
                  <button
                    type="button"
                    className="primary"
                    onClick={() => window.open(result.owner_pairing_url, '_blank', 'noopener,noreferrer')}
                  >
                    Open its console
                  </button>
                )}
              </div>
            </>
          )}
        </div>
        )}
      </dialog>

      {/* The addition itself: one sentence about what to do on the other
          machine, then what happened. Nothing here is typed or copied. */}
      <dialog ref={inviteRef} onClose={closeInvite} aria-label="Add to fleet">
        {inviteOpen && (
        <div className="dialog-pad">
          <div className="dialog-head">
            <div>
              <p className="kicker">Fleet</p>
              <h2>{inviteError ? `Could not add ${targetName}` : inviteTitle(inviteState, targetName)}</h2>
            </div>
            <button type="button" className="dialog-close" onClick={() => inviteRef.current?.close()} aria-label="Close">×</button>
          </div>

          {inviteError ? (
            <>
              <div className="error-note">
                <strong>That Spark could not be asked to join</strong>
                <p>{inviteError}</p>
              </div>
              <div className="dialog-foot">
                <button type="button" className="ghost" onClick={() => inviteRef.current?.close()}>Close</button>
              </div>
            </>
          ) : inviteState === 'done' ? (
            <>
              <p className="join-line">
                <span className="sdot on" aria-hidden="true" />
                {joinedBadge(attempt?.node?.manager_version ?? '')}
              </p>
              <dl className="facts">
                {joinedFacts(fleetSize(membership ?? null, peers), targetName).map(fact => (
                  <Fragment key={fact.label}>
                    <dt>{fact.label}</dt><dd>{fact.value}</dd>
                  </Fragment>
                ))}
              </dl>
              <div className="dialog-foot">
                <button type="button" className="primary" onClick={() => inviteRef.current?.close()}>Done</button>
              </div>
            </>
          ) : inviteSettled(inviteState) && attempt ? (
            <>
              <p className="muted dialog-note">{inviteOutcome(attempt, targetName)}</p>
              <div className="dialog-foot">
                <button type="button" className="ghost" onClick={() => inviteRef.current?.close()}>Close</button>
              </div>
            </>
          ) : (
            <>
              <p className="muted dialog-note">{inviteBody(targetName)}</p>
              <p className="join-line">
                <span className="sdot busy" aria-hidden="true" />
                {inviteWaitLine(inviteState)}
              </p>
              <div className="dialog-foot">
                <button type="button" className="quiet" onClick={() => inviteRef.current?.close()}>Cancel</button>
              </div>
            </>
          )}
        </div>
        )}
      </dialog>
    </div>
  )
}

// Every console asks whether a Spark wants to adopt it, because the machine
// being added is where the owner says yes. The answer is an owner session
// here and nothing else: no code is read, and none is shown.
export function FleetInvitationPrompt({ onAnswered }: { onAnswered: () => void }) {
  const [invitation, setInvitation] = useState<FleetInvitation | null>(null)
  const [answering, setAnswering] = useState(false)
  const [error, setError] = useState('')
  const ref = useRef<HTMLDialogElement>(null)

  useEffect(() => {
    let cancelled = false
    const poll = async () => {
      if (document.hidden) return
      try {
        const waiting = fleetInvitations(await api<unknown>('/api/v1/fleet/invitations'))
        if (!cancelled) setInvitation(waiting[0] ?? null)
      } catch {
        /* nothing waiting is the ordinary answer */
      }
    }
    void poll()
    const timer = setInterval(poll, INVITATION_POLL_MS)
    return () => {
      cancelled = true
      clearInterval(timer)
    }
  }, [])

  useEffect(() => {
    const dialog = ref.current
    if (!dialog) return
    if (invitation && !dialog.open) dialog.showModal()
    if (!invitation && dialog.open) dialog.close()
  }, [invitation])

  const answer = async (action: 'approve' | 'deny') => {
    if (!invitation) return
    setAnswering(true)
    setError('')
    try {
      await api(`/api/v1/fleet/invitations/${encodeURIComponent(invitation.id)}/${action}`, {
        method: 'POST',
        body: '{}',
      })
      setInvitation(null)
      onAnswered()
    } catch (problem) {
      setError(problem instanceof Error ? problem.message : 'That answer did not go through')
    } finally {
      setAnswering(false)
    }
  }

  return (
    <dialog ref={ref} onClose={() => setInvitation(null)} aria-label="A Spark wants to add this one">
      {invitation && (
        <div className="dialog-pad">
          <div className="dialog-head">
            <div>
              <p className="kicker">Fleet</p>
              <h2>{invitationTitle(invitation)}</h2>
            </div>
          </div>
          <p className="muted dialog-note">{invitationBody(invitation)}</p>
          {error && <p className="error-text dialog-note" role="alert">{error}</p>}
          <div className="dialog-foot">
            <button type="button" className="quiet" disabled={answering} onClick={() => answer('deny')}>Deny</button>
            <button type="button" className="primary" disabled={answering} onClick={() => answer('approve')}>Approve</button>
          </div>
        </div>
      )}
    </dialog>
  )
}
