import { describe, expect, it } from 'vitest'
import { CURATED, RECOMMENDED_ID, sortCatalog } from './catalog'

const entry = (id: string, display_name: string, spark_count = 1) => ({
  id, display_name, topology: { spark_count },
})

// The six recipes this build ships, in the order the manager happens to hand
// them over (alphabetical by filename), which is not an order at all.
const SHIPPED = [
  entry('deepseek-v4-flash-0731-2s', 'DeepSeek V4 Flash 0731', 2),
  entry('laguna-s-2-1-nvfp4-dflash-1s', 'Laguna S 2.1 + DFlash'),
  entry('nemotron-omni-30b-a3b-nvfp4-1s', 'Nemotron Omni 30B-A3B'),
  entry('qwen35-122b-a10b-nvfp4-1s', 'Qwen 3.5 122B-A10B'),
  entry('qwen36-27b-nvfp4-1s', 'Qwen 3.6 27B'),
  entry('qwen36-35b-a3b-nvfp4-1s', 'Qwen 3.6 35B-A3B (Unsloth)'),
]

describe('sortCatalog', () => {
  it('puts the curated shelf first, in the order it is written', () => {
    expect(sortCatalog(SHIPPED).map(item => item.id).slice(0, CURATED.length)).toEqual([...CURATED])
  })

  it('keeps the recommended model at the very top', () => {
    expect(sortCatalog(SHIPPED)[0].id).toBe(RECOMMENDED_ID)
  })

  it('sorts an uncurated recipe last, never first', () => {
    const withNew = [entry('brand-new-model-1s', 'Aaaa Brand New'), ...SHIPPED]
    const order = sortCatalog(withNew).map(item => item.id)
    expect(order[0]).toBe(RECOMMENDED_ID)
    expect(order.indexOf('brand-new-model-1s')).toBeGreaterThan(CURATED.length - 1)
  })

  it('orders the uncurated tail by spark count, then by name', () => {
    const tail = sortCatalog(SHIPPED).map(item => item.id).slice(CURATED.length)
    expect(tail).toEqual([
      'nemotron-omni-30b-a3b-nvfp4-1s',
      'qwen35-122b-a10b-nvfp4-1s',
      'deepseek-v4-flash-0731-2s',
    ])
  })

  it('is stable whatever order the catalog arrives in, and does not mutate it', () => {
    const reversed = [...SHIPPED].reverse()
    expect(sortCatalog(reversed).map(item => item.id)).toEqual(sortCatalog(SHIPPED).map(item => item.id))
    expect(reversed[0].id).toBe('qwen36-35b-a3b-nvfp4-1s')
  })

  it('leaves a catalog of only uncurated recipes in a deterministic order', () => {
    const order = sortCatalog([
      entry('z-two-spark-2s', 'Zed', 2),
      entry('b-one-spark-1s', 'Bee'),
      entry('a-one-spark-1s', 'Ay'),
    ]).map(item => item.id)
    expect(order).toEqual(['a-one-spark-1s', 'b-one-spark-1s', 'z-two-spark-2s'])
  })
})
