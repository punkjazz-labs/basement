import { describe, expect, it } from 'vitest'
import {
  bannerFor, bannerItems, boxHealth, loadsRefused, shortAge, summarize, temperatureLight, unitStatus,
  HOT_C, STALE_MS, VERY_HOT_C, type BoxReading,
} from './fleetHealth'
import { boxHeadroom } from './headroom'

const GB = 1_000_000_000

const reading = (name: string, freeGB: number | undefined, over: Partial<BoxReading> = {}): BoxReading => ({
  id: `node-${name}`,
  name,
  self: false,
  answering: true,
  ageMs: 4000,
  headroom: boxHeadroom({ id: `node-${name}`, name, totalBytes: 128.5 * GB, availableBytes: freeGB === undefined ? undefined : freeGB * GB }),
  temperature: 63,
  ...over,
})

describe('one box, one light', () => {
  it('is green with nothing to say while all is well', () => {
    const box = boxHealth(reading('spark-2b4f', 19.4))
    expect(box.light).toBe('ok')
    expect(box.issues).toEqual([])
    expect(box.stale).toBe(false)
    expect(box.loadsRefused).toBe(false)
  })

  // The owner's rule: a box under its reserve takes no new model beside the
  // one it runs. The words say so only while it is true.
  it('turns amber under the reserve, and says loads are refused', () => {
    const box = boxHealth(reading('edgexpert-5e12', 7.2))
    expect(box.light).toBe('warn')
    expect(box.loadsRefused).toBe(true)
    expect(box.issues[0].text).toBe('memory tight (7.2 GB free, loads refused)')
  })

  it('turns red when memory is all but gone', () => {
    const box = boxHealth(reading('edgexpert-5e12', 1.2))
    expect(box.light).toBe('fail')
    expect(box.issues[0].text).toBe('out of memory (1.2 GB free, loads refused)')
  })

  it('says nothing about memory it has not read', () => {
    const box = boxHealth(reading('spark-9a07', undefined))
    expect(box.loadsRefused).toBe(false)
    expect(box.issues).toEqual([])
  })

  it('turns red for a box the fleet cannot reach', () => {
    const box = boxHealth(reading('spark-9a07', 17.8, { answering: false, ageMs: 200_000 }))
    expect(box.light).toBe('fail')
    expect(box.issues[0]).toMatchObject({ kind: 'offline', text: 'not answering' })
  })

  // A heartbeat older than the manager's own bound is stale: the dot goes
  // hollow, and the box wants a look even while the fleet still lists it.
  it('goes hollow when its heartbeat is stale', () => {
    const box = boxHealth(reading('spark-c3d1', 20.1, { ageMs: STALE_MS + 15_000 }))
    expect(box.stale).toBe(true)
    expect(box.light).toBe('warn')
    expect(box.issues[0].text).toBe('no heartbeat for 45 s')
    expect(boxHealth(reading('spark-c3d1', 20.1, { ageMs: STALE_MS })).stale).toBe(false)
  })

  // This box reads its own meters; it has no heartbeat to age.
  it('never calls the box the console runs on stale', () => {
    expect(boxHealth(reading('spark-f7e0', 18.5, { self: true, ageMs: 10 * STALE_MS })).stale).toBe(false)
  })

  it('reads a hot GPU as amber, and a very hot one as red', () => {
    expect(temperatureLight(HOT_C - 1)).toBe('ok')
    expect(temperatureLight(HOT_C)).toBe('warn')
    expect(temperatureLight(VERY_HOT_C)).toBe('fail')
    expect(temperatureLight(undefined)).toBe('ok')
    expect(boxHealth(reading('spark-2b4f', 19.4, { temperature: 88 })).issues[0].text).toBe('GPU hot (88 °C)')
  })

  it('turns red for a model that failed on it', () => {
    const box = boxHealth(reading('spark-2b4f', 19.4, { failedModel: 'Qwen 3.8 27B' }))
    expect(box.light).toBe('fail')
    expect(box.issues[0].text).toBe('Qwen 3.8 27B failed')
  })

  it('puts the worst trouble first', () => {
    const box = boxHealth(reading('edgexpert-5e12', 7.2, { answering: false }))
    expect(box.issues.map(issue => issue.kind)).toEqual(['offline', 'memory'])
    expect(box.light).toBe('fail')
  })

  it('reads a box still joining as amber, not as gone', () => {
    const box = boxHealth(reading('spark-new1', undefined, { answering: false, joining: true }))
    expect(box.light).toBe('warn')
    expect(box.issues[0].text).toBe('joining the fleet')
  })

  it('refuses loads only under the reserve', () => {
    const roomy = boxHeadroom({ id: 'a', name: 'a', totalBytes: 128 * GB, availableBytes: 12 * GB })
    const tight = boxHeadroom({ id: 'b', name: 'b', totalBytes: 128 * GB, availableBytes: 11.9 * GB })
    expect(loadsRefused(roomy)).toBe(false)
    expect(loadsRefused(tight)).toBe(true)
  })

  it('says a quiet spell short', () => {
    expect(shortAge(3_400)).toBe('3 s')
    expect(shortAge(125_000)).toBe('2 min')
    expect(shortAge(3 * 3600_000)).toBe('3 h')
    expect(shortAge(72 * 3600_000)).toBe('3 d')
  })
})

