import type { Generation, MediaGenerationConfig, StagedMedia } from './api'

export type CanvasShape = 'horizontal' | 'vertical' | 'square'

export interface CanvasShapeOption {
  shape: CanvasShape
  label: string
  ratio: string
}

export interface CanvasSizeOption {
  shortEdge: number
  width: number
  height: number
  label: string
  value: string
}

export interface DurationOption {
  blocks: number
  frames: number
  seconds: number
  label: string
}

// Size is two questions, not one. Which way up the clip is, and how big it is.
// Keeping them apart is what makes a horizontal clip reachable at all: the
// presets this replaced all derived width from the short edge, so every one of
// them came out square or taller than wide and the console could not generate
// a landscape video.
export const canvasShapes = (): CanvasShapeOption[] => [
  { shape: 'horizontal', label: 'Horizontal', ratio: '16:9' },
  { shape: 'vertical', label: 'Vertical', ratio: '9:16' },
  { shape: 'square', label: 'Square', ratio: '1:1' },
]

// ---- Modes ------------------------------------------------------------------

// The two modes basement drives. A recipe declares its own graphs, so a newer
// recipe can name a mode this build has never heard of; the row shows that
// name rather than hiding a mode the engine would accept.
export const MODE_TEXT_TO_VIDEO = 'text_to_video'
export const MODE_IMAGE_TO_VIDEO = 'image_to_video'

export interface ModeOption {
  mode: string
  label: string
}

const MODE_LABELS: Record<string, string> = {
  [MODE_TEXT_TO_VIDEO]: 'Text to video',
  [MODE_IMAGE_TO_VIDEO]: 'Image to video',
}

// A recipe reports its modes sorted by name, which puts image before text.
// The row reads text first instead: it is the mode every media recipe offers
// and the one a new run starts in.
const MODE_ORDER = [MODE_TEXT_TO_VIDEO, MODE_IMAGE_TO_VIDEO]

export const modeLabel = (mode: string): string => MODE_LABELS[mode] ?? mode.replace(/_/g, ' ')

export function modeOptions(config: MediaGenerationConfig): ModeOption[] {
  const known = MODE_ORDER.filter(mode => config.modes.includes(mode))
  const extra = config.modes.filter(mode => !MODE_ORDER.includes(mode))
  return [...known, ...extra].map(mode => ({ mode, label: modeLabel(mode) }))
}

// 16:9 is the shape moving pictures are made in, and unlike 4:3 or 3:2 it
// lands exactly on a 32-pixel grid at useful sizes, so nothing here is a
// rounded approximation of a ratio it does not actually have.
const WIDE_LONG = 16
const WIDE_SHORT = 9

const gcd = (a: number, b: number): number => (b === 0 ? a : gcd(b, a % b))
const lcm = (a: number, b: number): number => a / gcd(a, b) * b

// The rungs of the size ladder, as short edges. A 16:9 canvas is 16k by 9k,
// so k has to be the step that puts both edges on the recipe's own grid; the
// tiers are that step multiplied out until an edge runs past a declared limit.
// The same rungs serve all three shapes, which is the point: turning a clip on
// its side never changes what it costs to render.
export function canvasTiers(config: MediaGenerationConfig): number[] {
  const multiple = config.canvas_multiple
  if (multiple <= 0 || config.max_short_edge <= 0 || config.max_long_edge <= 0) return []
  const step = lcm(multiple / gcd(WIDE_SHORT, multiple), multiple / gcd(WIDE_LONG, multiple))
  if (step <= 0) return []
  const tiers: number[] = []
  for (let k = step; ; k += step) {
    const shortEdge = WIDE_SHORT * k
    const longEdge = WIDE_LONG * k
    if (shortEdge > config.max_short_edge || longEdge > config.max_long_edge) break
    // A canvas far below the one the recipe was written for is a different
    // product rather than a faster one, so the ladder starts at half of it.
    if (shortEdge * 2 >= config.default_short_edge) tiers.push(shortEdge)
  }
  return tiers
}

