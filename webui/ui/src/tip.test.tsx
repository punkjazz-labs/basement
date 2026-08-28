import { describe, expect, it } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import { Tip } from './tip'

const SENTENCE = 'Cool and quiet caps the chip at 2200 MHz.'

// The id the trigger points at, read off the markup: React makes it, so the
// test reads it rather than expecting one.
const describedBy = (markup: string) => /aria-describedby="([^"]+)"/.exec(markup)?.[1] ?? ''

describe('Tip', () => {
  it('points the trigger at the text a screen reader reads', () => {
    const markup = renderToStaticMarkup(<Tip text={SENTENCE} label="What the power modes do" />)
    const id = describedBy(markup)
    expect(id).not.toBe('')
    expect(markup).toContain(`id="${id}"`)
    expect(markup).toContain('role="tooltip"')
    expect(markup).toContain(SENTENCE)
  })

  // Hover is not the only way in. The trigger is a button, so it takes focus
  // from the keyboard and the CSS opens the same text on focus.
  it('gives the tooltip a focusable trigger', () => {
    const markup = renderToStaticMarkup(<Tip text={SENTENCE} label="What the power modes do" />)
    expect(markup).toContain('<button type="button"')
    expect(markup).toContain('aria-label="What the power modes do"')
  })

  // With words of its own the trigger is named by those words, so a second
  // name would only say the same thing twice.
  it('takes its name from the words it hangs on', () => {
    const markup = renderToStaticMarkup(<Tip text={SENTENCE}>What a role changes</Tip>)
    expect(markup).toContain('What a role changes')
    expect(markup).not.toContain('aria-label')
    expect(markup).toContain('aria-describedby')
  })
})
