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
//
// Each line names the thing it speaks of. The happy line speaks of the recipes
// themselves, which really were updated. The other two speak of the signed
// list this console holds and of the feed it reads that list from, and neither
// of those is the recipes.
export function feedNote(health: RecipeFeedHealth | null | undefined, nowMs: number): FeedNote | null {
  if (!health || health.state === 'never_fetched') return null
  if (health.stale) {
    const days = ageInDays(health.accepted_generated_at, nowMs)
    if (days !== null) {
      return {
        text: `Recipe list ${days} ${days === 1 ? 'day' : 'days'} old; may be missing updates.`,
        warn: true,
      }
    }
  }
  if (health.state === 'unreachable') {
    const since = relativeTime(health.fetched_at, nowMs)
    return since
      ? { text: `Recipe feed unreachable since ${since}, showing the last signed copy`, warn: false }
      : null
  }
  if (health.state === 'ok') {
    const updated = relativeTime(health.accepted_generated_at, nowMs)
    return updated ? { text: `Recipes updated ${updated}`, warn: false } : null
  }
  return null
}

// ---- The button that asks the feed now -------------------------------------

// The whole vocabulary of that button. Three words, and no sentence anywhere
// else: a check that brought a newer index is answered by the line above,
// which then says the recipes were updated just now, but a check that found
// the same index moves nothing on screen, so the button answers for itself.
export const CHECK_IDLE = 'Check now'
export const CHECK_BUSY = 'Checking'
export const CHECK_UNCHANGED = 'Nothing new'

// How long the button holds "Nothing new" before it offers the check again.
export const CHECK_HOLD_MS = 4000

// Both sides as a moment, or null when there is no index to speak of. Text is
// never compared: the same instant written two ways is the same index.
function moment(iso: string | null | undefined): number | null {
  if (!iso) return null
  const at = Date.parse(iso)
  return Number.isNaN(at) ? null : at
}

// Whether the check accepted the same index the console already had. A
// timestamp on one side only is a change: this Spark either gained its first
// accepted index or lost the one it had.
export function feedUnchanged(before: string | null | undefined, after: string | null | undefined): boolean {
  return moment(before) === moment(after)
}

// What the button says. Busy wins over everything, because a check that is
// running has not answered yet.
export function checkLabel(busy: boolean, unchanged: boolean): string {
  if (busy) return CHECK_BUSY
  return unchanged ? CHECK_UNCHANGED : CHECK_IDLE
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
