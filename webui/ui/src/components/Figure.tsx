import type { ReactNode } from 'react'

// A reading and its unit, "56 °C" or "7.4 GB": the number in the cell's own
// tabular mono, the unit after it small and quiet in the text face, so a
// figure reads as one quantity and a column of them lines up on the digits.
// Text with no unit to split off is returned as it came.
export function figure(text: string): ReactNode {
  const at = text.lastIndexOf(' ')
  if (at <= 0) return text
  return <>{text.slice(0, at)}<small className="fig-unit">{text.slice(at)}</small></>
}
