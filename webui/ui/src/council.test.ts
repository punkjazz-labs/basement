import { describe, expect, it } from 'vitest'
import {
  type ChatRequest, type CouncilDelta, type CouncilModel, type CouncilStage, type CouncilTransport,
  answerLabel, chairmanOf, chairmanPrompt, councilByline, councilHistory, councilOffered, councilRecord,
  draftRequest, parseRanking, readRanking, reviewPackets, reviewPrompt, runCouncil, seedFrom,
  servingChatModels, shuffled, stageState, standings,
} from './council'

const model = (id: string, name: string): CouncilModel => ({ id, name })
const alpha = model('alpha-1', 'alpha one')
const beta = model('beta-1', 'beta one')
const gamma = model('gamma-1', 'gamma one')
const COUNCIL = [alpha, beta, gamma]

const SEED = 4242

// The review prompt's first line, which is how the fake transport tells a
// review request from a draft without depending on wording anywhere else.
const REVIEW_MARK = 'This question was put to several models:'

const stageOf = (request: ChatRequest): 'draft' | 'review' | 'chairman' => {
  const last = request.messages[request.messages.length - 1].content
  if (last.startsWith(REVIEW_MARK)) return 'review'
  if (last.startsWith('Several models answered the same question')) return 'chairman'
  return 'draft'
}

interface FakeOptions {
  drafts?: Record<string, string | Error>
  reviews?: Record<string, string | Error>
  // One entry per chairman attempt, in order.
  chairman?: (string | Error)[]
}

const settle = (value: string | Error | undefined, fallback: string): Promise<string> =>
  value instanceof Error ? Promise.reject(value) : Promise.resolve(value ?? fallback)

const fake = (options: FakeOptions) => {
  const requests: ChatRequest[] = []
  const deltas: CouncilDelta[] = []
  const stages: CouncilStage[] = []
  let attempt = 0
  const transport: CouncilTransport = {
    answer: request => {
      requests.push(request)
      return stageOf(request) === 'review'
        ? settle(options.reviews?.[request.model], '1. Answer A: clear\n2. Answer B: fine')
        : settle(options.drafts?.[request.model], `answer from ${request.model}`)
    },
    stream: async (request, onDelta) => {
      requests.push(request)
      const answer = options.chairman?.[attempt]
      attempt += 1
      if (answer instanceof Error) {
        onDelta({ text: 'half a ' })
        throw answer
      }
      const text = answer ?? 'the final answer'
      onDelta({ text })
      return text
    },
  }
  const events = {
    onStage: (stage: CouncilStage) => stages.push(stage),
    onDelta: (delta: CouncilDelta) => deltas.push(delta),
  }
  return { transport, events, requests, deltas, stages, chairmanAttempts: () => attempt }
}

const run = (models = COUNCIL, question = 'flat or per seat?', history: { role: 'user' | 'assistant'; content: string }[] = []) =>
  ({ question, history, models, seed: SEED })

describe('which models the council can be offered from', () => {
  it('takes serving text models only, in name order, without repeats', () => {
    const models = servingChatModels([
      { serving: true, id: 'gamma-1', name: 'gamma one' },
      { serving: true, id: 'video-1', name: 'video one', media: true },
      { serving: false, id: 'stopped-1', name: 'stopped one' },
      { serving: true, id: 'alpha-1', name: 'alpha one' },
      { serving: true, id: 'alpha-1', name: 'alpha one' },
      { serving: true, name: 'no id at all' },
    ])
    expect(models).toEqual([alpha, gamma])
  })

  it('is offered from two models and not from one or none', () => {
    expect(councilOffered([])).toBe(false)
    expect(councilOffered([alpha])).toBe(false)
    expect(councilOffered([alpha, beta])).toBe(true)
  })
})

