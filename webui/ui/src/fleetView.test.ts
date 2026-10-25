import { describe, expect, it } from 'vitest'
import { DS, GLM, box, serving, sixBoxes, viewOf } from './testFleet'
import { LOCAL_MACHINE, heartbeatKey } from './telemetry'

describe('the fleet as every screen reads it', () => {
  it('draws the boxes unit by unit, head first, then the boxes in no unit', () => {
    const rows = [...sixBoxes(), box('spark-ab12', 'DGX Spark', 60)]
    const view = viewOf(rows)
    expect(view.rows.map(row => row.displayName)).toEqual([
      'spark-f7e0', 'spark-2b4f', 'spark-9a07', 'spark-c3d1', 'edgexpert-41b8', 'edgexpert-5e12', 'spark-ab12',
    ])
    expect(view.unassigned.map(row => row.displayName)).toEqual(['spark-ab12'])
    expect(view.health.map(box => box.name)).toEqual(view.rows.map(row => row.displayName))
  })

  it('says what each box is in one line', () => {
    const view = viewOf([...sixBoxes(), box('spark-ab12', 'DGX Spark', 60), box('spark-cd34', 'DGX Spark', 60, [serving('qwen')])])
    const line = (name: string) => view.roleLine(view.findBox(name)!)
    expect(line('spark-f7e0')).toBe('head · TP4 · GLM 5.3 Flash')
    expect(line('spark-2b4f')).toBe('member · TP4 · GLM 5.3 Flash')
    expect(line('edgexpert-5e12')).toBe('member · TP2 · DeepSeek V4 Flash 0731')
    expect(line('spark-ab12')).toBe('unassigned · idle')
    expect(line('spark-cd34')).toBe('unassigned · qwen')
  })

  // The brief's own line, for the fixture's tight box.
  it('reads the tight box into the health line and the banner', () => {
    const view = viewOf(sixBoxes())
    expect(view.summary.text).toBe('1 needs attention · edgexpert-5e12 memory tight (7.2 GB free, loads refused)')
    expect(view.banner?.boxID).toBe('node-edgexpert-5e12')
    expect(view.healthOf('node-edgexpert-5e12')?.light).toBe('warn')
  })

  it('says all is well when it is', () => {
    const view = viewOf(sixBoxes({ 'edgexpert-5e12': { inventory: { ...box('x', 'EdgeXpert', 60).inventory! } } }))
    expect(view.summary.text).toBe('All 6 boxes healthy')
    expect(view.banner).toBeNull()
  })

  it('ages every heartbeat but this box\'s own', () => {
    const view = viewOf(sixBoxes(), { ages: { 'spark-9a07': 45 } })
    expect(view.ageOf(view.findBox('spark-9a07')!)).toBe(45_000)
    expect(view.ageOf(view.findBox('spark-f7e0')!)).toBeUndefined()
    expect(view.healthOf('node-spark-9a07')?.stale).toBe(true)
  })

  it('finds a box by name or by id, and a unit by its placement', () => {
    const view = viewOf(sixBoxes())
    expect(view.findBox('edgexpert-41b8')?.nodeID).toBe('node-edgexpert-41b8')
    expect(view.findBox('node-edgexpert-41b8')?.displayName).toBe('edgexpert-41b8')
    expect(view.findBox('nowhere')).toBeUndefined()
    expect(view.findUnit('dep_ds_tp2')?.recipeID).toBe(DS)
    expect(view.unitOf(view.findBox('spark-c3d1')!)?.recipeID).toBe(GLM)
  })

  it('knows where each box keeps its meters', () => {
    const view = viewOf(sixBoxes())
    expect(view.seriesKey(view.findBox('spark-f7e0')!)).toBe(LOCAL_MACHINE)
    expect(view.seriesKey(view.findBox('edgexpert-41b8')!)).toBe(heartbeatKey('node-edgexpert-41b8'))
  })

  // A Spark that leads no fleet still has itself to show.
  it('holds this box alone when there is no fleet', () => {
    const view = viewOf([], { placements: [], summary: null })
    expect(view.rows).toHaveLength(1)
    expect(view.rows[0].isSelf).toBe(true)
    expect(view.unassigned[0].isSelf).toBe(true)
    expect(view.multi).toBe(false)
    expect(view.health).toHaveLength(1)
  })

  // A Spark added by address reports nothing to the fleet: it has a row, but
  // no light and no say in the health line.
  it('lists a Spark added by address last, with no health', () => {
    const peer = box('spark-a5d6', 'DGX Spark', 50, [], { legacyPeerOnly: true, inventory: undefined, answering: false })
    const view = viewOf([peer], { placements: [], summary: null })
    expect(view.rows.map(row => row.displayName)).toEqual(['This Spark', 'spark-a5d6'])
    expect(view.health.map(box => box.name)).toEqual(['This Spark'])
    expect(view.roleLine(peer)).toBe('added by address · not in the fleet')
  })
})