export function canvasSizes(config: MediaGenerationConfig, shape: CanvasShape): CanvasSizeOption[] {
  return canvasTiers(config).map(shortEdge => {
    const longEdge = shortEdge / WIDE_SHORT * WIDE_LONG
    const [width, height] = shape === 'square'
      ? [shortEdge, shortEdge]
      : shape === 'horizontal' ? [longEdge, shortEdge] : [shortEdge, longEdge]
    return { shortEdge, width, height, value: `${width}x${height}`, label: `${width} × ${height}` }
  })
}

// The rung nearest the canvas the recipe was written for, so a recipe that
// names a default short edge still gets its own size chosen for it even when
// that exact number is not itself on the 16:9 ladder.
export function defaultCanvasTier(config: MediaGenerationConfig): number {
  const tiers = canvasTiers(config)
  if (tiers.length === 0) return 0
  return tiers.reduce((best, tier) => (
    Math.abs(tier - config.default_short_edge) < Math.abs(best - config.default_short_edge) ? tier : best
  ))
}

export interface FitCanvas {
  shape: CanvasShape
  shortEdge: number
  width: number
  height: number
}

// The canvas a staged source image asks for: the largest rung that fits
// inside the image, so the run is never asked to invent pixels the source
// does not have. The shape is read off the source as well, because putting a
// portrait photograph on a landscape canvas is cropping rather than sizing.
// An image smaller than every rung still has to run somewhere, so it gets the
// smallest one instead of nothing.
export function fitCanvasToImage(
  config: MediaGenerationConfig,
  sourceWidth: number,
  sourceHeight: number,
): FitCanvas | null {
  if (sourceWidth <= 0 || sourceHeight <= 0) return null
  const shape: CanvasShape = sourceWidth > sourceHeight
    ? 'horizontal'
    : sourceWidth < sourceHeight ? 'vertical' : 'square'
  const options = canvasSizes(config, shape)
  if (options.length === 0) return null
  const fitting = options.filter(option => option.width <= sourceWidth && option.height <= sourceHeight)
  const chosen = fitting.length > 0 ? fitting[fitting.length - 1] : options[0]
  return { shape, shortEdge: chosen.shortEdge, width: chosen.width, height: chosen.height }
}

// The source and the canvas side by side, under the size picker. It reads the
// two sizes in play right now rather than the fit's own answer, so changing
// the size by hand leaves a true line instead of a stale one.
export const fitArithmetic = (
  sourceWidth: number,
  sourceHeight: number,
  width: number,
  height: number,
): string => `${sourceWidth}×${sourceHeight} → ${width}×${height}`

// ---- The staged first frame -------------------------------------------------

// The formats the staging endpoint accepts. It sniffs the bytes rather than
// trusting a name, and this list is the same closed set, so the file picker
// offers exactly what the Spark will keep.
export const IMAGE_TYPES = ['image/png', 'image/jpeg', 'image/webp'] as const
export const IMAGE_ACCEPT = IMAGE_TYPES.join(',')

// The first file worth sending from a drop or a picker. A file the browser
// gave no type for is still sent: the Spark reads the bytes, and refusing it
// here would be a guess made from a file name.
export function pickImage<T extends { type: string }>(files: readonly T[]): T | null {
  return files.find(file => (IMAGE_TYPES as readonly string[]).includes(file.type)) ?? files[0] ?? null
}

export interface StagedFrame {
  id: string
  name: string
  width: number
  height: number
}

// What the Spark answered about a staged image, joined to the name of the
// file that was sent. The size comes from the answer rather than from a
// decode in this browser, because the Spark measured the bytes it stored. An
// answer carrying neither size cannot fit a canvas, so it is refused here
// instead of fitting to a zero.
export function stagedFrame(name: string, response: StagedMedia): StagedFrame | null {
  if (!response.id || !(response.width > 0) || !(response.height > 0)) return null
  return { id: response.id, name, width: response.width, height: response.height }
}