describe('anonymous labelling', () => {
  it('labels by letter past the alphabet', () => {
    expect([0, 1, 25, 26, 27].map(answerLabel)).toEqual(['A', 'B', 'Z', 'AA', 'AB'])
  })

  it('shuffles the same way for the same seed and differently for another', () => {
    const items = ['a', 'b', 'c', 'd', 'e']
    expect(shuffled(items, 7)).toEqual(shuffled(items, 7))
    expect(shuffled(items, 7)).not.toEqual(shuffled(items, 8))
    expect([...shuffled(items, 7)].sort()).toEqual(items)
  })

  it('shows every reviewer every answer, its own included, in its own order', () => {
    const answers = COUNCIL.map(item => ({ model: item, text: `answer from ${item.name}` }))
    const packets = reviewPackets(answers, SEED)
    expect(packets.map(packet => packet.reviewer)).toEqual(COUNCIL)
    for (const packet of packets) {
      expect(packet.entries.map(entry => entry.label)).toEqual(['A', 'B', 'C'])
      expect(packet.entries.map(entry => entry.answer.model.id).sort()).toEqual(['alpha-1', 'beta-1', 'gamma-1'])
    }
    const orders = packets.map(packet => packet.entries.map(entry => entry.answer.model.id).join())
    expect(new Set(orders).size).toBeGreaterThan(1)
    expect(reviewPackets(answers, SEED)).toEqual(packets)
  })

  it('never names a model in what a reviewer reads', () => {
    const answers = COUNCIL.map(item => ({ model: item, text: 'a draft' }))
    const prompt = reviewPrompt('flat or per seat?', reviewPackets(answers, SEED)[0])
    for (const item of COUNCIL) expect(prompt).not.toContain(item.name)
    expect(prompt).toContain('Answer A:')
    expect(prompt).toContain('Do not add facts that are not in them')
  })

  it('gives the chairman the names, the rankings and the honesty rule', () => {
    const answers = [{ model: alpha, text: 'flat' }, { model: beta, text: 'per seat' }]
    const prompt = chairmanPrompt('flat or per seat?', answers, [{ reviewer: beta, order: [alpha, beta] }])
    expect(prompt).toContain('alpha one:')
    expect(prompt).toContain('beta one: 1 alpha one, 2 beta one')
    expect(prompt).toContain('Do not invent facts, numbers, names or sources that are not in them')
  })

  it('tells the chairman plainly when nobody ranked anything', () => {
    const prompt = chairmanPrompt('q', [{ model: alpha, text: 'flat' }], [])
    expect(prompt).toContain('No model produced a readable ranking.')
  })

  it('writes prompts without em dashes or emoji', () => {
    const answers = COUNCIL.map(item => ({ model: item, text: 'a draft' }))
    const prompts = [
      reviewPrompt('q', reviewPackets(answers, SEED)[0]),
      chairmanPrompt('q', answers, []),
    ]
    for (const prompt of prompts) {
      expect(prompt).not.toMatch(/—/)
      expect(prompt).not.toMatch(/\p{Extended_Pictographic}/u)
    }
  })
})

describe('reading a ranking', () => {
  const labels = ['A', 'B', 'C']

  it('reads a numbered list in the shape the prompt asked for', () => {
    expect(parseRanking('1. Answer C: clearest\n2. Answer A: fine\n3. Answer B: vague', labels)).toEqual(['C', 'A', 'B'])
  })

  it('reads a numbered list that dropped the word answer', () => {
    expect(parseRanking('2) B - shorter\n1) C - best\n3) A', labels)).toEqual(['C', 'B', 'A'])
  })

  it('reads a sentence that never numbered anything', () => {
    expect(parseRanking('Answer B is best, then answer A. Answer C adds nothing.', labels)).toEqual(['B', 'A', 'C'])
  })

  it('reads bare capitals and ignores an English article', () => {
    expect(parseRanking('C first because it gives a plan, then B.', labels)).toEqual(['C', 'B'])
  })

  it('ignores labels nobody was shown and repeats', () => {
    expect(parseRanking('1. Answer Z\n2. Answer B\n3. Answer B\n4. Answer A', labels)).toEqual(['B', 'A'])
  })

  it('returns nothing readable rather than a guess', () => {
    expect(parseRanking('I cannot rank these fairly.', labels)).toEqual([])
    expect(parseRanking('', labels)).toEqual([])
  })

  it('maps labels back to the models that reviewer was shown', () => {
    const answers = COUNCIL.map(item => ({ model: item, text: 'draft' }))
    const packet = reviewPackets(answers, SEED)[0]
    const ranking = readRanking(packet, '1. Answer B\n2. Answer C\n3. Answer A')
    expect(ranking?.order.map(item => item.id)).toEqual([
      packet.entries[1].answer.model.id,
      packet.entries[2].answer.model.id,
      packet.entries[0].answer.model.id,
    ])
    expect(readRanking(packet, 'no idea')).toBeNull()
  })
})

