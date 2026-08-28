import { api, idempotency, type APIKey } from './api'
import { confirmBox } from './confirm'

// The one API key data flow in the console. The API page and the Minutes page
// both list, create and revoke the same keys, so they call the same four
// functions rather than each holding their own copy of the paths, the
// idempotency header and the words of the revoke question.

const KEYS_PATH = '/api/v1/keys'

export const listKeys = (): Promise<APIKey[]> => api<APIKey[]>(KEYS_PATH)

// A created key answers with its secret once and never again, so the caller
// has to show it before the answer is thrown away.
export const createKey = (name: string): Promise<{ key: APIKey; secret: string }> =>
  api<{ key: APIKey; secret: string }>(KEYS_PATH, {
    method: 'POST',
    headers: idempotency(),
    body: JSON.stringify({ name }),
  })

// Revoking takes effect the moment it is asked for, so it is asked about
// first, in the same words wherever a key is revoked from.
export const confirmRevoke = (key: APIKey) =>
  confirmBox({
    title: `Revoke “${key.name}”?`,
    body: 'Clients using this key stop working immediately.',
    confirmLabel: 'Revoke key',
    danger: true,
  })

export const deleteKey = (key: APIKey): Promise<unknown> =>
  api(`${KEYS_PATH}/${encodeURIComponent(key.id)}`, { method: 'DELETE', body: '{}' })

// The whole revoke: ask, and delete only on a yes. It answers whether the key
// went, so each screen knows whether it has a list to read again. A refused
// question deletes nothing.
export async function revokeKey(key: APIKey): Promise<boolean> {
  const { ok } = await confirmRevoke(key)
  if (!ok) return false
  await deleteKey(key)
  return true
}
