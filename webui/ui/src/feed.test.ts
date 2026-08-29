import { describe, expect, it } from 'vitest'
import type { RecipeFeedHealth } from './api'
import {
  ageInDays, feedNote, relativeTime, revokeBody, revoked, rowRevocation,
  STALE_FEED_TITLE, UNREACHABLE_FEED_TITLE,
} from './feed'

const NOW = Date.parse('2026-08-12T12:00:00Z')
const ago = (ms: number): string => new Date(NOW - ms).toISOString()
const MINUTE = 60 * 1000
const HOUR = 60 * MINUTE
const DAY = 24 * HOUR

const health = (overrides: Partial<RecipeFeedHealth> = {}): RecipeFeedHealth => ({
  state: 'ok',
  accepted_generated_at: ago(3 * HOUR),
  fetched_at: ago(20 * MINUTE),
  stale: false,
  ...overrides,
})

describe('how long ago something happened', () => {
  it('counts in the units a person would use', () => {
    expect(relativeTime(ago(20 * 1000), NOW)).toBe('just now')
    expect(relativeTime(ago(MINUTE), NOW)).toBe('1 minute ago')
    expect(relativeTime(ago(9 * MINUTE), NOW)).toBe('9 minutes ago')
    expect(relativeTime(ago(HOUR), NOW)).toBe('1 hour ago')
    expect(relativeTime(ago(5 * HOUR), NOW)).toBe('5 hours ago')
    expect(relativeTime(ago(DAY), NOW)).toBe('1 day ago')
    expect(relativeTime(ago(31 * DAY), NOW)).toBe('31 days ago')
  })

  it('never counts down from a clock that disagrees, and never guesses', () => {
    expect(relativeTime(new Date(NOW + HOUR).toISOString(), NOW)).toBe('just now')
    expect(relativeTime('', NOW)).toBe('')
    expect(relativeTime(null, NOW)).toBe('')
    expect(relativeTime('not a date', NOW)).toBe('')
  })

  it('reports whole days, or nothing at all', () => {
    expect(ageInDays(ago(45 * DAY), NOW)).toBe(45)
    expect(ageInDays(ago(HOUR), NOW)).toBe(0)
    expect(ageInDays(null, NOW)).toBeNull()
  })
})

describe('the line under the models table', () => {
  // The line states the fact and stops. Nothing on this page explains itself
  // in a sentence the eye has to read past, so the happy line carries no
  // tooltip at all: there is nothing left to say about it.
  it('says when the feed was last updated while all is well', () => {
    expect(feedNote(health(), NOW)).toEqual({ text: 'Recipes updated 3 hours ago', warn: false })
  })

  // What an unreachable feed means for the models on screen is the tooltip,
  // not the line: the last signed copy is what the table is drawn from.
  it('says the last signed copy is standing in while the feed cannot be reached', () => {
    expect(feedNote(health({ state: 'unreachable', fetched_at: ago(2 * DAY) }), NOW)).toEqual({
      text: 'Recipes unreachable since 2 days ago',
      warn: false,
      title: UNREACHABLE_FEED_TITLE,
    })
  })

  it('warns about an old index whatever the last attempt did', () => {
    const expected = {
      text: 'Recipes 40 days old',
      warn: true,
      title: STALE_FEED_TITLE,
    }
    expect(feedNote(health({ stale: true, accepted_generated_at: ago(40 * DAY) }), NOW)).toEqual(expected)
    expect(feedNote(health({ state: 'unreachable', stale: true, accepted_generated_at: ago(40 * DAY) }), NOW))
      .toEqual(expected)
    expect(feedNote(health({ stale: true, accepted_generated_at: ago(DAY) }), NOW)?.text)
      .toBe('Recipes 1 day old')
  })

  it('says nothing about a feed that has never been fetched', () => {
    expect(feedNote(health({ state: 'never_fetched', accepted_generated_at: null, fetched_at: null }), NOW))
      .toBeNull()
    expect(feedNote(null, NOW)).toBeNull()
    expect(feedNote(undefined, NOW)).toBeNull()
  })

  it('says nothing when the timestamp the line would quote is missing', () => {
    expect(feedNote(health({ accepted_generated_at: null }), NOW)).toBeNull()
    expect(feedNote(health({ state: 'unreachable', fetched_at: null }), NOW)).toBeNull()
    expect(feedNote(health({ stale: true, accepted_generated_at: null }), NOW)).toBeNull()
    expect(feedNote(health({ state: 'something new' }), NOW)).toBeNull()
  })
})

describe('a recipe the publisher has withdrawn', () => {
  const withdrawn = { revoked: true, revoked_reason: 'The weights were republished with a licence change.' }

  it('reads the row as revoked when nothing of it is installed here', () => {
    const state = rowRevocation(withdrawn)
    expect(state).toEqual({
      revoked: true,
      installedRevoked: false,
      installBlocked: true,
      reason: 'The weights were republished with a licence change.',
    })
    expect(revoked(state)).toBe(true)
  })

  it('keeps an installed model its own status and marks the recipe instead', () => {
    const state = rowRevocation(withdrawn, { revoked: true, revoked_reason: 'Withdrawn by the publisher.' })
    expect(state.revoked).toBe(false)
    expect(state.installedRevoked).toBe(true)
    expect(state.installBlocked).toBe(true)
    // The reason is the one recorded against the version really installed.
    expect(state.reason).toBe('Withdrawn by the publisher.')
  })

  it('still refuses a new install when only the version on offer was withdrawn', () => {
    const state = rowRevocation(withdrawn, { revoked: false })
    expect(state.revoked).toBe(false)
    expect(state.installedRevoked).toBe(false)
    expect(state.installBlocked).toBe(true)
    expect(revoked(state)).toBe(false)
  })

  it('says nothing at all about a recipe nobody withdrew', () => {
    const state = rowRevocation({}, { revoked: false })
    expect(state).toEqual({ revoked: false, installedRevoked: false, installBlocked: false, reason: '' })
    expect(revoked(state)).toBe(false)
  })

  it('tells a serving model what the revocation means for it, and tells a stopped one nothing', () => {
    const serving = rowRevocation(withdrawn, { revoked: true, revoked_reason: 'Withdrawn by the publisher.' })
    expect(revokeBody(serving, true))
      .toBe('Withdrawn by the publisher. This model keeps serving until you stop it.')
    expect(revokeBody(serving, false)).toBe('Withdrawn by the publisher.')
    expect(revokeBody(rowRevocation(withdrawn), true))
      .toBe('The weights were republished with a licence change.')
  })

  it('carries no reason rather than inventing one', () => {
    const state = rowRevocation({ revoked: true })
    expect(state.reason).toBe('')
    expect(revokeBody(state, false)).toBe('')
    expect(revokeBody(rowRevocation({ revoked: true }, { revoked: true }), true))
      .toBe('This model keeps serving until you stop it.')
  })
})
