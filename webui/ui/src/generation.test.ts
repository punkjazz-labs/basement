import { createElement } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { describe, expect, it } from 'vitest'
import {
  canvasOptions, durationOptions, generationElapsedSeconds, generationState, generationTerminal,
  type CanvasOption,
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

describe('canvasOptions', () => {
  it('derives the approved shapes from the recipe canvas', () => {
    expect(canvasOptions(config()).map((option: CanvasOption) => option.value)).toEqual([
      '768x768',
      '768x1152',
      '544x960',
      '768x1344',
    ])
  })

  it('keeps every offered dimension on the declared grid and inside both bounds', () => {
    const recipe = config({ default_short_edge: 384, max_short_edge: 512, max_long_edge: 768, canvas_multiple: 16 })
    const options = canvasOptions(recipe)
    expect(options.length).toBeGreaterThan(0)
    for (const option of options) {
      expect(option.width % recipe.canvas_multiple).toBe(0)
      expect(option.height % recipe.canvas_multiple).toBe(0)
      expect(Math.min(option.width, option.height)).toBeLessThanOrEqual(recipe.max_short_edge)
      expect(Math.max(option.width, option.height)).toBeLessThanOrEqual(recipe.max_long_edge)
    }
  })

  it('omits a preset the recipe canvas excludes instead of clamping it', () => {
    expect(canvasOptions(config({ max_long_edge: 1200 })).map(option => option.value)).toEqual([
      '768x768',
      '768x1152',
      '544x960',
    ])
  })

  it('omits relative shapes that do not land exactly on the recipe grid', () => {
    expect(canvasOptions(config({ default_short_edge: 640, max_short_edge: 640, max_long_edge: 1120 }))).toEqual([
      { width: 640, height: 640, value: '640x640', label: '640 × 640' },
      { width: 640, height: 960, value: '640x960', label: '640 × 960' },
      { width: 640, height: 1120, value: '640x1120', label: '640 × 1120' },
    ])
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
