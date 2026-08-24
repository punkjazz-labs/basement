import { describe, expect, it } from 'vitest'
import type { FleetCandidate, FleetInviteProgress, FleetNodeSummary, FleetSummary, Peer } from './api'
import {
  consoleKey, fleetInvitations, fleetNodeFor, fleetSize, fleetStatusNote, fleetSummary,
  foundLine, foundSparks, invitationBody, invitationTitle, inviteBody, inviteName,
  inviteOutcome, inviteSettled, inviteTitle, inviteWaitLine, isFleetInviteProgress,
  joinedBadge, joinedFacts, localRoleLine, membershipRows, nodeFacts, nodeHostname, nodeName,
  nodeReported, nodeServing, nodeStatus, peerRoleLine, readIgnored, rememberIgnored,
  shouldSweepForSparks, sparkSubline,
} from './fleetInvite'

const node = (overrides: Partial<FleetNodeSummary> = {}): FleetNodeSummary => ({
  node_id: 'node-lead',
  display_name: 'attic',
  role: 'controller',
  status: 'fresh',
  console_url: 'http://attic.local:7070',
  node_url: 'https://attic.local:7071',
  manager_version: 'v0.5.11',
  ...overrides,
})

const summary = (overrides: Partial<FleetSummary> = {}): FleetSummary => ({
  fleet_id: 'fleet-one',
  role: 'controller',
  controller_node_id: 'node-lead',
  controller_console_url: 'http://attic.local:7070',
  migration_state: '',
  nodes: [node()],
  ...overrides,
})

const progress = (overrides: Partial<FleetInviteProgress> = {}): FleetInviteProgress => ({
  console_url: 'http://loft.local:7070',
  node_url: 'https://loft.local:7071',
  display_name: 'loft',
  node_id: 'node-loft',
  state: 'waiting',
  reason: '',
  expires_at: '2026-08-12T10:10:00Z',
  node: null,
  ...overrides,
})

const candidate = (name: string, overrides: Partial<FleetCandidate> = {}): FleetCandidate => ({
  name,
  address: `${name}.local`,
  gb10_hint: true,
  basement: { base_url: `http://${name}.local:7070`, running: true, version: 'v0.5.11' },
  ...overrides,
})

const peer = (name: string, baseURL: string): Peer => ({ id: name, name, base_url: baseURL })

describe('reading one addition', () => {
  it('accepts a progress object the manager really sends', () => {
    expect(isFleetInviteProgress(progress())).toBe(true)
    expect(isFleetInviteProgress(progress({ state: 'done', node: { node_id: 'node-loft' } as never }))).toBe(true)
  })

  it('refuses anything that is not one attempt in a state this console knows', () => {
    expect(isFleetInviteProgress(null)).toBe(false)
    expect(isFleetInviteProgress(progress({ state: 'inviting' }))).toBe(false)
    expect(isFleetInviteProgress(progress({ console_url: '' }))).toBe(false)
    expect(isFleetInviteProgress({ console_url: 'http://loft.local:7070', state: 'waiting' })).toBe(false)
  })

  it('keeps polling only until the attempt has an answer', () => {
    expect(['waiting', 'adopting'].every(state => !inviteSettled(state))).toBe(true)
    expect(['done', 'denied', 'expired', 'failed'].every(inviteSettled)).toBe(true)
  })
})

describe('what the add dialog says', () => {
  it('names the machine the owner has to press Approve on', () => {
    expect(inviteName(progress(), 'loft.local')).toBe('loft')
    expect(inviteName(null, 'loft.local')).toBe('loft.local')
    expect(inviteTitle('waiting', 'loft')).toBe('Approve on loft')
    expect(inviteBody('loft')).toBe("Press Approve on loft's console. Opened in a new tab.")
  })

  it('says which half of the exchange is running', () => {
    expect(inviteWaitLine('waiting')).toBe('Waiting…')
    expect(inviteWaitLine('adopting')).toBe('Adding…')
  })

  it('reports a denial and an expiry as answers, not as errors', () => {
    expect(inviteOutcome(progress({ state: 'denied' }), 'loft')).toBe('Denied on loft.')
    expect(inviteOutcome(progress({ state: 'expired' }), 'loft'))
      .toBe('Request expired. Try adding again.')
  })

  it('shows a failure in the words the manager used, and says something when it has none', () => {
    expect(inviteOutcome(progress({ state: 'failed', reason: 'that Spark is already in another fleet' }), 'loft'))
      .toBe('that Spark is already in another fleet')
    expect(inviteOutcome(progress({ state: 'failed' }), 'loft')).toBe('Adding loft stopped before it finished.')
  })

  it('never shows a waiting attempt its own network noise', () => {
    const waiting = progress({ reason: 'dial tcp: connection refused' })
    expect(inviteSettled(waiting.state)).toBe(false)
  })

  it('celebrates with facts about this fleet and nothing invented', () => {
    expect(inviteTitle('done', 'loft')).toBe('loft joined the fleet')
    expect(joinedBadge('v0.5.11')).toBe('In fleet · secure channel established · v0.5.11')
    expect(joinedBadge('')).toBe('In fleet · secure channel established')
    expect(joinedFacts(2, 'loft')).toEqual([
      { label: 'Fleet', value: '2 Sparks, managed from this console' },
      { label: 'Updates', value: 'Rolling upgrades now include loft' },
    ])
    expect(joinedFacts(1, 'loft')[0].value).toBe('1 Spark, managed from this console')
  })
})

