import { stateCopy, terminal, type FleetDeploymentView, type Job } from './api'
import type { BoxHealth, IssueKind, Light } from './fleetHealth'

// What happened lately, newest first: the Events column on Monitor. Charts say
// how much; this says what changed, so a dip in a line has a reason beside it.
//
// Two sources, both already in the console's hands. The jobs this Spark ran
// and the jobs the fleet ran through its placements are the record of every
// install, start, stop and check. What no job records (a box that stopped
// answering, memory that went under the reserve) is noticed here, between one
// read of the fleet and the next, while the console is open. Nothing is
// invented to fill the column: a console opened a moment ago starts with the
// jobs and adds a change only once it has seen one.

export type EventTone = Light | 'busy'

export interface FleetEvent {
  id: string
  at: number
  tone: EventTone
  // What the event is about: a model or a box.
  subject: string
  text: string
  // Where it happened, when that is a box the console can open.
  boxID?: string
  boxName?: string
  // The job behind it, which the progress window can open.
  jobID?: string
  // A job that ran on another box, carried whole with its placement.
  deploymentID?: string
  job?: Job
  // The kind of trouble a box event is about, so a change the console saw
  // and the same trouble still standing are told once.
  issue?: IssueKind
  // A trouble that is still the state of the box now, not a moment past.
  ongoing?: boolean
}

// What each kind of job did once it finished well, and what to call it while
// it runs or after it failed.
const DONE: Record<string, string> = {
  install: 'installed',
  start: 'started',
  stop: 'stopped',
  remove: 'uninstalled',
  'smoke-test': 'health check passed',
  benchmark: 'speed measured',
  adopt: 'taken on by the fleet',
}
const NOUN: Record<string, string> = {
  install: 'install',
  start: 'start',
  stop: 'stop',
  remove: 'uninstall',
  'smoke-test': 'health check',
  benchmark: 'speed measurement',
  adopt: 'adoption',
}

// A start that stopped another model first is a swap: the engine plans the
// stop of whatever was running into the start itself (ADR 0003), so the step
// is the record that one model made way for another.
const swapped = (job: Job): boolean =>
  job.kind === 'start' && (job.steps ?? []).some(step => step.operation === 'stop_container' && step.state === 'completed')

export function jobText(job: Job): { tone: EventTone; text: string } {
  const noun = NOUN[job.kind] ?? job.kind
  if (job.state === 'failed') return { tone: 'fail', text: `${noun} failed` }
  if (job.state === 'cancelled') return { tone: 'warn', text: `${noun} cancelled` }
  if (terminal(job.state)) return { tone: 'ok', text: swapped(job) ? 'swapped in' : DONE[job.kind] ?? `${noun} done` }
  return { tone: 'busy', text: `${noun} · ${(stateCopy[job.state] ?? job.state).toLowerCase()}` }
}

const stamp = (job: Job): number => {
  const at = Date.parse(job.updated_at || job.created_at)
  return Number.isFinite(at) ? at : 0
}

// Every job this console can see, as events. A job the fleet ran through a
// placement names the box it ran on; this Spark's own jobs name this Spark.
// A job both lists carry is told once.
export function jobEvents(input: {
  jobs: readonly Job[]
  deployments: readonly FleetDeploymentView[]
  nameOf: (recipeID: string) => string
  self: { id: string; name: string }
  boxName: (nodeID: string) => string
}): FleetEvent[] {
  const seen = new Set<string>()
  const events: FleetEvent[] = []
  for (const job of input.jobs) {
    if (seen.has(job.id)) continue
    seen.add(job.id)
    const { tone, text } = jobText(job)
    events.push({
      id: `job:${job.id}`, at: stamp(job), tone, subject: input.nameOf(job.recipe_id), text,
      boxID: input.self.id, boxName: input.self.name, jobID: job.id,
    })
  }
  for (const deployment of input.deployments) {
    const job = deployment.job
    if (!job || seen.has(job.id)) continue
    seen.add(job.id)
    const { tone, text } = jobText(job)
    const owner = deployment.owner_node_id
    events.push({
      id: `job:${job.id}`, at: stamp(job), tone, subject: input.nameOf(job.recipe_id || deployment.recipe_id), text,
      boxID: owner, boxName: input.boxName(owner), deploymentID: deployment.deployment_id, job,
    })
  }
  return events
}

// ---- Changes seen between two reads of the fleet -----------------------------

// What a box looked like at one read: its light, and which kinds of trouble it
// had. Enough to tell a change from the same state read twice.
export interface BoxSnapshot {
  name: string
  kinds: IssueKind[]
}

export const snapshotOf = (boxes: readonly BoxHealth[]): Map<string, BoxSnapshot> =>
  new Map(boxes.map(box => [box.id, { name: box.name, kinds: box.issues.map(issue => issue.kind) }]))

