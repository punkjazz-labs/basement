import { describe, expect, it } from 'vitest'
import { Children, isValidElement, type ReactElement } from 'react'
import { OfflineError, type UpdateAttemptStatus, type UpdateInfo } from './api'
import { ManagerUpdateSidebar } from './views/ManagerUpdate'
import {
  fleetUpgradeRowState, fleetUpgradeStateWord, followManagerUpdate,
  initialManagerUpdateDialogState, isInstallableManagerUpdate, managerUpdateCard,
  managerUpdateDialogReducer, orderedFleetUpgradeNodes, updateRefusal, type UpdatePollEvent,
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

describe('manager update dialog', () => {
  it('the sidebar control opens the dialog and it stays open through the restart window', () => {
    let state = initialManagerUpdateDialogState
    const sidebar = ManagerUpdateSidebar({
      info: availableUpdate(),
      managerVersion: 'v1.9.9',
      onOpen: () => {
        state = managerUpdateDialogReducer(state, { type: 'open_from_sidebar' })
      },
    })
    const updateControl = Children.toArray(sidebar.props.children).find(child =>
      isValidElement<{ className?: string }>(child) && child.props.className === 'side-update',
    ) as ReactElement<{ children: string; onClick: () => void }> | undefined

    expect(updateControl?.props.children).toBe('Update 1.10.0 available')
    updateControl?.props.onClick()
    expect(state).toEqual({ open: true, reconnecting: false })

    state = managerUpdateDialogReducer(state, { type: 'reconnecting', value: true })
    expect(state).toEqual({ open: true, reconnecting: true })
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
