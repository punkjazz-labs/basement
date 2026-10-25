import { describe, expect, it } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import {
  answeredIn, attachedServices, checkedAgo, guardLine, healthRead, kindLabel, licenceHref, territoryLine, topologyLine,
  type AttachedService,
} from './attached'
import { AttachedCard, AttachedProblems } from './attachedCard'

const NOW = Date.parse('2026-10-25T12:00:00Z')
const ago = (seconds: number) => new Date(NOW - seconds * 1000).toISOString()

// The shipped example declaration (docs/examples/attached-services), as the
// manager reports it once it has been checked.
const fastH3 = (overrides: Partial<AttachedService> = {}): AttachedService => ({
  file: 'fasth3-gpu-pair.yaml',
  declaration: {
    schema_version: 1,
    id: 'fasth3-gpu-pair',
    display_name: 'FastH3 (MiniMax H3 Omni, FastVideo)',
    model_by: 'MiniMax / FastVideo',
    kind: 'video-generation',
    served_model_name: 'fasth3-omniref',
    managed_by: 'systemd: fasth3-serve.service (head-box), h3-ray.service + h3-memguard.service (both)',
    endpoint: 'http://127.0.0.1:8790 (head; reach via ssh tunnel)',
    health_url: 'http://127.0.0.1:8790/health',
    health_interval_seconds: 30,
    topology: {
      spark_count: 2,
      parallelism: 'TP1 x SP2 pair (Ray)',
      nodes: [{ node_name: 'head-box', role: 'head/rank0' }, { node_name: 'worker-box', role: 'worker/rank1' }],
    },
    licence: 'MiniMax H3 Community License Agreement',
    licence_territory_exclusions: ['European Union', 'United Kingdom', 'Republic of Korea', 'United States of America'],
  },
  health: { status: 'healthy', last_checked_at: ago(12), latency_ms: 3 },
  nodes: [
    { name: 'head-box', role: 'head/rank0', this_spark: true, protected: true },
    { name: 'worker-box', role: 'worker/rank1', this_spark: false, protected: true },
  ],
  ...overrides,
})

const textOf = (markup: string) => markup.replace(/<[^>]*>/g, ' ').replace(/\s+/g, ' ')

describe('attachedServices', () => {
  it('keeps what the card can draw and drops the rest', () => {
    const parsed = attachedServices({
      services: [fastH3(), { file: 'x.yaml' }, { ...fastH3(), health: { status: 'great' } }],
      errors: [{ file: 'broken.yaml', error: 'the file is empty' }, { file: 3 }],
    })
    expect(parsed.services.map(service => service.declaration.id)).toEqual(['fasth3-gpu-pair'])
    expect(parsed.errors).toEqual([{ file: 'broken.yaml', error: 'the file is empty' }])
  })

  it('reads an answer it does not understand as nothing to show', () => {
    expect(attachedServices(null)).toEqual({ services: [], errors: [] })
    expect(attachedServices({ error: 'not found' })).toEqual({ services: [], errors: [] })
  })
})

describe('attached service copy', () => {
  it('states the topology the declaration gives', () => {
    expect(topologyLine(fastH3().declaration)).toBe('2 Sparks · TP1 x SP2 pair (Ray)')
    const single = fastH3().declaration
    expect(topologyLine({ ...single, topology: { spark_count: 1, nodes: [{ node_name: 'box-a' }] } })).toBe('1 Spark')
  })

  it('writes the kind in sentence case and keeps a kind it has never seen', () => {
    expect(kindLabel('video-generation')).toBe('Video generation')
    expect(kindLabel('chat')).toBe('Chat')
    expect(kindLabel('speech-to-text')).toBe('Speech to text')
  })

  it('counts seconds since the last check, then reads like any other age', () => {
    expect(checkedAgo(ago(12), NOW)).toBe('checked 12 s ago')
    expect(checkedAgo(ago(0), NOW)).toBe('checked just now')
    expect(checkedAgo(ago(180), NOW)).toBe('checked 3 minutes ago')
    expect(checkedAgo(undefined, NOW)).toBe('')
  })

  it('says healthy, unhealthy, or not checked yet, with a live line only after a check', () => {
    expect(healthRead(fastH3().health, NOW))
      .toEqual({ tone: 'live', word: 'Healthy', parts: ['checked 12 s ago', 'answered in 3 ms'] })
    expect(healthRead({ status: 'unhealthy', last_checked_at: ago(40), latency_ms: 5000, last_error: 'no answer within 5s' }, NOW))
      .toEqual({ tone: 'fail', word: 'Unhealthy', parts: ['checked 40 s ago', 'no answer within 5s'] })
    expect(healthRead({ status: 'unknown', latency_ms: 0 }, NOW)).toEqual({ tone: '', word: 'Not checked yet', parts: [] })
  })

  it('states how fast the check answered in ms, then in seconds', () => {
    expect(answeredIn(3)).toBe('answered in 3 ms')
    expect(answeredIn(999)).toBe('answered in 999 ms')
    expect(answeredIn(1240)).toBe('answered in 1.2 s')
  })

  it('carries the licence territory exclusions in the install dialog wording', () => {
    expect(territoryLine(fastH3().declaration)).toBe(
      'This licence does not grant rights in these territories: European Union, United Kingdom, Republic of Korea, United States of America.')
    expect(territoryLine({ ...fastH3().declaration, licence_territory_exclusions: undefined })).toBeUndefined()
  })

  it('says the Sparks are kept free only when this console can keep them free', () => {
    expect(guardLine(fastH3())).toEqual({
      warn: false,
      text: 'Basement does not install, start, stop or update this service. It keeps its own models off these Sparks while fasth3-gpu-pair.yaml is in place.',
    })
    const typo = fastH3({
      nodes: [
        { name: 'head-box', role: 'head/rank0', this_spark: true, protected: true },
        { name: 'wroker-box', role: 'worker/rank1', this_spark: false, protected: false },
      ],
    })
    expect(guardLine(typo)).toEqual({
      warn: true,
      text: 'This console cannot match wroker-box to a Spark it places models on, so it cannot keep its own models off it. Check the node name in fasth3-gpu-pair.yaml.',
    })
  })

  it('links a licence only over https', () => {
    expect(licenceHref({ ...fastH3().declaration, licence_url: 'https://example.com/LICENSE' })).toBe('https://example.com/LICENSE')
    expect(licenceHref({ ...fastH3().declaration, licence_url: 'javascript:alert(1)' })).toBeUndefined()
    expect(licenceHref(fastH3().declaration)).toBeUndefined()
  })
})

