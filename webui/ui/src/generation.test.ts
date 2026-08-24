import { createElement } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  canvasSizes, canvasTiers, defaultCanvasTier, durationArithmetic, durationOptions, finishedTransitions,
  fitArithmetic, fitCanvasToImage, generationElapsedSeconds, generationRequest, generationState,
  generationStatusMap, generationTerminal, IMAGE_ACCEPT, markUnseen, modeOptions, pickImage,
  playableVideoBlob, reuseValues, sizeWaitHint, sizeWaitLabel, sortGenerationsNewestFirst, stagedFrame,
  stagedFrameCaption,
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

describe('modeOptions', () => {
  it('reads text to video first, whatever order the recipe reports its graphs in', () => {
    expect(modeOptions(config({ modes: ['image_to_video', 'text_to_video'] }))).toEqual([
      { mode: 'text_to_video', label: 'Text to video' },
      { mode: 'image_to_video', label: 'Image to video' },
    ])
  })

  it('offers exactly what the recipe declares, and nothing else', () => {
    expect(modeOptions(config()).map(option => option.mode)).toEqual(['text_to_video'])
  })

  // A recipe can name a graph this build has never heard of. Hiding it would
  // hide a mode the engine would honour, so the raw name is shown instead.
  it('shows a mode it does not know by its own name', () => {
    expect(modeOptions(config({ modes: ['text_to_video', 'video_to_video'] }))).toEqual([
      { mode: 'text_to_video', label: 'Text to video' },
      { mode: 'video_to_video', label: 'video to video' },
    ])
  })
})

describe('fitCanvasToImage', () => {
  it('picks the largest canvas that fits inside the source image', () => {
    // The rungs are 1024×576, 1536×864, 2048×1152 and 2560×1440.
    expect(fitCanvasToImage(h3, 1620, 912)).toEqual({
      shape: 'horizontal', shortEdge: 864, width: 1536, height: 864,
    })
  })

  it('reads the shape off the source rather than cropping it to the current one', () => {
    expect(fitCanvasToImage(h3, 912, 1620)?.shape).toBe('vertical')
    expect(fitCanvasToImage(h3, 1200, 1200)?.shape).toBe('square')
  })

  it('gives a source smaller than every rung the smallest one', () => {
    expect(fitCanvasToImage(h3, 640, 360)).toEqual({
      shape: 'horizontal', shortEdge: 576, width: 1024, height: 576,
    })
  })

  it('never returns a canvas wider or taller than the source when one fits', () => {
    const fit = fitCanvasToImage(h3, 2100, 1180)
    expect(fit).not.toBeNull()
    expect(fit!.width).toBeLessThanOrEqual(2100)
    expect(fit!.height).toBeLessThanOrEqual(1180)
    expect(fit!.width).toBe(2048)
  })

  it('has nothing to answer when the source has no size or the ladder is empty', () => {
    expect(fitCanvasToImage(h3, 0, 0)).toBeNull()
    expect(fitCanvasToImage(config({ max_short_edge: 200, max_long_edge: 400 }), 1620, 912)).toBeNull()
  })
})

describe('fitArithmetic', () => {
  it('shows the source and the canvas, and nothing it had to guess', () => {
    expect(fitArithmetic(1620, 912, 1536, 864)).toBe('1620×912 → 1536×864')
  })
})

describe('stagedFrame', () => {
  it('joins the name that was sent to the size the Spark measured', () => {
    const frame = stagedFrame('tram-dusk.jpg', { id: 'abc123', bytes: 812_004, width: 1620, height: 912 })
    expect(frame).toEqual({ id: 'abc123', name: 'tram-dusk.jpg', width: 1620, height: 912 })
    expect(stagedFrameCaption(frame!)).toBe('tram-dusk.jpg · 1620×912')
  })

  it('refuses an answer that cannot size a canvas', () => {
    expect(stagedFrame('x.png', { id: 'abc123', bytes: 10, width: 0, height: 912 })).toBeNull()
    expect(stagedFrame('x.png', { id: '', bytes: 10, width: 1620, height: 912 })).toBeNull()
  })
})

describe('pickImage', () => {
  it('takes the first file in a format the staging endpoint keeps', () => {
    const files = [{ type: 'text/plain' }, { type: 'image/webp' }, { type: 'image/png' }]
    expect(pickImage(files)).toBe(files[1])
    expect(IMAGE_ACCEPT).toBe('image/png,image/jpeg,image/webp')
  })

  // The Spark sniffs the bytes, so a file the browser named nothing is still
  // worth sending; refusing it here would be a guess made from a file name.
  it('still sends a file the browser gave no type for', () => {
    const files = [{ type: '' }]
    expect(pickImage(files)).toBe(files[0])
    expect(pickImage([])).toBeNull()
  })
})

