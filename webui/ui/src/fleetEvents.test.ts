import { beforeEach, describe, expect, it } from 'vitest'
import type { FleetDeploymentView, Job } from './api'
import {
  healthChanges, jobEvents, jobText, latestEvents, recordHealth, resetEvents, seenEvents, snapshotOf,
} from './fleetEvents'
import { boxHealth, type BoxReading } from './fleetHealth'
import { boxHeadroom } from './headroom'

const GB = 1_000_000_000

const job = (id: string, kind: string, state: string, recipeID: string, at: string): Job => ({
  id, kind, state, recipe_id: recipeID, created_at: at, updated_at: at, steps: [],
})

const health = (name: string, freeGB: number, over: Partial<BoxReading> = {}) => boxHealth({
  id: `node-${name}`, name, self: false, answering: true, ageMs: 3000,
  headroom: boxHeadroom({ id: `node-${name}`, name, totalBytes: 128 * GB, availableBytes: freeGB * GB }),
  ...over,
})

const names: Record<string, string> = { 'glm53-flash-exl3-2s': 'GLM 5.3 Flash', 'nemotron-omni-30b-a3b-nvfp4-1s': 'Nemotron Omni' }
const nameOf = (id: string) => names[id] ?? id

describe('what a job says on the timeline', () => {
  it('says what finished, in a word or two', () => {
    expect(jobText(job('a', 'start', 'ready', 'x', ''))).toEqual({ tone: 'ok', text: 'started' })
    expect(jobText(job('a', 'stop', 'stopped', 'x', ''))).toEqual({ tone: 'ok', text: 'stopped' })
    expect(jobText(job('a', 'benchmark', 'ready', 'x', ''))).toEqual({ tone: 'ok', text: 'speed measured' })
    expect(jobText(job('a', 'remove', 'removed', 'x', ''))).toEqual({ tone: 'ok', text: 'uninstalled' })
  })

  it('says what failed in red and what was cancelled in amber', () => {
    expect(jobText(job('a', 'install', 'failed', 'x', ''))).toEqual({ tone: 'fail', text: 'install failed' })
    expect(jobText(job('a', 'smoke-test', 'cancelled', 'x', ''))).toEqual({ tone: 'warn', text: 'health check cancelled' })
  })

  it('says what is still running in its own phase words', () => {
    expect(jobText(job('a', 'install', 'downloading_models', 'x', ''))).toEqual({ tone: 'busy', text: 'install · downloading model' })
  })
})

describe('the jobs as events', () => {
  const self = { id: 'node-spark-f7e0', name: 'spark-f7e0' }

  it('names the model, and the box the job ran on', () => {
    const events = jobEvents({
      jobs: [job('j1', 'install', 'failed', 'nemotron-omni-30b-a3b-nvfp4-1s', '2026-10-24T09:54:41Z')],
      deployments: [], nameOf, self, boxName: () => '',
    })
    expect(events).toEqual([expect.objectContaining({
      subject: 'Nemotron Omni', text: 'install failed', tone: 'fail', boxName: 'spark-f7e0', jobID: 'j1',
    })])
  })

  // A job the fleet ran on another box is told there, and carried whole so
  // the progress window can follow it.
  it('names the box a placement ran its job on, and tells a job once', () => {
    const remote = job('j2', 'stop', 'stopped', 'deepseek-v4-flash-0731-2s', '2026-10-25T09:00:00Z')
    const deployment = {
      deployment_id: 'dep_ds_tp2', recipe_id: 'deepseek-v4-flash-0731-2s', owner_node_id: 'node-edgexpert-41b8',
      job: remote,
    } as unknown as FleetDeploymentView
    const events = jobEvents({
      jobs: [remote], deployments: [deployment], nameOf, self, boxName: id => (id === 'node-edgexpert-41b8' ? 'edgexpert-41b8' : ''),
    })
    expect(events).toHaveLength(1)
    const only = jobEvents({ jobs: [], deployments: [deployment], nameOf, self, boxName: () => 'edgexpert-41b8' })
    expect(only[0]).toMatchObject({ boxName: 'edgexpert-41b8', deploymentID: 'dep_ds_tp2', text: 'stopped' })
  })

  it('keeps the newest few, newest first', () => {
    const events = jobEvents({
      jobs: Array.from({ length: 12 }, (_, index) =>
        job(`j${index}`, 'start', 'ready', 'x', `2026-10-${String(10 + index).padStart(2, '0')}T10:00:00Z`)),
      deployments: [], nameOf, self, boxName: () => '',
    })
    const latest = latestEvents([events], 8)
    expect(latest).toHaveLength(8)
    expect(latest[0].jobID).toBe('j11')
    expect(latest[7].jobID).toBe('j4')
  })
})

describe('changes seen between two reads', () => {
  // A console that has only just opened has nothing to compare against, so
  // a box that was already tight is not news.
  it('says nothing about the first read', () => {
    expect(healthChanges(null, [health('edgexpert-5e12', 7.2)], 1)).toEqual([])
  })

  it('tells a new trouble once, in its own words and colour', () => {
    const before = snapshotOf([health('edgexpert-5e12', 19)])
    const events = healthChanges(before, [health('edgexpert-5e12', 7.2)], 100)
    expect(events).toEqual([expect.objectContaining({ subject: 'edgexpert-5e12', text: 'memory tight (7.2 GB free, loads refused)', tone: 'warn', at: 100 })])
    // The same trouble read again, with a different figure, is not a change.
    expect(healthChanges(snapshotOf([health('edgexpert-5e12', 7.2)]), [health('edgexpert-5e12', 6.9)], 200)).toEqual([])
  })

  it('tells a trouble that cleared', () => {
    const before = snapshotOf([health('spark-9a07', 17.8, { answering: false })])
    const events = healthChanges(before, [health('spark-9a07', 17.8)], 300)
    expect(events.map(event => event.text)).toEqual(['answering again'])
    expect(events[0].tone).toBe('ok')
  })

  it('tells a box that joined or left', () => {
    const before = snapshotOf([health('spark-f7e0', 19)])
    const events = healthChanges(before, [health('spark-new1', 19)], 400)
    expect(events.map(event => `${event.subject} ${event.text}`).sort()).toEqual(['spark-f7e0 left the fleet', 'spark-new1 joined the fleet'])
  })
})

describe('the changes this console has seen', () => {
  beforeEach(() => resetEvents())

  it('keeps every change after the first read, newest first', () => {
    recordHealth([health('edgexpert-5e12', 19)], 1)
    expect(seenEvents()).toEqual([])
    recordHealth([health('edgexpert-5e12', 7.2)], 2)
    recordHealth([health('edgexpert-5e12', 19)], 3)
    expect(seenEvents().map(event => event.text)).toEqual(['memory back above the reserve', 'memory tight (7.2 GB free, loads refused)'])
  })
})
