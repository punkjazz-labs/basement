import { Fragment, useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import {
  adoptedName, api, bareHost, copyText, formatBytes, idempotency, rankCandidates,
  type AdoptStatus, type FleetCandidate, type FleetInvitation, type FleetInviteProgress,
  type FleetPowerMode, type FleetSummary, type Peer, type PeerSummary,
} from '../api'
import type { AppState } from '../App'
import { confirmBox, noticeBox } from '../confirm'
import { Mark } from '../mark'
import { Tip } from '../tip'
import { FORM_IGNORED_BY_MANAGERS, IGNORED_BY_MANAGERS } from '../fields'
import {
  fleetInvitations, fleetNodeFor, fleetSize, fleetStatusNote, fleetSummary, foundLine, foundSparks,
  invitationBody, invitationTitle, inviteBody, inviteName, inviteOutcome, inviteSettled, inviteTitle,
  inviteWaitLine, isFleetInviteProgress, joinedBadge, joinedFacts, localRoleLine, membershipRows,
  nodeFacts, nodeHostname, nodeName, nodeReported, nodeServing, nodeStatus, peerRoleLine,
  readIgnored, rememberIgnored, shouldSweepForSparks, sparkSubline,
  INVITATION_POLL_MS, INVITE_POLL_MS, MEMBERSHIP_POLL_MS,
} from '../fleetInvite'
import {
  fleetRows, localPowerRow, powerFanOut, powerFanOutBusy, powerRefusalLine, powerRefusedTitle,
  powerRow, retiredPowerSets,
  COOL_MODE, COOL_MODE_LABEL, EVERY_SPARK, FLEET_POWER_MODE_PATH, FULL_MODE, FULL_MODE_LABEL,
  LOCAL_POWER_MODE_PATH, POWER_MODE_BUSY, POWER_MODE_NOTE, POWER_REFUSED_TITLE,
  type FleetRow, type PowerRow, type PowerState,
} from '../fleetModels'

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
// What this page calls the machine the console is open on, in the table and
// at the power switch alike.
const THIS_SPARK = 'This Spark'
// The same one-line install the website hands out. Nothing here is generated
// per machine, so it can be copied straight into the other Spark's terminal.
const INSTALL_COMMAND = 'curl -fsSL basement.punkjazz.ai/install.sh | sh'

// The adoption steps arrive with their own state; these only translate it
// into the phase list the Activity view already uses. An unknown state stays
// pending on screen and keeps its own word.
const STEP_CLASS: Record<string, string> = { done: 'complete', running: 'active', failed: 'failed', pending: 'pending' }
const STEP_WORD: Record<string, string> = { done: 'Done', running: 'Working', failed: 'Failed', pending: 'Waiting' }

// One Spark's power switch: the machine, the two modes, and whatever else
// that row carries. The mode holds across restarts, so this is a setting and
// not an action. A Spark that has reported no mode gets a dead switch and no
// selection: nothing here guesses that a silent Spark runs at full speed.
export function PowerSwitch({ name, power, onSet, children }: {
  name: string
  power: PowerRow
  onSet: (mode: string) => void
  children?: ReactNode
}) {
  return (
    <div className="prow">
      <span className="nm">{name}</span>
      <span className="seg" role="group" aria-label={`Power mode for ${name}`}>
        <button
          type="button"
          aria-pressed={power.mode === FULL_MODE}
          disabled={power.disabled}
          onClick={() => onSet(FULL_MODE)}
        >
          {FULL_MODE_LABEL}
        </button>
        <button
          type="button"
          aria-pressed={power.mode === COOL_MODE}
          disabled={power.disabled}
          onClick={() => onSet(COOL_MODE)}
        >
          {COOL_MODE_LABEL}
        </button>
      </span>
      {children}
      {power.busy && <span className="busy">{POWER_MODE_BUSY}</span>}
      {/* That Spark's own words about a GPU that did not take the mode. The
          mode the owner chose stays chosen while this stands: the setting is
          stored, and it goes back on the chip at the next start. A refusal is
          not tooltip material, so it stays on the screen. */}
      {power.failure !== '' && <p className="powerfail">{power.failure}</p>}
    </div>
  )
}

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

  // The power mode this console last set on each Spark, and what that Spark
  // answered about it. The membership poll is up to ten seconds behind a
  // change, and that Spark's own heartbeat is behind that again, so without
  // this the switch would jump back to the old mode for a moment.
  const [powerSet, setPowerSet] = useState<Map<string, PowerState>>(new Map())
  // Every Spark a power change is running on now. A fleet-wide run names all
  // of them at once, so no row can take a click the run is about to overwrite.
  const [powerSetting, setPowerSetting] = useState<Set<string>>(new Set())
  // What this Spark says about its own chip, for a console that leads no
  // fleet. null is a mode this console has not read, which is not a mode.
  const [localPower, setLocalPower] = useState<PowerState | null>(null)
  const [localPowerBusy, setLocalPowerBusy] = useState(false)

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

  // Every Spark in the fleet that no peer row already speaks for: one added
  // the two-click way was never a peer of this console, and had no row here
  // at all until now.
  const memberRows = useMemo(
    () => membershipRows(membership ?? null, peers, window.location.origin),
    [membership, peers],
  )

  // Which Spark this console speaks for. A controller answers for every Spark
  // in its fleet, its own machine included; every other console answers for
  // the one machine it runs on, and a member for none: the controller owns
  // that machine's settings, and a switch that could only be refused would be
  // a lie.
  const leadsFleet = membership?.role === 'controller'
  const isMember = membership?.role === 'member'

  // These rows do not poll the machines they describe: their freshness is the
  // membership summary's, so the summary is what gets re-read, once for the
  // whole fleet and only while something on this screen depends on it. The
  // power switches depend on it too: a mode set from another console reaches
  // this one through the same summary.
  const memberRowCount = memberRows.length
  useEffect(() => {
    if (memberRowCount === 0 && !leadsFleet) return
    const poll = () => {
      if (document.hidden) return
      void refreshMembership()
    }
    const timer = setInterval(poll, MEMBERSHIP_POLL_MS)
    return () => clearInterval(timer)
  }, [memberRowCount, leadsFleet, refreshMembership])

  // Every Spark this console can set the mode on, in the order the fleet
  // reports them. Read at the moment the summary arrives, which is the only
  // moment any of it is fresh.
  const sparks = useMemo(
    () => (leadsFleet ? fleetRows(membership ?? null, peers, window.location.origin, Date.now()) : []),
    [leadsFleet, membership, peers],
  )
  // What each Spark's switch shows: what that Spark reported, what this
  // console set on it a moment ago, and whether a change is running there.
  const powerRows = useMemo(
    () => new Map(sparks.map(spark =>
      [spark.nodeID, powerRow(spark, powerSet.get(spark.nodeID), powerSetting)])),
    [sparks, powerSet, powerSetting],
  )

  // A mode this console set is kept only until the fleet read carries it.
  // This runs on every read, so the answer is retired the moment it has
  // nothing left to add, and every later change is read from the fleet like
  // any other fact about that Spark.
  useEffect(() => {
    const retired = retiredPowerSets(sparks, powerSet, powerSetting)
    if (retired.length === 0) return
    setPowerSet(previous => {
      const next = new Map(previous)
      for (const nodeID of retired) next.delete(nodeID)
      return next
    })
  }, [sparks, powerSet, powerSetting])

  // Set the power mode on one Spark, or on every Spark in the fleet. One call
  // per Spark, sent one after another, through the fleet door: the controller
  // takes its own node id there too, so this console holds one way to write
  // the setting rather than a local one and a remote one.
  //
  // Every Spark named is locked for the whole run. A Spark answers with the
  // mode it now holds and with its own sentence if the GPU refused the
  // change, and both stand until that Spark's next heartbeat carries them. A
  // call that was refused outright changed nothing there, so that row goes
  // back to what it last reported and the refusal is said in one notice at
  // the end, one line per Spark.
  const setPowerMode = async (targets: FleetRow[], mode: string) => {
    if (mode === '' || targets.length === 0) return
    if (targets.some(target => powerSetting.has(target.nodeID))) return
    const named = targets.map(target => target.nodeID)
    setPowerSetting(previous => new Set([...previous, ...named]))
    setPowerSet(previous => {
      const next = new Map(previous)
      for (const id of named) next.set(id, { mode, failure: '' })
      return next
    })
    const refused: string[] = []
    try {
      for (const target of targets) {
        try {
          const answer = await api<FleetPowerMode>(FLEET_POWER_MODE_PATH, {
            method: 'POST',
            headers: idempotency(),
            body: JSON.stringify({ node_id: target.nodeID, mode }),
          })
          setPowerSet(previous =>
            new Map(previous).set(target.nodeID, { mode: answer.mode, failure: answer.failure ?? '' }))
        } catch (problem) {
          setPowerSet(previous => {
            const next = new Map(previous)
            next.delete(target.nodeID)
            return next
          })
          refused.push(powerRefusalLine(
            target.displayName, problem instanceof Error ? problem.message : String(problem)))
        }
      }
    } finally {
      setPowerSetting(previous => {
        const next = new Set(previous)
        for (const id of named) next.delete(id)
        return next
      })
    }
    // A run over one Spark has one Spark to name; a fleet-wide run names none
    // in the title and every refused one in the lines under it.
    if (refused.length > 0) {
      noticeBox(
        targets.length === 1 ? powerRefusedTitle(targets[0].displayName) : POWER_REFUSED_TITLE,
        refused.join('\n'),
      )
    }
  }

  // What this Spark holds now, read from its own door. A console that leads a
  // fleet never asks: the summary already carries the mode of every Spark in
  // it, this machine included.
  useEffect(() => {
    // undefined is a summary that has not answered yet, which is not the same
    // as no fleet: the read waits for it rather than asking on a guess.
    if (membership === undefined || leadsFleet || isMember) return
    let cancelled = false
    api<PowerState>(LOCAL_POWER_MODE_PATH)
      .then(state => {
        if (!cancelled) setLocalPower({ mode: state.mode ?? '', failure: state.failure ?? '' })
      })
      .catch(() => {
        /* a manager that cannot read the mode leaves the switch dead */
      })
    return () => {
      cancelled = true
    }
  }, [membership, leadsFleet, isMember])

  // The same change on a Spark that answers for itself. The answer is the
  // whole new state, so what the machine now holds and its own sentence about
  // a GPU that refused the cap both come back together.
  const setLocalPowerMode = async (mode: string) => {
    if (mode === '' || localPowerBusy) return
    setLocalPowerBusy(true)
    try {
      const answer = await api<PowerState>(LOCAL_POWER_MODE_PATH, {
        method: 'POST',
        headers: idempotency(),
        body: JSON.stringify({ mode }),
      })
      setLocalPower({ mode: answer.mode ?? '', failure: answer.failure ?? '' })
    } catch (problem) {
      noticeBox(powerRefusedTitle(THIS_SPARK), problem instanceof Error ? problem.message : String(problem))
    } finally {
      setLocalPowerBusy(false)
    }
  }

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
      {peers.length === 0 && memberRows.length === 0 ? (
        // One peer is what basement supports today, so both ways in live
        // here and both disappear once a second Spark exists.
        <div className="empty">
          <p>One Spark here. Add another to see your fleet.</p>
          <div className="empty-actions">
            <button className="primary" onClick={openFind}>Find a second Spark</button>
            <button className="quiet" onClick={() => openAdd()}>Add by address</button>
          </div>
        </div>
      ) : (
        <div className="mtable fleet">
          <div className="mthead" aria-hidden="true">
            <span>Spark</span><span>Serving</span><span className="r">Memory free</span><span className="r">Disk free</span><span className="r">Version</span><span style={{ paddingLeft: 20 }}>Status</span><span /><span />
          </div>

          <div className="mrow">
            <div className="m-id">
              <div>
                <div className="nm">{THIS_SPARK}</div>
                <div className="use">{sparkSubline(system?.hostname ?? '', localRoleLine(membership ?? null))}</div>
              </div>
            </div>
            <div className="m-id">
              {thisRecipe ? (
                <>
                  <Mark recipe={thisRecipe} recipeIDs={[thisRecipe.id]} size={24} />
                  <div className="nm" style={{ fontSize: 13 }}>{thisRecipe.display_name}</div>
                </>
              ) : (
                <span className="faint">Idle</span>
              )}
            </div>
            <div className="m-num"><span className="n">{formatBytes(system?.memory_available_bytes)}</span></div>
            <div className="m-num"><span className="n">{formatBytes(system?.storage_available_bytes)}</span></div>
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
                        <Mark recipe={peerRecipe} recipeIDs={[peerRecipe.id]} size={24} />
                        <div className="nm" style={{ fontSize: 13 }}>{peerRecipe.display_name}</div>
                      </>
                    ) : (
                      <span className="faint">{reachable ? 'Idle' : 'n/a'}</span>
                    )}
                  </div>
                  <div className="m-num"><span className="n">{reachable ? formatBytes(summary?.system?.memory_available_bytes) : 'n/a'}</span></div>
                  <div className="m-num"><span className="n">{reachable ? formatBytes(summary?.system?.storage_available_bytes) : 'n/a'}</span></div>
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

          {/* A Spark that joined the fleet itself. Every number in its row is
              one it reported in its own heartbeat, and n/a means it has not
              reported that yet rather than that it has none. */}
          {memberRows.map(node => {
            const key = `node:${node.node_id}`
            const open = expanded === key
            const toggle = () => setExpanded(open ? '' : key)
            const status = nodeStatus(node)
            const serving = nodeServing(node)
            const servingRecipe = recipes.find(recipe => recipe.id === serving?.recipe_id)
            const fleetNote = fleetStatusNote(node)
            return (
              <Fragment key={key}>
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
                      <div className="nm">{nodeName(node)}</div>
                      <div className="use">
                        {sparkSubline(nodeHostname(node), peerRoleLine(membership ?? null, node))}
                      </div>
                    </div>
                  </div>
                  <div className="m-id">
                    {serving ? (
                      <>
                        <Mark
                          recipe={servingRecipe}
                          recipeIDs={[serving.recipe_id]}
                          name={serving.recipe_id}
                          size={24}
                        />
                        {/* Named by this console's catalog when it knows the
                            recipe, and by the id that Spark sent when it does
                            not. */}
                        <div className="nm" style={{ fontSize: 13 }}>
                          {servingRecipe?.display_name ?? serving.recipe_id}
                        </div>
                      </>
                    ) : (
                      <span className="faint">{nodeReported(node) ? 'Idle' : 'n/a'}</span>
                    )}
                  </div>
                  <div className="m-num"><span className="n">{formatBytes(node.inventory?.memory_available_bytes)}</span></div>
                  <div className="m-num"><span className="n">{formatBytes(node.inventory?.storage_available_bytes)}</span></div>
                  <div className="m-num"><span className="n">{node.manager_version || 'n/a'}</span></div>
                  <div className="m-status">
                    <span className={`sdot ${status.dot}`} aria-hidden="true" />
                    <span>
                      {status.word}
                      {fleetNote && <span className="peer-note">{fleetNote}</span>}
                    </span>
                  </div>
                  <div className="m-actions" onKeyDown={event => event.stopPropagation()}>
                    <button
                      className="ghost"
                      onClick={event => {
                        event.stopPropagation()
                        window.open(node.console_url, '_blank', 'noopener,noreferrer')
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
                      {nodeFacts(node, Date.now()).map(fact => (
                        <Fragment key={fact.label}>
                          <dt>{fact.label}</dt>
                          <dd>{fact.label === 'Address' ? <code>{fact.value}</code> : fact.value}</dd>
                        </Fragment>
                      ))}
                    </dl>
                    {/* No Remove here: this Spark is a member of the fleet,
                        and the manager has no endpoint that takes one out.
                        A button that could not do it would be a lie. */}
                  </div>
                )}
              </Fragment>
            )
          })}
        </div>
      )}

      {/* How hard each Spark runs its chip. One switch per machine, and the
          sentence that explains the choice in a tooltip over the list rather
          than under every row. A member console shows none: the controller
          owns this machine's settings. */}
      {membership !== undefined && !isMember && (
        <section className="power-list" aria-label="Power mode">
          <header>
            <h2>Power</h2>
            <Tip text={POWER_MODE_NOTE} label="What the power modes do" />
          </header>
          {leadsFleet ? (
            sparks.map(spark => {
              const power = powerRows.get(spark.nodeID)
              if (!power) return null
              return (
                <PowerSwitch
                  key={spark.nodeID}
                  name={spark.displayName}
                  power={power}
                  onSet={mode => setPowerMode([spark], mode)}
                >
                  {/* One fan-out for the fleet, on the machine the console
                      runs on: it copies this Spark's mode to every other one.
                      It is dead for as long as any Spark it would name is
                      already mid-change. */}
                  {spark.isSelf && sparks.length > 1 && (
                    <button
                      type="button"
                      className="ghost"
                      disabled={power.disabled || powerFanOutBusy(sparks, powerSetting)}
                      onClick={() => setPowerMode(powerFanOut(sparks), power.mode)}
                    >
                      {EVERY_SPARK}
                    </button>
                  )}
                </PowerSwitch>
              )
            })
          ) : (
            <PowerSwitch
              name={THIS_SPARK}
              power={localPowerRow(localPower, localPowerBusy)}
              onSet={setLocalPowerMode}
            />
          )}
        </section>
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
              <p className="kicker">Sparks</p>
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
              <strong>Generate an API key on that Spark's API tab</strong>
              <p>Open that console, create a key, and copy it. Shown only once.</p>
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
                  Spark's API tab a moment ago, it is pasted once and
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
                Basement checks that Spark before saving anything.
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
              <p className="kicker">Sparks</p>
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
                    Sparks that are off or on another network will not show up. Add one by address instead.
                  </p>
                </>
              ) : (
                <>
                  <p className="muted dialog-note">
                    These machines answered on your network.
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
                Basement installs itself over SSH. Your password goes only to that machine and is never stored here.
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
                    Its console will ask for this pairing token the first time you open it.
                  </p>
                  <div className="snippet token">
                    <button type="button" className="ghost copy" onClick={copyToken}>
                      {tokenCopied === 'done' ? 'Copied' : tokenCopied === 'failed' ? 'Select it by hand' : 'Copy'}
                    </button>
                    <pre><code>{result.owner_pairing_token}</code></pre>
                  </div>
                  <p className="faint dialog-note">It stays valid, so keep it like a password.</p>
                </>
              )}
              <p className="muted dialog-note">The fleet table shows what it is serving.</p>
              {hasTwoSparkRecipe && (
                <p className="faint dialog-note">Two-Spark models can now be installed, from the Models tab.</p>
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
              <p className="kicker">Sparks</p>
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
// Every pending invitation renders, not just the oldest: a stranger on the
// network can occupy the three invitation slots, and if only the first were
// shown, the real console's request could sit buried behind that noise with
// no way to see or clear it.
export function FleetInvitationPrompt({ onAnswered }: { onAnswered: () => void }) {
  const [invitations, setInvitations] = useState<FleetInvitation[]>([])
  const [answering, setAnswering] = useState('')
  const [error, setError] = useState('')
  const ref = useRef<HTMLDialogElement>(null)

  useEffect(() => {
    let cancelled = false
    const poll = async () => {
      if (document.hidden) return
      try {
        const waiting = fleetInvitations(await api<unknown>('/api/v1/fleet/invitations'))
        if (!cancelled) setInvitations(waiting)
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
    if (invitations.length > 0 && !dialog.open) dialog.showModal()
    if (invitations.length === 0 && dialog.open) dialog.close()
  }, [invitations])

  const answer = async (invitation: FleetInvitation, action: 'approve' | 'deny') => {
    setAnswering(invitation.id)
    setError('')
    try {
      await api(`/api/v1/fleet/invitations/${encodeURIComponent(invitation.id)}/${action}`, {
        method: 'POST',
        body: '{}',
      })
      setInvitations(rest => rest.filter(entry => entry.id !== invitation.id))
      onAnswered()
    } catch (problem) {
      setError(problem instanceof Error ? problem.message : 'That answer did not go through')
    } finally {
      setAnswering('')
    }
  }

  return (
    <dialog ref={ref} onClose={() => setInvitations([])} aria-label="A Spark wants to add this one">
      {invitations.length > 0 && (
        <div className="dialog-pad">
          {invitations.map((invitation, index) => (
            <div key={invitation.id} className={index > 0 ? 'invitation-entry' : undefined}>
              <div className="dialog-head">
                <div>
                  {index === 0 && <p className="kicker">Sparks</p>}
                  <h2>{invitationTitle(invitation)}</h2>
                </div>
              </div>
              <p className="muted dialog-note">{invitationBody(invitation)}</p>
              <div className="dialog-foot">
                <button type="button" className="quiet" disabled={answering !== ''} onClick={() => void answer(invitation, 'deny')}>Deny</button>
                <button type="button" className="primary" disabled={answering !== ''} onClick={() => void answer(invitation, 'approve')}>Approve</button>
              </div>
            </div>
          ))}
          {error && <p className="error-text dialog-note" role="alert">{error}</p>}
        </div>
      )}
    </dialog>
  )
}
