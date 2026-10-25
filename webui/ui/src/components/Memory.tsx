import { formatBytes } from '../api'
import { loadsRefused } from '../fleetHealth'
import type { BoxHeadroom } from '../headroom'
import { HeadroomBar, headroomTitle } from '../headroomBar'

// A box's free memory in a table cell or a chip: the thin bar with its reserve
// tick (the OpenAI budget bar), the figure, and the words "loads refused"
// only while the memory rule really refuses a new load there. The bar and the
// rule are v2's; this is only the one way every screen now lays them out.
export function MemoryFree({ headroom, refused = true, size = 'thin' }: {
  headroom: BoxHeadroom
  // Whether to say "loads refused" when it is true. A chip has no room for it.
  refused?: boolean
  size?: 'thin' | 'mini'
}) {
  const unread = headroom.state === 'unknown'
  return (
    <span className={`memfree ${headroom.state}`} title={headroomTitle(headroom)}>
      <HeadroomBar headroom={headroom} size={size} />
      <span className="num">{unread ? 'n/a' : formatBytes(headroom.box.availableBytes)}</span>
      {refused && loadsRefused(headroom) && <span className="refused">loads refused</span>}
    </span>
  )
}
