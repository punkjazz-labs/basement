import { createElement } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { describe, expect, it } from 'vitest'
import {
  canvasSizes, canvasTiers, defaultCanvasTier, durationOptions, generationElapsedSeconds,
  generationState, generationTerminal, playableVideoBlob,
} from './generation'
import type { Generation, MediaGenerationConfig } from './api'
import { GenerationProgress } from './views/Generate'

const config = (overrides: Partial<MediaGenerationConfig> = {}): MediaGenerationConfig => ({
  modes: ['text_to_video'],
  default_short_edge: 768,
  max_short_edge: 768,
  max_long_edge: 1344,
  canvas_multiple: 32,
  frame_block: 17,
  frame_offset: 5,
  frames_per_second: 24,
  min_blocks: 1,
  max_blocks: 21,
  default_blocks: 7,
  concurrent_generations: 1,
  max_prompt_length: 8000,
  ...overrides,
})

// The canvas the shipped MiniMax H3 recipe declares.
const h3 = config({ default_short_edge: 768, max_short_edge: 1440, max_long_edge: 2560 })

describe('canvasSizes', () => {
  it('offers a horizontal canvas, which the shape presets it replaced never could', () => {
    const horizontal = canvasSizes(h3, 'horizontal')
    expect(horizontal.length).toBeGreaterThan(0)
    for (const option of horizontal) expect(option.width).toBeGreaterThan(option.height)
  })

  it('turns a clip on its side without changing what it costs to render', () => {
    const pixels = (shape: 'horizontal' | 'vertical' | 'square') =>
      canvasSizes(h3, shape).map(option => option.width * option.height)
    expect(pixels('vertical')).toEqual(pixels('horizontal'))
    expect(canvasSizes(h3, 'vertical').map(option => option.value)).toEqual([
      '576x1024', '864x1536', '1152x2048', '1440x2560',
    ])
    expect(canvasSizes(h3, 'horizontal').map(option => option.value)).toEqual([
      '1024x576', '1536x864', '2048x1152', '2560x1440',
    ])
  })

  it('keeps a square square', () => {
    for (const option of canvasSizes(h3, 'square')) expect(option.width).toBe(option.height)
  })

  it('holds every offered edge to the recipe grid and both declared bounds', () => {
    const recipe = config({ default_short_edge: 384, max_short_edge: 720, max_long_edge: 1280, canvas_multiple: 16 })
    const options = (['horizontal', 'vertical', 'square'] as const).flatMap(shape => canvasSizes(recipe, shape))
    expect(options.length).toBeGreaterThan(0)
    for (const option of options) {
      expect(option.width % recipe.canvas_multiple).toBe(0)
      expect(option.height % recipe.canvas_multiple).toBe(0)
      expect(Math.min(option.width, option.height)).toBeLessThanOrEqual(recipe.max_short_edge)
      expect(Math.max(option.width, option.height)).toBeLessThanOrEqual(recipe.max_long_edge)
    }
  })

  it('drops a rung the long edge cannot hold rather than clamping it to fit', () => {
    expect(canvasTiers(h3)).toEqual([576, 864, 1152, 1440])
    expect(canvasTiers(config({ ...h3, max_long_edge: 1600 }))).toEqual([576, 864])
  })

  it('never offers a canvas far below the one the recipe was written for', () => {
    // 288 x 512 is on the grid and inside both bounds, and is still not a
    // faster version of a 768-pixel canvas.
    expect(canvasTiers(h3)).not.toContain(288)
  })

  it('starts on the rung nearest the recipe default, not the cheapest one', () => {
    expect(defaultCanvasTier(h3)).toBe(864)
    expect(defaultCanvasTier(config({ ...h3, default_short_edge: 1200 }))).toBe(1152)
  })

  it('offers nothing rather than an approximation when the grid excludes 16:9', () => {
    expect(canvasTiers(config({ max_short_edge: 200, max_long_edge: 400 }))).toEqual([])
    expect(canvasSizes(config({ max_short_edge: 200, max_long_edge: 400 }), 'horizontal')).toEqual([])
  })
})

