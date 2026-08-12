import type {
  FleetCandidate, FleetInvitation, FleetInviteProgress, FleetNodeSummary, FleetSummary, Peer,
} from './api'

// The console half of adding a Spark in two clicks (ADR 0019). Everything
// here is pure: what one poll answer means, which rows can still be added,
// and the words each state is shown as. Fleet.tsx owns the dialogs and the
// timers.
//
// No screen in this flow renders an address, a fingerprint or a code, and no
// screen renders the words the transport uses for roles. A person reads what
// a Spark does, not what the membership table calls it.

export const INVITE_POLL_MS = 2000

// Every console asks whether something is waiting for it, including one
// nobody is adding anything from, so this poll stays slow.
export const INVITATION_POLL_MS = 10000

// The found strip is for a fleet a person is still assembling by hand.
export const SWEEP_NODE_LIMIT = 4

const SETTLED_STATES = new Set(['done', 'denied', 'expired', 'failed'])
const INVITE_STATES = new Set(['waiting', 'adopting', ...SETTLED_STATES])

const text = (value: unknown): value is string => typeof value === 'string'

export const inviteSettled = (state: string): boolean => SETTLED_STATES.has(state)

export function isFleetInviteProgress(value: unknown): value is FleetInviteProgress {
  if (typeof value !== 'object' || value === null) return false
  const progress = value as Record<string, unknown>
  return text(progress.console_url) && progress.console_url.length > 0 &&
    text(progress.state) && INVITE_STATES.has(progress.state) &&
    text(progress.display_name) &&
    (progress.node === null || progress.node === undefined || typeof progress.node === 'object')
}

// Only invitations that name what is asking are shown: a prompt the owner
// cannot read is a prompt they cannot answer.
export function fleetInvitations(value: unknown): FleetInvitation[] {
  const list = (value as { invitations?: unknown } | null)?.invitations
  if (!Array.isArray(list)) return []
  return list.filter((entry): entry is FleetInvitation => {
    if (typeof entry !== 'object' || entry === null) return false
    const invitation = entry as Record<string, unknown>
    return text(invitation.id) && invitation.id.length > 0 &&
      text(invitation.controller_name) && invitation.controller_name.length > 0
  })
}

export function fleetSummary(value: unknown): FleetSummary | null {
  if (typeof value !== 'object' || value === null) return null
  const summary = value as Record<string, unknown>
  if (!text(summary.role) || !Array.isArray(summary.nodes)) return null
  return value as FleetSummary
}

// Console URLs are compared through this and never displayed by it. The
// manager normalizes the same way, so a peer's stored base URL, a membership
// row and a swept console all meet on one key.
export const consoleKey = (url: string): string => url.trim().replace(/\/+$/, '').toLowerCase()

export const fleetNodeFor = (
  summary: FleetSummary | null,
  consoleURL: string,
): FleetNodeSummary | undefined => {
  const key = consoleKey(consoleURL)
  if (!key) return undefined
  return summary?.nodes.find(node => consoleKey(node.console_url) === key)
}

// A node the manager has not finished admitting keeps its membership state as
// its status; one that is really in reports how fresh its heartbeat is
// instead (fresh, stale, unreachable, version-mismatch). So these three are
// the whole of "not in the fleet yet", and every other word means in.
const PENDING_MEMBERSHIP = new Set(['legacy-pending', 'adopting', 'adoption-uncertain'])

export const inFleet = (node?: FleetNodeSummary): boolean =>
  node !== undefined && node.status !== '' && !PENDING_MEMBERSHIP.has(node.status)

// The name of the Spark this fleet is managed from, as it calls itself.
export const leadName = (summary: FleetSummary | null): string =>
  summary?.nodes.find(node => node.node_id === summary.controller_node_id)?.display_name ?? ''

// Roles in plain words. A Spark with no fleet gets no line at all: there is
// nothing to lead or follow yet.
export function localRoleLine(summary: FleetSummary | null): string {
  if (!summary || summary.role === 'standalone') return ''
  if (summary.role === 'controller') return 'leads the fleet'
  const lead = leadName(summary)
  return lead ? `follows ${lead}` : 'follows the Spark that leads this fleet'
}

export function peerRoleLine(summary: FleetSummary | null, node?: FleetNodeSummary): string {
  if (!summary || !inFleet(node)) return ''
  if (node?.node_id === summary.controller_node_id) return 'leads the fleet'
  const lead = leadName(summary)
  return lead ? `follows ${lead}` : ''
}

// The faint line under a Spark's name: what it answers to on the network,
// then what it does in the fleet.
export const sparkSubline = (hostname: string, roleLine: string): string =>
  [hostname, roleLine].filter(Boolean).join(' · ') || 'n/a'

// The word a row's status cell adds for a Spark that is really in the fleet.
export const fleetStatusNote = (node?: FleetNodeSummary): string => (inFleet(node) ? 'In fleet' : '')

