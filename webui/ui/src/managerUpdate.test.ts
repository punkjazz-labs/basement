import { describe, expect, it } from 'vitest'
import { Children, isValidElement, type ReactElement } from 'react'
import { OfflineError, type UpdateAttemptStatus, type UpdateInfo } from './api'
import { ManagerUpdateSidebar } from './views/ManagerUpdate'
import {
  discoveredAttemptAction, fleetResolveHoldouts, fleetUpgradeResolveWord,
  fleetUpgradeRowState, fleetUpgradeRunView, fleetUpgradeStateWord,
  fleetUpgradeTerminal, followManagerUpdate, initialManagerUpdateDialogState,
  isInstallableManagerUpdate, managerUpdateCard, managerUpdateDialogReducer,
  orderedFleetUpgradeNodes, unitUpdateNote, updateRefusal, type UpdatePollEvent,
} from './managerUpdate'
import type { FleetUpgradeNode } from './api'

const availableUpdate = (overrides: Partial<UpdateInfo> = {}): UpdateInfo => ({
  current_version: 'v1.9.9',
  fleet_role: 'standalone',
  fleet_node_count: 0,
  latest_version: '1.10.0',
  target_version: 'v1.10.0',
  checked: true,
  update_available: true,
  signed: true,
  compatible: true,
  installable: true,
  ...overrides,
})

const status = (state: string, overrides: Partial<UpdateAttemptStatus> = {}): UpdateAttemptStatus => ({
  schema_version: 1,
  attempt_id: 'update-test',
  state,
  running_version: 'v1.9.9',
  target_version: 'v1.10.0',
  updated_at: '2026-08-05T00:00:00Z',
  ...overrides,
})

describe('manager update availability', () => {
  it('does not offer a downgrade when the local build is newer', () => {
    expect(isInstallableManagerUpdate(availableUpdate({
      current_version: 'v2.0.0',
      latest_version: '1.12.9',
      target_version: 'v1.12.9',
    }))).toBe(false)
  })
})

// The sidebar renders one manager line and, when there is something newer, one
// pill. These pull them back out of the element tree so each state can be read
// the way the person in front of the console reads it.
const sidebarPill = (info: UpdateInfo | null, onOpen: () => void = () => {}) => {
  const sidebar = ManagerUpdateSidebar({ info, managerVersion: 'v1.9.9', onOpen })
  return Children.toArray(sidebar.props.children).find(child =>
    isValidElement<{ className?: string }>(child) && child.props.className?.startsWith('side-update'),
  ) as ReactElement<{ className: string; children: string; onClick: () => void }> | undefined
}

const sidebarManagerLine = (info: UpdateInfo | null): string => {
  const sidebar = ManagerUpdateSidebar({ info, managerVersion: 'v1.9.9', onOpen: () => {} })
  const line = Children.toArray(sidebar.props.children).find(child =>
    isValidElement<{ className?: string }>(child) && child.props.className === 'side-manager',
  ) as ReactElement<{ children: unknown }> | undefined
  const text = (node: unknown): string => {
    if (typeof node === 'string' || typeof node === 'number') return String(node)
    if (Array.isArray(node)) return node.map(text).join('')
    if (isValidElement<{ children?: unknown }>(node)) return text(node.props.children)
    return ''
  }
  return text(line?.props.children).replace(/\s+/g, ' ').trim()
}

describe('fleet update cards', () => {
  it('selects the local, controller, and member cards from the reported role', () => {
    expect(managerUpdateCard(availableUpdate())).toBe('standalone')
    expect(managerUpdateCard(availableUpdate({ fleet_role: 'controller', fleet_node_count: 3 }))).toBe('controller')
    expect(managerUpdateCard(availableUpdate({ fleet_role: 'member', fleet_node_count: 3 }))).toBe('member')
  })
})