describe('durationOptions', () => {
  it('builds every valid block on the recipe frame grid', () => {
    const options = durationOptions(config({ min_blocks: 1, max_blocks: 3 }))
    expect(options).toEqual([
      { blocks: 1, frames: 22, seconds: 22 / 24, label: '0.9s' },
      { blocks: 2, frames: 39, seconds: 39 / 24, label: '1.6s' },
      { blocks: 3, frames: 56, seconds: 56 / 24, label: '2.3s' },
    ])
  })

  it('honors non-default bounds and frame rates without inventing presets', () => {
    expect(durationOptions(config({ frame_block: 8, frame_offset: 1, frames_per_second: 10, min_blocks: 2, max_blocks: 4 }))).toEqual([
      { blocks: 2, frames: 17, seconds: 1.7, label: '1.7s' },
      { blocks: 3, frames: 25, seconds: 2.5, label: '2.5s' },
      { blocks: 4, frames: 33, seconds: 3.3, label: '3.3s' },
    ])
  })
})

describe('generation state formatting', () => {
  it('uses the store terminal states and gives running a plain action label', () => {
    expect(generationState('queued')).toBe('Queued')
    expect(generationState('running')).toBe('Generating')
    expect(generationState('completed')).toBe('Completed')
    expect(generationState('failed')).toBe('Failed')
    expect(generationState('cancelled')).toBe('Cancelled')
    expect(generationState('interrupted')).toBe('Interrupted')
    expect(['completed', 'failed', 'cancelled', 'interrupted'].every(generationTerminal)).toBe(true)
    expect(generationTerminal('running')).toBe(false)
  })

  it('keeps an unknown server state visible instead of renaming it', () => {
    expect(generationState('new_server_state')).toBe('new_server_state')
  })

  it('computes elapsed time from the server timestamps', () => {
    const running = {
      id: 'gen-1', model_id: 'media', mode: 'text_to_video', prompt: 'fog', blocks: 1,
      short_edge: 768, width: 1152, height: 768, frames: 22, seed: 1, status: 'running',
      created_at: '2026-08-04T10:00:00Z', started_at: '2026-08-04T10:01:00Z',
    } satisfies Generation
    expect(generationElapsedSeconds(running, Date.parse('2026-08-04T10:03:30Z'))).toBe(150)
  })
})

describe('generation progress rendering', () => {
  const running = (progress: Partial<Generation> = {}): Generation => ({
    id: 'gen-1', model_id: 'media', mode: 'text_to_video', prompt: 'fog', blocks: 1,
    short_edge: 768, width: 1152, height: 768, frames: 22, seed: 1, status: 'running',
    created_at: '2026-08-04T10:00:00Z', started_at: '2026-08-04T10:01:00Z',
    ...progress,
  })

  it('renders ComfyUI step counts as determinate progress', () => {
    const markup = renderToStaticMarkup(createElement(GenerationProgress, {
      generation: running({ progress_value: 7, progress_max: 20, progress_phase: '14' }),
    }))
    expect(markup).toContain('role="progressbar"')
    expect(markup).toContain('aria-valuenow="7"')
    expect(markup).toContain('aria-valuemax="20"')
    expect(markup).toContain('width:35%')
    // The runtime's node id is recorded but deliberately not shown: it means
    // nothing to the person reading this screen.
    expect(markup).not.toContain('14')
    expect(markup).toContain('Generating')
    expect(markup).toContain('7 of 20')
    expect(markup).toContain('35%')
    expect(markup).not.toContain('sdot busy')
  })

  it('keeps the indeterminate state when ComfyUI reported no maximum', () => {
    const markup = renderToStaticMarkup(createElement(GenerationProgress, {
      generation: running({ progress_value: 0, progress_max: 0, progress_phase: '14' }),
    }))
    expect(markup).not.toContain('role="progressbar"')
    expect(markup).toContain('sdot busy')
    expect(markup).not.toContain('Node')
    expect(markup).toContain('Generating')
    expect(markup).toContain('Elapsed')
    expect(markup).not.toContain('%')
  })
})

describe('playableVideoBlob', () => {
  it('re-labels an octet-stream blob as video/mp4', () => {
    const blob = new Blob(['x'], { type: 'application/octet-stream' })
    expect(playableVideoBlob(blob).type).toBe('video/mp4')
  })
  it('keeps a blob that already carries a video type', () => {
    const blob = new Blob(['x'], { type: 'video/mp4' })
    expect(playableVideoBlob(blob)).toBe(blob)
  })
})
