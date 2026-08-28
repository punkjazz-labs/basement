import { describe, expect, it } from 'vitest'
import { CURATED, LOGOS, RECOMMENDED_ID, RELEASED, labFor, logoFor, sortCatalog } from './catalog'

const entry = (id: string, display_name: string, model_by: string, spark_count = 1) => ({
  id, display_name, model_by, topology: { spark_count },
})

// The recipes this build ships, in the order the manager happens to hand them
// over (alphabetical by filename), which is not an order at all.
const SHIPPED = [
  entry('deepseek-v4-flash-0731-2s', 'DeepSeek V4 Flash 0731', 'DeepSeek', 2),
  entry('deepseek-v4-flash-0731-ud-iq3-xxs-1s', 'DeepSeek V4 Flash 0731 3-bit', 'DeepSeek'),
  entry('inkling-small-nvfp4-2s', 'Inkling Small NVFP4', 'Thinking Machines', 2),
  entry('laguna-s-2-1-nvfp4-dflash-1s', 'Laguna S 2.1 + DFlash', 'poolside'),
  entry('minimax-h3-comfyui-1s', 'MiniMax H3', 'MiniMax'),
  entry('nemotron-omni-30b-a3b-nvfp4-1s', 'Nemotron Omni 30B-A3B', 'NVIDIA'),
  entry('qwen35-122b-a10b-nvfp4-1s', 'Qwen 3.5 122B-A10B', 'Qwen team, Alibaba'),
  entry('qwen36-27b-nvfp4-1s', 'Qwen 3.6 27B', 'Qwen team, Alibaba'),
  entry('qwen36-35b-a3b-nvfp4-1s', 'Qwen 3.6 35B-A3B (Unsloth)', 'Qwen team, Alibaba'),
  entry('qwen38-27b-nvfp4-1s', 'Qwen 3.8 27B', 'Qwen team, Alibaba'),
  entry(
    'qwen38-27b-obliterated-q8-0-1s',
    'Qwen 3.8 27B Obliterated',
    'Qwen team, Alibaba; abliteration by OBLITERATUS',
  ),
]

// The dividers the catalog pane draws, in the order it draws them.
const labsOf = (sorted: readonly { model_by?: string }[]) =>
  sorted.map(recipe => labFor(recipe)).filter((lab, index, all) => all.indexOf(lab) === index)

describe('labFor', () => {
  it('reads both forms of the Qwen team name as one lab', () => {
    expect(labFor({ model_by: 'Qwen team, Alibaba' })).toBe('Qwen · Alibaba')
    expect(labFor({ model_by: 'Qwen team, Alibaba; abliteration by OBLITERATUS' })).toBe('Qwen · Alibaba')
  })

  it('says every other lab in the recipe own words', () => {
    expect(labFor({ model_by: 'poolside' })).toBe('poolside')
    expect(labFor({ model_by: 'DeepSeek' })).toBe('DeepSeek')
    expect(labFor({ model_by: 'MiniMax' })).toBe('MiniMax')
    expect(labFor({ model_by: 'NVIDIA' })).toBe('NVIDIA')
    expect(labFor({ model_by: 'Thinking Machines' })).toBe('Thinking Machines')
  })

  it('falls back to the publisher, then to Community', () => {
    expect(labFor({ publisher: 'Comfy-Org' })).toBe('Comfy-Org')
    expect(labFor({ model_by: '  ', publisher: 'Comfy-Org' })).toBe('Comfy-Org')
    expect(labFor({})).toBe('Community')
  })
})

