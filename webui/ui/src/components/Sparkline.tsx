// The two ways the console draws a history. A sparkline sits inside a row as
// one more column (Modal's "Active resources", Sentry's trend column): no
// axis, no labels, just the shape. A chart is the one chart a drawer or a
// unit card shows, titled with its average. Both read the series telemetry.ts
// already keeps; neither collects anything.

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

// The polyline for a series, scaled into a box of the given size. A flat
// series is drawn through the middle rather than pinned to an edge.
export function linePoints(points: readonly number[], width: number, height: number, floor?: number, ceiling?: number): string {
  if (points.length < 2) return ''
  const low = floor ?? Math.min(...points)
  const high = ceiling ?? Math.max(...points)
  const range = high - low
  return points.map((value, index) => {
    const x = (index / (points.length - 1)) * width
    const y = range > 0 ? height - ((Math.min(Math.max(value, low), high) - low) / range) * height : height / 2
    return `${x.toFixed(1)},${y.toFixed(1)}`
  }).join(' ')
}

export function Sparkline({ points, label, tone = 'plain', floor, ceiling }: {
  points: readonly number[]
  // What the line is, for a screen reader and the tooltip: "GPU power, last
  // 9 min".
  label: string
  tone?: ChartTone
  floor?: number
  ceiling?: number
}) {
  const coords = linePoints(points, 64, 16, floor, ceiling)
  if (!coords) {
    return <svg className="sparkline empty" viewBox="0 0 64 18" role="img" aria-label={`${label}: collecting samples`} />
  }
  const last = coords.split(' ').pop()?.split(',') ?? ['64', '9']
  return (
    <svg className={`sparkline ${tone}`} viewBox="-1 -1 66 18" preserveAspectRatio="none" role="img" aria-label={label}>
      <title>{label}</title>
      <polyline points={coords} vectorEffect="non-scaling-stroke" />
      <circle cx={last[0]} cy={last[1]} r="1.4" />
    </svg>
  )
}

const CHART_W = 100
const CHART_H = 40

// The one chart a drawer or a card carries. The header says what it is and
// its average over the window; the plot runs from zero, so two charts of the
// same metric read on the same scale.
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
  const drawable = points.length >= 2
  const top = max ?? Math.max(...points, 0) * 1.15
  const coords = drawable ? linePoints(points, CHART_W, CHART_H, 0, top > 0 ? top : 1) : ''
  const span = stamps ? windowText(stamps.slice(-points.length)) : ''
  const latest = points.length > 0 ? points[points.length - 1] : undefined
  return (
    <figure className={`mchart ${tone}`}>
      <figcaption>
        <span className="t">{title}</span>
        {drawable && (
          <span className="avg">
            {format(mean(points))}<small>{unit} avg</small>
          </span>
        )}
      </figcaption>
      {drawable ? (
        <div className="plot">
          <svg viewBox={`0 0 ${CHART_W} ${CHART_H}`} preserveAspectRatio="none" role="img"
            aria-label={`${title}: now ${latest === undefined ? 'n/a' : `${format(latest)} ${unit}`}, average ${format(mean(points))} ${unit}`}>
            <line className="grid" x1="0" x2={CHART_W} y1={CHART_H / 3} y2={CHART_H / 3} vectorEffect="non-scaling-stroke" />
            <line className="grid" x1="0" x2={CHART_W} y1={(CHART_H * 2) / 3} y2={(CHART_H * 2) / 3} vectorEffect="non-scaling-stroke" />
            <polygon className="area" points={`0,${CHART_H} ${coords} ${CHART_W},${CHART_H}`} />
            <polyline points={coords} vectorEffect="non-scaling-stroke" />
          </svg>
          <span className="ymax">{format(top)}</span>
          <div className="axis">
            <span>{span || `${points.length} samples`}</span>
            <span>now {latest === undefined ? 'n/a' : format(latest)} {unit}</span>
          </div>
        </div>
      ) : (
        <p className="mchart-empty">{empty ?? 'Collecting samples. The line starts after the second reading.'}</p>
      )}
    </figure>
  )
}