describe('what the machine being added is asked', () => {
  it('asks in the name of the console that wants to manage it', () => {
    const invitation = { id: 'inv-1', controller_name: 'attic', controller_console_url: 'http://attic.local:7070', expires_at: '' }
    expect(invitationTitle(invitation)).toBe("Join attic's fleet?")
    expect(invitationBody(invitation))
      .toBe("attic will manage this Spark's models and updates. It keeps serving.")
  })

  it('keeps only the invitations it can name', () => {
    const answer = {
      invitations: [
        { id: 'inv-1', controller_name: 'attic', controller_console_url: 'http://attic.local:7070', expires_at: '' },
        { id: '', controller_name: 'attic' },
        { id: 'inv-2', controller_name: '' },
        'not an invitation',
      ],
    }
    expect(fleetInvitations(answer).map(entry => entry.id)).toEqual(['inv-1'])
    expect(fleetInvitations(null)).toEqual([])
    expect(fleetInvitations({})).toEqual([])
  })
})

describe('joining rows to membership', () => {
  it('matches a peer to its membership row however the URL was written', () => {
    expect(consoleKey(' HTTP://Attic.local:7070/ ')).toBe('http://attic.local:7070')
    expect(fleetNodeFor(summary(), 'http://attic.local:7070/')?.node_id).toBe('node-lead')
    expect(fleetNodeFor(summary(), 'http://loft.local:7070')).toBeUndefined()
    expect(fleetNodeFor(null, 'http://attic.local:7070')).toBeUndefined()
  })

  it('reads a role as what the Spark does, never as what the table calls it', () => {
    expect(localRoleLine(summary())).toBe('leads the fleet')
    expect(localRoleLine(summary({ role: 'member' }))).toBe('follows attic')
    expect(localRoleLine(summary({ role: 'standalone' }))).toBe('')
    expect(localRoleLine(null)).toBe('')
  })

  it('says which Spark a member follows, and says nothing until it is really in', () => {
    const loft = node({ node_id: 'node-loft', display_name: 'loft', role: 'member', console_url: 'http://loft.local:7070' })
    const two = summary({ nodes: [node(), loft] })
    expect(peerRoleLine(two, loft)).toBe('follows attic')
    expect(peerRoleLine(two, node())).toBe('leads the fleet')
    expect(peerRoleLine(two, { ...loft, status: 'legacy-pending' })).toBe('')
    expect(peerRoleLine(two, { ...loft, status: 'adopting' })).toBe('')
    expect(peerRoleLine(two, undefined)).toBe('')
  })

  it('marks a Spark that is really in the fleet, whatever its heartbeat says', () => {
    for (const status of ['fresh', 'stale', 'unreachable', 'version-mismatch']) {
      expect(fleetStatusNote(node({ status }))).toBe('In fleet')
    }
    for (const status of ['legacy-pending', 'adopting', 'adoption-uncertain', '']) {
      expect(fleetStatusNote(node({ status }))).toBe('')
    }
    expect(fleetStatusNote(undefined)).toBe('')
  })

  it('shows the network name first and the role after it', () => {
    expect(sparkSubline('attic.local', 'leads the fleet')).toBe('attic.local · leads the fleet')
    expect(sparkSubline('attic.local', '')).toBe('attic.local')
    expect(sparkSubline('', 'follows attic')).toBe('follows attic')
    expect(sparkSubline('', '')).toBe('n/a')
  })
})