describe('sortCatalog', () => {
  it('reads the labs in the order the curated shelf meets them', () => {
    expect(labsOf(sortCatalog(SHIPPED))).toEqual([
      'Qwen · Alibaba', 'poolside', 'DeepSeek', 'MiniMax', 'NVIDIA', 'Thinking Machines',
    ])
  })

  it('puts the newest model of a lab at the top of its group', () => {
    const order = sortCatalog(SHIPPED).map(item => item.id)
    expect(order.slice(0, 6)).toEqual([
      // August 2026, then April 2026, then February 2026.
      'qwen38-27b-nvfp4-1s',
      'qwen38-27b-obliterated-q8-0-1s',
      'qwen36-35b-a3b-nvfp4-1s',
      'qwen36-27b-nvfp4-1s',
      'qwen35-122b-a10b-nvfp4-1s',
      'laguna-s-2-1-nvfp4-dflash-1s',
    ])
  })

  // The recommended model no longer opens the table. The lab it belongs to
  // still does, and the hero still names it.
  it('keeps the recommended model on the curated shelf', () => {
    expect(CURATED[0]).toBe(RECOMMENDED_ID)
    expect(sortCatalog(SHIPPED).map(item => item.id)).toContain(RECOMMENDED_ID)
  })

  it('holds two models released in the same month in the order they had', () => {
    const order = sortCatalog(SHIPPED).map(item => item.id)
    expect(order.indexOf('deepseek-v4-flash-0731-ud-iq3-xxs-1s'))
      .toBeLessThan(order.indexOf('deepseek-v4-flash-0731-2s'))
  })

  it('sorts a model with no recorded release date last inside its lab', () => {
    const undated = entry('qwen-something-new-1s', 'Qwen Something New', 'Qwen team, Alibaba')
    const order = sortCatalog([undated, ...SHIPPED]).map(item => item.id)
    expect(RELEASED[undated.id]).toBeUndefined()
    expect(order.indexOf(undated.id)).toBe(order.indexOf('qwen35-122b-a10b-nvfp4-1s') + 1)
  })

  // A lab arrives with the first recipe of its own that the curated shelf
  // meets, so a lab nobody has written up yet reads after the ones that are
  // curated, never in front of them.
  it('sorts a lab that is on no curated shelf after the labs that are', () => {
    const stranger = entry('brand-new-model-1s', 'Aaaa Brand New', 'Some New Lab')
    const labs = labsOf(sortCatalog([stranger, ...SHIPPED]))
    expect(labs.indexOf('Some New Lab')).toBeGreaterThan(labs.indexOf('poolside'))
    expect(labs[0]).toBe('Qwen · Alibaba')
  })

  it('is stable whatever order the catalog arrives in, and does not mutate it', () => {
    const reversed = [...SHIPPED].reverse()
    expect(sortCatalog(reversed).map(item => item.id)).toEqual(sortCatalog(SHIPPED).map(item => item.id))
    expect(reversed[0].id).toBe('qwen38-27b-obliterated-q8-0-1s')
  })

  it('leaves a catalog of only uncurated recipes in a deterministic order', () => {
    const order = sortCatalog([
      entry('z-two-spark-2s', 'Zed', 'Zeta Lab', 2),
      entry('b-one-spark-1s', 'Bee', 'Beta Lab'),
      entry('a-one-spark-1s', 'Ay', 'Beta Lab'),
    ]).map(item => item.id)
    expect(order).toEqual(['a-one-spark-1s', 'b-one-spark-1s', 'z-two-spark-2s'])
  })
})

describe('logoFor', () => {
  it('gives every shipped recipe the mark of the lab that made it', () => {
    for (const item of SHIPPED) expect(logoFor([item.id])).not.toBe('')
    expect(logoFor(['minimax-h3-comfyui-1s'])).toBe('/logos/minimax.webp')
    expect(logoFor(['inkling-small-nvfp4-2s'])).toBe('/logos/thinkingmachines.webp')
    expect(logoFor(['deepseek-v4-flash-0731-ud-iq3-xxs-1s'])).toBe('/logos/deepseek.webp')
  })

  // The Qwen 3.8 Flash Next recipe arrives in its own change. Its mark and
  // its release date are already here, so it lands with both.
  it('is ready for the recipe that has not landed yet', () => {
    expect(logoFor(['qwen38-flash-next-nvfp4-2s'])).toBe('/logos/qwen.webp')
    expect(RELEASED['qwen38-flash-next-nvfp4-2s']).toBe('2026-08')
  })

  // A model this build has never seen gets no mark at all, and the caller
  // draws a quiet initial block. Nothing here lends it another lab's logo.
  it('gives an unknown recipe no mark rather than the wrong one', () => {
    expect(logoFor(['brand-new-model-1s'])).toBe('')
    expect(logoFor([])).toBe('')
    expect(Object.values(LOGOS)).not.toContain('')
  })
})
