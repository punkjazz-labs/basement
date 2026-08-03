import { describe, expect, it } from 'vitest'
import { adoptedName, bareHost, rankCandidates, type FleetCandidate } from './api'

const candidate = (name: string, extra: Partial<FleetCandidate> = {}): FleetCandidate => ({
  name,
  address: `${name}.local`,
  gb10_hint: false,
  basement: null,
  ...extra,
})

describe('rankCandidates', () => {
  it('puts Sparks already running basement first, then likely GB10s', () => {
    const ranked = rankCandidates([
      candidate('plain'),
      candidate('gb10', { gb10_hint: true }),
      candidate('paired', { basement: { base_url: 'http://paired.local:7070' } }),
    ])
    expect(ranked.map(entry => entry.name)).toEqual(['paired', 'gb10', 'plain'])
  })

  it('keeps every candidate, including hosts it knows nothing about', () => {
    const found = [candidate('one'), candidate('two'), candidate('three', { gb10_hint: true })]
    expect(rankCandidates(found)).toHaveLength(3)
  })

  it('leaves the sweep order alone inside a group and does not mutate the input', () => {
    const found = [candidate('b', { gb10_hint: true }), candidate('a', { gb10_hint: true })]
    expect(rankCandidates(found).map(entry => entry.name)).toEqual(['b', 'a'])
    expect(found.map(entry => entry.name)).toEqual(['b', 'a'])
  })
})

describe('bareHost', () => {
  it('leaves a host the sweep already answered with alone', () => {
    expect(bareHost('spark-worker.local')).toBe('spark-worker.local')
    expect(bareHost('192.168.99.137')).toBe('192.168.99.137')
    expect(bareHost('  spark-worker.local  ')).toBe('spark-worker.local')
  })

  it('drops a scheme, a port, a path and any credentials', () => {
    expect(bareHost('http://spark-worker.local:7070')).toBe('spark-worker.local')
    expect(bareHost('https://user:secret@spark-worker.local:7070/console')).toBe('spark-worker.local')
    expect(bareHost('spark-worker.local:7070')).toBe('spark-worker.local')
    expect(bareHost('spark-worker.local/console?x=1')).toBe('spark-worker.local')
  })

  it('keeps an IPv6 literal whole', () => {
    expect(bareHost('http://[fe80::1]:7070')).toBe('[fe80::1]')
    expect(bareHost('fe80::1')).toBe('fe80::1')
  })
})

describe('adoptedName', () => {
  it('reads the name from either result shape and stays empty when absent', () => {
    expect(adoptedName({ peer: 'spark-worker' })).toBe('spark-worker')
    expect(adoptedName({ peer: { id: '1', name: 'spark-worker', base_url: 'http://x:7070' } })).toBe('spark-worker')
    expect(adoptedName(undefined)).toBe('')
    expect(adoptedName({})).toBe('')
  })
})