export const stagedFrameCaption = (frame: StagedFrame): string =>
  `${frame.name} · ${frame.width}×${frame.height}`

const secondsLabel = (seconds: number): string => {
  const rounded = Math.round(seconds * 10) / 10
  return `${Number.isInteger(rounded) ? rounded.toFixed(0) : rounded.toFixed(1)}s`
}

export function durationOptions(config: MediaGenerationConfig): DurationOption[] {
  if (config.frames_per_second <= 0 || config.min_blocks > config.max_blocks) return []
  const options: DurationOption[] = []
  for (let blocks = config.min_blocks; blocks <= config.max_blocks; blocks += 1) {
    const frames = config.frame_block * blocks + config.frame_offset
    const seconds = frames / config.frames_per_second
    options.push({ blocks, frames, seconds, label: secondsLabel(seconds) })
  }
  return options
}

// The word a size's wait carries next to it, in the fewest words that stay
// honest. The shortest measured wait is not "1× the wait": that phrasing
// asks the reader to do arithmetic on a number that means nothing to them,
// so it gets its own plain words instead. Every other entry rounds to a
// whole multiple, because a fractional multiple of a wait is not a number
// anyone plans around.
export function sizeWaitLabel(config: MediaGenerationConfig, shortEdge: number): string {
  const waits = config.size_waits
  if (!waits || waits.length === 0) return ''
  const entry = waits.find(wait => wait.short_edge === shortEdge)
  if (!entry) return ''
  const shortest = Math.min(...waits.map(wait => wait.factor))
  if (entry.factor === shortest) return 'shortest wait'
  return `about ${Math.round(entry.factor)}× the wait`
}

// The one line of context above the size picker. A recipe that measured its
// own waits gets to say so plainly; one that did not still owes the reader a
// warning, just a vaguer one.
export function sizeWaitHint(config: MediaGenerationConfig): string {
  if (config.size_waits && config.size_waits.length > 0) {
    return 'Waits measured for a 5 second clip; longer clips wait more.'
  }
  return 'A bigger size waits much longer, up to hours.'
}

// The same sum durationOptions runs per block, spelled out once more so the
// duration picker can show its own arithmetic next to the slider rather than
// asking the reader to trust a hidden formula.
export function durationArithmetic(config: MediaGenerationConfig, blocks: number): string {
  const frames = config.frame_block * blocks + config.frame_offset
  return `= ${frames} frames at ${config.frames_per_second} fps`
}

export interface GenerationRequest {
  model_id: string
  mode: string
  prompt: string
  blocks: number
  width: number
  height: number
  seed?: number
  first_frame?: string
}

export interface GenerationRequestInput {
  recipeID: string
  mode: string
  prompt: string
  blocks: number
  width: number
  height: number
  seed?: number
  firstFrame?: string | null
}

// The body POST /api/v1/generate is given. mode always travels, even when it
// is the default one, so a request names the graph it wants rather than
// leaning on what the server reads an absent field as. first_frame travels
// only with image mode: the server refuses either mismatch, and a frame still
// staged from an earlier image run must not ride along with a text run.
export function generationRequest(input: GenerationRequestInput): GenerationRequest {
  const request: GenerationRequest = {
    model_id: input.recipeID,
    mode: input.mode,
    prompt: input.prompt.trim(),
    blocks: input.blocks,
    width: input.width,
    height: input.height,
  }
  if (input.seed !== undefined) request.seed = input.seed
  if (input.mode === MODE_IMAGE_TO_VIDEO && input.firstFrame) request.first_frame = input.firstFrame
  return request
}

export interface ReuseValues {
  prompt: string
  shape: CanvasShape
  shortEdge: number
  blocks: number
}