// The words for a trouble that has cleared.
const CLEARED: Record<IssueKind, string> = {
  offline: 'answering again',
  stale: 'heartbeat back',
  memory: 'memory back above the reserve',
  hot: 'GPU temperature back in range',
  failed: 'no failed model any more',
  joining: 'joined the fleet',
}

// Every change between two reads, as events at the time of the second. The
// first read of a console has nothing to compare against, so it yields none:
// a box that was already tight when the page opened is the banner's business,
// not news.
export function healthChanges(
  previous: ReadonlyMap<string, BoxSnapshot> | null,
  next: readonly BoxHealth[],
  at: number,
): FleetEvent[] {
  if (previous === null) return []
  const events: FleetEvent[] = []
  const present = new Set<string>()
  for (const box of next) {
    present.add(box.id)
    const before = previous.get(box.id)
    if (!before) {
      events.push({ id: `seen:${box.id}:${at}`, at, tone: 'ok', subject: box.name, text: 'joined the fleet', boxID: box.id, boxName: box.name })
      continue
    }
    for (const issue of box.issues) {
      if (before.kinds.includes(issue.kind)) continue
      events.push({
        id: `issue:${box.id}:${issue.kind}:${at}`, at, tone: issue.light, subject: box.name, text: issue.text,
        boxID: box.id, boxName: box.name, issue: issue.kind,
      })
    }
    for (const kind of before.kinds) {
      if (box.issues.some(issue => issue.kind === kind)) continue
      events.push({
        id: `clear:${box.id}:${kind}:${at}`, at, tone: 'ok', subject: box.name, text: CLEARED[kind],
        boxID: box.id, boxName: box.name,
      })
    }
  }
  for (const [id, before] of previous) {
    if (present.has(id)) continue
    events.push({ id: `gone:${id}:${at}`, at, tone: 'warn', subject: before.name, text: 'left the fleet' })
  }
  return events
}

// The column itself: newest first, at most limit.
export function latestEvents(lists: readonly (readonly FleetEvent[])[], limit = 8): FleetEvent[] {
  return lists.flat().sort((left, right) => right.at - left.at).slice(0, limit)
}

// ---- What is wrong now --------------------------------------------------------

const SEVERITY: Record<EventTone, number> = { fail: 0, warn: 1, busy: 2, ok: 3 }

// Every trouble a box has right now, from the same health the banner reads,
// as events that are still going on: failures first, then the rest, in the
// order the boxes are drawn.
export function alertEvents(boxes: readonly BoxHealth[], at: number): FleetEvent[] {
  const alerts: FleetEvent[] = boxes.flatMap(box => box.issues.map(issue => ({
    id: `alert:${box.id}:${issue.kind}`, at, tone: issue.light, subject: box.name, text: issue.text,
    boxID: box.id, boxName: box.name, issue: issue.kind, ongoing: true,
  })))
  return alerts.sort((left, right) => SEVERITY[left.tone] - SEVERITY[right.tone])
}

// The timeline: what is wrong now, then what happened, newest first, at most
// limit in all. A change the console saw that is still how the box is now is
// told once, as the alert that stands for it.
export function timeline(alerts: readonly FleetEvent[], history: readonly (readonly FleetEvent[])[], limit = 8): FleetEvent[] {
  const standing = new Set(alerts.map(alert => `${alert.boxID}|${alert.issue}`))
  const past = history.flat().filter(event => !(event.issue && standing.has(`${event.boxID}|${event.issue}`)))
  return [...alerts, ...latestEvents([past], limit)].slice(0, limit)
}

// The first line of an event. A job names its model and what happened to it
// ("GLM 5.3 Flash started"); a box's own news is about the box, which the
// second line already names, so it says only what happened.
export function eventTitle(event: FleetEvent): { subject: string; text: string } {
  if (event.boxName && event.subject === event.boxName) {
    return { subject: '', text: event.text.charAt(0).toUpperCase() + event.text.slice(1) }
  }
  return { subject: event.subject, text: event.text }
}

// ---- The changes this console has seen --------------------------------------

// Kept for as long as the console is open, like the meters in telemetry.ts.
// App records every read of the fleet; Monitor draws what came of it.

const KEPT = 40
let last: Map<string, BoxSnapshot> | null = null
let seen: FleetEvent[] = []
let revision = 0
const listeners = new Set<() => void>()

export function recordHealth(boxes: readonly BoxHealth[], at = Date.now()): void {
  const changes = healthChanges(last, boxes, at)
  last = snapshotOf(boxes)
  if (changes.length === 0) return
  seen = [...changes, ...seen].slice(0, KEPT)
  revision += 1
  for (const listener of listeners) listener()
}

export const seenEvents = (): readonly FleetEvent[] => seen

export function subscribeEvents(listener: () => void): () => void {
  listeners.add(listener)
  return () => {
    listeners.delete(listener)
  }
}

export const eventsRevision = (): number => revision

// For the tests only.
export function resetEvents(): void {
  last = null
  seen = []
  revision = 0
}
