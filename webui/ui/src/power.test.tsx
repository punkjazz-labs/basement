/// <reference types="vite/client" />
import { describe, expect, it } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import { PowerFanOut, PowerSwitch } from './components/Power'
import { COOL_MODE, COOL_MODE_LABEL, FULL_MODE, FULL_MODE_LABEL, localPowerRow } from './fleetModels'
import modelsSource from './views/Models.tsx?raw'
import fleetSource from './views/Fleet.tsx?raw'
import controlSource from './powerControl.ts?raw'
import appSource from './App.tsx?raw'
import drawerSource from './components/BoxDrawer.tsx?raw'

describe('the power switch', () => {
  it('draws both modes and marks the one the Spark holds', () => {
    const markup = renderToStaticMarkup(
      <PowerSwitch name="spark-a5d6" power={localPowerRow({ mode: COOL_MODE, failure: '' }, false)} onSet={() => {}} />,
    )
    expect(markup).toContain(FULL_MODE_LABEL)
    expect(markup).toContain(COOL_MODE_LABEL)
    expect(markup).toContain('aria-label="Power mode for spark-a5d6"')
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
    const markup = renderToStaticMarkup(<PowerSwitch name="spark-f7e0" power={power} onSet={() => {}} />)
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

// It reads the fleet: the button every Spark already holds is pressed, and
// neither is while they differ. It never stands in for the fleet.
describe('the fleet-wide change, read back', () => {
  it('presses the mode every Spark holds', () => {
    const markup = renderToStaticMarkup(<PowerFanOut busy={false} mode={COOL_MODE} onSet={() => {}} />)
    expect(/aria-pressed="true"[^>]*>Cool and quiet/.test(markup)).toBe(true)
    expect(/aria-pressed="false"[^>]*>Full speed/.test(markup)).toBe(true)
  })

  it('presses neither while the fleet is mixed', () => {
    expect(renderToStaticMarkup(<PowerFanOut busy={false} onSet={() => {}} />)).not.toContain('aria-pressed="true"')
  })
})

// Where the switch lives. There is no browser in this suite, so the screens
// are read as source. This is a rename-fragile guard: it proves nothing the
// day one of these constants is called something else, so a rename has to
// bring these lines with it.
//
// The calls behind the switch live in one shared control (powerControl.ts).
// The Sparks table draws a switch per box, the page header carries the one
// fleet-wide change, and a box's drawer carries that box's switch; the
// Models page itself sets nothing.
describe('where the power mode is set', () => {
  it('sets the mode through the one shared control', () => {
    expect(controlSource).toContain('FLEET_POWER_MODE_PATH')
    expect(controlSource).toContain('LOCAL_POWER_MODE_PATH')
    expect(fleetSource).not.toContain('FLEET_POWER_MODE_PATH')
    expect(fleetSource).not.toContain('LOCAL_POWER_MODE_PATH')
  })

  it('draws a switch per box on the Sparks page and in the box drawer', () => {
    expect(fleetSource).toContain('power.switchFor(row)')
    expect(fleetSource).toContain('<PowerSwitch compact')
    expect(drawerSource).toContain('<PowerSwitch')
  })

  it('puts the fleet-wide change and its note in the Sparks header', () => {
    expect(appSource).toContain("tab === 'Fleet' && power.fanOut")
    expect(appSource).toContain('POWER_MODE_NOTE')
  })

  it('sets no mode from the Models page', () => {
    expect(modelsSource).not.toContain('FLEET_POWER_MODE_PATH')
    expect(modelsSource).not.toContain('LOCAL_POWER_MODE_PATH')
    expect(modelsSource).not.toContain('EVERY_SPARK')
    expect(modelsSource).not.toContain('FULL_MODE_LABEL')
    expect(modelsSource).not.toContain('POWER_MODE_NOTE')
  })

  // The box chips on a serving card still carry the "cool" tag, which is a
  // read of what that Spark reported and not a control.
  it('keeps the cool tag on the Models box chips', () => {
    expect(modelsSource).toContain('COOL_TAG')
    expect(modelsSource).toContain('powerRow(')
  })
})