describe('the aggregate and the chairman', () => {
  it('picks the model the others placed highest', () => {
    const rankings = [
      { reviewer: alpha, order: [gamma, beta, alpha] },
      { reviewer: beta, order: [gamma, alpha, beta] },
      { reviewer: gamma, order: [beta, gamma, alpha] },
    ]
    expect(standings(COUNCIL, rankings).map(row => row.model.name)).toEqual(['gamma one', 'beta one', 'alpha one'])
    expect(chairmanOf(COUNCIL, rankings).id).toBe(gamma.id)
  })

  it('scores on the reviewers that placed a model, not on the ones that did not', () => {
    const rankings = [
      { reviewer: alpha, order: [beta] },
      { reviewer: beta, order: [gamma, alpha] },
    ]
    expect(standings(COUNCIL, rankings)).toEqual([
      { model: beta, score: 1 },
      { model: gamma, score: 1 },
      { model: alpha, score: 2 },
    ])
  })

  it('breaks a tie on model name', () => {
    const rankings = [
      { reviewer: alpha, order: [beta, gamma] },
      { reviewer: beta, order: [gamma, beta] },
    ]
    expect(chairmanOf([gamma, beta], rankings).name).toBe('beta one')
  })

  it('falls back to name order when no ranking could be read', () => {
    expect(chairmanOf([gamma, beta, alpha], []).name).toBe('alpha one')
    expect(standings([gamma, alpha], []).map(row => row.score)).toEqual([null, null])
  })

  it('puts a model no reviewer placed behind every model one did', () => {
    const rankings = [{ reviewer: alpha, order: [gamma] }]
    expect(standings([alpha, gamma], rankings).map(row => row.model.name)).toEqual(['gamma one', 'alpha one'])
  })
})

describe('the byline', () => {
  it('says who wrote it and how many answers it read', () => {
    const record = councilRecord(
      [{ model: alpha, text: 'a' }, { model: beta, text: 'b' }, { model: gamma, text: 'c' }],
      [],
      alpha,
      true,
    )
    expect(councilByline(record)).toEqual({ model: 'alpha one', text: ' wrote this from 3 reviewed answers' })
  })

  it('says the council could not finish, and nothing more', () => {
    const record = councilRecord([{ model: alpha, text: 'a' }, { model: beta, text: 'b' }], [], beta, false)
    expect(councilByline(record)).toEqual({ model: 'beta one', text: ' · the council could not finish' })
  })
})

describe('the progress line', () => {
  it('strikes what is done, inks what is running and leaves the rest plain', () => {
    expect(['answering', 'reviewing', 'writing'].map(stage => stageState(stage as CouncilStage, 'reviewing')))
      .toEqual(['done', 'now', ''])
  })
})

