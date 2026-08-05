import { useCallback, useEffect, useRef, useState } from 'react'
import {
  api, ApiError, OfflineError,
  type SystemInfo, type UpdateAttemptStatus, type UpdateInfo,
} from '../api'
import {
  followManagerUpdate, isInstallableManagerUpdate,
  isUpdateApplyResult, isUpdateAttemptStatus, updateRefusal,
  type UpdateRefusal,
} from '../managerUpdate'

const PROGRESS = [
  { state: 'checking_signature', label: 'Verify release', copy: 'Verifying the signed release.' },
  { state: 'downloading', label: 'Download manager', copy: 'Downloading the manager.' },
  { state: 'verifying', label: 'Check download', copy: 'Checking the download.' },
  { state: 'waiting_for_root', label: 'Restart basement', copy: 'Preparing to restart basement.' },
] as const

interface ManagerUpdateBodyProps {
  info: UpdateInfo | null
  onInfoChange: (info: UpdateInfo) => void
  onReconnectingChange: (reconnecting: boolean) => void
  onManagerReady: () => void
  onOpenModels: () => void
  onOpenActivity: () => void
  onOpenGeneration: () => void
}

export function ManagerUpdateSidebar({ info, managerVersion, onOpen }: {
  info: UpdateInfo | null
  managerVersion?: string
  onOpen: () => void
}) {
  const available = isInstallableManagerUpdate(info) ||
    Boolean(info?.manual_upgrade_required || info?.manual_bootstrap_required)
  const updateVersion = (info?.target_version ?? info?.latest_version ?? '').replace(/^v/, '')

  return (
    <>
      <button className="side-manager" type="button" aria-haspopup="dialog" onClick={onOpen}>
        manager {managerVersion ?? ''}
      </button>
      {available && (
        <button className="side-update" type="button" aria-haspopup="dialog" onClick={onOpen}>
          {updateVersion ? `Update ${updateVersion} available` : 'Update available'}
        </button>
      )}
    </>
  )
}

const displayVersion = (version?: string): string => {
  if (!version) return 'n/a'
  return version.startsWith('v') ? version : `v${version}`
}

function SignedRelease() {
  return <span className="update-signed">Signed release verified</span>
}

function StateHead({ mark, tone = '', title, children, signed = false }: {
  mark: string
  tone?: 'complete' | 'warn' | 'fail' | ''
  title: React.ReactNode
  children?: React.ReactNode
  signed?: boolean
}) {
  return (
    <div className="update-state-head">
      <span className={`update-state-mark ${tone}`}>{mark}</span>
      <div className="update-state-copy">
        <h2>{title}</h2>
        {children}
      </div>
      {signed && <SignedRelease />}
    </div>
  )
}