export interface FoundSpark {
  name: string
  consoleURL: string
  version: string
}

// A running basement console this fleet has never met. Anything already a
// peer, already a member, this console itself, or ignored for this session is
// not offered: the strip only ever asks about a machine the owner has not
// answered for yet.
export function foundSparks(
  candidates: FleetCandidate[],
  peers: Peer[],
  summary: FleetSummary | null,
  ignored: string[],
  selfConsoleURL: string,
): FoundSpark[] {
  const known = new Set<string>([consoleKey(selfConsoleURL), ...ignored.map(consoleKey)])
  for (const peer of peers) known.add(consoleKey(peer.base_url))
  for (const node of summary?.nodes ?? []) known.add(consoleKey(node.console_url))
  const found: FoundSpark[] = []
  for (const candidate of candidates) {
    const consoleURL = candidate.basement?.base_url ?? ''
    const key = consoleKey(consoleURL)
    if (!candidate.basement?.running || key === '' || known.has(key)) continue
    known.add(key)
    found.push({
      name: candidate.name || candidate.address,
      consoleURL,
      version: candidate.basement.version ?? '',
    })
  }
  return found
}

// The strip's second line: what that machine is running, and that it is not
// one of yours. A sweep that reported no version says only the second half.
export const foundLine = (spark: FoundSpark): string =>
  [spark.version, 'not in your fleet'].filter(Boolean).join(' · ')

// How many Sparks this fleet holds. The membership summary answers for a
// fleet; a console that has only ever paired peers counts those plus itself.
export const fleetSize = (summary: FleetSummary | null, peers: Peer[]): number =>
  summary && summary.nodes.length > 0 ? summary.nodes.length : peers.length + 1

export const shouldSweepForSparks = (size: number): boolean => size < SWEEP_NODE_LIMIT

const IGNORED_KEY = 'basement.fleet.ignored'

// Ignoring is "not now", not a decision worth keeping, so it lives for the
// session. A browser that refuses storage simply forgets, which shows the
// strip again rather than losing the machine.
export function readIgnored(store: Storage | null): string[] {
  try {
    const raw = store?.getItem(IGNORED_KEY)
    const parsed = raw ? JSON.parse(raw) : []
    return Array.isArray(parsed) ? parsed.filter((entry): entry is string => typeof entry === 'string') : []
  } catch {
    return []
  }
}

export function rememberIgnored(store: Storage | null, consoleURL: string): string[] {
  const next = [...new Set([...readIgnored(store), consoleKey(consoleURL)])]
  try {
    store?.setItem(IGNORED_KEY, JSON.stringify(next))
  } catch {
    /* the list still holds for this screen */
  }
  return next
}

// ---- What the add dialog says ----------------------------------------------

export const inviteName = (progress: FleetInviteProgress | null, fallback: string): string =>
  progress?.display_name || fallback || 'that Spark'

export const inviteTitle = (state: string, name: string): string => {
  if (state === 'done') return `${name} joined the fleet`
  if (inviteSettled(state)) return `Could not add ${name}`
  return `Approve on ${name}`
}

export const inviteBody = (name: string): string =>
  `Press Approve on ${name}'s console. We opened it in a new tab.`

export const inviteWaitLine = (state: string): string => (state === 'adopting' ? 'Adding…' : 'Waiting…')

// What stopped, in the words that answer actually carries. A denial and an
// expiry are answers rather than errors, so they read as answers. A waiting
// attempt's reason is a network blip the next poll may clear, so it is never
// shown here.
export function inviteOutcome(progress: FleetInviteProgress, name: string): string {
  switch (progress.state) {
    case 'denied':
      return `Denied on ${name}.`
    case 'expired':
      return 'The request expired. Add again when you are ready.'
    default:
      return progress.reason || `Adding ${name} stopped before it finished.`
  }
}

// The line under the title of a finished addition. The version is the one the
// new node reported for itself; absent, the line simply stops earlier.
export const joinedBadge = (version: string): string =>
  ['In fleet', 'secure channel established', version].filter(Boolean).join(' · ')

export interface JoinedFact {
  label: string
  value: string
}

export function joinedFacts(size: number, name: string): JoinedFact[] {
  return [
    { label: 'Fleet', value: `${size} ${size === 1 ? 'Spark' : 'Sparks'}, managed from this console` },
    { label: 'Updates', value: `Rolling upgrades now include ${name}` },
  ]
}

// ---- What the machine being added says --------------------------------------

export const invitationTitle = (invitation: FleetInvitation): string =>
  `Join ${invitation.controller_name}'s fleet?`

export const invitationBody = (invitation: FleetInvitation): string =>
  `${invitation.controller_name} will manage this Spark's models and updates. Everything it serves keeps running.`
