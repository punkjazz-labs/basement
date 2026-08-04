import { describe, expect, it } from 'vitest'
import { stepCopy, stepElapsedSeconds, stepOperation, type Step } from './api'

describe('stepOperation', () => {
  it('reads a two-Spark step as the operation it is', () => {
    // A distributed job stores "download_artifact:head" and
    // "download_artifact:worker". Matching those literally is what left the
    // deployment dialog with no download detail on a two-Spark install.
    expect(stepOperation('download_artifact:head')).toBe('download_artifact')
    expect(stepOperation('download_artifact:worker')).toBe('download_artifact')
    expect(stepOperation('pull_image:worker')).toBe('pull_image')
    expect(stepOperation('rollback_stop_container:head')).toBe('stop_container')
  })

  it('leaves a single-Spark step alone', () => {
    expect(stepOperation('download_artifact')).toBe('download_artifact')
    expect(stepOperation('rollback_start_container')).toBe('start_container')
  })
})

describe('stepCopy', () => {
  it('names the Spark a two-Spark step ran on', () => {
    expect(stepCopy('download_artifact:worker')).toBe('Download model files (second Spark)')
    expect(stepCopy('download_artifact:head')).toBe('Download model files (this Spark)')
    expect(stepCopy('download_artifact')).toBe('Download model files')
  })
})

describe('stepElapsedSeconds', () => {
  const step = (overrides: Partial<Step> = {}): Step => ({
    index: 0,
    operation: 'wait_http',
    state: 'running',
    ...overrides,
  })

  it('counts from the step\'s own started_at, not from whenever it is asked', () => {
    // This is the bug: closing and reopening the deployment dialog used to
    // reset a mount-time counter to zero. The real fix reads the server's
    // started_at every time, so asking twice a minute apart just advances.
    const startedAt = '2026-08-04T10:00:00.000Z'
    const running = step({ started_at: startedAt })
    const firstAsk = Date.parse('2026-08-04T10:04:00.000Z')
    const secondAskAfterReopen = Date.parse('2026-08-04T10:09:30.000Z')
    expect(stepElapsedSeconds(running, firstAsk)).toBe(240)
    expect(stepElapsedSeconds(running, secondAskAfterReopen)).toBe(570)
  })

  it('reports a finished step\'s fixed duration instead of still counting up', () => {
    const finished = step({
      state: 'completed',
      started_at: '2026-08-04T10:00:00.000Z',
      completed_at: '2026-08-04T10:03:30.000Z',
    })
    const duration = stepElapsedSeconds(finished, Date.parse('2026-08-04T10:03:30.000Z'))
    // Asking long after completion must not change the answer.
    const askedLater = stepElapsedSeconds(finished, Date.parse('2026-08-04T11:00:00.000Z'))
    expect(duration).toBe(210)
    expect(askedLater).toBe(210)
  })

  it('never goes negative when the client clock runs behind the server', () => {
    const justStarted = step({ started_at: '2026-08-04T10:00:05.000Z' })
    expect(stepElapsedSeconds(justStarted, Date.parse('2026-08-04T10:00:00.000Z'))).toBe(0)
  })

  it('returns null when the server has not recorded a start yet', () => {
    expect(stepElapsedSeconds(step({ started_at: undefined }), Date.now())).toBeNull()
    expect(stepElapsedSeconds(step({ started_at: '' }), Date.now())).toBeNull()
  })

  it('returns null for an unparseable timestamp rather than a bogus duration', () => {
    expect(stepElapsedSeconds(step({ started_at: 'not-a-date' }), Date.now())).toBeNull()
  })
})
