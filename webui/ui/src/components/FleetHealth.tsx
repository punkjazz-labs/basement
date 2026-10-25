import { bannerItems, type BoxHealth, type FleetHealthSummary } from '../fleetHealth'
import { Icon } from './Icon'

// How the fleet is doing, under the page title on Models, Sparks and Monitor.
// One element in either state: a calm fleet is one quiet line, and a fleet
// with something wrong is one banner that lists every box that needs a look,
// worst first, each with the way in to it. Nothing says the same thing twice.

const LIGHT_WORD = { ok: 'healthy', warn: 'needs a look', fail: 'needs attention' } as const

export const dotTitle = (box: BoxHealth): string =>
  `${box.name}: ${box.issues.length > 0 ? box.issues.map(issue => issue.text).join(', ') : LIGHT_WORD.ok}${
    box.stale ? ' (heartbeat stale)' : ''}`

// One box's light, wherever a box is named: a table row, a chip, a drawer
// header. Hollow while its heartbeat is stale.
export function HealthDot({ box }: { box?: BoxHealth }) {
  if (!box) return <i className="hdot unknown" aria-hidden="true" />
  return <i className={`hdot ${box.light} ${box.stale ? 'stale' : ''}`} aria-hidden="true" title={dotTitle(box)} />
}

// "2 of 6 boxes need attention", or the one box's name when there is one.
export function attentionTitle(summary: FleetHealthSummary): string {
  const count = summary.attention.length
  if (summary.total === 1) return `${summary.attention[0]?.name ?? 'This box'} needs attention`
  return `${count} of ${summary.total} boxes ${count === 1 ? 'needs' : 'need'} attention`
}

export function FleetStatus({ summary, boxes, onOpenBox }: {
  summary: FleetHealthSummary
  boxes: readonly BoxHealth[]
  onOpenBox: (boxID: string) => void
}) {
  if (summary.total === 0) return null
  if (summary.attention.length === 0) {
    return (
      <p className="fleet-calm" role="status">
        <i className="hdot ok" aria-hidden="true" />
        {summary.text}
      </p>
    )
  }
  return (
    <section className={`attention ${summary.light}`} aria-label="Needs attention">
      <header className="attention-head">
        <strong>{attentionTitle(summary)}</strong>
        {/* Every box at a glance, one light each (all-smi's LED grid). A
            dot opens its box, calm or not. */}
        {boxes.length > 1 && (
          <div className="leds" role="group" aria-label="Boxes">
            {boxes.map(box => (
              <button
                type="button"
                key={box.id}
                className={`led ${box.light} ${box.stale ? 'stale' : ''}`}
                title={dotTitle(box)}
                aria-label={`Open ${dotTitle(box)}`}
                onClick={() => onOpenBox(box.id)}
              />
            ))}
          </div>
        )}
      </header>
      <ul className="attention-list">
        {bannerItems(summary).map(item => (
          <li key={item.boxID} className={item.light}>
            <Icon name={item.light === 'fail' ? 'failure' : 'alert'} className="at-icon" />
            <span className="at-copy">
              <strong>{item.title}</strong>
              {item.body && <span>{item.body}</span>}
            </span>
            <button type="button" className="ghost at-open" aria-label={`Open ${item.boxName}`} onClick={() => onOpenBox(item.boxID)}>
              Open<Icon name="chevronRight" size={14} />
            </button>
          </li>
        ))}
      </ul>
    </section>
  )
}
