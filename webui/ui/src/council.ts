// Council: one question, every serving chat model answers it, each of them
// ranks the answers without knowing who wrote which, and the model they
// ranked best writes the final answer.
//
// Everything here is pure. The stage requests, the anonymous labelling and
// its per-reviewer shuffle, the lenient ranking parse, the aggregate that
// picks the chairman, and every fallback decision live here; Playground.tsx
// owns the fetches, the rendering and the abort. runCouncil takes the
// transport as an argument for the same reason the Go side takes seams: the
// whole run is exercised in tests without a network.

export interface CouncilModel {
  // The id the /v1 endpoint routes on, and the name a person reads.
  id: string
  name: string
}

export interface ChatMessage {
  role: 'user' | 'assistant'
  content: string
}

export interface ChatRequest {
  model: string
  messages: ChatMessage[]
}

export interface CouncilAnswer {
  model: CouncilModel
  text: string
}

// One reviewer's verdict, best first. Reviewers who produced nothing readable
// are absent rather than present and empty.
export interface CouncilRanking {
  reviewer: CouncilModel
  order: CouncilModel[]
}

// What the byline and the work panel render from, in display form: names
// only, because nothing downstream needs the ids again.
export interface CouncilRecord {
  chairman: string
  winner: string
  reviewed: number
  answers: { model: string; text: string }[]
  rankings: { reviewer: string; order: string[] }[]
  // False when the chairman never produced an answer and the winner's own
  // stage one answer is standing in for it.
  finished: boolean
}

export const COUNCIL_STAGES = ['answering', 'reviewing', 'writing'] as const
export type CouncilStage = (typeof COUNCIL_STAGES)[number]

// Stages already passed are struck through, the current one is in ink, the
// rest are waiting.
export const stageState = (stage: CouncilStage, current: CouncilStage): 'done' | 'now' | '' => {
  const at = COUNCIL_STAGES.indexOf(current)
  const here = COUNCIL_STAGES.indexOf(stage)
  return here < at ? 'done' : here === at ? 'now' : ''
}

// A model the console can actually put in front of the council: serving right
// now, addressable by an id, named, and answering text rather than pixels. A
// media runtime speaks nothing OpenAI-compatible (ADR 0007), so it is not a
// council member and never counts towards the two that open the option.
export interface ServingCandidate {
  serving: boolean
  id?: string
  name?: string
  media?: boolean
}

export function servingChatModels(candidates: ServingCandidate[]): CouncilModel[] {
  const seen = new Set<string>()
  const models: CouncilModel[] = []
  for (const candidate of candidates) {
    if (!candidate.serving || candidate.media) continue
    if (!candidate.id || !candidate.name) continue
    if (seen.has(candidate.id)) continue
    seen.add(candidate.id)
    models.push({ id: candidate.id, name: candidate.name })
  }
  // Name order is the order the picker shows and the order that breaks a tie
  // in the aggregate, so both come from one place.
  return models.sort((left, right) => left.name.localeCompare(right.name))
}

// Below two models there is no council to offer, and the picker stays exactly
// as it is with a single model serving.
export const councilOffered = (models: CouncilModel[]): boolean => models.length >= 2

// ---- Deterministic shuffling -------------------------------------------------
// The reviewers must not all see the answers in the same order, or position
// alone would decide the ranking. The order is drawn from the turn rather
// than from Math.random, so a test can pin exactly what each reviewer saw.

export function seedFrom(...parts: (string | number)[]): number {
  let hash = 0x811c9dc5
  for (const part of parts) {
    for (const character of String(part)) {
      hash ^= character.charCodeAt(0)
      hash = Math.imul(hash, 0x01000193) >>> 0
    }
    hash = Math.imul(hash ^ 0x2f, 0x01000193) >>> 0
  }
  return hash >>> 0
}

const randomFrom = (seed: number): (() => number) => {
  let state = (seed || 1) >>> 0
  return () => {
    state = (state + 0x6d2b79f5) >>> 0
    let value = Math.imul(state ^ (state >>> 15), 1 | state)
    value = (value + Math.imul(value ^ (value >>> 7), 61 | value)) ^ value
    return ((value ^ (value >>> 14)) >>> 0) / 4294967296
  }
}

