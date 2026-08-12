import type { RecipeFeedHealth } from './api'

// What the console says about the recipe feed, and about a recipe the
// publisher has withdrawn. Everything here is pure: Models.tsx renders these
// answers and owns nothing about when they apply.
//
// Two rules run through the whole file. Nothing is invented: a line is only
// produced when the manager reported the fact it would rest on. And nothing
// is dramatised: a revoked recipe stops new installs, it does not stop a
// model that is already serving.

const MINUTE = 60 * 1000
const HOUR = 60 * MINUTE
const DAY = 24 * HOUR

// How long ago something happened, in the words a person would use. An empty
// string means there is no timestamp to speak from, which every caller reads
// as "say nothing" rather than as "just now". A timestamp in the future comes
// from a clock disagreement, not from the future, so it reads as just now
// instead of counting down.
export function relativeTime(iso: string | null | undefined, nowMs: number): string {
  if (!iso) return ''
  const at = Date.parse(iso)
  if (Number.isNaN(at)) return ''
  const age = Math.max(nowMs - at, 0)
  if (age < MINUTE) return 'just now'
  if (age < HOUR) {
    const minutes = Math.floor(age / MINUTE)
    return `${minutes} ${minutes === 1 ? 'minute' : 'minutes'} ago`
  }
  if (age < DAY) {
    const hours = Math.floor(age / HOUR)
    return `${hours} ${hours === 1 ? 'hour' : 'hours'} ago`
  }
  const days = Math.floor(age / DAY)
  return `${days} ${days === 1 ? 'day' : 'days'} ago`
}

// Whole days old, or null when there is no timestamp to count from.
export function ageInDays(iso: string | null | undefined, nowMs: number): number | null {
  if (!iso) return null
  const at = Date.parse(iso)
  if (Number.isNaN(at)) return null
  return Math.floor(Math.max(nowMs - at, 0) / DAY)
}

export interface FeedNote {
  text: string
  // An old index could be missing a revocation, which is worth an amber line.
  // Everything else the feed reports is ordinary and stays quiet.
  warn: boolean
}

// The line under the models table. A feed that has never been fetched gets no
// line at all: there is no feed running yet, and a sentence about one would be
// noise rather than news. Every other case needs the timestamp it would quote,
// and says nothing without it.
export function feedNote(health: RecipeFeedHealth | null | undefined, nowMs: number): FeedNote | null {
  if (!health || health.state === 'never_fetched') return null
  if (health.stale) {
    const days = ageInDays(health.accepted_generated_at, nowMs)
    if (days !== null) {
      return {
        text: `Recipe feed · ${days} ${days === 1 ? 'day' : 'days'} old. Newer recipes and revocations may be missing.`,
        warn: true,
      }
    }
  }
  if (health.state === 'unreachable') {
    const since = relativeTime(health.fetched_at, nowMs)
    return since ? { text: `Recipe feed · unreachable since ${since}, showing the last signed copy`, warn: false } : null
  }
  if (health.state === 'ok') {
    const updated = relativeTime(health.accepted_generated_at, nowMs)
    return updated ? { text: `Recipe feed · updated ${updated}`, warn: false } : null
  }
  return null
}

// ---- A recipe the publisher has withdrawn ----------------------------------

export const REVOKE_TITLE = 'Revoked by the publisher'

// Said only about a model that is really serving right now. An installed but
// stopped model is not kept alive by anything, so it is never told that it is.
export const REVOKE_STILL_SERVING = 'This model keeps serving until you stop it.'

// Only the two fields either side of a revocation carries, so a recipe, an
// installed model and a test fixture all fit.
interface Revocable {
  revoked?: boolean
  revoked_reason?: string
}

export interface RowRevocation {
  // The row itself reads as revoked: the publisher withdrew the version on
  // offer and this Spark has no install of it to speak for.
  revoked: boolean
  // Something installed here runs a version the publisher withdrew. The row
  // keeps its own true status and adds a note; nothing stops on its own.
  installedRevoked: boolean
  // No new install of the offered version can start. The manager refuses one,
  // so the console must not offer a button that would only fail.
  installBlocked: boolean
  // The publisher's own wording, carried through unchanged, or empty when the
  // revocation came without one.
  reason: string
}

export function rowRevocation(recipe: Revocable, model?: Revocable | null): RowRevocation {
  const installedRevoked = Boolean(model?.revoked)
  const offerRevoked = Boolean(recipe.revoked)
  return {
    revoked: offerRevoked && !model,
    installedRevoked,
    installBlocked: offerRevoked,
    reason: (installedRevoked ? model?.revoked_reason : recipe.revoked_reason) ?? '',
  }
}

export const revoked = (state: RowRevocation): boolean => state.revoked || state.installedRevoked

// The muted line under the title of the revoke box: the publisher's reason,
// and for a model that is serving, what that means for it right now.
export function revokeBody(state: RowRevocation, serving: boolean): string {
  const sentences = [state.reason.trim(), serving && state.installedRevoked ? REVOKE_STILL_SERVING : '']
  return sentences.filter(Boolean).join(' ')
}
