import type { Banner, BoxHealth, FleetHealthSummary } from '../fleetHealth'

// The line under the page title on Models, Sparks and Monitor: one dot per
// box, then the fleet in one phrase. It is all-smi's LED grid and the AWS
// health widget in one row: six boxes in sixty pixels, and words only for the
// boxes that need them. Every dot opens that box's drawer.

const LIGHT_WORD = { ok: 'healthy', warn: 'needs a look', fail: 'needs attention' } as const

export const dotTitle = (box: BoxHealth): string =>
  `${box.name}: ${box.issues.length > 0 ? box.issues.map(issue => issue.text).join(', ') : LIGHT_WORD.ok}${
    box.stale ? ' (heartbeat stale)' : ''}`

// One box's light, wherever a box is named: the strip, a table row, a drawer
// header. Hollow while its heartbeat is stale.
export function HealthDot({ box }: { box?: BoxHealth }) {
  if (!box) return <i className="hdot unknown" aria-hidden="true" />
  return <i className={`hdot ${box.light} ${box.stale ? 'stale' : ''}`} aria-hidden="true" title={dotTitle(box)} />
}

export function FleetHealthLine({ summary, boxes, onOpenBox }: {
  summary: FleetHealthSummary
  boxes: readonly BoxHealth[]
  onOpenBox: (box: BoxHealth) => void
}) {
  if (summary.total === 0) return null
  return (
    <div className={`health-line ${summary.light}`}>
      <div className="leds" role="group" aria-label="Boxes">
        {boxes.map(box => (
          <button
            type="button"
            key={box.id}
            className={`led ${box.light} ${box.stale ? 'stale' : ''}`}
            title={dotTitle(box)}
            aria-label={`Open ${dotTitle(box)}`}
            onClick={() => onOpenBox(box)}
          />
        ))}
      </div>
      <span className="health-text">{summary.text}</span>
    </div>
  )
}

// The amber line that appears only while something is wrong, with the one
// action that leads to it (Vercel's inline banner, Better Stack's
// incidents-first home). A fleet with nothing wrong renders nothing here.
export function AttentionBanner({ banner, onOpen }: { banner: Banner | null; onOpen: (boxID: string) => void }) {
  if (!banner) return null
  return (
    <section className={`attention ${banner.light}`} aria-label="Needs attention">
      <i className="attention-mark" aria-hidden="true">!</i>
      <div className="attention-copy">
        <strong>{banner.title}</strong>
        {banner.body && <span>{banner.body}</span>}
      </div>
      <button type="button" className="ghost" onClick={() => onOpen(banner.boxID)}>Open {banner.boxName}</button>
    </section>
  )
}