// What "generate another one like this" carries forward from a finished
// generation. The size and duration are read back off the current grid
// rather than off the generation's own numbers, because a recipe update can
// have retired the exact size or block count the original run used; when
// that happens the closest current offering stands in rather than silently
// failing to fill the form. The seed never comes back: a reuse is a new
// take, not a repeat of the same one.
export function reuseValues(generation: Generation, config: MediaGenerationConfig): ReuseValues {
  const shape: CanvasShape = generation.width > generation.height
    ? 'horizontal'
    : generation.width < generation.height ? 'vertical' : 'square'
  const match = canvasSizes(config, shape).find(
    option => option.width === generation.width && option.height === generation.height,
  )
  const duration = durationOptions(config).find(option => option.frames === generation.frames)
  return {
    prompt: generation.prompt,
    shape,
    shortEdge: match ? match.shortEdge : defaultCanvasTier(config),
    blocks: duration ? duration.blocks : config.default_blocks,
  }
}

export const generationTerminal = (status: string): boolean =>
  ['completed', 'failed', 'cancelled', 'interrupted'].includes(status)

export const generationActive = (status: string): boolean =>
  status === 'queued' || status === 'running'

const GENERATION_STATE: Record<string, string> = {
  queued: 'Queued',
  running: 'Generating',
  completed: 'Completed',
  failed: 'Failed',
  cancelled: 'Cancelled',
  interrupted: 'Interrupted',
}

export const generationState = (status: string): string => GENERATION_STATE[status] ?? status

export function generationElapsedSeconds(generation: Generation, nowMs: number): number | null {
  if (!generation.started_at) return null
  const started = Date.parse(generation.started_at)
  if (Number.isNaN(started)) return null
  const end = generation.finished_at ? Date.parse(generation.finished_at) : nowMs
  if (Number.isNaN(end)) return null
  return Math.max((end - started) / 1000, 0)
}

export function formatElapsed(seconds: number): string {
  const whole = Math.max(Math.floor(seconds), 0)
  const hours = Math.floor(whole / 3600)
  const minutes = Math.floor((whole % 3600) / 60)
  const remainder = whole % 60
  if (hours > 0) return `${hours}:${String(minutes).padStart(2, '0')}:${String(remainder).padStart(2, '0')}`
  return `${minutes}:${String(remainder).padStart(2, '0')}`
}

// The file endpoint deliberately serves application/octet-stream so nothing
// from it can render in the page's origin. Safari refuses to play a blob
// with that type, so the console re-labels its own copy before playback.
export const playableVideoBlob = (blob: Blob): Blob =>
  blob.type.startsWith('video/') ? blob : new Blob([blob], { type: 'video/mp4' })

export type GenerationStatusMap = Record<string, string>

export function generationStatusMap(generations: Generation[]): GenerationStatusMap {
  const map: GenerationStatusMap = {}
  for (const generation of generations) map[generation.id] = generation.status
  return map
}

// Ids that just crossed into a state worth telling someone about while they
// are away from this tab: completed or failed. A status already carrying
// that value on the previous map is not a transition, and neither is an id
// seen here for the first time, because a run that finished before the
// console ever looked at it should not chime the moment the console loads.
export function finishedTransitions(previous: GenerationStatusMap, current: GenerationStatusMap): string[] {
  const ids: string[] = []
  for (const [id, status] of Object.entries(current)) {
    if (status !== 'completed' && status !== 'failed') continue
    const before = previous[id]
    if (before === undefined || before === status) continue
    ids.push(id)
  }
  return ids
}

// A run that finishes while the tab is hidden goes on this set so the tab
// title keeps flashing until someone comes back to look. Always returns a
// new Set, even when nothing was added, so callers never have to guess
// whether the result aliases what they passed in.
export function markUnseen(unseen: ReadonlySet<string>, ids: readonly string[]): Set<string> {
  const next = new Set(unseen)
  for (const id of ids) next.add(id)
  return next
}

// The strip and the generations list both want every run newest first. The
// server's created_at is RFC 3339 with a fixed-width fractional part, so
// comparing it as a string already sorts it in time order.
export function sortGenerationsNewestFirst(generations: Generation[]): Generation[] {
  return [...generations].sort((left, right) => right.created_at.localeCompare(left.created_at))
}