describe('fleet update progress', () => {
  const node = (displayName: string, sequence: number, state: string): FleetUpgradeNode => ({
    run_id: 'fleet-upgrade-test',
    node_id: `node-${sequence}`,
    display_name: displayName,
    sequence,
    role: sequence === 2 ? 'controller' : 'member',
    state,
    running_version: state === 'succeeded' ? 'v2.0.0' : 'v1.0.0',
    target_version: 'v2.0.0',
    updated_at: '2026-08-05T00:00:00Z',
  })

  it('uses only the specified plain words for node states', () => {
    expect([
      'pending', 'waiting_for_idle', 'staging', 'staged', 'applying',
      'checking_health', 'succeeded', 'failed', 'rolled_back',
    ].map(fleetUpgradeStateWord)).toEqual([
      'Waiting', 'Waiting for work to finish', 'Downloading', 'Downloading',
      'Restarting', 'Restarting', 'Done', 'Failed', 'Rolled back',
    ])
  })

  it('renders nodes in the run sequence without mutating the response', () => {
    const response = [node('spark-head', 2, 'pending'), node('spark-worker', 0, 'succeeded'), node('spark-mid', 1, 'applying')]
    expect(orderedFleetUpgradeNodes(response).map(item => item.display_name)).toEqual(['spark-worker', 'spark-mid', 'spark-head'])
    expect(response.map(item => item.display_name)).toEqual(['spark-head', 'spark-worker', 'spark-mid'])
  })

  it('marks completed rows done and only the first unfinished row active', () => {
    const ordered = [node('spark-worker', 0, 'succeeded'), node('spark-mid', 1, 'checking_health'), node('spark-head', 2, 'pending')]
    expect(ordered.map((_, index) => fleetUpgradeRowState(ordered, index))).toEqual(['done', 'active', ''])
  })
})

describe('a failed fleet update offers resolution instead of progress', () => {
  const run = (state: string, nodes: FleetUpgradeNode[]) => ({
    run_id: 'fleet-upgrade-test', fleet_id: 'fleet-test', controller_node_id: 'node-2',
    release_tag: 'v2.0.0', target_version: 'v2.0.0', manifest_sha256: 'digest',
    state, created_at: '2026-08-05T00:00:00Z', updated_at: '2026-08-05T00:00:00Z', nodes,
  })
  const node = (displayName: string, sequence: number, state: string, resolveState?: string): FleetUpgradeNode => ({
    run_id: 'fleet-upgrade-test', node_id: `node-${sequence}`, display_name: displayName,
    sequence, role: sequence === 2 ? 'controller' : 'member', state,
    running_version: state === 'succeeded' ? 'v2.0.0' : 'v1.0.0', target_version: 'v2.0.0',
    resolve_state: resolveState, updated_at: '2026-08-05T00:00:00Z',
  })

  it('treats a resolved run as terminal so it can be superseded like any settled run', () => {
    expect(['succeeded', 'failed', 'resolved'].map(fleetUpgradeTerminal)).toEqual([true, true, true])
    expect(['applying', 'staging', 'waiting_for_idle'].map(fleetUpgradeTerminal)).toEqual([false, false, false])
  })

  it('chooses the failed card while the run needs attention', () => {
    const failed = run('failed', [node('spark-worker', 0, 'succeeded'), node('spark-mid', 1, 'rolled_back'), node('spark-head', 2, 'pending')])
    expect(fleetUpgradeRunView(failed)).toBe('failed')
  })

  it('keeps pointing at the machine a resolve could not reach', () => {
    const partial = run('resolved', [
      node('spark-worker', 0, 'succeeded', 'resolved'),
      node('spark-mid', 1, 'rolled_back', 'unreachable'),
      node('spark-head', 2, 'pending', 'resolved'),
    ])
    expect(fleetUpgradeRunView(partial)).toBe('resolved_holdouts')
    expect(fleetResolveHoldouts(partial.nodes).map(item => item.display_name)).toEqual(['spark-mid'])
  })

  it('settles completely once every node released its lock', () => {
    const settled = run('resolved', [
      node('spark-worker', 0, 'succeeded', 'resolved'),
      node('spark-mid', 1, 'rolled_back', 'resolved'),
      node('spark-head', 2, 'pending', 'resolved'),
    ])
    expect(fleetUpgradeRunView(settled)).toBe('resolved')
    expect(fleetResolveHoldouts(settled.nodes)).toEqual([])
  })

  it('uses plain words for resolve outcomes', () => {
    expect(fleetUpgradeResolveWord('resolved')).toBe('Released')
    expect(fleetUpgradeResolveWord('unreachable')).toBe('Not reached')
    expect(fleetUpgradeResolveWord(undefined)).toBe('')
  })

  it('names the failed states so the failed card does not show raw identifiers', () => {
    expect(fleetUpgradeStateWord('failed_before_handoff')).toBe('Failed')
    expect(fleetUpgradeStateWord('recovery_required')).toBe('Needs recovery')
  })
})

