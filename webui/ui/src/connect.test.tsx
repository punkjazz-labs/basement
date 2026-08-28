import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { APIKey } from './api'
import Connect from './views/Connect'

// The API page, driven as a page rather than read as a picture. Revoking is
// destructive and it is two steps: the console asks, and only a yes deletes.
// Nothing rendered proves that, so this file presses the button the page drew
// and reads what reached the manager.
//
// There is no browser in this suite, so the view is driven by hand: the
// component is called, its state is kept between calls, its effects are run,
// and the handler on a drawn button is invoked. Everything from the view down
// is the real code, including the shared key flow in keys.ts. Only the manager
// and the dialog are stubbed, in the same way keys.test.ts stubs them.

const ORIGIN = 'http://192.168.10.148:7070'

// The manager: a list it holds, the calls it received, and the question it was
// asked. Reads answer from the list and a DELETE takes a key out of it, so a
// page that does not read the list again shows a key the manager no longer has.
const stub = vi.hoisted(() => ({
  held: [] as { id: string; name: string; created_at: string; last_used_at?: string }[],
  sent: [] as { path: string; method: string }[],
  asked: [] as { title: string; body?: string; confirmLabel?: string; danger?: boolean }[],
  notices: [] as string[],
  // The open question. It is answered by the test, never by the stub, so the
  // moment before the answer is a state this file can look at.
  say: null as null | ((result: { ok: boolean; checked: boolean }) => void),
  refuse: null as null | ((problem: Error) => void),
}))

// The hook cells of the one component under test, kept between renders.
const cells = vi.hoisted(() => ({
  list: [] as { value?: unknown; deps?: unknown[] }[],
  at: 0,
  effects: [] as (() => void)[],
  dirty: false,
}))

vi.mock('./api', () => ({
  api: (path: string, options: RequestInit = {}) => {
    const method = (options.method ?? 'GET').toUpperCase()
    stub.sent.push({ path, method })
    if (method === 'DELETE') {
      const id = decodeURIComponent(path.slice(path.lastIndexOf('/') + 1))
      stub.held = stub.held.filter(key => key.id !== id)
      return Promise.resolve({})
    }
    return Promise.resolve(stub.held.map(key => ({ ...key })))
  },
  copyText: () => Promise.resolve(),
  idempotency: () => ({ 'Idempotency-Key': 'stub-uuid' }),
}))

vi.mock('./confirm', () => ({
  confirmBox: (options: { title: string }) => {
    stub.asked.push(options)
    return new Promise((resolve, reject) => {
      stub.say = resolve
      stub.refuse = reject
    })
  },
  noticeBox: (title: string) => {
    stub.notices.push(title)
    return Promise.resolve()
  },
}))

// The smallest thing that can run a function component: state that survives a
// render, and effects that run after one. React batches and this does not, so
// a state change only marks the page for redrawing; settle() below does it.
vi.mock('react', async importOriginal => {
  const actual = await importOriginal<typeof import('react')>()
  return {
    ...actual,
    useState: (initial: unknown) => {
      const at = cells.at++
      const cell = cells.list[at] ??
        (cells.list[at] = { value: typeof initial === 'function' ? (initial as () => unknown)() : initial })
      return [cell.value, (next: unknown) => {
        const value = typeof next === 'function' ? (next as (was: unknown) => unknown)(cell.value) : next
        if (Object.is(value, cell.value)) return
        cell.value = value
        cells.dirty = true
      }]
    },
    useEffect: (run: () => void | (() => void), deps?: unknown[]) => {
      const at = cells.at++
      const before = cells.list[at]
      const changed = !before || !deps || !before.deps ||
        deps.length !== before.deps.length || deps.some((dep, n) => !Object.is(dep, before.deps?.[n]))
      cells.list[at] = { deps }
      if (changed) cells.effects.push(() => void run())
    },
  }
})

// What the page drew, last time it was drawn.
interface Drawn { type: unknown; props: Record<string, unknown> }

const isDrawn = (node: unknown): node is Drawn =>
  typeof node === 'object' && node !== null && 'type' in node && 'props' in node

const parts = (node: unknown): Drawn[] => {
  if (Array.isArray(node)) return node.flatMap(parts)
  if (!isDrawn(node)) return []
  return [node, ...parts(node.props.children)]
}

const words = (node: unknown): string => {
  if (typeof node === 'string' || typeof node === 'number') return String(node)
  if (Array.isArray(node)) return node.map(words).join('')
  if (isDrawn(node)) return words(node.props.children)
  return ''
}

let page: unknown = null

const draw = () => {
  cells.at = 0
  cells.dirty = false
  page = Connect({})
  cells.effects.splice(0).forEach(run => run())
}

