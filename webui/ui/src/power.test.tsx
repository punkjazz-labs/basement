/// <reference types="vite/client" />
import { describe, expect, it } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import { PowerFanOut, PowerSwitch } from './views/Fleet'
import { COOL_MODE, COOL_MODE_LABEL, FULL_MODE, FULL_MODE_LABEL, localPowerRow } from './fleetModels'
import modelsSource from './views/Models.tsx?raw'
import fleetSource from './views/Fleet.tsx?raw'

describe('the power switch', () => {
  it('draws both modes and marks the one the Spark holds', () => {
    const markup = renderToStaticMarkup(
      <PowerSwitch name="spark-a393" power={localPowerRow({ mode: COOL_MODE, failure: '' }, false)} onSet={() => {}} />,
    )
    expect(markup).toContain(FULL_MODE_LABEL)
    expect(markup).toContain(COOL_MODE_LABEL)
    expect(markup).toContain('aria-label="Power mode for spark-a393"')
    // The cool button is the pressed one, and the other is not.
    expect(/aria-pressed="true">Cool and quiet/.test(markup)).toBe(true)
    expect(/aria-pressed="false">Full speed/.test(markup)).toBe(true)
  })

  // A Spark that has reported no mode gets a dead switch: nothing here
  // guesses that a silent machine runs at full speed.
  it('is dead while the mode is unknown', () => {
    const markup = renderToStaticMarkup(
      <PowerSwitch name="This Spark" power={localPowerRow(null, false)} onSet={() => {}} />,
    )
    expect(markup.match(/disabled/g)).toHaveLength(2)
    expect(markup).not.toContain('aria-pressed="true"')
  })

  // A GPU that refused the cap says so in its own words, on the screen. A
  // refusal is not tooltip material.
  it('says a refusal out loud', () => {
    const power = localPowerRow({ mode: FULL_MODE, failure: 'The driver did not take the clock cap.' }, false)
    const markup = renderToStaticMarkup(<PowerSwitch name="spark-f1cc" power={power} onSet={() => {}} />)
    expect(markup).toContain('The driver did not take the clock cap.')
  })
})

// The one change that reaches every Spark. It takes no Spark's row, so a
// controller whose own GPU reports no mode can still set the fleet: that is a
// real state on a machine with no nvidia-smi command, and it used to leave
// the fleet with no path at all.
describe('the fleet-wide change', () => {
  it('offers both modes whatever any one Spark reports', () => {
    const markup = renderToStaticMarkup(<PowerFanOut busy={false} onSet={() => {}} />)
    expect(markup).toContain('Set for every Spark')
    expect(markup).toContain(FULL_MODE_LABEL)
    expect(markup).toContain(COOL_MODE_LABEL)
    expect(markup).not.toContain('disabled')
  })

  // The one thing that holds it: a run already under way.
  it('is dead while a run is under way', () => {
    const markup = renderToStaticMarkup(<PowerFanOut busy onSet={() => {}} />)
    expect(markup.match(/disabled/g)).toHaveLength(2)
  })

  // A dead local switch must not reach it. The control takes only a busy
  // flag, so there is no row for it to inherit anything from.
  it('takes no Spark row at all', () => {
    const dead = localPowerRow(null, false)
    expect(dead.disabled).toBe(true)
    const markup = renderToStaticMarkup(<PowerFanOut busy={false} onSet={() => {}} />)
    expect(markup).not.toContain('disabled')
  })
})

// Where the switch lives. There is no browser in this suite, so the two
// screens are read as source. This is a rename-fragile guard: it proves
// nothing the day one of these constants is called something else, so a
// rename has to bring these lines with it.
describe('where the power mode is set', () => {
  const models = modelsSource
  const fleet = fleetSource

  it('sets the mode from the Sparks page', () => {
    expect(fleet).toContain('FLEET_POWER_MODE_PATH')
    expect(fleet).toContain('LOCAL_POWER_MODE_PATH')
    expect(fleet).toContain('EVERY_SPARK')
    expect(fleet).toContain('POWER_MODE_NOTE')
  })

  it('sets no mode from the Models page', () => {
    expect(models).not.toContain('FLEET_POWER_MODE_PATH')
    expect(models).not.toContain('LOCAL_POWER_MODE_PATH')
    expect(models).not.toContain('EVERY_SPARK')
    expect(models).not.toContain('FULL_MODE_LABEL')
    expect(models).not.toContain('POWER_MODE_NOTE')
  })

  // The strip chip still carries the "cool" tag, which is a read of what that
  // Spark reported and not a control.
  it('keeps the cool tag on the Models strip', () => {
    expect(models).toContain('COOL_TAG')
    expect(models).toContain('powerRow(')
  })
})
