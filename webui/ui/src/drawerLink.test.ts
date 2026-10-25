import { describe, expect, it } from 'vitest'
import { readDrawer, sameDrawer, writeDrawer } from './drawerLink'

// The drawer is deep-linkable, so a screenshot or a shared link opens it.
describe('the drawer in the address bar', () => {
  it('reads a box and a unit', () => {
    expect(readDrawer('?box=edgexpert-5e12')).toEqual({ kind: 'box', name: 'edgexpert-5e12' })
    expect(readDrawer('?unit=dep_glm_tp4')).toEqual({ kind: 'unit', id: 'dep_glm_tp4' })
    expect(readDrawer('')).toBeNull()
    expect(readDrawer('?box=')).toBeNull()
  })

  it('opens the box when both are named', () => {
    expect(readDrawer('?unit=dep_glm_tp4&box=spark-2b4f')).toEqual({ kind: 'box', name: 'spark-2b4f' })
  })

  it('writes one drawer and keeps every other parameter', () => {
    expect(writeDrawer('?debug=1', { kind: 'box', name: 'spark-9a07' })).toBe('?debug=1&box=spark-9a07')
    expect(writeDrawer('?box=spark-9a07', { kind: 'unit', id: 'dep_ds_tp2' })).toBe('?unit=dep_ds_tp2')
    expect(writeDrawer('?debug=1&box=spark-9a07', null)).toBe('?debug=1')
    expect(writeDrawer('?box=spark-9a07', null)).toBe('')
  })

  it('round-trips a name that needs escaping', () => {
    const search = writeDrawer('', { kind: 'box', name: 'lab box #2' })
    expect(readDrawer(search)).toEqual({ kind: 'box', name: 'lab box #2' })
  })

  it('tells one drawer from another', () => {
    expect(sameDrawer({ kind: 'box', name: 'a' }, { kind: 'box', name: 'a' })).toBe(true)
    expect(sameDrawer({ kind: 'box', name: 'a' }, { kind: 'unit', id: 'a' })).toBe(false)
    expect(sameDrawer(null, null)).toBe(true)
    expect(sameDrawer(null, { kind: 'unit', id: 'a' })).toBe(false)
  })
})
