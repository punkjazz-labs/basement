import type { Generation, MediaGenerationConfig } from './api'

export interface CanvasOption {
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

// The approved four shapes expressed relative to the recipe's own default
// short edge. A different model therefore gets shapes on its canvas rather
// than MiniMax-sized literals. A ratio that does not land exactly on that
// recipe's grid is omitted, never rounded or clamped.
const CANVAS_PRESETS = [
  { width: [1, 1], height: [1, 1] },
  { width: [1, 1], height: [3, 2] },
  { width: [17, 24], height: [5, 4] },
  { width: [1, 1], height: [7, 4] },
] as const

const scaled = (base: number, [numerator, denominator]: readonly [number, number]): number | null => {
  const value = base * numerator
  return value % denominator === 0 ? value / denominator : null
}

export function canvasOptions(config: MediaGenerationConfig): CanvasOption[] {
  const seen = new Set<string>()
  const options: CanvasOption[] = []
  for (const preset of CANVAS_PRESETS) {
    const width = scaled(config.default_short_edge, preset.width)
    const height = scaled(config.default_short_edge, preset.height)
    if (width === null || height === null || width <= 0 || height <= 0) continue
    if (width % config.canvas_multiple !== 0 || height % config.canvas_multiple !== 0) continue
    if (Math.min(width, height) > config.max_short_edge) continue
    if (Math.max(width, height) > config.max_long_edge) continue
    const value = `${width}x${height}`
    if (seen.has(value)) continue
    seen.add(value)
    options.push({ width, height, value, label: `${width} × ${height}` })
  }
  return options
}

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

export const generationMode = (mode: string): string => {
  if (mode === 'text_to_video') return 'Text to video'
  if (mode === 'image_to_video') return 'Image to video'
  return mode
}