export function shuffled<T>(items: readonly T[], seed: number): T[] {
  const next = randomFrom(seed)
  const order = [...items]
  for (let index = order.length - 1; index > 0; index -= 1) {
    const pick = Math.floor(next() * (index + 1))
    const held = order[index]
    order[index] = order[pick]
    order[pick] = held
  }
  return order
}

// A, B, ... Z, AA, AB. Spreadsheet letters rather than numbers, so a label
// can never be read as a rank.
export function answerLabel(index: number): string {
  let label = ''
  let value = index
  for (;;) {
    label = String.fromCharCode(65 + (value % 26)) + label
    value = Math.floor(value / 26) - 1
    if (value < 0) return label
  }
}

// What one reviewer is shown: every answer, its own included, relabelled in
// an order drawn for that reviewer alone.
export interface ReviewPacket {
  reviewer: CouncilModel
  entries: { label: string; answer: CouncilAnswer }[]
}

export function reviewPackets(answers: CouncilAnswer[], seed: number): ReviewPacket[] {
  return answers.map(({ model }) => ({
    reviewer: model,
    entries: shuffled(answers, seedFrom(seed, model.id)).map((answer, index) => ({
      label: answerLabel(index),
      answer,
    })),
  }))
}

// ---- Prompts -----------------------------------------------------------------
// Kept here as plain strings so a test can read them. Both forbid inventing
// anything that is not in the material being judged or synthesized, which is
// the same honesty rule the rest of the console holds itself to.

export function reviewPrompt(question: string, packet: ReviewPacket): string {
  const answers = packet.entries
    .map(entry => `Answer ${entry.label}:\n${entry.answer.text}`)
    .join('\n\n')
  const example = packet.entries.map((entry, index) => `${index + 1}. Answer ${entry.label}: reason`).join('\n')
  return [
    'This question was put to several models:',
    question,
    'Here are the answers they gave. They are anonymous, and one of them is your own.',
    answers,
    'Rank every answer from best to worst, one per line, best first, in this form:',
    example,
    'Give one short reason for each. Judge only what the answers say. Do not add facts that are not in them, and write nothing after the list.',
  ].join('\n\n')
}

export function chairmanPrompt(question: string, answers: CouncilAnswer[], rankings: CouncilRanking[]): string {
  const drafts = answers.map(answer => `${answer.model.name}:\n${answer.text}`).join('\n\n')
  const verdicts = rankings.length > 0
    ? rankings
      .map(ranking => `${ranking.reviewer.name}: ${ranking.order.map((model, index) => `${index + 1} ${model.name}`).join(', ')}`)
      .join('\n')
    : 'No model produced a readable ranking.'
  return [
    'Several models answered the same question, then ranked each other without names. You write the answer the person reads.',
    'The question:',
    question,
    'The answers:',
    drafts,
    'How the models ranked them, best first:',
    verdicts,
    'Write the final answer to the question in your own words, using only what these answers contain. Do not invent facts, numbers, names or sources that are not in them. Where they disagree, say which way you come down and why. Do not mention this process, the rankings or the other models.',
  ].join('\n\n')
}

// ---- Requests ----------------------------------------------------------------

export const draftRequest = (model: CouncilModel, history: ChatMessage[], question: string): ChatRequest => ({
  model: model.id,
  messages: [...history, { role: 'user', content: question }],
})

// A reviewer is handed the question and the answers in one message and
// nothing else: the conversation so far is already inside the answers, and a
// second copy of it only gives the reviewer more to drift into.
export const reviewRequest = (packet: ReviewPacket, question: string): ChatRequest => ({
  model: packet.reviewer.id,
  messages: [{ role: 'user', content: reviewPrompt(question, packet) }],
})

// The chairman keeps the conversation history: a follow-up question means
// nothing without the turns before it.
export const chairmanRequest = (
  chairman: CouncilModel,
  history: ChatMessage[],
  question: string,
  answers: CouncilAnswer[],
  rankings: CouncilRanking[],
): ChatRequest => ({
  model: chairman.id,
  messages: [...history, { role: 'user', content: chairmanPrompt(question, answers, rankings) }],
})

// ---- Reading a ranking -------------------------------------------------------
// Models rank in whatever shape they feel like. Three readings are tried in
// order, and a reviewer none of them can read is simply absent from the
// rankings and from the aggregate. It is never an error: a review that cannot
// be parsed costs a vote, not the turn.

