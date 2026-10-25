import { formatBytes } from './api'
import type { BoxHeadroom } from './headroom'

// One box's memory, drawn the same way wherever a box appears: how much is in
// use, and a tick where the reserve begins. A box past that tick is tight and
// turns amber; only a box with almost nothing left turns red. Green stays the
// serving light and is never used here.

const STATE_WORD: Record<BoxHeadroom['state'], string> = {
  ok: '',
  tight: 'Tight',
  full: 'Out of memory',
  unknown: 'Not reported',
}

// "18.9 GB free", or n/a for a box that has reported nothing.
export const freeText = (headroom: BoxHeadroom): string =>
  headroom.state === 'unknown' ? 'n/a' : `${formatBytes(headroom.box.availableBytes)} free`

// "18.9 / 132 GB": free of total, with the unit said once when both share it.
export function freeOfTotal(headroom: BoxHeadroom): string {
  if (headroom.state === 'unknown') return 'n/a'
  const free = formatBytes(headroom.box.availableBytes)
  const total = formatBytes(headroom.box.totalBytes)
  const unit = total.split(' ')[1]
  return free.endsWith(` ${unit}`) ? `${free.slice(0, -unit.length - 1)} / ${total}` : `${free} / ${total}`
}

// The sentence behind the bar, for its tooltip and for a screen reader.
export function headroomTitle(headroom: BoxHeadroom): string {
  if (headroom.state === 'unknown') return `${headroom.box.name}: memory not reported yet`
  const word = STATE_WORD[headroom.state]
  return `${headroom.box.name}: ${formatBytes(headroom.box.availableBytes)} free of ${
    formatBytes(headroom.box.totalBytes)}, ${formatBytes(headroom.reserve)} kept free${word ? ` · ${word}` : ''}`
}

export function HeadroomBar({ headroom, size = 'thin' }: { headroom: BoxHeadroom; size?: 'thin' | 'mini' }) {
  const title = headroomTitle(headroom)
  return (
    <span className={`hbar ${size} ${headroom.state}`} role="img" aria-label={title} title={title}>
      {headroom.state !== 'unknown' && (
        <>
          <span className="fill" style={{ width: `${(headroom.usedFraction * 100).toFixed(1)}%` }} />
          <span className="tick" style={{ left: `${((1 - headroom.reserveFraction) * 100).toFixed(1)}%` }} />
        </>
      )}
    </span>
  )
}
