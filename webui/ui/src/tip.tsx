import { useId, type ReactNode } from 'react'

// The one tooltip in the console. It carries the sentence a control needs but
// does not want on screen, and it adds no dependency: the text is a real
// element that CSS shows on hover and on focus, and the trigger is a button,
// so the sentence is reachable by pointer and by keyboard alike.
//
// The text is an element rather than a CSS content string on purpose. A
// screen reader can only read what is in the page, so the trigger points at
// this element with aria-describedby and the same words serve both.
//
// With children the tooltip hangs on the words it explains, and those words
// name the trigger. With none it draws the quiet mark beside a control that
// has no words of its own, and label is then the name a screen reader reads.
export function Tip({ text, label, children }: {
  text: string
  label?: string
  children?: ReactNode
}) {
  const id = useId()
  return (
    <span className="tip">
      <button
        type="button"
        className={children ? 'tip-trigger' : 'tip-trigger mark'}
        aria-describedby={id}
        aria-label={children ? undefined : label}
      >
        {children ?? <span aria-hidden="true">?</span>}
      </button>
      <span className="tip-text" role="tooltip" id={id}>{text}</span>
    </span>
  )
}
