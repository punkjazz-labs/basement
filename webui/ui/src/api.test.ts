import { afterEach, describe, expect, it, vi } from 'vitest'
import { api, ApiError, formatTokens, OfflineError, runtimeLabel } from './api'

const jsonResponse = (body: unknown, init: { status?: number; ok?: boolean } = {}) => ({
  ok: init.ok ?? true,
  status: init.status ?? 200,
  statusText: 'status text',
  json: async () => body,
}) as Response

describe('api', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('reports a network-level failure as an OfflineError, not the raw fetch rejection', async () => {
    // This is the shape a real network failure takes: fetch() rejecting with
    // a TypeError, the same thing a dropped Spark, a killed Wi-Fi connection
    // or a manager mid-restart all produce indistinguishably.
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('Failed to fetch')))
    await expect(api('/api/v1/jobs/1/cancel', { method: 'POST' })).rejects.toThrow(OfflineError)
    await expect(api('/api/v1/jobs/1/cancel', { method: 'POST' })).rejects.toThrow(
      'Cannot reach this Spark. It may be offline, or its manager may be restarting. Try again once it answers.',
    )
  })

  it('still raises ApiError with the server’s own message for a real HTTP error response', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(jsonResponse({ error: 'a deployment is already running' }, { ok: false, status: 409 })),
    )
    const problem = await api('/api/v1/models/x/install', { method: 'POST' }).catch(error => error)
    expect(problem).toBeInstanceOf(ApiError)
    expect(problem).not.toBeInstanceOf(OfflineError)
    expect((problem as ApiError).status).toBe(409)
    expect((problem as ApiError).message).toBe('a deployment is already running')
  })

  it('lets a genuine user-initiated abort through as itself, not as the machine being unreachable', async () => {
    const abort = new DOMException('The user aborted a request.', 'AbortError')
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(abort))
    const problem = await api('/v1/whatever').catch(error => error)
    expect(problem).toBe(abort)
    expect(problem).not.toBeInstanceOf(OfflineError)
  })

  it('returns the parsed body on success without touching either error path', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse({ ok: true })))
    await expect(api('/api/v1/system')).resolves.toEqual({ ok: true })
  })
})

describe('formatTokens', () => {
  it('keeps small counts exact and shortens large ones', () => {
    expect(formatTokens(0)).toBe('0')
    expect(formatTokens(842)).toBe('842')
    expect(formatTokens(1200)).toBe('1.2K')
    expect(formatTokens(1_200_000)).toBe('1.2M')
    expect(formatTokens(3_400_000_000)).toBe('3.4B')
    expect(formatTokens(250_000_000)).toBe('250M')
  })
})

describe('runtimeLabel', () => {
  it('spells each shipped runtime the way its own project does', () => {
    expect(runtimeLabel('vllm')).toBe('vLLM')
    expect(runtimeLabel('sglang')).toBe('SGLang')
    expect(runtimeLabel('llamacpp')).toBe('llama.cpp')
  })

  // Naming an unknown runtime after one it is not would be a lie the console
  // has no way to notice; the recipe's own word is the honest fallback.
  it('keeps an unknown kind as itself and says nothing when there is none', () => {
    expect(runtimeLabel('tensorrt')).toBe('tensorrt')
    expect(runtimeLabel(undefined)).toBe('the pinned runtime')
    expect(runtimeLabel('')).toBe('the pinned runtime')
  })
})
