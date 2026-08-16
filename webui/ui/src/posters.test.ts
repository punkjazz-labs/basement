import { beforeEach, describe, expect, it, vi } from 'vitest'
import { cachedPoster, forgetPoster, storePoster } from './posters'

// A Storage stand-in good enough to exercise the cache logic without a real
// browser. A finite capacity makes setItem throw once writing a key that is
// not already present would push the key count past it, the way real
// localStorage throws QuotaExceededError once the origin's quota is used up.
function fakeStorage(capacity = Infinity, initial: Record<string, string> = {}): Storage {
  const data = new Map(Object.entries(initial))
  return {
    getItem: (key: string) => (data.has(key) ? (data.get(key) as string) : null),
    setItem: (key: string, value: string) => {
      if (!data.has(key) && data.size >= capacity) {
        throw new DOMException('storage quota exceeded', 'QuotaExceededError')
      }
      data.set(key, value)
    },
    removeItem: (key: string) => {
      data.delete(key)
    },
    clear: () => data.clear(),
    key: (index: number) => Array.from(data.keys())[index] ?? null,
    get length() {
      return data.size
    },
  }
}

// cachedPoster always reads window.localStorage, so tests that exercise it
// stub window with a fake store rather than passing one as an argument.
beforeEach(() => {
  vi.stubGlobal('window', { localStorage: fakeStorage() })
})

describe('storePoster and cachedPoster', () => {
  it('reads back what was stored', () => {
    storePoster('run-1', 'data:image/jpeg;base64,aaa')
    expect(cachedPoster('run-1')).toBe('data:image/jpeg;base64,aaa')
  })

  it('returns null for an id that was never stored', () => {
    expect(cachedPoster('missing')).toBeNull()
  })
})

describe('storePoster eviction on a throwing store', () => {
  it('evicts the oldest id first when the store is out of room', () => {
    // Capacity 3 holds two poster entries plus the index key at once, so the
    // third store call must evict run-1 before it can fit.
    const store = fakeStorage(3)
    storePoster('run-1', 'poster-1', store)
    storePoster('run-2', 'poster-2', store)
    storePoster('run-3', 'poster-3', store)

    expect(store.getItem('basement.generate.poster.run-1')).toBeNull()
    expect(store.getItem('basement.generate.poster.run-2')).toBe('poster-2')
    expect(store.getItem('basement.generate.poster.run-3')).toBe('poster-3')
    expect(JSON.parse(store.getItem('basement.generate.posters') as string)).toEqual(['run-2', 'run-3'])
  })

  it('evicts more than one entry in a single write when one is not enough', () => {
    // The store starts already full: two poster entries, the index, and one
    // unrelated key from elsewhere in the app. Freeing room for run-3 takes
    // two evictions, not one.
    const store = fakeStorage(3, {
      'basement.generate.poster.run-1': 'poster-1',
      'basement.generate.poster.run-2': 'poster-2',
      'basement.generate.posters': JSON.stringify(['run-1', 'run-2']),
      'unrelated-key': 'x',
    })

    storePoster('run-3', 'poster-3', store)

    expect(store.getItem('basement.generate.poster.run-1')).toBeNull()
    expect(store.getItem('basement.generate.poster.run-2')).toBeNull()
    expect(store.getItem('basement.generate.poster.run-3')).toBe('poster-3')
    expect(store.getItem('unrelated-key')).toBe('x')
    expect(JSON.parse(store.getItem('basement.generate.posters') as string)).toEqual(['run-3'])
  })

  it('gives up silently once the index is empty and the write still fails', () => {
    const store = fakeStorage(0)
    expect(() => storePoster('run-1', 'poster-1', store)).not.toThrow()
    expect(store.getItem('basement.generate.poster.run-1')).toBeNull()
  })
})

describe('the 200-id cap', () => {
  it('evicts the oldest id once the index would exceed 200 entries', () => {
    const store = fakeStorage()
    for (let index = 0; index < 200; index += 1) {
      storePoster(`run-${index}`, `poster-${index}`, store)
    }
    // The index is now full at 200. One more write must push out run-0.
    storePoster('run-200', 'poster-200', store)

    const ids = JSON.parse(store.getItem('basement.generate.posters') as string)
    expect(ids).toHaveLength(200)
    expect(ids).not.toContain('run-0')
    expect(ids[ids.length - 1]).toBe('run-200')
    expect(store.getItem('basement.generate.poster.run-0')).toBeNull()
    expect(store.getItem('basement.generate.poster.run-200')).toBe('poster-200')
  })
})

describe('forgetPoster', () => {
  it('removes the entry and drops it from the index', () => {
    const store = fakeStorage()
    storePoster('run-1', 'poster-1', store)
    storePoster('run-2', 'poster-2', store)

    forgetPoster('run-1', store)

    expect(store.getItem('basement.generate.poster.run-1')).toBeNull()
    expect(JSON.parse(store.getItem('basement.generate.posters') as string)).toEqual(['run-2'])
  })

  it('is a no-op for an id that was never stored', () => {
    const store = fakeStorage()
    expect(() => forgetPoster('missing', store)).not.toThrow()
  })
})
