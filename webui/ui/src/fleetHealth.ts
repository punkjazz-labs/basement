import { formatBytes } from './api'
import type { BoxHeadroom } from './headroom'
import { HEALTH_WORD, type UnitHealth } from './units'

// How the fleet is doing, in one phrase and one light per box. Models, Sparks
// and Monitor all open with this line, so the owner reads the state of every
// box before any number. It counts problems only: a box with nothing wrong
// adds nothing to the sentence, and a fleet with nothing wrong says so in
// three words.
//
// Pure: no DOM, no fetch, no React. App reads the boxes once and every screen
// draws the same answer.

// A heartbeat older than this is stale. It mirrors the manager's own bound
// (HeartbeatFreshness in internal/fleet/heartbeat.go), which fleetModels.ts
// already repeats for the same reason.
export const STALE_MS = 30_000

// The console's own lines for a hot GPU. They colour a reading; they are not
// a claim about where the chip throttles, and no copy says they are.
export const HOT_C = 85
export const VERY_HOT_C = 95

// ok is a box with nothing to say. warn wants a look: memory under the
// reserve, a hot GPU, a heartbeat gone quiet. fail is a box that cannot be
// reached, a model that failed on it, or memory all but gone.
export type Light = 'ok' | 'warn' | 'fail'

export type IssueKind = 'offline' | 'failed' | 'memory' | 'hot' | 'stale' | 'joining'

export interface Issue {
  kind: IssueKind
  light: Light
  // A short phrase that follows the box's name: "memory tight (7.2 GB free,
  // loads refused)", "not answering".
  text: string
}

// What the console knows about one box, before any judgement.
export interface BoxReading {
  id: string
  name: string
  // The box this console runs on. Its meters are its own, read every few
  // seconds, so it has no heartbeat to age.
  self: boolean
  // Whether the fleet can still act on this box.
  answering: boolean
  // Still being admitted to the fleet.
  joining?: boolean
  // How long ago this box last reported, in milliseconds. Absent when it has
  // never reported, or for this box itself.
  ageMs?: number
  headroom: BoxHeadroom
  temperature?: number
  // The name of a model this box reports as failed, if any.
  failedModel?: string
}

export interface BoxHealth extends BoxReading {
  light: Light
  // A heartbeat older than STALE_MS. The dot is drawn hollow.
  stale: boolean
  // Below the reserve: a new model cannot load here beside what the box
  // already holds. A switch that stops the current model first may still fit,
  // which is why the words are "loads refused" and not "full".
  loadsRefused: boolean
  // Worst first.
  issues: Issue[]
}

const RANK: Record<Light, number> = { ok: 0, warn: 1, fail: 2 }
const worse = (left: Light, right: Light): Light => (RANK[right] > RANK[left] ? right : left)

// A GPU temperature as a light: muted while in range.
export function temperatureLight(celsius?: number): Light {
  if (!celsius) return 'ok'
  if (celsius >= VERY_HOT_C) return 'fail'
  if (celsius >= HOT_C) return 'warn'
  return 'ok'
}

// Memory as a light, from the same headroom every bar draws.
export function memoryLight(headroom: BoxHeadroom): Light {
  if (headroom.state === 'full') return 'fail'
  if (headroom.state === 'tight') return 'warn'
  return 'ok'
}

// Whether a new load beside what the box holds is refused by the memory rule.
// The rule wants the reserve still free after the model lands, so a box
// already under its reserve refuses any load that frees nothing first.
export const loadsRefused = (headroom: BoxHeadroom): boolean =>
  headroom.state === 'tight' || headroom.state === 'full'

// "45 s", "3 min", "2 h": how long a box has been quiet, said short.
export function shortAge(ms: number): string {
  const seconds = Math.max(0, Math.round(ms / 1000))
  if (seconds < 60) return `${seconds} s`
  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) return `${minutes} min`
  const hours = Math.floor(minutes / 60)
  if (hours < 48) return `${hours} h`
  return `${Math.floor(hours / 24)} d`
}

export function boxHealth(reading: BoxReading): BoxHealth {
  const issues: Issue[] = []
  const stale = !reading.self && reading.ageMs !== undefined && reading.ageMs > STALE_MS
  if (reading.joining) {
    issues.push({ kind: 'joining', light: 'warn', text: 'joining the fleet' })
  } else if (!reading.answering) {
    issues.push({ kind: 'offline', light: 'fail', text: 'not answering' })
  } else if (stale) {
    issues.push({ kind: 'stale', light: 'warn', text: `no heartbeat for ${shortAge(reading.ageMs ?? 0)}` })
  }
  if (reading.failedModel) {
    issues.push({ kind: 'failed', light: 'fail', text: `${reading.failedModel} failed` })
  }
  const room = reading.headroom
  const refused = loadsRefused(room)
  if (refused) {
    const free = formatBytes(room.box.availableBytes)
    issues.push({
      kind: 'memory',
      light: memoryLight(room),
      text: room.state === 'full'
        ? `out of memory (${free} free, loads refused)`
        : `memory tight (${free} free, loads refused)`,
    })
  }
  const heat = temperatureLight(reading.temperature)
  if (heat !== 'ok') issues.push({ kind: 'hot', light: heat, text: `GPU hot (${reading.temperature} °C)` })
  issues.sort((left, right) => RANK[right.light] - RANK[left.light])
  return {
    ...reading,
    light: issues.reduce<Light>((light, issue) => worse(light, issue.light), 'ok'),
    stale,
    loadsRefused: refused,
    issues,
  }
}

// ---- The fleet in one phrase ------------------------------------------------

export interface FleetHealthSummary {
  total: number
  // Every box with something to say, worst first.
  attention: BoxHealth[]
  light: Light
  text: string
}

const PHRASES_SHOWN = 2

