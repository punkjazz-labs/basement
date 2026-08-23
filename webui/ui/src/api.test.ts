import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  api, apiBlob, ApiError, apiUpload, formatTokens, installConfirmationsComplete, installRequest,
  licenceArtifacts, OfflineError, runtimeLabel, setCSRF, territoryEligibilityLabel,
  type Artifact, type Recipe,
} from './api'

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
      'Cannot reach this Spark. It may be offline or restarting. Try again soon.',
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

  it('fetches an authenticated attachment as a Blob for local playback', async () => {
    const file = new Blob(['video bytes'], { type: 'application/octet-stream' })
    const fetch = vi.fn().mockResolvedValue({ ...jsonResponse({}), blob: async () => file })
    vi.stubGlobal('fetch', fetch)

    await expect(apiBlob('/api/v1/generations/gen-1/file')).resolves.toBe(file)
    expect(fetch).toHaveBeenCalledWith(
      '/api/v1/generations/gen-1/file',
      expect.objectContaining({ credentials: 'same-origin' }),
    )
  })

  it('stages an image as multipart with the same authentication a JSON mutation carries', async () => {
    setCSRF('csrf-token')
    const fetch = vi.fn().mockResolvedValue(
      jsonResponse({ id: 'sha256hex', bytes: 4, width: 1620, height: 912 }),
    )
    vi.stubGlobal('fetch', fetch)

    await expect(apiUpload('/api/v1/generations/media', new Blob(['png!']), 'tram.png'))
      .resolves.toEqual({ id: 'sha256hex', bytes: 4, width: 1620, height: 912 })

    const [path, init] = fetch.mock.calls[0] as [string, RequestInit]
    expect(path).toBe('/api/v1/generations/media')
    expect(init.method).toBe('POST')
    expect(init.credentials).toBe('same-origin')
    const headers = new Headers(init.headers)
    expect(headers.get('X-CSRF-Token')).toBe('csrf-token')
    // Only the browser can write the multipart boundary, so setting a
    // content type here would produce a body the server cannot parse.
    expect(headers.get('Content-Type')).toBeNull()
    const form = init.body as FormData
    expect(form).toBeInstanceOf(FormData)
    expect((form.get('file') as File).name).toBe('tram.png')
  })

  it('surfaces the staging endpoint’s own refusal', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(
      jsonResponse({ error: 'not an image' }, { ok: false, status: 415 }),
    ))
    const problem = await apiUpload('/api/v1/generations/media', new Blob(['x']), 'notes.txt').catch(error => error)
    expect(problem).toBeInstanceOf(ApiError)
    expect((problem as ApiError).status).toBe(415)
    expect((problem as ApiError).message).toBe('not an image')
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
    expect(runtimeLabel('comfyui')).toBe('ComfyUI')
  })

  // Naming an unknown runtime after one it is not would be a lie the console
  // has no way to notice; the recipe's own word is the honest fallback.
  it('keeps an unknown kind as itself and says nothing when there is none', () => {
    expect(runtimeLabel('tensorrt')).toBe('tensorrt')
    expect(runtimeLabel(undefined)).toBe('the pinned runtime')
    expect(runtimeLabel('')).toBe('the pinned runtime')
  })
})

const artifact = (overrides: Partial<Artifact> = {}): Artifact => ({
  role: 'primary',
  repository: 'publisher/model',
  revision: 'revision',
  expected_bytes: 1,
  licence: 'Model Community Licence',
  licence_url: 'https://huggingface.co/publisher/model/blob/revision/LICENSE',
  ...overrides,
})

const recipeWith = (...artifacts: Artifact[]): Recipe => ({ artifacts } as Recipe)

describe('install confirmations', () => {
  it('puts the checkbox values into the install request instead of constants', () => {
    expect(installRequest(false, true, false)).toEqual({
      confirmed: true,
      accept_licence: false,
      confirm_territory_eligibility: true,
      activate: false,
    })
    expect(installRequest(true, false, true)).toEqual({
      confirmed: true,
      accept_licence: true,
      confirm_territory_eligibility: false,
      activate: true,
    })
  })

  it('requires both confirmations when the recipe excludes territories', () => {
    const recipe = recipeWith(artifact({ licence_territory_exclusions: ['Northern Reach'] }))
    expect(installConfirmationsComplete(recipe, false, false)).toBe(false)
    expect(installConfirmationsComplete(recipe, true, false)).toBe(false)
    expect(installConfirmationsComplete(recipe, false, true)).toBe(false)
    expect(installConfirmationsComplete(recipe, true, true)).toBe(true)
  })

  it('requires only licence acceptance when the recipe has no territory exclusions', () => {
    const recipe = recipeWith(artifact())
    expect(territoryEligibilityLabel(recipe)).toBeUndefined()
    expect(installConfirmationsComplete(recipe, false, true)).toBe(false)
    expect(installConfirmationsComplete(recipe, true, false)).toBe(true)
  })

  it('uses the recipe territory names in the self-attestation label', () => {
    const recipe = recipeWith(artifact({
      licence_territory_exclusions: ['Northern Reach', 'Southern Reach', 'Island Reach'],
    }))
    expect(territoryEligibilityLabel(recipe)).toBe(
      'I confirm I am not located in Northern Reach, Southern Reach or Island Reach',
    )
  })

  it('returns every artifact that carries licence data for display', () => {
    const primary = artifact()
    const drafter = artifact({
      role: 'drafter',
      repository: 'publisher/drafter',
      licence: 'Drafter Licence',
      licence_url: 'https://huggingface.co/publisher/drafter/blob/revision/LICENSE',
    })
    expect(licenceArtifacts(recipeWith(primary, drafter))).toEqual([primary, drafter])
  })
})
