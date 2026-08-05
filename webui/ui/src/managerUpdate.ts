import type { UpdateAttemptStatus, UpdateInfo } from './api'

const UPDATE_STATES = new Set([
  'checking_signature',
  'downloading',
  'verifying',
  'waiting_for_root',
  'restarting',
  'checking_health',
  'succeeded',
  'rolled_back',
  'recovery_required',
  'failed_before_handoff',
])

const TERMINAL_STATES = new Set([
  'succeeded',
  'rolled_back',
  'recovery_required',
  'failed_before_handoff',
])

const RESTART_DEADLINE_STATES = new Set(['waiting_for_root', 'restarting', 'checking_health'])

export const RECONNECT_TIMEOUT_MS = 3 * 60 * 1000
export const UPDATE_POLL_MS = 1000

const own = (value: object, key: string): boolean => Object.prototype.hasOwnProperty.call(value, key)

export function isUpdateAttemptStatus(value: unknown): value is UpdateAttemptStatus {
  if (typeof value !== 'object' || value === null) return false
  const status = value as Record<string, unknown>
  return status.schema_version === 1 &&
    typeof status.attempt_id === 'string' && status.attempt_id.length > 0 &&
    typeof status.state === 'string' && UPDATE_STATES.has(status.state) &&
    typeof status.running_version === 'string' &&
    typeof status.target_version === 'string' &&
    typeof status.updated_at === 'string' &&
    (!own(status, 'failure') || typeof status.failure === 'string')
}

export const isInactiveUpdateStatus = (value: unknown): value is { active: false } =>
  typeof value === 'object' && value !== null && (value as Record<string, unknown>).active === false

export function isUpdateApplyResult(value: unknown): value is { accepted: true; attempt_id: string; state: string } {
  if (typeof value !== 'object' || value === null) return false
  const result = value as Record<string, unknown>
  return result.accepted === true && typeof result.attempt_id === 'string' && result.attempt_id.length > 0 &&
    typeof result.state === 'string' && UPDATE_STATES.has(result.state)
}

const parseVersion = (value: string): [bigint, bigint, bigint] | null => {
  const match = /^v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/.exec(value)
  if (!match) return null
  return [BigInt(match[1]), BigInt(match[2]), BigInt(match[3])]
}

export function isNewerManagerVersion(current: string, candidate: string): boolean {
  const running = parseVersion(current)
  const target = parseVersion(candidate)
  if (!running || !target) return false
  for (let index = 0; index < running.length; index += 1) {
    if (target[index] > running[index]) return true
    if (target[index] < running[index]) return false
  }
  return false
}

export const isInstallableManagerUpdate = (info: UpdateInfo | null): boolean => Boolean(
  info?.checked &&
  info.update_available &&
  info.signed &&
  info.compatible &&
  info.installable &&
  info.target_version &&
  isNewerManagerVersion(info.current_version, info.target_version),
)

export interface ManagerUpdateDialogState {
  open: boolean
  reconnecting: boolean
}

export type ManagerUpdateDialogAction =
  | { type: 'open_from_sidebar' }
  | { type: 'close' }
  | { type: 'reconnecting'; value: boolean }

export const initialManagerUpdateDialogState: ManagerUpdateDialogState = {
  open: false,
  reconnecting: false,
}

export function managerUpdateDialogReducer(
  state: ManagerUpdateDialogState,
  action: ManagerUpdateDialogAction,
): ManagerUpdateDialogState {
  switch (action.type) {
    case 'open_from_sidebar':
      return { ...state, open: true }
    case 'close':
      return { ...state, open: false }
    case 'reconnecting':
      return { ...state, reconnecting: action.value }
  }
}

export type UpdateRefusal = {
  kind: 'job' | 'generation'
  message: string
}

export function updateRefusal(message: string): UpdateRefusal | null {
  if (!message.startsWith('cannot update while ')) return null
  return { kind: message.includes('generation ') ? 'generation' : 'job', message }
}

export type UpdatePollEvent =
  | { kind: 'status'; status: UpdateAttemptStatus }
  | { kind: 'reconnecting' }
  | { kind: 'manager_version'; version: string }

export type UpdatePollOutcome =
  | { kind: 'terminal'; status: UpdateAttemptStatus }
  | { kind: 'inactive' }
  | { kind: 'timeout' }
  | { kind: 'aborted' }

interface UpdatePollDependencies {
  readStatus: () => Promise<unknown>
  readManagerVersion?: () => Promise<string>
  wait?: (milliseconds: number, signal: AbortSignal) => Promise<void>
  now?: () => number
}

const defaultWait = (milliseconds: number, signal: AbortSignal): Promise<void> =>
  new Promise(resolve => {
    const timer = window.setTimeout(resolve, milliseconds)
    signal.addEventListener('abort', () => {
      window.clearTimeout(timer)
      resolve()
    }, { once: true })
  })

// The updater may use 45 seconds to prove the target healthy and another
// 45 seconds to prove a rollback healthy. Three minutes also covers service
// stop and start time while still putting a clear bound on a lost manager.
export async function followManagerUpdate(
  expectedAttemptID: string | undefined,
  dependencies: UpdatePollDependencies,
  onEvent: (event: UpdatePollEvent) => void,
  signal: AbortSignal,
): Promise<UpdatePollOutcome> {
  const now = dependencies.now ?? Date.now
  const wait = dependencies.wait ?? defaultWait
  let attemptID = expectedAttemptID
  let transientStartedAt: number | null = null
  let restartStartedAt: number | null = null

  while (!signal.aborted) {
    let payload: unknown
    try {
      payload = await dependencies.readStatus()
    } catch {
      transientStartedAt ??= now()
      onEvent({ kind: 'reconnecting' })
      const startedAt = restartStartedAt ?? transientStartedAt
      if (now() - startedAt >= RECONNECT_TIMEOUT_MS) return { kind: 'timeout' }
      await wait(UPDATE_POLL_MS, signal)
      continue
    }

    if (isInactiveUpdateStatus(payload)) {
      if (!attemptID && transientStartedAt === null) return { kind: 'inactive' }
      transientStartedAt ??= now()
      onEvent({ kind: 'reconnecting' })
    } else if (!isUpdateAttemptStatus(payload) || (attemptID && payload.attempt_id !== attemptID)) {
      transientStartedAt ??= now()
      onEvent({ kind: 'reconnecting' })
    } else {
      attemptID ??= payload.attempt_id
      transientStartedAt = null
      if (RESTART_DEADLINE_STATES.has(payload.state)) restartStartedAt ??= now()
      onEvent({ kind: 'status', status: payload })

      if (TERMINAL_STATES.has(payload.state)) return { kind: 'terminal', status: payload }

      if (restartStartedAt !== null && dependencies.readManagerVersion) {
        try {
          const version = await dependencies.readManagerVersion()
          if (version) onEvent({ kind: 'manager_version', version })
        } catch {
          onEvent({ kind: 'reconnecting' })
        }
      }
    }

    const startedAt = restartStartedAt ?? transientStartedAt
    if (startedAt !== null && now() - startedAt >= RECONNECT_TIMEOUT_MS) return { kind: 'timeout' }
    await wait(UPDATE_POLL_MS, signal)
  }
  return { kind: 'aborted' }
}
