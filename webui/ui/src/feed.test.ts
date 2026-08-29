import { describe, expect, it } from 'vitest'
import type { RecipeFeedCheck, RecipeFeedHealth } from './api'
import {
  ageInDays, checkFoundNothingNew, checkLabel, feedNote, feedUnchanged, relativeTime, revokeBody, revoked,
  rowRevocation,
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
  // The recipes really were updated, so this line names them.
  it('says when the feed was last updated while all is well', () => {
    expect(feedNote(health(), NOW)).toEqual({ text: 'Recipes updated 3 hours ago', warn: false })
  })

  // The feed is what cannot be reached, and the signed list this console
  // already holds is what the table is drawn from meanwhile. Neither of those
  // is the recipes, so neither line names them.
  it('says the last signed copy is standing in while the feed cannot be reached', () => {
    expect(feedNote(health({ state: 'unreachable', fetched_at: ago(2 * DAY) }), NOW)).toEqual({
      text: 'Recipe feed unreachable since 2 days ago, showing the last signed copy',
      warn: false,
    })
  })

  it('warns about an old index whatever the last attempt did', () => {
    const expected = {
      text: 'Recipe list 40 days old; may be missing updates.',
      warn: true,
    }
    expect(feedNote(health({ stale: true, accepted_generated_at: ago(40 * DAY) }), NOW)).toEqual(expected)
    expect(feedNote(health({ state: 'unreachable', stale: true, accepted_generated_at: ago(40 * DAY) }), NOW))
      .toEqual(expected)
    expect(feedNote(health({ stale: true, accepted_generated_at: ago(DAY) }), NOW)?.text)
      .toBe('Recipe list 1 day old; may be missing updates.')
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

describe('the button that asks the feed now', () => {
  const index = ago(3 * HOUR)
  // What the manager answers a forced check with: the health that check
  // produced, plus whether it reported a check made moments earlier.
  const checked = (overrides: Partial<RecipeFeedCheck> = {}): RecipeFeedCheck => ({
    state: 'ok',
    accepted_generated_at: index,
    fetched_at: ago(0),
    stale: false,
    refreshed_recently: false,
    ...overrides,
  })

  it('answers Nothing new only after a check that reached the feed', () => {
    expect(checkFoundNothingNew(index, checked())).toBe(true)
    expect(checkFoundNothingNew(index, checked({ accepted_generated_at: ago(MINUTE) }))).toBe(false)
  })

  // A check that never reached the feed leaves the accepted index exactly
  // where it was, so the timestamps agree while the check found nothing at
  // all. The state is what separates the two, whatever the timestamps say.
  it('claims nothing at all when the check did not reach the feed', () => {
    expect(checkFoundNothingNew(index, checked({ state: 'unreachable' }))).toBe(false)
    expect(checkFoundNothingNew(index, checked({ state: 'unreachable', fetched_at: ago(2 * DAY) }))).toBe(false)
    expect(checkFoundNothingNew(null, checked({ state: 'never_fetched', accepted_generated_at: null, fetched_at: null })))
      .toBe(false)
    expect(checkFoundNothingNew(index, checked({ state: 'never_fetched' }))).toBe(false)
  })

  // The manager answers a second check inside its 30 second window with the
  // health of the check before it. That check reached the feed, so its answer
  // is still true.
  it('speaks for a check made moments earlier as readily as for its own', () => {
    expect(checkFoundNothingNew(index, checked({ refreshed_recently: true }))).toBe(true)
    expect(checkFoundNothingNew(index, checked({ refreshed_recently: true, accepted_generated_at: ago(MINUTE) })))
      .toBe(false)
    expect(checkFoundNothingNew(index, checked({ refreshed_recently: true, state: 'unreachable' }))).toBe(false)
  })

  it('answers for itself when the check found the same index', () => {
    expect(feedUnchanged(index, index)).toBe(true)
    expect(checkLabel(false, feedUnchanged(index, index))).toBe('Nothing new')
  })

  it('goes straight back when the check brought a newer index', () => {
    expect(feedUnchanged(index, ago(MINUTE))).toBe(false)
    expect(checkLabel(false, feedUnchanged(index, ago(MINUTE)))).toBe('Check now')
  })

  it('reads the same instant written two ways as the same index', () => {
    expect(feedUnchanged('2026-08-12T09:00:00Z', '2026-08-12T09:00:00.000+00:00')).toBe(true)
    expect(feedUnchanged('2026-08-12T09:00:00Z', '2026-08-12T10:00:00+01:00')).toBe(true)
  })

  it('counts a first accepted index, and a lost one, as a change', () => {
    expect(feedUnchanged(null, index)).toBe(false)
    expect(feedUnchanged(index, null)).toBe(false)
    expect(feedUnchanged(null, null)).toBe(true)
    expect(feedUnchanged(undefined, 'not a date')).toBe(true)
  })

  it('says nothing but Checking while the check runs', () => {
    expect(checkLabel(true, false)).toBe('Checking')
    expect(checkLabel(true, true)).toBe('Checking')
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
