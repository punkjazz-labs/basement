import { describe, expect, it } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import { Mark } from './mark'

// The letter a mark draws, with the markup taken off. No browser is needed:
// this renders to a string, the same way the rest of this suite stays pure.
const letterOf = (markup: string) => markup.replace(/<[^>]*>/g, '')

describe('Mark', () => {
  it('draws the lab mark of a recipe it knows', () => {
    const markup = renderToStaticMarkup(
      <Mark recipe={{ model_by: 'DeepSeek' }} recipeIDs={['deepseek-v4-flash-0731-2s']} size={28} />,
    )
    expect(markup).toContain('/logos/deepseek.webp')
    expect(markup).toContain('width="28"')
  })

  // The marks are keyed by lab, so a recipe the feed adds without a console
  // build draws its own lab's logo instead of a letter block between two rows
  // that carry it.
  it('draws the lab mark for a recipe id it has never seen', () => {
    const markup = renderToStaticMarkup(
      <Mark recipe={{ model_by: 'DeepSeek' }} recipeIDs={['deepseek-v5-2s']} size={28} />,
    )
    expect(markup).toContain('/logos/deepseek.webp')
  })

  it('takes the initial from the lab, not from the model name', () => {
    const markup = renderToStaticMarkup(
      <Mark recipe={{ model_by: 'Some New Lab' }} name="Future Model 9000" size={28} />,
    )
    expect(markup).toContain('class="mark"')
    expect(letterOf(markup)).toBe('S')
  })

  it('names the model when it knows no recipe', () => {
    expect(letterOf(renderToStaticMarkup(<Mark name="Zebra weights" size={24} />))).toBe('Z')
  })

  // A model can serve on this Spark whose recipe this console does not hold.
  // App.tsx then passes an undefined name down, and Playground stays mounted
  // under every tab, so a mark that trusted the name would blank the whole
  // console rather than one row. It asks for nothing it cannot do without.
  it('draws an empty mark rather than throwing when it is told nothing', () => {
    expect(() => renderToStaticMarkup(<Mark size={30} />)).not.toThrow()
    expect(letterOf(renderToStaticMarkup(<Mark size={30} />))).toBe('')
    expect(letterOf(renderToStaticMarkup(<Mark recipeIDs={[]} name={undefined} size={30} />))).toBe('')
    expect(renderToStaticMarkup(<Mark size={30} />)).toContain('class="mark"')
  })

  it('draws an empty mark for a name that is only spaces', () => {
    expect(letterOf(renderToStaticMarkup(<Mark name="   " size={24} />))).toBe('')
  })
})
