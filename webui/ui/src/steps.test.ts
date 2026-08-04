import { describe, expect, it } from 'vitest'
import { benchmarkReceipt, stepCopy, stepElapsedSeconds, stepOperation, type Job, type Step } from './api'

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

describe('benchmarkReceipt', () => {
  const job = (overrides: Partial<Job> = {}): Job => ({
    id: 'job-1',
    kind: 'benchmark',
    recipe_id: 'some-model',
    state: 'ready',
    created_at: '2026-08-04T10:00:00.000Z',
    updated_at: '2026-08-04T10:00:30.000Z',
    steps: [],
    ...overrides,
  })

  const measureStep = (operation: string, overrides: Partial<Step> = {}): Step => ({
    index: 0,
    operation,
    state: 'completed',
    receipt: { tokens_per_second: 42.5, time_to_first_token_ms: 120, completion_tokens: 256 },
    ...overrides,
  })

  it('finds the celebration receipt on a single-Spark job', () => {
    const finished = job({ steps: [measureStep('wait_http'), measureStep('measure_throughput')] })
    expect(benchmarkReceipt(finished)?.tokens_per_second).toBe(42.5)
  })

  // This is task #53's console-side twin: a two-Spark job records the step
  // as "measure_throughput:head" (see stepName in engine.go), so matching
  // the bare "measure_throughput" operation string left the celebration
  // screen never appearing for DeepSeek's distributed benchmark even though
  // the job reached "ready" with a real number in the step receipt.
  it('finds the celebration receipt on a two-Spark job despite the role suffix', () => {
    const finished = job({ steps: [measureStep('wait_http'), measureStep('measure_throughput:head')] })
    expect(benchmarkReceipt(finished)?.tokens_per_second).toBe(42.5)
  })

  it('also matches the worker-role suffix, not just head', () => {
    const finished = job({ steps: [measureStep('measure_throughput:worker')] })
    expect(benchmarkReceipt(finished)?.tokens_per_second).toBe(42.5)
  })

  it('is undefined for a non-benchmark job', () => {
    const finished = job({ kind: 'install', steps: [measureStep('measure_throughput')] })
    expect(benchmarkReceipt(finished)).toBeUndefined()
  })

  it('is undefined while the benchmark is still running', () => {
    const running = job({ state: 'benchmarking', steps: [measureStep('measure_throughput', { state: 'running' })] })
    expect(benchmarkReceipt(running)).toBeUndefined()
  })

  it('is undefined when the benchmark failed or was cancelled', () => {
    const failed = job({ state: 'failed', error: 'timed out', steps: [measureStep('measure_throughput', { state: 'failed', receipt: undefined })] })
    const cancelled = job({ state: 'cancelled', steps: [measureStep('measure_throughput', { state: 'cancelled' })] })
    expect(benchmarkReceipt(failed)).toBeUndefined()
    expect(benchmarkReceipt(cancelled)).toBeUndefined()
  })
})