// Among boxes with the same light, the one that cannot be reached leads, then
// a failed model, then memory (least free first), then heat, then a quiet
// heartbeat.
const KIND_RANK: Record<IssueKind, number> = { offline: 0, failed: 1, memory: 2, hot: 3, stale: 4, joining: 5 }

function attentionOrder(left: BoxHealth, right: BoxHealth): number {
  const light = RANK[right.light] - RANK[left.light]
  if (light !== 0) return light
  const kind = KIND_RANK[left.issues[0].kind] - KIND_RANK[right.issues[0].kind]
  if (kind !== 0) return kind
  return (left.headroom.box.availableBytes ?? Infinity) - (right.headroom.box.availableBytes ?? Infinity)
}

// "All 6 boxes healthy", or "1 needs attention · edgexpert-5e12 memory tight
// (7.2 GB free, loads refused)". countWord is how the fleet counts its boxes
// ("6 boxes", "4 Sparks"), which units.ts already knows how to say.
export function summarize(boxes: readonly BoxHealth[], countWord = `${boxes.length} boxes`): FleetHealthSummary {
  const attention = boxes.filter(box => box.issues.length > 0).sort(attentionOrder)
  const light = attention.reduce<Light>((worst, box) => worse(worst, box.light), 'ok')
  if (boxes.length === 0) return { total: 0, attention, light, text: '' }
  if (attention.length === 0) {
    const text = boxes.length === 1 ? `${boxes[0].name} is healthy` : `All ${countWord} healthy`
    return { total: boxes.length, attention, light, text }
  }
  const phrases = attention.slice(0, PHRASES_SHOWN).map(box => `${box.name} ${box.issues[0].text}`)
  const more = attention.length - PHRASES_SHOWN
  if (more > 0) phrases.push(`${more} more`)
  const head = boxes.length === 1
    ? ''
    : `${attention.length} ${attention.length === 1 ? 'needs' : 'need'} attention · `
  return { total: boxes.length, attention, light, text: `${head}${phrases.join(' · ')}` }
}

// ---- The banner -------------------------------------------------------------

export interface Banner {
  light: Light
  title: string
  body: string
  // The box the one action opens.
  boxID: string
  boxName: string
}

// What the banner says about the box that needs attention most. Nothing at
// all while every box is calm: the banner is never drawn to say all is well.
export function bannerFor(summary: FleetHealthSummary): Banner | null {
  const box = summary.attention[0]
  if (!box) return null
  const others = summary.attention.length - 1
  const tail = others > 0 ? ` ${others} more ${others === 1 ? 'box needs' : 'boxes need'} attention.` : ''
  const { title, body } = describe(box)
  return { light: summary.light === 'fail' ? 'fail' : box.issues[0].light, title, body: `${body}${tail}`.trim(), boxID: box.id, boxName: box.name }
}

// One line of the banner for every box that needs attention, worst first.
// Each names its own box, so each carries its own way in.
export const bannerItems = (summary: FleetHealthSummary): Banner[] =>
  summary.attention.map(box => ({ light: box.light, ...describe(box), boxID: box.id, boxName: box.name }))

// A box's worst trouble as a title and a sentence about what it means.
function describe(box: BoxHealth): { title: string; body: string } {
  const issue = box.issues[0]
  const room = box.headroom
  let title = `${box.name} ${issue.text}`
  let body = ''
  switch (issue.kind) {
    case 'memory':
      title = `${box.name} is ${room.state === 'full' ? 'out of memory' : 'tight on memory'}`
      body = `${formatBytes(room.box.availableBytes)} free, under the ${formatBytes(room.reserve)} it keeps free. ` +
        'New models cannot load there until the one it runs stops.'
      break
    case 'offline':
      title = `${box.name} is not answering`
      body = box.ageMs !== undefined
        ? `Last heard from ${shortAge(box.ageMs)} ago. Its models cannot be started or stopped from here.`
        : 'Its models cannot be started or stopped from here.'
      break
    case 'stale':
      title = `${box.name} has gone quiet`
      body = `No heartbeat for ${shortAge(box.ageMs ?? 0)}. What it shows may be out of date.`
      break
    case 'failed':
      title = `${issue.text.replace(/ failed$/, '')} failed on ${box.name}`
      body = 'Open the box to see what it runs now. Activity has the failed step.'
      break
    case 'hot':
      title = `${box.name} GPU is hot`
      body = `${box.temperature} °C now.`
      break
    case 'joining':
      title = `${box.name} is joining the fleet`
      body = 'It answers for nothing until it has joined.'
      break
  }
  return { title, body }
}

// ---- A unit, in one word --------------------------------------------------------

export interface UnitStatus {
  word: string
  // The .health class it wears: plain, amber or red.
  tone: UnitHealth
}

// How a unit is doing, its own state read together with its boxes'. A unit
// whose members all answer is still not healthy while one of them refuses
// loads or runs hot: the group header must not say Healthy over rows that
// say otherwise.
export function unitStatus(health: UnitHealth, members: readonly (BoxHealth | undefined)[]): UnitStatus {
  const issues = members.flatMap(member => member?.issues ?? [])
  const worst = members.reduce<Light>((light, member) => (member ? worse(light, member.light) : light), 'ok')
  if (health === 'failed') return { word: HEALTH_WORD.failed, tone: 'failed' }
  if (health === 'degraded') return { word: HEALTH_WORD.degraded, tone: worst === 'fail' ? 'failed' : 'degraded' }
  if (issues.length === 0) return { word: HEALTH_WORD.healthy, tone: 'healthy' }
  const word = issues.every(issue => issue.kind === 'memory') ? 'Memory tight' : 'Needs attention'
  return { word, tone: worst === 'fail' ? 'failed' : 'degraded' }
}