export function parseRanking(text: string, labels: string[]): string[] {
  const known = new Set(labels)
  const order: string[] = []
  const push = (label: string) => {
    const clean = label.toUpperCase()
    if (known.has(clean) && !order.includes(clean)) order.push(clean)
  }

  // "1. Answer C: clearest" and "2) B - shorter", numbered by the model
  // itself, which is the shape the prompt asks for.
  const numbered = [...text.matchAll(/^[^\S\n]*(\d+)[.)]?[^\S\n]*(?:answer[^\S\n]*)?([A-Za-z]{1,2})\b/gim)]
    .map(match => ({ rank: Number(match[1]), label: match[2].toUpperCase() }))
    .filter(entry => known.has(entry.label))
  if (numbered.length > 0) {
    for (const entry of [...numbered].sort((left, right) => left.rank - right.rank)) push(entry.label)
    return order
  }

  // "Answer B is best, then Answer A", in the order the labels appear.
  const named = [...text.matchAll(/answer[^\S\n]*([A-Za-z]{1,2})\b/gi)]
  if (named.length > 0) {
    for (const match of named) push(match[1])
    return order
  }

  // Bare letters, uppercase only: a lowercase "a" is the English article far
  // more often than it is a verdict.
  for (const match of text.matchAll(/\b([A-Z]{1,2})\b/g)) push(match[1])
  return order
}

export function readRanking(packet: ReviewPacket, text: string): CouncilRanking | null {
  const byLabel = new Map(packet.entries.map(entry => [entry.label, entry.answer.model]))
  const order = parseRanking(text, [...byLabel.keys()])
    .map(label => byLabel.get(label))
    .filter((model): model is CouncilModel => Boolean(model))
  if (order.length === 0) return null
  return { reviewer: packet.reviewer, order }
}

// ---- The aggregate -----------------------------------------------------------

export interface CouncilStanding {
  model: CouncilModel
  // Mean position across the reviewers that ranked this model at all. Null
  // means no reviewer placed it, which sorts last without pretending to be a
  // number.
  score: number | null
}

export function standings(models: CouncilModel[], rankings: CouncilRanking[]): CouncilStanding[] {
  const rows = models.map(model => {
    const positions = rankings
      .map(ranking => ranking.order.findIndex(ranked => ranked.id === model.id))
      .filter(position => position >= 0)
      .map(position => position + 1)
    const score = positions.length > 0
      ? positions.reduce((total, position) => total + position, 0) / positions.length
      : null
    return { model, score }
  })
  return rows.sort((left, right) => {
    if (left.score === null && right.score === null) return left.model.name.localeCompare(right.model.name)
    if (left.score === null) return 1
    if (right.score === null) return -1
    return left.score - right.score || left.model.name.localeCompare(right.model.name)
  })
}

// Zero configuration: the chairman is the model the others ranked best. With
// no readable ranking at all every model ties and the name order decides,
// which is a rule rather than a coin toss.
export const chairmanOf = (models: CouncilModel[], rankings: CouncilRanking[]): CouncilModel =>
  standings(models, rankings)[0].model

export function councilRecord(
  answers: CouncilAnswer[],
  rankings: CouncilRanking[],
  winner: CouncilModel,
  finished: boolean,
): CouncilRecord {
  return {
    chairman: winner.name,
    winner: winner.name,
    reviewed: answers.length,
    answers: answers.map(answer => ({ model: answer.model.name, text: answer.text })),
    rankings: rankings.map(ranking => ({
      reviewer: ranking.reviewer.name,
      order: ranking.order.map(model => model.name),
    })),
    finished,
  }
}

// The byline under a final answer, split so the model name can be set in the
// mono face the rest of the console names models in.
export const councilByline = (record: CouncilRecord): { model: string; text: string } => ({
  model: record.chairman,
  text: record.finished ? ` wrote this from ${record.reviewed} reviewed answers` : ' · the council could not finish',
})

// ---- History -----------------------------------------------------------------

export interface TranscriptEntry {
  role: 'user' | 'assistant'
  content: string
  council?: CouncilRecord
}