describe('history', () => {
  it('sends the questions and the final answers, never the drafts or the rankings', () => {
    const record = councilRecord(
      [{ model: alpha, text: 'a draft nobody should see again' }, { model: beta, text: 'another draft' }],
      [{ reviewer: alpha, order: [beta, alpha] }],
      beta,
      true,
    )
    const history = councilHistory([
      { role: 'user', content: 'first question' },
      { role: 'assistant', content: 'the final answer', council: record },
      { role: 'assistant', content: '   ' },
    ])
    expect(history).toEqual([
      { role: 'user', content: 'first question' },
      { role: 'assistant', content: 'the final answer' },
    ])
    expect(JSON.stringify(history)).not.toContain('draft')
  })

  it('puts the history and the question in front of every model', () => {
    const history = [{ role: 'user' as const, content: 'earlier' }, { role: 'assistant' as const, content: 'earlier answer' }]
    expect(draftRequest(alpha, history, 'now what?')).toEqual({
      model: 'alpha-1',
      messages: [...history, { role: 'user', content: 'now what?' }],
    })
  })
})

describe('a council turn', () => {
  it('answers, reviews and writes, and reports who wrote it from what', async () => {
    const harness = fake({})
    const outcome = await runCouncil(run(), harness.transport, harness.events)
    expect(harness.stages).toEqual(['answering', 'reviewing', 'writing'])
    expect(outcome.text).toBe('the final answer')
    expect(outcome.council?.finished).toBe(true)
    expect(outcome.council?.reviewed).toBe(3)
    expect(outcome.council?.rankings).toHaveLength(3)
    expect(outcome.council?.answers.map(answer => answer.model)).toEqual(['alpha one', 'beta one', 'gamma one'])
    expect(outcome.council?.chairman).toBe(outcome.council?.winner)
    expect(harness.requests.filter(request => stageOf(request) === 'draft')).toHaveLength(3)
    expect(harness.requests.filter(request => stageOf(request) === 'review')).toHaveLength(3)
  })

  it('never sends a draft or a ranking back as conversation history', async () => {
    const harness = fake({})
    await runCouncil(run(COUNCIL, 'flat or per seat?', [{ role: 'user', content: 'earlier' }]), harness.transport, harness.events)
    const chairman = harness.requests.find(request => stageOf(request) === 'chairman')
    expect(chairman?.messages.slice(0, -1)).toEqual([{ role: 'user', content: 'earlier' }])
  })

  it('drops a model that failed as long as two answers remain', async () => {
    const harness = fake({ drafts: { 'gamma-1': new Error('gamma fell over') } })
    const outcome = await runCouncil(run(), harness.transport, harness.events)
    expect(outcome.council?.reviewed).toBe(2)
    expect(outcome.council?.answers.map(answer => answer.model)).toEqual(['alpha one', 'beta one'])
    expect(outcome.text).toBe('the final answer')
  })

  it('drops a model that answered with nothing', async () => {
    const harness = fake({ drafts: { 'beta-1': '   ' } })
    const outcome = await runCouncil(run(), harness.transport, harness.events)
    expect(outcome.council?.answers.map(answer => answer.model)).toEqual(['alpha one', 'gamma one'])
  })

  it('becomes a plain answer when only one model answers', async () => {
    const harness = fake({
      drafts: { 'beta-1': new Error('down'), 'gamma-1': new Error('down') },
    })
    const outcome = await runCouncil(run(), harness.transport, harness.events)
    expect(outcome.council).toBeUndefined()
    expect(outcome.text).toBe('answer from alpha-1')
    expect(harness.stages).toEqual(['answering'])
    expect(harness.deltas).toEqual([{ text: 'answer from alpha-1', replace: true }])
    expect(harness.chairmanAttempts()).toBe(0)
  })

  it('fails the way a plain turn fails when no model answers', async () => {
    const harness = fake({
      drafts: { 'alpha-1': new Error('first problem'), 'beta-1': new Error('second'), 'gamma-1': new Error('third') },
    })
    await expect(runCouncil(run(), harness.transport, harness.events)).rejects.toThrow('first problem')
  })

  it('runs the council even when only one model was left to answer at all', async () => {
    const harness = fake({})
    const outcome = await runCouncil(run([alpha, beta]), harness.transport, harness.events)
    expect(outcome.council?.reviewed).toBe(2)
  })

  it('shrinks the rankings when a review fails and still writes an answer', async () => {
    const harness = fake({ reviews: { 'beta-1': new Error('review fell over'), 'gamma-1': 'nothing readable here' } })
    const outcome = await runCouncil(run(), harness.transport, harness.events)
    expect(outcome.council?.rankings.map(ranking => ranking.reviewer)).toEqual(['alpha one'])
    expect(outcome.council?.finished).toBe(true)
    expect(outcome.text).toBe('the final answer')
  })

  it('writes an answer when every review fails', async () => {
    const harness = fake({
      reviews: { 'alpha-1': new Error('a'), 'beta-1': new Error('b'), 'gamma-1': new Error('c') },
    })
    const outcome = await runCouncil(run(), harness.transport, harness.events)
    expect(outcome.council?.rankings).toEqual([])
    // Nothing was readable, so the tie falls to name order.
    expect(outcome.council?.chairman).toBe('alpha one')
    expect(outcome.council?.finished).toBe(true)
  })

  it('retries the chairman once and replaces the half answer it left behind', async () => {
    const harness = fake({ chairman: [new Error('stream died'), 'the second answer'] })
    const outcome = await runCouncil(run(), harness.transport, harness.events)
    expect(harness.chairmanAttempts()).toBe(2)
    expect(outcome.text).toBe('the second answer')
    expect(outcome.council?.finished).toBe(true)
    expect(harness.deltas).toEqual([
      { text: 'half a ' },
      { text: '', thinking: '', replace: true },
      { text: 'the second answer' },
    ])
  })

  it('retries a chairman that streamed nothing at all', async () => {
    const harness = fake({ chairman: ['', 'the second answer'] })
    const outcome = await runCouncil(run(), harness.transport, harness.events)
    expect(harness.chairmanAttempts()).toBe(2)
    expect(outcome.text).toBe('the second answer')
  })

  it('shows the winner its own answer when the chairman never writes', async () => {
    const harness = fake({
      reviews: {
        'alpha-1': '1. Answer A\n2. Answer B\n3. Answer C',
        'beta-1': '1. Answer A\n2. Answer B\n3. Answer C',
        'gamma-1': '1. Answer A\n2. Answer B\n3. Answer C',
      },
      chairman: [new Error('first'), new Error('second')],
    })
    const outcome = await runCouncil(run(), harness.transport, harness.events)
    expect(harness.chairmanAttempts()).toBe(2)
    expect(outcome.council?.finished).toBe(false)
    const winner = outcome.council?.winner
    expect(outcome.text).toBe(outcome.council?.answers.find(answer => answer.model === winner)?.text)
    expect(harness.deltas[harness.deltas.length - 1]).toEqual({ text: outcome.text, thinking: '', replace: true })
    expect(councilByline(outcome.council!).text).toBe(' · the council could not finish')
  })

  it('stops on an aborted chairman rather than retrying it', async () => {
    const abort = new Error('aborted')
    abort.name = 'AbortError'
    const harness = fake({ chairman: [abort, 'never reached'] })
    await expect(runCouncil(run(), harness.transport, harness.events)).rejects.toThrow('aborted')
    expect(harness.chairmanAttempts()).toBe(1)
  })

  it('seeds the shuffle from the turn, so the same turn reviews the same way', async () => {
    const first = fake({})
    const second = fake({})
    await runCouncil({ ...run(), seed: seedFrom('a question', 2) }, first.transport, first.events)
    await runCouncil({ ...run(), seed: seedFrom('a question', 2) }, second.transport, second.events)
    const reviews = (harness: typeof first) =>
      harness.requests.filter(request => stageOf(request) === 'review').map(request => request.messages[0].content)
    expect(reviews(first)).toEqual(reviews(second))
  })
})
