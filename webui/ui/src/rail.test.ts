import { describe, expect, it } from 'vitest'
import { RAIL, TABS, railGroups, tabLabel, type Tab } from './rail'

// The tabs a console with a text model serving offers: everything except the
// media tab, which App swaps in for the chat only while a video model serves.
const TEXT_TABS = TABS.filter(tab => tab !== 'Generate')
const MEDIA_TABS = TABS.filter(tab => tab !== 'Playground')

const names = (visible: readonly Tab[]) =>
  railGroups(visible).map(group => [group.label, group.tabs.map(tabLabel)])

describe('the rail', () => {
  it('groups the screens the way the approved rail does', () => {
    expect(names(TEXT_TABS)).toEqual([
      ['', ['Models']],
      ['Apps', ['Chat', 'Redactor']],
      ['System', ['Roles', 'Sparks', 'Monitor', 'Storage', 'Activity', 'API']],
    ])
  })

  it('renames only the three screens the world calls something else', () => {
    expect(tabLabel('Playground')).toBe('Chat')
    expect(tabLabel('Fleet')).toBe('Sparks')
    expect(tabLabel('Connect')).toBe('API')
    expect(tabLabel('Models')).toBe('Models')
    expect(tabLabel('Monitor')).toBe('Monitor')
  })

  // The media tab takes the chat's place in the same group, so a Spark
  // serving a video model still reads as one list of apps.
  it('puts the media tab where the chat was', () => {
    expect(names(MEDIA_TABS)[1]).toEqual(['Apps', ['Generate', 'Redactor']])
  })

  // A group with nothing left is not drawn: a label over an empty space says
  // there is a screen there.
  it('drops a group that has no screen on offer', () => {
    expect(names(['Models'])).toEqual([['', ['Models']]])
  })

  // Every screen the console has reaches the rail. A tab added to TABS and
  // forgotten here would be unreachable, which is the failure this catches.
  it('gives every tab exactly one place', () => {
    const placed = RAIL.flatMap(group => group.tabs)
    expect([...placed].sort()).toEqual([...TABS].sort())
  })

  // The rail names no screen that does not exist yet: the Minutes app has no
  // console surface, so it has no row.
  it('names no screen the console does not have', () => {
    expect(RAIL.flatMap(group => group.tabs.map(tabLabel))).not.toContain('Minutes')
  })
})
