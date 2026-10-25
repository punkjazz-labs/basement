import { useEffect, useRef, type ReactNode } from 'react'

// The console's one drill-down: a panel from the right, about 520px wide, over
// the screen it was opened from (Railway's service drawer, Modal's container
// drawer). The overview stays on screen behind it, so the owner never loses
// their place. Esc, the X and a click on the dimmed page all close it. On a
// phone it is a full-screen sheet (styles.css).

const FOCUSABLE = 'a[href], button:not([disabled]), input:not([disabled]), select, textarea, [tabindex]:not([tabindex="-1"])'

export function Drawer({ label, onClose, children }: {
  // What the drawer is about, for the accessibility tree.
  label: string
  onClose: () => void
  children: ReactNode
}) {
  const panel = useRef<HTMLDivElement>(null)

  // Focus moves into the drawer when it opens and back to whatever opened it
  // when it closes, so a keyboard never loses its place either.
  useEffect(() => {
    const opener = document.activeElement instanceof HTMLElement ? document.activeElement : null
    panel.current?.focus()
    return () => opener?.focus()
  }, [])

  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        event.preventDefault()
        onClose()
        return
      }
      // Tab stays inside the drawer while it is open.
      if (event.key !== 'Tab' || !panel.current) return
      const items = [...panel.current.querySelectorAll<HTMLElement>(FOCUSABLE)]
      if (items.length === 0) return
      const first = items[0]
      const last = items[items.length - 1]
      if (event.shiftKey && document.activeElement === first) {
        event.preventDefault()
        last.focus()
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault()
        first.focus()
      }
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [onClose])

  return (
    <div className="drawer-layer">
      <div className="drawer-scrim" aria-hidden="true" onClick={onClose} />
      <div className="drawer" role="dialog" aria-modal="true" aria-label={label} ref={panel} tabIndex={-1}>
        <button type="button" className="dialog-close drawer-close" onClick={onClose} aria-label="Close">×</button>
        {children}
      </div>
    </div>
  )
}