function ManagerUpdateBody({
  info, onInfoChange, onReconnectingChange, onManagerReady,
  onOpenModels, onOpenActivity, onOpenGeneration,
}: ManagerUpdateBodyProps) {
  const [attempt, setAttempt] = useState<UpdateAttemptStatus | null>(null)
  const [refusal, setRefusal] = useState<UpdateRefusal | null>(null)
  const [error, setError] = useState('')
  const [checking, setChecking] = useState(false)
  const [applying, setApplying] = useState(false)
  const [reconnecting, setReconnecting] = useState(false)
  const [reconnectTimedOut, setReconnectTimedOut] = useState(false)
  const [observedVersion, setObservedVersion] = useState('')
  const [showDiagnostics, setShowDiagnostics] = useState(false)
  const follower = useRef<AbortController | null>(null)
  const checkedStatus = useRef(false)

  const setReconnectState = useCallback((next: boolean) => {
    setReconnecting(next)
    onReconnectingChange(next)
  }, [onReconnectingChange])

  const startFollowing = useCallback((attemptID?: string, ambiguousFailure = '') => {
    follower.current?.abort()
    const controller = new AbortController()
    follower.current = controller
    setReconnectTimedOut(false)
    setError('')

    void followManagerUpdate(
      attemptID,
      {
        readStatus: () => api<unknown>('/api/v1/update/status', { signal: controller.signal }),
        readManagerVersion: async () => {
          const system = await api<SystemInfo>('/api/v1/system', { signal: controller.signal })
          return system.manager_version
        },
      },
      event => {
        if (controller.signal.aborted) return
        if (event.kind === 'reconnecting') {
          setReconnectState(true)
          return
        }
        if (event.kind === 'manager_version') {
          setObservedVersion(event.version)
          return
        }
        setAttempt(event.status)
        setReconnectState(event.status.state === 'restarting' || event.status.state === 'checking_health')
      },
      controller.signal,
    ).then(outcome => {
      if (controller.signal.aborted) return
      setReconnectState(false)
      if (outcome.kind === 'terminal') {
        setAttempt(outcome.status)
        if (outcome.status.state === 'succeeded' || outcome.status.state === 'rolled_back') onManagerReady()
      } else if (outcome.kind === 'timeout') {
        setReconnectTimedOut(true)
      } else if (outcome.kind === 'inactive') {
        setError(ambiguousFailure || 'The manager did not start an update. Check for updates, then try again.')
      }
    })
  }, [onManagerReady, setReconnectState])

  useEffect(() => () => {
    follower.current?.abort()
    onReconnectingChange(false)
  }, [onReconnectingChange])

  useEffect(() => {
    if (checkedStatus.current) return
    checkedStatus.current = true
    api<unknown>('/api/v1/update/status').then(payload => {
      if (!isUpdateAttemptStatus(payload)) return
      if (info?.update_available && info.target_version !== payload.target_version) return
      setAttempt(payload)
      if (!['succeeded', 'rolled_back', 'recovery_required', 'failed_before_handoff'].includes(payload.state)) {
        startFollowing(payload.attempt_id)
      }
    }).catch(() => {})
  }, [info, startFollowing])

  const checkForUpdates = async () => {
    setChecking(true)
    setError('')
    setRefusal(null)
    try {
      const next = await api<UpdateInfo>('/api/v1/update')
      onInfoChange(next)
    } catch (problem) {
      setError(problem instanceof Error ? problem.message : 'Could not check for updates')
    } finally {
      setChecking(false)
    }
  }

  const applyUpdate = async () => {
    setApplying(true)
    setError('')
    setRefusal(null)
    try {
      const payload = await api<unknown>('/api/v1/update/apply', { method: 'POST' })
      if (isUpdateApplyResult(payload)) {
        setAttempt({
          schema_version: 1,
          attempt_id: payload.attempt_id,
          state: payload.state,
          running_version: info?.current_version ?? '',
          target_version: info?.target_version ?? '',
          updated_at: new Date().toISOString(),
        })
        startFollowing(payload.attempt_id)
      } else {
        startFollowing(undefined, 'The update response was incomplete. Check for updates, then try again.')
      }
    } catch (problem) {
      const message = problem instanceof Error ? problem.message : 'Could not start the update'
      const blocked = updateRefusal(message)
      if (blocked) {
        setRefusal(blocked)
      } else if (problem instanceof OfflineError || (problem instanceof ApiError && message === 'a manager update is already in progress')) {
        startFollowing(undefined, message)
      } else {
        setError(message)
      }
    } finally {
      setApplying(false)
    }
  }

  const openRelease = () => {
    if (info?.release_url) window.open(info.release_url, '_blank', 'noopener,noreferrer')
  }

  if (refusal) {
    const generation = refusal.kind === 'generation'
    return (
      <div className="update-view">
        <section className="card update-card">
          <StateHead
            mark="!"
            tone="warn"
            title={generation ? 'Finish the current generation before updating' : 'Finish the current job before updating'}
          >
            <p>{generation ? 'Basement will not interrupt a generation in progress.' : 'Basement will not interrupt work in progress.'}</p>
          </StateHead>
          <div className="update-blocker">
            <p><code>{refusal.message}</code></p>
            <button className="primary" type="button" onClick={generation ? onOpenGeneration : onOpenActivity}>
              {generation ? 'Open generation' : 'Open activity'}
            </button>
          </div>
        </section>
      </div>
    )
  }

  if (reconnectTimedOut) {
    return (
      <div className="update-view">
        <section className="card update-card">
          <StateHead mark="!" tone="fail" title="Basement did not reconnect">
            <p>The three-minute reconnect window ended.</p>
          </StateHead>
          <div className="update-notice fail">
            <strong>Check the manager service before trying again</strong>
            The console stopped waiting so it would not hide an unreachable manager.
          </div>
          <div className="update-actions">
            <button className="primary" type="button" onClick={() => startFollowing(attempt?.attempt_id)}>Try reconnecting</button>
          </div>
        </section>
      </div>
    )
  }

  const restarting = reconnecting || attempt?.state === 'restarting' || attempt?.state === 'checking_health'
  if (restarting) {
    return (
      <div className="update-view">
        <section className="card update-card">
          <StateHead mark="..." tone="warn" title="Waiting for basement to come back">
            <p>Restarting the manager and checking that it is ready.</p>
          </StateHead>
          {observedVersion && observedVersion !== info?.current_version && (
            <span className="update-version-returned">Manager {displayVersion(observedVersion)} answered</span>
          )}
        </section>
        <div className="update-notice">
          <strong>Your model stays on</strong>
          The updater does not stop the model container.
        </div>
      </div>
    )
  }

  if (attempt?.state === 'succeeded') {
    return (
      <div className="update-view">
        <section className="card update-card">
          <StateHead mark="OK" tone="complete" title="Update complete">
            <p>Basement is running <span className="mono">{displayVersion(attempt.running_version)}</span>.</p>
          </StateHead>
          <div className="update-actions">
            <button className="primary" type="button" onClick={onOpenModels}>Return to models</button>
          </div>
        </section>
      </div>
    )
  }

  if (attempt?.state === 'rolled_back') {
    return (
      <div className="update-view">
        <section className="card update-card">
          <StateHead mark="!" tone="fail" title="The update was rolled back">
            <p>Basement <span className="mono">{displayVersion(attempt.running_version)}</span> is running again.</p>
          </StateHead>
          <div className="update-notice fail">
            <strong>The new version could not start safely</strong>
            The previous version is running.
          </div>
          {showDiagnostics && attempt.failure && <div className="update-diagnostics"><code>{attempt.failure}</code></div>}
          <div className="update-actions">
            <button className="primary" type="button" onClick={onOpenModels}>Return to models</button>
            {attempt.failure && (
              <button className="quiet" type="button" onClick={() => setShowDiagnostics(value => !value)}>
                {showDiagnostics ? 'Hide diagnostics' : 'View diagnostics'}
              </button>
            )}
          </div>
        </section>
      </div>
    )
  }

  if (attempt?.state === 'failed_before_handoff' || attempt?.state === 'recovery_required') {
    const recovery = attempt.state === 'recovery_required'
    return (
      <div className="update-view">
        <section className="card update-card">
          <StateHead mark="!" tone="fail" title={recovery ? 'The update needs local recovery' : 'The update did not start'}>
            {attempt.failure && <p>{attempt.failure}</p>}
          </StateHead>
          <div className="update-actions">
            <button className="primary" type="button" onClick={onOpenModels}>Return to models</button>
          </div>
        </section>
      </div>
    )
  }

  const progressIndex = attempt ? PROGRESS.findIndex(step => step.state === attempt.state) : -1
  if (attempt && progressIndex >= 0) {
    return (
      <div className="update-view">
        <section className="card update-card">
          <StateHead mark={`${progressIndex + 1}/4`} tone="warn" title="Updating basement" signed>
            <p>{PROGRESS[progressIndex].copy}</p>
          </StateHead>
          <div className="update-progress-list" aria-label="Update progress">
            {PROGRESS.map((step, index) => {
              const state = index < progressIndex ? 'done' : index === progressIndex ? 'active' : ''
              return (
                <div className={`update-progress-row ${state}`} data-api-state={step.state} key={step.state}>
                  <i className="update-progress-dot" aria-hidden="true" />
                  <span>{step.label}</span>
                  <span className="value">{state === 'done' ? 'Done' : state === 'active' ? 'Working' : 'Waiting'}</span>
                </div>
              )
            })}
          </div>
        </section>
      </div>
    )
  }

  if (!info) return <div className="empty">Checking for updates...</div>

  if (info.manual_upgrade_required || info.manual_bootstrap_required) {
    return (
      <div className="update-view">
        <section className="card update-card">
          <StateHead mark="!" tone="warn" title="This machine needs a one-time manual upgrade first">
            <p>{info.reason}</p>
          </StateHead>
          {info.release_url && (
            <div className="update-actions">
              <button className="primary" type="button" onClick={openRelease}>Open installer instructions</button>
            </div>
          )}
        </section>
      </div>
    )
  }

  if (isInstallableManagerUpdate(info)) {
    const target = displayVersion(info.target_version)
    return (
      <div className="update-view">
        <section className="card update-card">
          <StateHead mark="UP" tone="complete" title={<>Update basement to <span className="mono">{target}</span></>} signed>
            <p>The console reconnects after basement restarts. Model serving continues.</p>
          </StateHead>
          <dl className="update-facts">
            <dt>Running</dt><dd>{displayVersion(info.current_version)}</dd>
            <dt>Update</dt><dd>{target}</dd>
          </dl>
          {error && <div className="error-note"><p>{error}</p></div>}
          <div className="update-actions">
            <button className="primary" type="button" disabled={applying} onClick={applyUpdate}>
              {applying ? 'Starting update' : `Update to ${target}`}
            </button>
            {info.release_url && <button className="quiet" type="button" onClick={openRelease}>View release</button>}
          </div>
        </section>
      </div>
    )
  }

  if (!info.checked) {
    return (
      <div className="update-view">
        <section className="card update-card">
          <StateHead mark="!" tone="warn" title="Could not check for updates">
            {info.note && <p>{info.note}</p>}
          </StateHead>
          {error && <div className="error-note"><p>{error}</p></div>}
          <div className="update-actions">
            <button className="primary" type="button" disabled={checking} onClick={checkForUpdates}>
              {checking ? 'Checking' : 'Check for updates'}
            </button>
          </div>
        </section>
      </div>
    )
  }

  return (
    <div className="update-view">
      <section className="card update-card">
        <StateHead mark="OK" tone="complete" title="Basement is up to date">
          <p>Running <span className="mono">{displayVersion(info.current_version)}</span></p>
        </StateHead>
        {info.note && <div className="update-note">{info.note}</div>}
        {error && <div className="error-note"><p>{error}</p></div>}
        <div className="update-actions">
          <button className="primary" type="button" disabled={checking} onClick={checkForUpdates}>
            {checking ? 'Checking' : 'Check for updates'}
          </button>
        </div>
      </section>
    </div>
  )
}