describe('the Spark found on this network', () => {
  it('offers only a console that answered, and only once', () => {
    const found = foundSparks(
      [candidate('loft'), candidate('loft'), candidate('shed', { basement: { base_url: 'http://shed.local:7070', running: false } })],
      [], summary(), [], 'http://attic.local:7070',
    )
    expect(found).toEqual([{ name: 'loft', consoleURL: 'http://loft.local:7070', version: 'v0.5.11' }])
  })

  it('leaves out this console, the peers it already has, and the fleet it already leads', () => {
    const found = foundSparks(
      [candidate('attic'), candidate('cellar'), candidate('loft')],
      [peer('cellar', 'http://cellar.local:7070/')],
      summary({ nodes: [node(), node({ node_id: 'node-loft', console_url: 'HTTP://Loft.local:7070' })] }),
      [], 'http://attic.local:7070',
    )
    expect(found).toEqual([])
  })

  it('drops a machine the owner said not now to', () => {
    expect(foundSparks([candidate('loft')], [], null, ['http://loft.local:7070'], '')).toEqual([])
  })

  it('says what it is running without inventing a version', () => {
    expect(foundLine({ name: 'loft', consoleURL: '', version: 'v0.5.11' })).toBe('v0.5.11 · not in your fleet')
    expect(foundLine({ name: 'loft', consoleURL: '', version: '' })).toBe('not in your fleet')
  })

  it('sweeps only while the fleet is small enough to be assembled by hand', () => {
    expect(fleetSize(summary(), [])).toBe(1)
    expect(fleetSize(null, [peer('cellar', 'http://cellar.local:7070')])).toBe(2)
    expect(shouldSweepForSparks(3)).toBe(true)
    expect(shouldSweepForSparks(4)).toBe(false)
  })
})

describe('the ignore list', () => {
  const fakeStorage = (): Storage => {
    const held = new Map<string, string>()
    return {
      getItem: key => held.get(key) ?? null,
      setItem: (key, value) => void held.set(key, value),
      removeItem: key => void held.delete(key),
      clear: () => held.clear(),
      key: () => null,
      get length() { return held.size },
    } as Storage
  }

  it('remembers one machine per key, however its URL was written', () => {
    const store = fakeStorage()
    rememberIgnored(store, 'http://loft.local:7070/')
    expect(rememberIgnored(store, 'HTTP://Loft.local:7070')).toEqual(['http://loft.local:7070'])
    expect(readIgnored(store)).toEqual(['http://loft.local:7070'])
  })

  it('forgets rather than throws when the browser refuses storage', () => {
    const refusing = {
      getItem: () => { throw new Error('denied') },
      setItem: () => { throw new Error('denied') },
    } as unknown as Storage
    expect(readIgnored(refusing)).toEqual([])
    expect(rememberIgnored(refusing, 'http://loft.local:7070')).toEqual(['http://loft.local:7070'])
    expect(readIgnored(null)).toEqual([])
  })

  it('reads nothing out of a stored value that is not a list of URLs', () => {
    const store = fakeStorage()
    store.setItem('basement.fleet.ignored', '{"not":"a list"}')
    expect(readIgnored(store)).toEqual([])
  })
})

describe('reading the membership summary', () => {
  it('takes a summary with a role and a node list, and nothing else', () => {
    expect(fleetSummary(summary())?.role).toBe('controller')
    expect(fleetSummary({ role: 'standalone', nodes: [] })?.nodes).toEqual([])
    expect(fleetSummary({ role: 'controller' })).toBeNull()
    expect(fleetSummary(null)).toBeNull()
  })
})

