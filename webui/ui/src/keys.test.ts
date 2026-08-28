import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { APIKey } from './api'
import { confirmRevoke, createKey, deleteKey, listKeys, revokeKey } from './keys'

// Two screens reach the manager's key list through this module, and neither of
// them names a path, builds the create call or words the revoke question any
// more. Nothing else in the suite would notice if one of those changed, so
// this stands in for the network and for the dialog and reads what was sent.
const stub = vi.hoisted(() => ({
  sent: [] as { path: string; options: RequestInit }[],
  asked: [] as { title: string; body?: string; confirmLabel?: string; danger?: boolean }[],
  answer: { ok: true, checked: false },
}))

vi.mock('./api', () => ({
  api: (path: string, options: RequestInit = {}) => {
    stub.sent.push({ path, options })
    return Promise.resolve([])
  },
  idempotency: () => ({ 'Idempotency-Key': 'stub-uuid' }),
}))

vi.mock('./confirm', () => ({
  confirmBox: (options: { title: string }) => {
    stub.asked.push(options)
    return Promise.resolve(stub.answer)
  },
}))

const key = (over: Partial<APIKey> = {}): APIKey => ({
  id: 'key-1', name: 'minutes', created_at: '2026-08-25T09:00:00Z', ...over,
})

const headers = (options: RequestInit): Record<string, string> =>
  (options.headers ?? {}) as Record<string, string>

beforeEach(() => {
  stub.sent.length = 0
  stub.asked.length = 0
  stub.answer = { ok: true, checked: false }
})

describe('the shared key flow', () => {
  it('reads the list from the manager key path', async () => {
    await listKeys()
    expect(stub.sent).toHaveLength(1)
    expect(stub.sent[0].path).toBe('/api/v1/keys')
    expect(stub.sent[0].options.method ?? 'GET').toBe('GET')
  })

  // A create that is sent twice must not mint two keys, so the header goes
  // with it. It is the one call in this module that carries one.
  it('creates a key by name, under an idempotency key', async () => {
    await createKey('minutes')
    expect(stub.sent).toHaveLength(1)
    expect(stub.sent[0].path).toBe('/api/v1/keys')
    expect(stub.sent[0].options.method).toBe('POST')
    expect(headers(stub.sent[0].options)['Idempotency-Key']).toBe('stub-uuid')
    expect(stub.sent[0].options.body).toBe('{"name":"minutes"}')
  })

  it('deletes one key, by an id it escapes', async () => {
    await deleteKey(key({ id: 'laptop key/2' }))
    expect(stub.sent).toHaveLength(1)
    expect(stub.sent[0].path).toBe('/api/v1/keys/laptop%20key%2F2')
    expect(stub.sent[0].options.method).toBe('DELETE')
    expect(stub.sent[0].options.body).toBe('{}')
  })

  // The words of the question and the red dialog are part of the flow: the
  // key stops working the moment the owner says yes.
  it('asks the revoke question in the console words, in red', async () => {
    await confirmRevoke(key())
    expect(stub.asked).toEqual([{
      title: 'Revoke “minutes”?',
      body: 'Clients using this key stop working immediately.',
      confirmLabel: 'Revoke key',
      danger: true,
    }])
  })
})

describe('revoking a key', () => {
  it('asks first and deletes nothing on a no', async () => {
    stub.answer = { ok: false, checked: false }
    expect(await revokeKey(key())).toBe(false)
    expect(stub.asked).toHaveLength(1)
    expect(stub.sent).toHaveLength(0)
  })

  it('deletes the key on a yes, and says it went', async () => {
    expect(await revokeKey(key())).toBe(true)
    expect(stub.sent).toHaveLength(1)
    expect(stub.sent[0].options.method).toBe('DELETE')
    expect(stub.sent[0].path).toBe('/api/v1/keys/key-1')
  })
})