interface ManagerUpdateDialogProps extends ManagerUpdateBodyProps {
  open: boolean
  reconnecting: boolean
  onClose: () => void
}

export default function ManagerUpdateDialog({
  open, reconnecting, onClose, ...bodyProps
}: ManagerUpdateDialogProps) {
  const ref = useRef<HTMLDialogElement>(null)

  useEffect(() => {
    const dialog = ref.current
    if (!dialog) return
    if (open && !dialog.open) dialog.showModal()
    if (!open && dialog.open) dialog.close()
  }, [open])

  if (!open) return <dialog ref={ref} className="update-dialog" onClose={onClose} />

  return (
    <dialog ref={ref} className="update-dialog" onClose={onClose} aria-labelledby="manager-update-title">
      <div className="dialog-pad">
        <div className="dialog-head">
          <div>
            <p className="kicker">Manager</p>
            <h2 id="manager-update-title">Update basement</h2>
          </div>
          {reconnecting && <span className="update-dialog-reconnecting" role="status">Reconnecting</span>}
          <button className="dialog-close" type="button" onClick={onClose} aria-label="Close">×</button>
        </div>
        <div className="update-dialog-body" aria-live="polite">
          <ManagerUpdateBody {...bodyProps} />
        </div>
      </div>
    </dialog>
  )
}
