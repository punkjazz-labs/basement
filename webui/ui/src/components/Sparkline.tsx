import { useId } from 'react'

// The two ways the console draws a history. A sparkline sits inside a row as
// one more column (Modal's "Active resources", Sentry's trend column): no
// axis, no labels, just the shape and where it ends. A chart is the one chart
// a drawer or a unit card shows, titled with its low, its average and its
// latest reading. Both read the series telemetry.ts already keeps; neither
// collects anything.

export type ChartTone = 'live' | 'plain' | 'warn' | 'fail'

const mean = (points: readonly number[]): number =>
  points.length === 0 ? 0 : points.reduce((sum, value) => sum + value, 0) / points.length

export const average = mean

// How long a window of samples covers, from the stamps telemetry.ts keeps.
// "last 9 min". Empty when there are not two stamps to measure between.
export function windowText(stamps: readonly number[]): string {
  if (stamps.length < 2) return ''
  const span = stamps[stamps.length - 1] - stamps[0]
  if (!(span > 0)) return ''
  if (span < 60_000) return `last ${Math.max(1, Math.round(span / 1000))} s`
  const minutes = Math.round(span / 60_000)
  if (minutes < 120) return `last ${minutes} min`
  return `last ${Math.round(minutes / 60)} h`
}

// A series as points in a box of the given size, spread evenly across it. A
// flat series is drawn through the middle rather than pinned to an edge.
function plotted(points: readonly number[], width: number, height: number, floor?: number, ceiling?: number): [number, number][] {
  if (points.length < 2) return []
  const low = floor ?? Math.min(...points)
  const high = ceiling ?? Math.max(...points)
  const range = high - low
  return points.map((value, index) => [
    (index / (points.length - 1)) * width,
    range > 0 ? height - ((Math.min(Math.max(value, low), high) - low) / range) * height : height / 2,
  ])
}

// The same points, as an SVG points list.
export function linePoints(points: readonly number[], width: number, height: number, floor?: number, ceiling?: number): string {
  return plotted(points, width, height, floor, ceiling).map(([x, y]) => `${x.toFixed(1)},${y.toFixed(1)}`).join(' ')
}

// The scale a line is drawn on: the window's own low and high, with room
// under the line (about 30% of the range) and over it (about 15%), so it
// neither hugs the top nor lies on the floor, and never below zero. Every
// figure on these charts is a reading that cannot go negative. A metric with
// a natural top (a box's memory) is drawn from zero to that top instead.
export function chartDomain(points: readonly number[], max?: number): [number, number] {
  if (max !== undefined && max > 0) return [0, max]
  const low = Math.min(...points)
  const high = Math.max(...points)
  const range = high - low > 0 ? high - low : Math.max(Math.abs(high) * 0.1, 1)
  return [Math.max(0, low - range * 0.3), high + range * 0.15]
}

const sign = (value: number): number => (value < 0 ? -1 : 1)

// A smooth line through the points that never overshoots them: monotone
// cubic interpolation in x (the curve d3 calls monotoneX), so a reading
// between two others is never drawn above or below both of them.
export function smoothPath(xy: readonly [number, number][]): string {
  if (xy.length < 2) return ''
  const at = ([x, y]: readonly [number, number]) => `${x.toFixed(2)},${y.toFixed(2)}`
  if (xy.length === 2) return `M${at(xy[0])}L${at(xy[1])}`
  const slope = (a: readonly [number, number], b: readonly [number, number]) => (b[1] - a[1]) / (b[0] - a[0] || 1)
  const tangents = xy.map((point, index) => {
    if (index === 0 || index === xy.length - 1) return 0
    const before = slope(xy[index - 1], point)
    const after = slope(point, xy[index + 1])
    const h0 = point[0] - xy[index - 1][0]
    const h1 = xy[index + 1][0] - point[0]
    const blend = (before * h1 + after * h0) / (h0 + h1)
    return (sign(before) + sign(after)) * Math.min(Math.abs(before), Math.abs(after), 0.5 * Math.abs(blend)) || 0
  })
  // The two ends lean the way their one neighbour does.
  const last = xy.length - 1
  tangents[0] = (3 * slope(xy[0], xy[1]) - tangents[1]) / 2
  tangents[last] = (3 * slope(xy[last - 1], xy[last]) - tangents[last - 1]) / 2
  let path = `M${at(xy[0])}`
  for (let index = 0; index < last; index += 1) {
    const [x0, y0] = xy[index]
    const [x1, y1] = xy[index + 1]
    const third = (x1 - x0) / 3
    path += `C${at([x0 + third, y0 + third * tangents[index]])} ${at([x1 - third, y1 - third * tangents[index + 1]])} ${at([x1, y1])}`
  }
  return path
}

// A gradient id unique to one drawing. useId's own characters are not all
// safe inside url(#…), so only the plain ones are kept.
const useGradientID = (): string => `fade${useId().replace(/[^a-zA-Z0-9_-]/g, '')}`