// What the models are sent: the questions and the final answers, nothing
// else. Drafts and rankings are working material and never become context,
// so a later turn is never argued at by three earlier voices.
export const councilHistory = (entries: TranscriptEntry[]): ChatMessage[] =>
  entries
    .filter(entry => entry.content.trim().length > 0)
    .map(entry => ({ role: entry.role, content: entry.content }))

// ---- The run -----------------------------------------------------------------

export interface CouncilDelta {
  text?: string
  thinking?: string
  // The answer so far is wrong and this is the whole of it: a retried
  // chairman, or the winner's own answer standing in for one.
  replace?: boolean
}

export interface CouncilTransport {
  // One complete answer, with nothing shown while it is produced.
  answer(request: ChatRequest): Promise<string>
  // The final answer, token by token; resolves with the whole of it.
  stream(request: ChatRequest, onDelta: (delta: CouncilDelta) => void): Promise<string>
}

export interface CouncilRun {
  question: string
  history: ChatMessage[]
  models: CouncilModel[]
  seed: number
}

export interface CouncilEvents {
  onStage(stage: CouncilStage): void
  onDelta(delta: CouncilDelta): void
}

export interface CouncilOutcome {
  text: string
  // Absent when the turn produced a plain answer after all: fewer than two
  // models answered, so there was nothing to review.
  council?: CouncilRecord
}

// The same wording a failed plain turn falls back to, so a council turn that
// loses every model says no more than the console already knows.
const REQUEST_FAILED = 'The request failed.'

const isAbort = (problem: unknown): boolean =>
  typeof problem === 'object' && problem !== null && (problem as { name?: unknown }).name === 'AbortError'

export async function runCouncil(
  run: CouncilRun,
  transport: CouncilTransport,
  events: CouncilEvents,
): Promise<CouncilOutcome> {
  events.onStage('answering')
  const drafted = await Promise.allSettled(
    run.models.map(model => transport.answer(draftRequest(model, run.history, run.question))),
  )
  const answers: CouncilAnswer[] = []
  let failure: unknown = null
  drafted.forEach((result, index) => {
    if (result.status === 'fulfilled' && result.value.trim().length > 0) {
      answers.push({ model: run.models[index], text: result.value.trim() })
      return
    }
    if (failure === null) failure = result.status === 'rejected' ? result.reason : new Error(REQUEST_FAILED)
  })

  // A model that never answered is dropped without a word as long as two
  // answers remain. Below that there is nothing to review, so the turn is a
  // plain answer from the model that did answer.
  if (answers.length < 2) {
    if (answers.length === 1) {
      events.onDelta({ text: answers[0].text, replace: true })
      return { text: answers[0].text }
    }
    throw failure instanceof Error ? failure : new Error(REQUEST_FAILED)
  }

  events.onStage('reviewing')
  const packets = reviewPackets(answers, run.seed)
  const reviewed = await Promise.allSettled(packets.map(packet => transport.answer(reviewRequest(packet, run.question))))
  const rankings: CouncilRanking[] = []
  reviewed.forEach((result, index) => {
    if (result.status !== 'fulfilled') return
    const ranking = readRanking(packets[index], result.value)
    if (ranking) rankings.push(ranking)
  })

  const winner = chairmanOf(answers.map(answer => answer.model), rankings)
  events.onStage('writing')
  const request = chairmanRequest(winner, run.history, run.question, answers, rankings)
  const write = async (): Promise<string> => {
    const text = await transport.stream(request, events.onDelta)
    if (text.trim().length === 0) throw new Error(REQUEST_FAILED)
    return text
  }
  try {
    return { text: await write(), council: councilRecord(answers, rankings, winner, true) }
  } catch (problem) {
    if (isAbort(problem)) throw problem
    events.onDelta({ text: '', thinking: '', replace: true })
    try {
      return { text: await write(), council: councilRecord(answers, rankings, winner, true) }
    } catch (second) {
      if (isAbort(second)) throw second
      // The answer is never lost to a chairman that will not write: the
      // model the others ranked best already answered this question.
      const standIn = answers.find(answer => answer.model.id === winner.id)?.text ?? answers[0].text
      events.onDelta({ text: standIn, thinking: '', replace: true })
      return { text: standIn, council: councilRecord(answers, rankings, winner, false) }
    }
  }
}