describe('generationRequest', () => {
  const base = { recipeID: 'minimax-h3-comfyui-1s', prompt: '  a tram at dusk  ', blocks: 7, width: 1536, height: 864 }

  it('names the mode even when it is the default one', () => {
    expect(generationRequest({ ...base, mode: 'text_to_video' })).toEqual({
      model_id: 'minimax-h3-comfyui-1s', mode: 'text_to_video', prompt: 'a tram at dusk',
      blocks: 7, width: 1536, height: 864,
    })
  })

  it('carries the staged frame in image mode', () => {
    expect(generationRequest({ ...base, mode: 'image_to_video', firstFrame: 'sha256hex' })).toMatchObject({
      mode: 'image_to_video', first_frame: 'sha256hex',
    })
  })

  // The server refuses this pair, and a frame kept in the slot across a
  // switch back to text must not silently turn a text run into an image one.
  it('never carries a staged frame in text mode', () => {
    expect(generationRequest({ ...base, mode: 'text_to_video', firstFrame: 'sha256hex' }))
      .not.toHaveProperty('first_frame')
  })

  it('sends a seed only when one was typed', () => {
    expect(generationRequest({ ...base, mode: 'text_to_video', seed: 0 })).toMatchObject({ seed: 0 })
    expect(generationRequest({ ...base, mode: 'text_to_video' })).not.toHaveProperty('seed')
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

const h3WithWaits = config({
  default_short_edge: 768, max_short_edge: 1440, max_long_edge: 2560,
  size_waits: [{ short_edge: 768, factor: 1 }, { short_edge: 1088, factor: 2.85 }, { short_edge: 1440, factor: 7.3 }],
})

describe('sizeWaitLabel', () => {
  it('names the fastest size plainly instead of claiming it takes one times as long', () => {
    expect(sizeWaitLabel(h3WithWaits, 768)).toBe('shortest wait')
  })

  it('rounds the factor to a whole number of times', () => {
    expect(sizeWaitLabel(h3WithWaits, 1088)).toBe('about 3× the wait')
    expect(sizeWaitLabel(h3WithWaits, 1440)).toBe('about 7× the wait')
  })

  it('gives no label when the recipe measured no waits, or not for this edge', () => {
    expect(sizeWaitLabel(h3, 768)).toBe('')
    expect(sizeWaitLabel(h3WithWaits, 864)).toBe('')
  })
})

describe('sizeWaitHint', () => {
  it('points to the measured waits when the recipe declares them', () => {
    expect(sizeWaitHint(h3WithWaits)).toBe(
      'Waits measured for a 5 second clip; longer clips wait more.',
    )
  })

  it('falls back to a plain warning when the recipe measured nothing', () => {
    expect(sizeWaitHint(h3)).toBe('A bigger size waits much longer, up to hours.')
  })
})

describe('durationArithmetic', () => {
  it('shows the frame count the block picker actually produces', () => {
    expect(durationArithmetic(h3, 7)).toBe('= 124 frames at 24 fps')
  })
})

describe('reuseValues', () => {
  const generation = (overrides: Partial<Generation> = {}): Generation => ({
    id: 'gen-1', model_id: 'media', mode: 'text_to_video', prompt: 'fog over the bay', blocks: 7,
    short_edge: 864, width: 1536, height: 864, frames: 124, seed: 42, status: 'completed',
    created_at: '2026-08-04T10:00:00Z',
    ...overrides,
  })

  it('refills an exact horizontal match, but never the seed', () => {
    const result = reuseValues(generation(), h3)
    expect(result).toEqual({ prompt: 'fog over the bay', shape: 'horizontal', shortEdge: 864, blocks: 7 })
    expect(result).not.toHaveProperty('seed')
  })

  it('refills an exact vertical match', () => {
    const result = reuseValues(generation({ width: 864, height: 1536, frames: 56, blocks: 3 }), h3)
    expect(result).toEqual({ prompt: 'fog over the bay', shape: 'vertical', shortEdge: 864, blocks: 3 })
  })

  it('falls back to the default tier when the exact size is no longer offered', () => {
    const result = reuseValues(generation({ width: 999, height: 999 }), h3)
    expect(result.shape).toBe('square')
    expect(result.shortEdge).toBe(defaultCanvasTier(h3))
  })

  it('falls back to default_blocks when the frame count is off the current grid', () => {
    const result = reuseValues(generation({ frames: 999 }), h3)
    expect(result.blocks).toBe(h3.default_blocks)
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
  // The elapsed line reads the wall clock, and one assertion below says the
  // runtime's node id ("14") appears nowhere in the markup. Against the real
  // clock that line holds an arbitrary number, so the test failed whenever the
  // elapsed time happened to contain 14. A fixed now makes the whole markup
  // the same on every run: 10:01:00 to 10:03:30 is "2:30".
  beforeEach(() => {
    vi.spyOn(Date, 'now').mockReturnValue(Date.parse('2026-08-04T10:03:30Z'))
  })
  afterEach(() => {
    vi.restoreAllMocks()
  })

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
    // The elapsed line is the only part of this markup that could hold a
    // number nobody chose, so it is pinned here as well as frozen above.
    expect(markup).toContain('Elapsed 2:30')
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

describe('generationStatusMap and finishedTransitions', () => {
  const run = (id: string, status: string): Generation => ({
    id, model_id: 'media', mode: 'text_to_video', prompt: 'x', blocks: 1,
    short_edge: 768, width: 1152, height: 768, frames: 22, seed: 1, status,
    created_at: '2026-08-04T10:00:00Z',
  })

  it('maps each generation to its status by id', () => {
    expect(generationStatusMap([run('a', 'running'), run('b', 'completed')])).toEqual({ a: 'running', b: 'completed' })
  })

  it('reports an id that just finished, completed or failed', () => {
    const before = { a: 'running', b: 'queued' }
    const after = { a: 'completed', b: 'failed' }
    expect(finishedTransitions(before, after)).toEqual(['a', 'b'])
  })

  it('does not report a status that has not changed', () => {
    expect(finishedTransitions({ a: 'completed' }, { a: 'completed' })).toEqual([])
  })

  it('does not report an id seen here for the first time, even if it is already finished', () => {
    // A page load populates the previous map for the first time from
    // whatever the server already reports; none of that is a transition,
    // so a run that finished before the console ever looked does not chime.
    expect(finishedTransitions({}, { a: 'completed' })).toEqual([])
  })

  it('ignores a transition into a state nobody is waiting to hear about', () => {
    expect(finishedTransitions({ a: 'running' }, { a: 'cancelled' })).toEqual([])
  })
})

describe('markUnseen', () => {
  it('adds every new id to the set', () => {
    const result = markUnseen(new Set(['a']), ['b', 'c'])
    expect([...result].sort()).toEqual(['a', 'b', 'c'])
  })

  it('returns an equivalent set, not the same reference, when nothing is added', () => {
    const original = new Set(['a'])
    const result = markUnseen(original, [])
    expect(result).not.toBe(original)
    expect([...result]).toEqual(['a'])
  })

  it('does not duplicate an id already marked unseen', () => {
    expect([...markUnseen(new Set(['a']), ['a'])]).toEqual(['a'])
  })
})

describe('sortGenerationsNewestFirst', () => {
  const run = (id: string, createdAt: string): Generation => ({
    id, model_id: 'media', mode: 'text_to_video', prompt: 'x', blocks: 1,
    short_edge: 768, width: 1152, height: 768, frames: 22, seed: 1, status: 'completed',
    created_at: createdAt,
  })

  it('puts the newest created_at first', () => {
    const oldest = run('a', '2026-08-04T10:00:00Z')
    const middle = run('b', '2026-08-04T11:00:00Z')
    const newest = run('c', '2026-08-04T12:00:00Z')
    expect(sortGenerationsNewestFirst([oldest, newest, middle]).map(item => item.id)).toEqual(['c', 'b', 'a'])
  })

  it('does not mutate the array it was given', () => {
    const oldest = run('a', '2026-08-04T10:00:00Z')
    const newest = run('c', '2026-08-04T12:00:00Z')
    const input = [oldest, newest]
    sortGenerationsNewestFirst(input)
    expect(input).toEqual([oldest, newest])
  })

  // The server trims the trailing zeros of the fractional seconds, so the
  // text of the older run can be a prefix of the newer one. Compared as text,
  // ".5Z" reads as newer than ".51Z", which is wrong.
  it('puts the newer run first when its text is longer', () => {
    const older = run('a', '2026-08-04T10:00:00.5Z')
    const newer = run('b', '2026-08-04T10:00:00.51Z')
    expect(sortGenerationsNewestFirst([older, newer]).map(item => item.id)).toEqual(['b', 'a'])
    expect(sortGenerationsNewestFirst([newer, older]).map(item => item.id)).toEqual(['b', 'a'])
  })

  // Two runs of the same millisecond keep the order the server sent, which is
  // already newest first.
  it('keeps the order it was given for two runs that read as the same time', () => {
    const first = run('a', '2026-08-04T10:00:00.1234561Z')
    const second = run('b', '2026-08-04T10:00:00.1234559Z')
    expect(sortGenerationsNewestFirst([first, second]).map(item => item.id)).toEqual(['a', 'b'])
    expect(sortGenerationsNewestFirst([second, first]).map(item => item.id)).toEqual(['b', 'a'])
  })

  // The gallery sorts every snapshot the event stream sends. A new run at the
  // head of that snapshot has to stay at the head.
  it('leaves a new run at the head of an event snapshot', () => {
    const snapshot = [
      run('c', '2026-08-04T10:00:00.51Z'),
      run('b', '2026-08-04T10:00:00.5Z'),
      run('a', '2026-08-04T09:00:00Z'),
    ]
    expect(sortGenerationsNewestFirst(snapshot).map(item => item.id)).toEqual(['c', 'b', 'a'])
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