// The soft fill under a line: the line's own colour, faint at the line and
// gone well before the floor, so the shape reads and the ground stays dark.
function Fade({ id }: { id: string }) {
  return (
    <defs>
      <linearGradient id={id} x1="0" y1="0" x2="0" y2="1">
        <stop offset="0" stopColor="currentColor" stopOpacity="0.18" />
        <stop offset="0.7" stopColor="currentColor" stopOpacity="0" />
      </linearGradient>
    </defs>
  )
}

const SPARK_W = 64
const SPARK_H = 16

export function Sparkline({ points, label, tone = 'plain', floor, ceiling }: {
  points: readonly number[]
  // What the line is, for a screen reader and the tooltip: "GPU power, last
  // 9 min".
  label: string
  tone?: ChartTone
  floor?: number
  ceiling?: number
}) {
  const fade = useGradientID()
  if (points.length < 2) {
    return <svg className="sparkline empty" viewBox="0 0 64 18" role="img" aria-label={`${label}: collecting samples`} />
  }
  const [low, high] = chartDomain(points)
  const xy = plotted(points, SPARK_W, SPARK_H, floor ?? low, ceiling ?? high)
  const line = smoothPath(xy)
  const [x, y] = xy[xy.length - 1]
  return (
    <svg className={`sparkline ${tone}`} viewBox="-2 -2 68 20" preserveAspectRatio="none" role="img" aria-label={label}>
      <title>{label}</title>
      <Fade id={fade} />
      <path className="area" d={`${line}L${SPARK_W},${SPARK_H}L0,${SPARK_H}Z`} fill={`url(#${fade})`} />
      <path className="line" d={line} vectorEffect="non-scaling-stroke" />
      <circle cx={x} cy={y} r="1.7" />
    </svg>
  )
}

const CHART_W = 100
const CHART_H = 40

// The one chart a drawer or a card carries. The header names the metric and
// gives its low, average and latest reading over the window, which is where
// the figures are read; the plot is drawn to the window's own range (see
// chartDomain), so its shape is what it shows.
export function MetricChart({ points, stamps, title, unit, format, tone = 'plain', max, empty }: {
  points: readonly number[]
  stamps?: readonly number[]
  title: string
  unit: string
  format: (value: number) => string
  tone?: ChartTone
  // The top of the scale, when the metric has a natural one (a box's memory).
  max?: number
  // What to say instead of a chart, when there is nothing to draw.
  empty?: string
}) {
  const fade = useGradientID()
  const drawable = points.length >= 2
  if (!drawable) {
    // Nothing to draw is one quiet line, never an empty frame.
    return (
      <p className="chart-empty">
        <i aria-hidden="true" />
        <span><span className="t">{title}</span> {empty ?? 'Collecting samples. The line starts after the second reading.'}</span>
      </p>
    )
  }
  const [low, high] = chartDomain(points, max)
  const xy = plotted(points, CHART_W, CHART_H, low, high)
  const line = smoothPath(xy)
  const span = stamps ? windowText(stamps.slice(-points.length)) : ''
  const latest = points[points.length - 1]
  const [x, y] = xy[xy.length - 1]
  const stat = (label: string, value: number) => (
    <span className="st"><small>{label}</small><b>{format(value)}</b></span>
  )
  return (
    <figure className={`mchart ${tone}`}>
      <figcaption>
        <span className="t">{title}</span>
        {span && <span className="win">{span}</span>}
        <span className="stats">
          {stat('min', Math.min(...points))}
          {stat('avg', mean(points))}
          {stat('now', latest)}
          <span className="u">{unit}</span>
        </span>
      </figcaption>
      <div className="plot">
        <svg viewBox={`0 0 ${CHART_W} ${CHART_H}`} preserveAspectRatio="none" role="img"
          aria-label={`${title}: now ${format(latest)} ${unit}, average ${format(mean(points))} ${unit}, low ${format(Math.min(...points))} ${unit}`}>
          <Fade id={fade} />
          <line className="grid" x1="0" x2={CHART_W} y1={CHART_H / 2} y2={CHART_H / 2} vectorEffect="non-scaling-stroke" />
          <line className="base" x1="0" x2={CHART_W} y1={CHART_H} y2={CHART_H} vectorEffect="non-scaling-stroke" />
          <path className="area" d={`${line}L${CHART_W},${CHART_H}L0,${CHART_H}Z`} fill={`url(#${fade})`} />
          <path className="line" d={line} vectorEffect="non-scaling-stroke" />
        </svg>
        {/* The latest reading, drawn as a dot in the page rather than in the
            stretched plot, so it stays round at every width. */}
        <i className="end" aria-hidden="true" style={{ left: `${x}%`, top: `${(y / CHART_H) * 100}%` }} />
      </div>
    </figure>
  )
}