// Let every answer arrive, and redraw for as long as the page keeps changing.
const settle = async () => {
  for (let round = 0; round < 20; round++) {
    await new Promise(resolve => setTimeout(resolve, 0))
    if (!cells.dirty) return
    draw()
  }
  throw new Error('the page never settled')
}

const open = async () => {
  draw()
  await settle()
}

const rows = () => parts(page).filter(part => part.props.className === 'key-row')

const nameOf = (row: Drawn): string =>
  words(parts(row).find(part => part.type === 'strong')?.props.children)

const listed = (): string[] => rows().map(nameOf)

// Press Revoke on the row that carries this name, the way an owner would.
const pressRevoke = async (name: string) => {
  const row = rows().find(one => nameOf(one) === name)
  if (!row) throw new Error(`no row for ${name}: the page lists ${listed().join(', ') || 'nothing'}`)
  const button = parts(row).find(part => part.type === 'button' && words(part.props.children) === 'Revoke')
  if (!button) throw new Error(`no Revoke button on the row for ${name}`)
  ;(button.props.onClick as () => void)()
  await settle()
}

const answer = async (ok: boolean) => {
  if (!stub.say) throw new Error('nothing asked the owner anything')
  stub.say({ ok, checked: false })
  stub.say = null
  await settle()
}

const deletes = (): string[] => stub.sent.filter(call => call.method === 'DELETE').map(call => call.path)
const reads = (): string[] => stub.sent.filter(call => call.method === 'GET').map(call => call.path)

const reload = vi.fn()

const key = (over: Partial<APIKey> = {}): APIKey => ({
  id: 'key-1', name: 'minutes', created_at: '2026-08-25T09:00:00Z', ...over,
})

const laptop = key({ id: 'key-2', name: 'laptop', last_used_at: '2026-08-27T10:00:00Z' })

beforeEach(() => {
  vi.stubGlobal('window', { location: { origin: ORIGIN, reload } })
  reload.mockClear()
  stub.held = [key(), laptop]
  stub.sent.length = 0
  stub.asked.length = 0
  stub.notices.length = 0
  stub.say = null
  stub.refuse = null
  cells.list.length = 0
  cells.at = 0
  cells.effects.length = 0
  cells.dirty = false
  page = null
})

describe('the key list on the API page', () => {
  it('reads the list once when it opens, and draws a row for each key', async () => {
    await open()
    expect(reads()).toEqual(['/api/v1/keys'])
    expect(listed()).toEqual(['minutes', 'laptop'])
  })
})

// Revoking cannot be undone and it stops a running client, so the page asks
// before it deletes. The question is the shared one: these tests read the
// words keys.ts puts in it, so a page that forks its own dialog fails here.
describe('revoking a key from the API page', () => {
  it('asks first, and deletes nothing while the question stands', async () => {
    await open()
    await pressRevoke('laptop')
    expect(stub.asked).toHaveLength(1)
    expect(stub.asked[0].title).toBe('Revoke “laptop”?')
    expect(stub.asked[0].confirmLabel).toBe('Revoke key')
    expect(stub.asked[0].danger).toBe(true)
    expect(deletes()).toEqual([])
    expect(listed()).toEqual(['minutes', 'laptop'])
  })

  it('deletes the key it asked about, once the owner says yes', async () => {
    await open()
    await pressRevoke('laptop')
    await answer(true)
    expect(deletes()).toEqual(['/api/v1/keys/key-2'])
  })

  it('deletes nothing when the owner says no', async () => {
    await open()
    await pressRevoke('laptop')
    await answer(false)
    expect(stub.asked).toHaveLength(1)
    expect(deletes()).toEqual([])
    expect(listed()).toEqual(['minutes', 'laptop'])
  })

  // A dialog that goes away without an answer must not delete either, and the
  // page says so rather than failing where nobody can see it.
  it('deletes nothing when the question never gets an answer', async () => {
    await open()
    await pressRevoke('laptop')
    stub.refuse?.(new Error('the dialog went away'))
    stub.refuse = null
    await settle()
    expect(deletes()).toEqual([])
    expect(stub.notices).toEqual(['Could not revoke the key'])
    expect(listed()).toEqual(['minutes', 'laptop'])
  })
})

// The manager holds the list, so the page has to read it again to be right.
// The revoked key going off the screen is the proof that it did.
describe('the list after a key is revoked', () => {
  it('drops the revoked key, keeps the others, and reloads no page', async () => {
    await open()
    expect(reads()).toHaveLength(1)

    await pressRevoke('laptop')
    await answer(true)

    expect(listed()).toEqual(['minutes'])
    expect(reads()).toHaveLength(2)
    expect(reload).not.toHaveBeenCalled()
  })

  it('keeps every row when the owner says no', async () => {
    await open()
    await pressRevoke('minutes')
    await answer(false)
    expect(listed()).toEqual(['minutes', 'laptop'])
    expect(reads()).toHaveLength(1)
  })
})