describe('the fleet in one phrase', () => {
  const calm = ['spark-f7e0', 'spark-2b4f', 'spark-9a07', 'spark-c3d1', 'edgexpert-41b8']
    .map(name => boxHealth(reading(name, 19)))

  it('says all is well in three words', () => {
    const summary = summarize([...calm, boxHealth(reading('edgexpert-5e12', 19))])
    expect(summary.text).toBe('All 6 boxes healthy')
    expect(summary.light).toBe('ok')
    expect(summarize(calm.slice(0, 4), '4 Sparks').text).toBe('All 4 Sparks healthy')
  })

  it('names a lone box rather than counting it', () => {
    expect(summarize([calm[0]]).text).toBe('spark-f7e0 is healthy')
  })

  // The brief's own sentence.
  it('counts the boxes that need attention and says why', () => {
    const summary = summarize([...calm, boxHealth(reading('edgexpert-5e12', 7.2))])
    expect(summary.text).toBe('1 needs attention · edgexpert-5e12 memory tight (7.2 GB free, loads refused)')
    expect(summary.light).toBe('warn')
    expect(summary.attention.map(box => box.name)).toEqual(['edgexpert-5e12'])
  })

  it('leads with the worst, and counts the rest', () => {
    const summary = summarize([
      boxHealth(reading('edgexpert-5e12', 7.2)),
      boxHealth(reading('spark-9a07', 17.8, { answering: false })),
      boxHealth(reading('spark-c3d1', 20.1, { temperature: 90 })),
      calm[0],
    ])
    expect(summary.text).toBe('3 need attention · spark-9a07 not answering · edgexpert-5e12 memory tight (7.2 GB free, loads refused) · 1 more')
    expect(summary.light).toBe('fail')
  })

  // Two tight boxes: the one with less free leads, whatever order the fleet
  // lists them in.
  it('leads with the tightest of two tight boxes', () => {
    const summary = summarize([boxHealth(reading('edgexpert-41b8', 9.6)), boxHealth(reading('edgexpert-5e12', 7.2))])
    expect(summary.attention.map(box => box.name)).toEqual(['edgexpert-5e12', 'edgexpert-41b8'])
    expect(bannerFor(summary)?.title).toBe('edgexpert-5e12 is tight on memory')
  })

  it('has nothing to say about no boxes', () => {
    expect(summarize([]).text).toBe('')
  })
})

describe('the attention banner', () => {
  it('is not drawn while all is well', () => {
    expect(bannerFor(summarize([boxHealth(reading('spark-f7e0', 19))]))).toBeNull()
  })

  it('names the tight box, its reserve, and what that means for loads', () => {
    const banner = bannerFor(summarize([boxHealth(reading('edgexpert-5e12', 7.2)), boxHealth(reading('spark-f7e0', 19))]))
    expect(banner).toMatchObject({
      light: 'warn',
      title: 'edgexpert-5e12 is tight on memory',
      boxID: 'node-edgexpert-5e12',
      boxName: 'edgexpert-5e12',
    })
    expect(banner?.body).toBe('7.2 GB free, under the 12.0 GB it keeps free. New models cannot load there until the one it runs stops.')
  })

  it('leads with a box that cannot be reached, and counts the others', () => {
    const banner = bannerFor(summarize([
      boxHealth(reading('edgexpert-5e12', 7.2)),
      boxHealth(reading('spark-9a07', 17.8, { answering: false, ageMs: 180_000 })),
    ]))
    expect(banner?.title).toBe('spark-9a07 is not answering')
    expect(banner?.light).toBe('fail')
    expect(banner?.body).toBe('Last heard from 3 min ago. Its models cannot be started or stopped from here. 1 more box needs attention.')
  })
})

// The banner lists every box that needs a look, each line standing alone.
describe('the banner, one line per box', () => {
  it('says nothing while all is well', () => {
    expect(bannerItems(summarize([boxHealth(reading('spark-f7e0', 19))]))).toEqual([])
  })

  it('gives every box its own line, worst first, without counting the others', () => {
    const items = bannerItems(summarize([
      boxHealth(reading('edgexpert-5e12', 7.2)),
      boxHealth(reading('spark-9a07', 17.8, { answering: false, ageMs: 180_000 })),
      boxHealth(reading('spark-f7e0', 19)),
    ]))
    expect(items.map(item => [item.boxName, item.light, item.title])).toEqual([
      ['spark-9a07', 'fail', 'spark-9a07 is not answering'],
      ['edgexpert-5e12', 'warn', 'edgexpert-5e12 is tight on memory'],
    ])
    expect(items[0].body).toBe('Last heard from 3 min ago. Its models cannot be started or stopped from here.')
  })
})

// The group header must never say Healthy over rows that say otherwise.
describe('a unit in one word', () => {
  const ok = boxHealth(reading('edgexpert-41b8', 19))
  const tight = boxHealth(reading('edgexpert-5e12', 7.2))
  const gone = boxHealth(reading('edgexpert-5e12', 19, { answering: false }))

  it('is healthy only while every box is', () => {
    expect(unitStatus('healthy', [ok, ok])).toEqual({ word: 'Healthy', tone: 'healthy' })
  })

  it('says memory is tight when that is the only trouble', () => {
    expect(unitStatus('healthy', [ok, tight])).toEqual({ word: 'Memory tight', tone: 'degraded' })
  })

  it('needs attention for any other trouble, in the colour of the worst', () => {
    expect(unitStatus('healthy', [tight, boxHealth(reading('x', 19, { temperature: 99 }))]))
      .toEqual({ word: 'Needs attention', tone: 'failed' })
  })

  it('keeps the unit\'s own word when it has one', () => {
    expect(unitStatus('degraded', [ok, gone])).toEqual({ word: 'Degraded', tone: 'failed' })
    expect(unitStatus('failed', [ok, ok])).toEqual({ word: 'Failed', tone: 'failed' })
  })
})