describe('manager update dialog', () => {
  it('the sidebar control opens the dialog and it stays open through the restart window', () => {
    let state = initialManagerUpdateDialogState
    const updateControl = sidebarPill(availableUpdate(), () => {
      state = managerUpdateDialogReducer(state, { type: 'open_from_sidebar' })
    })

    expect(updateControl?.props.children).toBe('Update to 1.10.0')
    updateControl?.props.onClick()
    expect(state).toEqual({ open: true, reconnecting: false })

    state = managerUpdateDialogReducer(state, { type: 'reconnecting', value: true })
    expect(state).toEqual({ open: true, reconnecting: true })
  })
})

describe('the manager line states its own update status', () => {
  it('says so when the check found nothing newer, instead of looking unchecked', () => {
    const current = availableUpdate({ update_available: false, target_version: undefined, latest_version: '1.9.9' })
    expect(sidebarManagerLine(current)).toBe('manager v1.9.9 · up to date')
    expect(sidebarPill(current)).toBeUndefined()
  })

  it('separates a check that never got an answer from a clean result', () => {
    expect(sidebarManagerLine(availableUpdate({ checked: false, update_available: false })))
      .toBe('manager v1.9.9 · could not check')
    expect(sidebarManagerLine(null)).toBe('manager v1.9.9 · checking')
  })

  it('does not call a build that cannot be compared up to date', () => {
    // What a development build actually reports. It is the state the console
    // was silent about, and being silent read as "nothing to update".
    expect(sidebarManagerLine(availableUpdate({
      current_version: 'dev',
      latest_version: undefined,
      target_version: undefined,
      update_available: false,
      signed: false,
      compatible: false,
      installable: false,
      note: 'development builds cannot use console updates',
    }))).toBe('manager v1.9.9 · development builds cannot use console updates')
  })

  it('drops the up-to-date claim the moment something newer exists', () => {
    expect(sidebarManagerLine(availableUpdate())).toBe('manager v1.9.9')
  })

  it('shows a newer release the console cannot install by itself', () => {
    // This used to render nothing at all, which read as being up to date.
    const manual = availableUpdate({ installable: false, manual_bootstrap_required: true })
    expect(sidebarPill(manual)?.props.children).toBe('1.10.0 needs a manual step')
    expect(sidebarPill(manual)?.props.className).toBe('side-update pending')
  })

  it('shows a newer release that failed verification rather than hiding it', () => {
    const unverified = availableUpdate({ installable: false, signed: false, target_version: undefined })
    expect(sidebarPill(unverified)?.props.children).toBe('1.10.0 could not be verified')
    expect(sidebarPill(unverified)?.props.className).toBe('side-update pending')
  })

  it('never invites a downgrade, whatever the server said was available', () => {
    const older = availableUpdate({ current_version: 'v2.0.0', latest_version: '1.9.0', target_version: 'v1.9.0' })
    expect(sidebarPill(older)?.props.children).not.toContain('Update to')
  })
})

