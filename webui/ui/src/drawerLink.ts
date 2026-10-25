// Which detail drawer is open, as the address bar says it: ?box=<name> for a
// box, ?unit=<placement id> for a unit. The drawer is the console's one
// drill-down, so a link or a screenshot can open it directly and the back
// button closes it. Pure: App reads and writes the search string through
// these two functions and nothing else.

export type DrawerTarget = { kind: 'box'; name: string } | { kind: 'unit'; id: string }

const BOX = 'box'
const UNIT = 'unit'

export function readDrawer(search: string): DrawerTarget | null {
  const params = new URLSearchParams(search)
  const box = params.get(BOX)?.trim()
  if (box) return { kind: 'box', name: box }
  const unit = params.get(UNIT)?.trim()
  if (unit) return { kind: 'unit', id: unit }
  return null
}

// The search string with this drawer open, or with none. Every other
// parameter the address carries stays as it was.
export function writeDrawer(search: string, target: DrawerTarget | null): string {
  const params = new URLSearchParams(search)
  params.delete(BOX)
  params.delete(UNIT)
  if (target?.kind === 'box') params.set(BOX, target.name)
  if (target?.kind === 'unit') params.set(UNIT, target.id)
  const next = params.toString()
  return next ? `?${next}` : ''
}

export const sameDrawer = (left: DrawerTarget | null, right: DrawerTarget | null): boolean =>
  left === right || (left !== null && right !== null && left.kind === right.kind &&
    (left.kind === 'box' ? left.name === (right as { name: string }).name : left.id === (right as { id: string }).id))
