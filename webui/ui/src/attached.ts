import { relativeTime } from './feed'

// Attached services (ADR 0025): model services an operator runs on these
// Sparks outside basement and declares to it. The console shows them and
// their health, and offers no control over them at all: basement does not
// install, start, stop or update them. Everything here is pure; the card in
// attachedCard.tsx draws it and Models.tsx polls for it.
//
// Every fact on the card is the operator's own declaration, shown as
// written. An absent optional field is n/a, never a guess.

export const ATTACHED_PATH = '/api/v1/attached-services'

// The manager checks each service every 30 seconds by default, so reading
// the list a little faster than that keeps "checked N s ago" honest without
// asking more often than anything can change.
export const ATTACHED_POLL_MS = 10000

export const ATTACHED_ROLE = 'Attached service · managed outside Basement'

export interface AttachedNodeDeclaration {
  node_name?: string
  node_id?: string
  role?: string
}

export interface AttachedDeclaration {
  schema_version: number
  id: string
  display_name: string
  model_by?: string
  kind: string
  served_model_name?: string
  managed_by: string
  endpoint?: string
  health_url: string
  health_interval_seconds?: number
  topology: { spark_count: number; parallelism?: string; nodes: AttachedNodeDeclaration[] }
  licence?: string
  licence_url?: string
  licence_territory_exclusions?: string[]
  notes?: string
}

export type AttachedStatus = 'healthy' | 'unhealthy' | 'unknown'

export interface AttachedHealth {
  status: AttachedStatus
  last_checked_at?: string
  latency_ms: number
  last_error?: string
}

// One Spark a declaration names. protected says this manager itself keeps
// its own models off that Spark; a name it cannot match is not protected.
export interface AttachedNode {
  name: string
  node_id?: string
  role?: string
  this_spark: boolean
  protected: boolean
}

export interface AttachedService {
  file: string
  declaration: AttachedDeclaration
  health: AttachedHealth
  nodes: AttachedNode[]
}

export interface AttachedProblem {
  file: string
  error: string
}

export interface AttachedServices {
  services: AttachedService[]
  errors: AttachedProblem[]
}

const text = (value: unknown): value is string => typeof value === 'string'
const record = (value: unknown): value is Record<string, unknown> => typeof value === 'object' && value !== null

const STATUSES: ReadonlySet<string> = new Set(['healthy', 'unhealthy', 'unknown'])

// The answer of GET /api/v1/attached-services, keeping only entries the card
// can draw. A manager that has never heard of the route answers with an
// error, which arrives here as nothing to show.
export function attachedServices(value: unknown): AttachedServices {
  const empty: AttachedServices = { services: [], errors: [] }
  if (!record(value)) return empty
  const services = Array.isArray(value.services) ? value.services.filter((entry): entry is AttachedService => {
    if (!record(entry) || !text(entry.file) || !record(entry.declaration) || !record(entry.health)) return false
    const declaration = entry.declaration
    return text(declaration.id) && text(declaration.display_name) && text(declaration.kind) &&
      text(declaration.managed_by) && record(declaration.topology) &&
      text(entry.health.status) && STATUSES.has(entry.health.status) && Array.isArray(entry.nodes)
  }) : []
  const errors = Array.isArray(value.errors)
    ? value.errors.filter((entry): entry is AttachedProblem => record(entry) && text(entry.file) && text(entry.error))
    : []
  return { services, errors }
}

// "video-generation" reads as "Video generation". The kind is the operator's
// word, so a kind this console has never seen is still shown, not replaced.
export const kindLabel = (kind: string): string => {
  const words = kind.split('-').filter(Boolean).join(' ')
  return words ? words[0].toUpperCase() + words.slice(1) : 'n/a'
}

const sparks = (count: number): string => (count === 1 ? '1 Spark' : `${count} Sparks`)

// How many Sparks, and how the model is split across them.
export const topologyLine = (declaration: AttachedDeclaration): string => {
  const parallelism = declaration.topology.parallelism?.trim()
  return parallelism
    ? `${sparks(declaration.topology.spark_count)} · ${parallelism}`
    : sparks(declaration.topology.spark_count)
}

// When the last check ran. Under a minute this counts seconds, because a
// check runs every half minute and "just now" would cover both of the last
// two; beyond that it reads like every other age on the console.
export function checkedAgo(iso: string | undefined, nowMs: number): string {
  if (!iso) return ''
  const at = Date.parse(iso)
  if (Number.isNaN(at)) return ''
  const seconds = Math.floor(Math.max(nowMs - at, 0) / 1000)
  if (seconds < 1) return 'checked just now'
  if (seconds < 60) return `checked ${seconds} s ago`
  return `checked ${relativeTime(iso, nowMs)}`
}

// How long the last check took to answer, in the units a reader expects.
export const answeredIn = (ms: number): string =>
  ms < 1000 ? `answered in ${Math.max(Math.round(ms), 0)} ms` : `answered in ${(ms / 1000).toFixed(1)} s`

export interface HealthRead {
  // The status chip's tone, from the set every serving card uses: live while
  // the service answers, fail when it does not, plain before the first check.
  tone: 'live' | 'fail' | ''
  word: string
  // The card's live line: when the check ran, then how fast it answered or
  // why it failed. Empty until there has been a check to speak of.
  parts: string[]
}

export function healthRead(health: AttachedHealth, nowMs: number): HealthRead {
  if (health.status === 'unknown' || !health.last_checked_at) return { tone: '', word: 'Not checked yet', parts: [] }
  const when = checkedAgo(health.last_checked_at, nowMs)
  if (health.status === 'healthy') {
    return { tone: 'live', word: 'Healthy', parts: [when, answeredIn(health.latency_ms)].filter(Boolean) }
  }
  return { tone: 'fail', word: 'Unhealthy', parts: [when, health.last_error ?? ''].filter(Boolean) }
}

const joined = (items: string[]): string => {
  if (items.length < 2) return items[0] ?? ''
  if (items.length === 2) return `${items[0]} and ${items[1]}`
  return `${items.slice(0, -1).join(', ')} and ${items[items.length - 1]}`
}

// The sentence the install dialog already uses for a licence that excludes
// territories, followed by the list as the declaration gives it.
export const territoryLine = (declaration: AttachedDeclaration): string | undefined => {
  const territories = (declaration.licence_territory_exclusions ?? []).filter(item => item.trim() !== '')
  if (territories.length === 0) return undefined
  return `This licence does not grant rights in these territories: ${territories.join(', ')}.`
}

// What basement does about the Sparks the service names. A name this manager
// cannot match to a Spark it places models on is said out loud, because the
// whole point of the declaration is to keep those Sparks free and a misspelt
// name would otherwise fail silently.
export function guardLine(service: AttachedService): { text: string; warn: boolean } {
  const loose = service.nodes.filter(node => !node.protected).map(node => node.name)
  if (loose.length > 0) {
    return {
      warn: true,
      text: `This console cannot match ${joined(loose)} to a Spark it places models on, so it cannot keep its own models off ${loose.length === 1 ? 'it' : 'them'}. Check the node name in ${service.file}.`,
    }
  }
  const where = service.nodes.length === 1 ? 'this Spark' : 'these Sparks'
  return {
    warn: false,
    text: `Basement does not install, start, stop or update this service. It keeps its own models off ${where} while ${service.file} is in place.`,
  }
}

// A licence link is drawn only for an https address. The manager refuses any
// other, and this is the second lock on the same door.
export const licenceHref = (declaration: AttachedDeclaration): string | undefined =>
  declaration.licence_url?.startsWith('https://') ? declaration.licence_url : undefined
