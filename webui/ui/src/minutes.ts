import type { APIKey } from './api'
import { relativeTime } from './feed'
import { DEFAULT_ROLE } from './roles'

// Minutes is a Mac menu bar app. It records and transcribes on the Mac and
// sends only the transcript here, so the only trace it leaves on this Spark is
// one API key. That key is the whole state this page reads: whether it exists,
// and whether a request has ever arrived with it.

// The name the console gives the key it creates for Minutes.
export const MINUTES_KEY_NAME = 'minutes'

// The release page, which always offers the newest build.
export const MINUTES_DOWNLOAD_URL = 'https://github.com/punkjazz-labs/minutes/releases/latest'

// The model name Minutes is given. role/standard follows whatever model is
// serving until a model is assigned to it, so it is the one name that keeps
// working after the model behind it changes.
export const MINUTES_MODEL = `role/${DEFAULT_ROLE}`

// The endpoint Minutes is pointed at. The console's own origin serves /v1 on
// the same port, so the address in the browser bar is the address the Mac
// needs.
export const minutesEndpoint = (origin: string): string => `${origin}/v1`

// The key this page is about. A used key wins over an unused one, so a second
// key of the same name cannot hide a setup that already works.
export function minutesKey(keys: readonly APIKey[]): APIKey | null {
  const named = keys.filter(key => key.name === MINUTES_KEY_NAME)
  return named.find(key => Boolean(key.last_used_at)) ?? named[0] ?? null
}

export type MinutesState = 'setup' | 'done'

// Only a request that arrived closes the setup. A key that exists but has
// served nothing proves nothing about the Mac, and a revoked key takes the
// page back to the first step.
export const minutesState = (keys: readonly APIKey[]): MinutesState =>
  minutesKey(keys)?.last_used_at ? 'done' : 'setup'

// What the key row says about the last request, in the console's own words for
// how long ago something happened.
export function lastUsedLine(key: APIKey | null, nowMs: number): string {
  const since = relativeTime(key?.last_used_at, nowMs)
  return since === '' ? 'n/a' : `last used ${since}`
}

// The line at the foot of the page. It reads the model this console already
// knows is serving, and green stays the serving color.
export const servingLine = (modelName?: string): string =>
  modelName ? `${modelName} is serving.` : 'No model is serving.'