describe('AttachedCard', () => {
  it('shows the service as one serving card, managed outside Basement, with no controls', () => {
    const markup = renderToStaticMarkup(<AttachedCard service={fastH3()} nowMs={NOW} />)
    const text = textOf(markup)
    for (const expected of [
      'FastH3 (MiniMax H3 Omni, FastVideo)',
      'Attached service · managed outside Basement',
      'Healthy',
      'checked 12 s ago',
      'answered in 3 ms',
      '2 Sparks · TP1 x SP2 pair (Ray)',
      'head-box head/rank0',
      'worker-box worker/rank1',
      'http://127.0.0.1:8790 (head; reach via ssh tunnel)',
      'systemd: fasth3-serve.service (head-box), h3-ray.service + h3-memguard.service (both)',
      'MiniMax / FastVideo',
      'Video generation',
      'fasth3-omniref',
      'MiniMax H3 Community License Agreement',
      'This licence does not grant rights in these territories: European Union, United Kingdom, Republic of Korea, United States of America.',
    ]) {
      expect(text).toContain(expected)
    }
    expect(markup).not.toContain('<button')
    expect(markup).not.toContain('<a ')
    expect(markup).not.toContain('—')
    // The serving card's own pieces: its frame with the green edge, the
    // green chip, and both Sparks lit while the service answers.
    expect(markup).toContain('class="mband attached-band live"')
    expect(markup).toContain('class="state-chip live"')
    expect(markup.match(/class="hdot ok"/g)).toHaveLength(2)
    // Closed, like any serving card, while there is nothing to fix.
    expect(markup).not.toMatch(/<details[^>]* open/)
  })

  it('shows the reason a check failed, and stops lighting the Sparks', () => {
    const service = fastH3({ health: { status: 'unhealthy', last_checked_at: ago(5), latency_ms: 1, last_error: 'the health check answered HTTP 503' } })
    const markup = renderToStaticMarkup(<AttachedCard service={service} nowMs={NOW} />)
    expect(textOf(markup)).toContain('Unhealthy checked 5 s ago the health check answered HTTP 503')
    expect(markup).toContain('class="state-chip fail"')
    expect(markup).toContain('class="mband attached-band "')
    expect(markup).not.toContain('hdot ok')
  })

  it('says nothing live before the first check', () => {
    const service = fastH3({ health: { status: 'unknown', latency_ms: 0 } })
    const markup = renderToStaticMarkup(<AttachedCard service={service} nowMs={NOW} />)
    expect(textOf(markup)).toContain('Not checked yet')
    expect(markup).toContain('class="state-chip "')
    expect(markup).not.toContain('mb-live')
    expect(markup).not.toContain('hdot ok')
  })

  it('opens by itself, and marks the Spark, when a name cannot be matched', () => {
    const service = fastH3({
      nodes: [
        { name: 'head-box', role: 'head/rank0', this_spark: true, protected: true },
        { name: 'wroker-box', role: 'worker/rank1', this_spark: false, protected: false },
      ],
    })
    const markup = renderToStaticMarkup(<AttachedCard service={service} nowMs={NOW} />)
    expect(markup).toMatch(/<details[^>]* open/)
    expect(markup).toContain('class="hdot warn"')
    expect(textOf(markup)).toContain('This console cannot match wroker-box to a Spark it places models on')
  })

  it('writes n/a for what the declaration leaves out', () => {
    const declaration = { ...fastH3().declaration, model_by: undefined, endpoint: undefined, served_model_name: undefined, licence: undefined, licence_territory_exclusions: undefined }
    const text = textOf(renderToStaticMarkup(<AttachedCard service={fastH3({ declaration })} nowMs={NOW} />))
    expect(text).toContain('Endpoint n/a')
    expect(text).toContain('Model by n/a')
    expect(text).toContain('Served as n/a')
    expect(text).toContain('Licence n/a')
    expect(text).not.toContain('territories')
  })
})

describe('AttachedProblems', () => {
  it('names each refused file and says it keeps no Spark free', () => {
    const text = textOf(renderToStaticMarkup(<AttachedProblems problems={[{ file: 'broken.yaml', error: 'the file is empty' }]} />))
    expect(text).toContain('broken.yaml : the file is empty')
    expect(text).toContain('Basement keeps no Spark free for a file it cannot read.')
  })
})