describe('an attempt discovered when the dialog opens', () => {
  it('announces an update that finished while nothing was watching', () => {
    // Closing the dialog aborts the live follower, which is otherwise the
    // only thing that tells the rest of the console to refresh. Reopening
    // used to show "Update complete" while the sidebar version, models and
    // jobs stayed stale for up to an hour.
    expect(discoveredAttemptAction('succeeded')).toBe('announce')
    expect(discoveredAttemptAction('rolled_back')).toBe('announce')
  })

  it('shows a settled failure without claiming anything changed', () => {
    expect(discoveredAttemptAction('recovery_required')).toBe('show')
    expect(discoveredAttemptAction('failed_before_handoff')).toBe('show')
  })

  it('keeps watching an attempt that is still moving', () => {
    for (const state of ['checking_signature', 'downloading', 'verifying', 'waiting_for_root', 'restarting', 'checking_health']) {
      expect(discoveredAttemptAction(state)).toBe('follow')
    }
  })
})

describe('manager update refusal', () => {
  it('keeps the backend busy message and identifies the install in progress', () => {
    const message = 'cannot update while install job job-17 for recipe-4 is running; finish or cancel it first'
    expect(updateRefusal(message)).toEqual({ kind: 'job', message })
  })

  it('keeps the backend busy message and identifies the generation in progress', () => {
    const message = 'cannot update while generation generation-8 for recipe-4 is running; finish or cancel it first'
    expect(updateRefusal(message)).toEqual({ kind: 'generation', message })
  })
})

describe('manager update restart polling', () => {
  it('does not surface connection loss or a partial response during the restart window', async () => {
    const responses: Array<unknown | Error> = [
      status('checking_signature'),
      new OfflineError(),
      { schema_version: 1, attempt_id: 'update-test', state: 'checking_health' },
      status('checking_health', { running_version: 'v1.10.0' }),
      status('succeeded', { running_version: 'v1.10.0' }),
    ]
    const events: UpdatePollEvent[] = []
    const controller = new AbortController()

    const outcome = await followManagerUpdate('update-test', {
      readStatus: async () => {
        const response = responses.shift()
        if (response instanceof Error) throw response
        return response
      },
      readManagerVersion: async () => 'v1.10.0',
      wait: async () => {},
      now: () => 0,
    }, event => events.push(event), controller.signal)

    expect(outcome).toEqual({
      kind: 'terminal',
      status: status('succeeded', { running_version: 'v1.10.0' }),
    })
    expect(events.filter(event => event.kind === 'reconnecting')).toHaveLength(2)
    expect(events).toContainEqual({ kind: 'manager_version', version: 'v1.10.0' })
  })

  it('stops reconnecting after the three-minute budget', async () => {
    let now = 0
    const controller = new AbortController()
    const outcome = await followManagerUpdate('update-test', {
      readStatus: async () => { throw new OfflineError() },
      wait: async milliseconds => { now += milliseconds },
      now: () => now,
    }, () => {}, controller.signal)

    expect(outcome).toEqual({ kind: 'timeout' })
    expect(now).toBe(180_000)
  })
})

describe('unit updates', () => {
  it('asks for the one action that enables them, and only when the machine said no', () => {
    expect(unitUpdateNote(availableUpdate({ unit_updates: { state: 'unavailable', note: 'Run the installer once to enable unit updates' } })))
      .toBe('Run the installer once to enable unit updates')
    expect(unitUpdateNote(availableUpdate({ unit_updates: { state: 'available' } }))).toBeNull()
  })

  it('says nothing before the updater has reported', () => {
    // A machine that has never run a protocol-2 update has probed nothing.
    // Asking for an installer run here would be a guess.
    expect(unitUpdateNote(availableUpdate({ unit_updates: { state: 'unknown' } }))).toBeNull()
    expect(unitUpdateNote(availableUpdate())).toBeNull()
    expect(unitUpdateNote(null)).toBeNull()
  })
})