describe('a Spark that joined the fleet without ever being a peer', () => {
  const SELF = 'http://attic.local:7070'
  const loft = (overrides: Partial<FleetNodeSummary> = {}): FleetNodeSummary => node({
    node_id: 'node-loft',
    display_name: 'loft',
    role: 'member',
    status: 'fresh',
    console_url: 'http://loft.local:7070',
    node_url: 'https://loft.local:7071',
    last_heartbeat_at: '2026-08-12T11:59:30Z',
    inventory: {
      hostname: 'loft-gb10',
      product_name: 'DGX Spark',
      dgx_spark: true,
      memory_total_bytes: 128_000_000_000,
      memory_available_bytes: 96_000_000_000,
      storage_total_bytes: 4_000_000_000_000,
      storage_available_bytes: 3_000_000_000_000,
    },
    installed_models: [
      { recipe_id: 'qwen36-27b-nvfp4-1s', recipe_version: 2, status: 'ready', active: true },
      { recipe_id: 'qwen36-35b-a3b-nvfp4-1s', recipe_version: 3, status: 'stopped', active: false },
    ],
    ...overrides,
  })
  const both = (extra: Partial<FleetNodeSummary> = {}) => summary({ nodes: [node(), loft(extra)] })

  it('gives a member with no peer row a row of its own', () => {
    expect(membershipRows(both(), [], SELF).map(entry => entry.node_id)).toEqual(['node-loft'])
  })

  it('never draws a second row for a Spark a peer row already speaks for', () => {
    expect(membershipRows(both(), [peer('loft', 'HTTP://Loft.local:7070/')], SELF)).toEqual([])
  })

  it('never draws a second row for this console itself', () => {
    // As the Spark that leads the fleet, by its own console URL while it
    // follows another, and as the only node of a standalone summary.
    expect(membershipRows(summary({ nodes: [node()] }), [], SELF)).toEqual([])
    const asMember = summary({
      role: 'member',
      controller_node_id: 'node-loft',
      nodes: [node({ node_id: 'node-attic', role: 'member' }), loft({ role: 'controller' })],
    })
    expect(membershipRows(asMember, [], SELF).map(entry => entry.node_id)).toEqual(['node-loft'])
    const alone = summary({ role: 'standalone', nodes: [node({ role: 'standalone', console_url: 'http://elsewhere:7070' })] })
    expect(membershipRows(alone, [], SELF)).toEqual([])
  })

  it('leaves out a node it could neither open nor tell apart, and repeats none', () => {
    expect(membershipRows(both({ console_url: '' }), [], SELF)).toEqual([])
    const twice = summary({ nodes: [node(), loft(), loft({ node_id: 'node-loft-again' })] })
    expect(membershipRows(twice, [], SELF).map(entry => entry.node_id)).toEqual(['node-loft'])
    expect(membershipRows(null, [], SELF)).toEqual([])
  })

  it('names it as it calls itself, and falls back rather than showing nothing', () => {
    expect(nodeName(loft())).toBe('loft')
    expect(nodeHostname(loft())).toBe('loft-gb10')
    expect(nodeName(loft({ display_name: '', inventory: undefined }))).toBe('loft.local')
    expect(nodeHostname(loft({ inventory: undefined }))).toBe('loft.local')
  })

  it('reads what it is serving from its own heartbeat', () => {
    expect(nodeServing(loft())?.recipe_id).toBe('qwen36-27b-nvfp4-1s')
    expect(nodeServing(loft({ installed_models: [] }))).toBeUndefined()
    expect(nodeServing(loft({
      installed_models: [{ recipe_id: 'qwen36-27b-nvfp4-1s', recipe_version: 2, status: 'starting', active: true }],
    }))).toBeUndefined()
  })

  it('tells a Spark with nothing running from one that has reported nothing', () => {
    expect(nodeReported(loft())).toBe(true)
    expect(nodeReported(loft({ inventory: undefined }))).toBe(false)
  })

  it('says what it is doing in the words the rest of the table uses', () => {
    expect(nodeStatus(loft())).toEqual({ word: 'Serving', dot: 'on' })
    expect(nodeStatus(loft({ installed_models: [] }))).toEqual({ word: 'Idle', dot: '' })
    expect(nodeStatus(loft({ status: 'stale' }))).toEqual({ word: 'No answer', dot: '' })
    expect(nodeStatus(loft({ status: 'unreachable' }))).toEqual({ word: 'Unreachable', dot: 'fail' })
    expect(nodeStatus(loft({ status: 'version-mismatch' }))).toEqual({ word: 'Different version', dot: '' })
    expect(nodeStatus(loft({ status: 'adopting' }))).toEqual({ word: 'Joining', dot: 'busy' })
    // A state this console has never heard of keeps its own word.
    expect(nodeStatus(loft({ status: 'quarantined' }))).toEqual({ word: 'quarantined', dot: '' })
  })

  it('marks it as really in the fleet, the same way a peer row is', () => {
    expect(fleetStatusNote(loft())).toBe('In fleet')
    expect(fleetStatusNote(loft({ status: 'adopting' }))).toBe('')
  })

  it('expands to what that Spark reported, and to n/a for what it did not', () => {
    const now = Date.parse('2026-08-12T12:00:00Z')
    expect(nodeFacts(loft(), now)).toEqual([
      { label: 'Address', value: 'http://loft.local:7070' },
      { label: 'Version', value: 'v0.5.11' },
      { label: 'Last heartbeat', value: 'just now' },
      { label: 'Models installed', value: '2 models' },
    ])
    expect(nodeFacts(loft({ installed_models: [loft().installed_models![0]] }), now)[3].value).toBe('1 model')
    const quiet = loft({ manager_version: '', last_heartbeat_at: undefined, installed_models: undefined })
    expect(nodeFacts(quiet, now).map(fact => fact.value))
      .toEqual(['http://loft.local:7070', 'n/a', 'n/a', 'n/a'])
  })
})
