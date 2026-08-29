import { describe, expect, it } from 'vitest'
import type {
  FleetDeploymentView, FleetModelSnapshot, FleetNodeSummary, FleetSummary, Job, NodeInventory, Peer, PlacementPlan,
} from './api'
import {
  deploymentActionPath, deploymentIndex, deploymentKey, fleetInstallRequest, fleetRows,
  heldSomewhere, heldTabLabel, initialPlacement, installRoute, joinCandidatesWithInventory,
  localPowerRow, machineNote, mergePlacements,
  modelChips, placedTarget, placementBusy, placementOptions, placementSwitchFrom, placementTargets,
  placementVerb, placementWord, powerBusy, powerFanOut, powerFanOutBusy, powerRefusalLine,
  powerRefusedTitle, powerRow, recipeBusy, retiredPowerSets,
  isTypical, openPillClass,
  rowActionRoute, rowPlacement, rowStateLine, servingPlace, servingSparkName, shortSparkName,
  shouldShowMemberBanner, speedText, splitModels, splitServing, workingNodes, workingPlacement,
  clearRecordBody, clearRecordTitle, releasePath,
  ACTION_REFUSAL, ADOPT_PATH, BOTH_SPARKS, BOTH_SPARKS_CHIP,
  CATALOG_EMPTY, CATALOG_TAB, CHOOSE_FOR_ME, CHOOSE_FOR_ME_NAME, GHOST_PILL, NO_SPEED, PRIMARY_PILL,
  CHOOSE_FOR_ME_NOTE, CLEAR_RECORD, CLEAR_RECORD_CONFIRM, COOL_MODE, COOL_MODE_LABEL, COOL_TAG,
  DISRUPTIVE_KINDS,
  FLEET_DEPLOYMENT_ACTIONS, FLEET_POWER_MODE_PATH, FULL_MODE, FULL_MODE_LABEL,
  LOCAL_POWER_MODE_PATH, MANY_SPARKS_TAB,
  NO_FLEET_ROW, NO_PLACEMENT_BACK,
  ONE_SPARK_TAB, PLACEMENT_REFUSED, PLACEMENT_WORKING, POWER_MODE_NOTE, POWER_REFUSED_TITLE,
  type ActionTarget, type FleetRow, type LabGroup, type PlacementTarget,
} from './fleetModels'

const inventory = (overrides: Partial<NodeInventory> = {}): NodeInventory => ({
  hostname: 'attic',
  product_name: 'DGX Spark',
  dgx_spark: true,
  memory_total_bytes: 128_000_000_000,
  memory_available_bytes: 18_000_000_000,
  storage_total_bytes: 4_000_000_000_000,
  storage_available_bytes: 1_100_000_000_000,
  ...overrides,
})

// The moment every row below is read at, and a heartbeat that landed ten
// seconds before it: inside the manager's own 30 s freshness bound.
const NOW = Date.parse('2026-08-23T10:00:00Z')
const RECENT = '2026-08-23T09:59:50Z'

const node = (overrides: Partial<FleetNodeSummary> = {}): FleetNodeSummary => ({
  node_id: 'node-lead',
  display_name: 'attic',
  role: 'controller',
  status: 'fresh',
  console_url: 'http://attic.local:7070',
  node_url: 'https://attic.local:7071',
  manager_version: 'v0.5.16',
  last_heartbeat_at: RECENT,
  inventory: inventory(),
  installed_models: [],
  power_mode: 'full',
  power_mode_failure: '',
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

const member = (overrides: Partial<FleetNodeSummary> = {}): FleetNodeSummary =>
  node({
    node_id: 'node-loft',
    display_name: 'loft',
    role: 'member',
    console_url: 'http://loft.local:7070',
    node_url: 'https://loft.local:7071',
    inventory: inventory({ hostname: 'loft', memory_available_bytes: 26_000_000_000 }),
    ...overrides,
  })

const peer = (name: string, baseURL: string): Peer => ({ id: name, name, base_url: baseURL })

const deployment = (overrides: Partial<FleetDeploymentView> = {}): FleetDeploymentView => ({
  deployment_id: 'deployment_one',
  recipe_id: 'qwen36-35b-a3b-nvfp4-1s',
  recipe_version: 3,
  recipe_fingerprint: 'fingerprint',
  topology_count: 1,
  owner_node_id: 'node-loft',
  owner_job_id: 'job-one',
  state: 'running',
  last_observed_at: '2026-08-23T10:00:00Z',
  created_at: '2026-08-23T09:00:00Z',
  updated_at: '2026-08-23T10:00:00Z',
  stale: false,
  nodes: [{
    deployment_id: 'deployment_one',
    node_id: 'node-loft',
    node_role: 'owner',
    rank: 0,
    reservation_id: 'reservation_one',
  }],
  ...overrides,
})

describe('one row per Spark', () => {
  it('marks the Spark this console runs on', () => {
    const rows = fleetRows(summary({ nodes: [node(), member()] }), [], 'http://attic.local:7070', NOW)
    expect(rows.map(row => [row.displayName, row.isSelf])).toEqual([['attic', true], ['loft', false]])
  })

  it('carries what each Spark reported about itself', () => {
    const rows = fleetRows(summary({ nodes: [member()] }), [], 'http://attic.local:7070', NOW)
    expect(rows[0].inventory?.memory_available_bytes).toBe(26_000_000_000)
    expect(rows[0].inventory?.storage_available_bytes).toBe(1_100_000_000_000)
    expect(rows[0].status).toEqual({ word: 'Idle', dot: '' })
  })

  it('reads the model a Spark serves and the ones it only holds', () => {
    const installed = [
      { recipe_id: 'qwen36-27b-nvfp4-1s', recipe_version: 2, status: 'stopped', active: false },
      { recipe_id: 'qwen36-35b-a3b-nvfp4-1s', recipe_version: 3, status: 'ready', active: true },
    ]
    const rows = fleetRows(summary({ nodes: [member({ installed_models: installed })] }), [], '', NOW)
    expect(rows[0].installedModels).toHaveLength(2)
    expect(rows[0].serving?.recipe_id).toBe('qwen36-35b-a3b-nvfp4-1s')
    expect(rows[0].status).toEqual({ word: 'Serving', dot: 'on' })
  })

  it('keeps one row for a Spark that is both a peer and a member', () => {
    const rows = fleetRows(
      summary({ nodes: [node(), member()] }),
      [peer('loft', 'http://loft.local:7070/')],
      'http://attic.local:7070',
      NOW,
    )
    expect(rows.map(row => row.displayName)).toEqual(['attic', 'loft'])
    expect(rows[1].legacyPeerOnly).toBeUndefined()
    expect(rows[1].inventory).toBeDefined()
  })

  it('keeps a Spark added by address that never joined the fleet', () => {
    const rows = fleetRows(summary(), [peer('shed', 'http://shed.local:7070')], 'http://attic.local:7070', NOW)
    expect(rows[1]).toMatchObject({
      nodeID: 'shed',
      displayName: 'shed',
      isSelf: false,
      legacyPeerOnly: true,
      installedModels: [],
    })
    expect(rows[1].inventory).toBeUndefined()
  })

  it('leaves out a node no row can be told apart from', () => {
    const rows = fleetRows(summary({ nodes: [node(), member({ console_url: '' })] }), [], '', NOW)
    expect(rows.map(row => row.nodeID)).toEqual(['node-lead'])
  })

  it('says whether each Spark still answers this console', () => {
    const answers = (status: string) =>
      fleetRows(summary({ nodes: [member({ status })] }), [], '', NOW)[0].answering
    expect(answers('fresh')).toBe(true)
    expect(answers('stale')).toBe(false)
    expect(answers('unreachable')).toBe(false)
    // A Spark the fleet has not finished admitting answers for nothing yet.
    expect(answers('adopting')).toBe(false)
  })

  it('reads the heartbeat itself for a Spark of another version', () => {
    // The manager writes "version-mismatch" over "stale" and "unreachable"
    // alike, so this one word says nothing about whether that Spark is there.
    const answers = (last_heartbeat_at?: string) =>
      fleetRows(
        summary({ nodes: [member({ status: 'version-mismatch', last_heartbeat_at })] }), [], '', NOW,
      )[0].answering
    expect(answers(RECENT)).toBe(true)
    // Exactly the manager's own bound, then past it.
    expect(answers('2026-08-23T09:59:30Z')).toBe(true)
    expect(answers('2026-08-23T09:59:29Z')).toBe(false)
    expect(answers('2026-08-23T06:00:00Z')).toBe(false)
    expect(answers(undefined)).toBe(false)
    expect(answers('')).toBe(false)
  })

  it('says a Spark added by address answers for nothing', () => {
    const rows = fleetRows(summary(), [peer('shed', 'http://shed.local:7070')], 'http://attic.local:7070', NOW)
    expect(rows[1].answering).toBe(false)
  })

  it('says nothing without a fleet summary', () => {
    expect(fleetRows(null, [], 'http://attic.local:7070', NOW)).toEqual([])
  })
})

describe('the power mode a Spark reports', () => {
  // One Spark's own row, built the way the screen builds it.
  const rowFor = (overrides: Partial<FleetNodeSummary> = {}): FleetRow =>
    fleetRows(summary({ nodes: [node(overrides)] }), [], 'http://attic.local:7070', NOW)[0]
  const idle: ReadonlySet<string> = new Set()
  // One of the three sentences the manager sends, word for word.
  const NO_TOOL = 'This machine has no nvidia-smi command, so the GPU clock did not change.'

  it('shows a capped Spark as capped, and tags its chip', () => {
    expect(powerRow(rowFor({ power_mode: COOL_MODE }), undefined, idle))
      .toEqual({ mode: 'cool', disabled: false, failure: '', tag: true, busy: false })
  })

  it('shows a Spark at full speed, with no tag', () => {
    expect(powerRow(rowFor({ power_mode: FULL_MODE }), undefined, idle))
      .toEqual({ mode: 'full', disabled: false, failure: '', tag: false, busy: false })
  })

  it('says nothing at all about a Spark that has reported no mode', () => {
    // Empty is not full speed and not capped: that Spark has sent no
    // heartbeat yet, so the control has nothing to select and nothing to set.
    // A word this console does not know is read the same way.
    for (const power_mode of ['', 'quiet']) {
      expect(powerRow(rowFor({ power_mode }), undefined, idle))
        .toEqual({ mode: '', disabled: true, failure: '', tag: false, busy: false })
    }
  })

  it('gives a Spark added by address no mode either', () => {
    const rows = fleetRows(summary(), [peer('shed', 'http://shed.local:7070')], 'http://attic.local:7070', NOW)
    expect(powerRow(rows[1], undefined, idle)).toMatchObject({ mode: '', disabled: true, tag: false })
  })

  it('keeps the chosen mode selected beside the Spark\'s own sentence', () => {
    // The setting is stored whatever the chip did with it, so the switch
    // never jumps back: the mode the owner chose stands, and the machine's
    // own words stand under it.
    const power = powerRow(rowFor({ power_mode: COOL_MODE, power_mode_failure: NO_TOOL }), undefined, idle)
    expect(power).toEqual({ mode: 'cool', disabled: false, failure: NO_TOOL, tag: true, busy: false })
  })

  it('locks the control while a change runs on that Spark', () => {
    const running = new Set(['node-lead'])
    expect(powerBusy('node-lead', running)).toBe(true)
    const power = powerRow(rowFor({ power_mode: FULL_MODE }), { mode: COOL_MODE, failure: '' }, running)
    expect(power).toEqual({ mode: 'cool', disabled: true, failure: '', tag: true, busy: true })
  })

  it('leaves a Spark alone while another one is being set', () => {
    expect(powerBusy('node-lead', new Set(['node-loft']))).toBe(false)
    expect(powerRow(rowFor({ power_mode: FULL_MODE }), undefined, new Set(['node-loft'])))
      .toMatchObject({ disabled: false, busy: false })
  })

  it('keeps a mode this console just set until a read carries it', () => {
    // The controller stores the mode before it answers, and that Spark reports
    // it in a heartbeat seconds later.
    const set = { mode: COOL_MODE, failure: '' }
    const behind = rowFor({ power_mode: FULL_MODE })
    expect(powerRow(behind, set, idle)).toMatchObject({ mode: 'cool', tag: true })
    expect(retiredPowerSets([behind], new Map([[behind.nodeID, set]]), idle)).toEqual([])
  })

  it('retires the answer once the read carries it, and follows the read from then on', () => {
    // The window this answer covers is over. Keeping it would pin the row to
    // a mode that Spark stopped holding as soon as anything else changed it.
    const set = { mode: COOL_MODE, failure: '' }
    const caught = rowFor({ power_mode: COOL_MODE })
    expect(retiredPowerSets([caught], new Map([[caught.nodeID, set]]), idle)).toEqual(['node-lead'])
    // A change made from another console, after the answer was retired.
    const elsewhere = rowFor({ power_mode: FULL_MODE })
    expect(powerRow(elsewhere, undefined, idle)).toMatchObject({ mode: 'full', tag: false })
  })

  it('keeps the answer while the call to that Spark is still running', () => {
    // The read it would be measured against is the one this call is about to
    // replace.
    const caught = rowFor({ power_mode: COOL_MODE })
    const held = new Map([[caught.nodeID, { mode: COOL_MODE, failure: '' }]])
    expect(retiredPowerSets([caught], held, new Set(['node-lead']))).toEqual([])
  })

  it('retires an answer for a Spark this console no longer has a row for', () => {
    expect(retiredPowerSets([], new Map([['node-gone', { mode: COOL_MODE, failure: '' }]]), idle))
      .toEqual(['node-gone'])
  })

  it('shows the answer\'s own sentence while the read carries an older one', () => {
    // Re-applying the same mode after a driver was fixed: the mode never
    // moves, so only the sentence says whether the machine took it. The
    // answer is the fresh half for a poll or two, in both directions.
    const cleared = { mode: COOL_MODE, failure: '' }
    const stale = rowFor({ power_mode: COOL_MODE, power_mode_failure: NO_TOOL })
    expect(powerRow(stale, cleared, idle)).toMatchObject({ mode: 'cool', failure: '' })
    expect(retiredPowerSets([stale], new Map([[stale.nodeID, cleared]]), idle)).toEqual([])
    const quiet = rowFor({ power_mode: COOL_MODE })
    expect(powerRow(quiet, { mode: COOL_MODE, failure: NO_TOOL }, idle))
      .toMatchObject({ mode: 'cool', failure: NO_TOOL })
    // Both halves agree, so there is nothing left to keep.
    expect(retiredPowerSets([stale], new Map([[stale.nodeID, { mode: COOL_MODE, failure: NO_TOOL }]]), idle))
      .toEqual(['node-lead'])
  })

  it('states the measured line as one string, and both modes in the approved words', () => {
    expect(POWER_MODE_NOTE).toBe(
      'Cool and quiet caps the chip at 2200 MHz. Measured on a GB10 Spark: about a third less ' +
      'peak power, 6 degrees cooler, the same answer speed.',
    )
    expect([FULL_MODE_LABEL, COOL_MODE_LABEL, COOL_TAG]).toEqual(['Full speed', 'Cool and quiet', 'cool'])
    expect(FLEET_POWER_MODE_PATH).toBe('/api/v1/fleet/power-mode')
    expect(LOCAL_POWER_MODE_PATH).toBe('/api/v1/system/power-mode')
  })
})

// A Spark that leads no fleet answers for itself, through its own door. The
// switch it draws has to keep every rule the fleet rows keep.
describe('the power mode of a Spark that answers for itself', () => {
  const NO_TOOL = 'This machine has no nvidia-smi command, so the GPU clock did not change.'

  it('shows the mode that Spark reported', () => {
    expect(localPowerRow({ mode: COOL_MODE, failure: '' }, false))
      .toEqual({ mode: 'cool', disabled: false, failure: '', tag: true, busy: false })
    expect(localPowerRow({ mode: FULL_MODE, failure: '' }, false))
      .toEqual({ mode: 'full', disabled: false, failure: '', tag: false, busy: false })
  })

  // A mode this console has not read is not full speed: it is nothing, and
  // the switch says nothing.
  it('draws no mode until it has read one', () => {
    expect(localPowerRow(null, false)).toMatchObject({ mode: '', disabled: true, tag: false })
    expect(localPowerRow({ mode: '', failure: '' }, false)).toMatchObject({ mode: '', disabled: true })
    expect(localPowerRow({ mode: 'silent', failure: NO_TOOL }, false))
      .toMatchObject({ mode: '', disabled: true, failure: '' })
  })

  it('locks the switch while a change is running', () => {
    expect(localPowerRow({ mode: FULL_MODE, failure: '' }, true))
      .toMatchObject({ mode: 'full', disabled: true, busy: true })
  })

  it('carries the machine\'s own sentence about a GPU that refused', () => {
    expect(localPowerRow({ mode: COOL_MODE, failure: NO_TOOL }, false))
      .toMatchObject({ mode: 'cool', failure: NO_TOOL, tag: true })
  })
})

describe('setting the mode for every Spark', () => {
  const bothRows = (extra: Partial<FleetNodeSummary> = {}, peers: Peer[] = []): FleetRow[] =>
    fleetRows(summary({ nodes: [node(), member(extra)] }), peers, 'http://attic.local:7070', NOW)

  it('names every Spark in the fleet, this console\'s own first', () => {
    expect(powerFanOut(bothRows()).map(row => row.nodeID)).toEqual(['node-lead', 'node-loft'])
  })

  it('keeps a Spark that has gone quiet, so its refusal is said out loud', () => {
    // The controller answers for that Spark by name. Dropping it silently
    // would leave the owner believing it took the mode.
    const rows = bothRows({ status: 'unreachable' })
    expect(rows[1].answering).toBe(false)
    expect(powerFanOut(rows).map(row => row.nodeID)).toEqual(['node-lead', 'node-loft'])
  })

  it('leaves out a Spark added by address', () => {
    // It never joined this fleet, so the controller holds no node of that
    // name and the call could only be refused for a machine nobody asked
    // about.
    const rows = bothRows({}, [peer('shed', 'http://shed.local:7070')])
    expect(rows.map(row => row.displayName)).toEqual(['attic', 'loft', 'shed'])
    expect(powerFanOut(rows).map(row => row.displayName)).toEqual(['attic', 'loft'])
  })

  it('shows the manager\'s own sentence when it already names the Spark', () => {
    const refusal = 'loft did not answer, so nothing changed there.'
    expect(powerRefusalLine('loft', refusal)).toBe(refusal)
  })

  it('names the Spark in front of a refusal that names none', () => {
    expect(powerRefusalLine('loft', 'Cannot reach this Spark. It may be offline or restarting. Try again soon.'))
      .toBe('loft: Cannot reach this Spark. It may be offline or restarting. Try again soon.')
  })

  it('does not read a longer word as the Spark\'s name', () => {
    // A Spark called "off" must not take the word "offline" for its own name
    // and lose the only thing that says which machine the line is about.
    expect(powerRefusalLine('off', 'offline for now, so nothing changed.'))
      .toBe('off: offline for now, so nothing changed.')
    expect(powerRefusalLine('off', 'off did not answer, so nothing changed there.'))
      .toBe('off did not answer, so nothing changed there.')
  })

  it('holds the fleet-wide button while any Spark it names is mid-change', () => {
    const rows = bothRows()
    expect(powerFanOutBusy(rows, new Set())).toBe(false)
    // The Spark being set is not the one whose card is open.
    expect(powerFanOutBusy(rows, new Set(['node-loft']))).toBe(true)
    // A Spark added by address is never named by the run, so work on it holds
    // nothing back.
    const withPeer = bothRows({}, [peer('shed', 'http://shed.local:7070')])
    expect(powerFanOutBusy(withPeer, new Set(['shed']))).toBe(false)
  })

  it('names the one Spark in the title of a single refusal', () => {
    expect(powerRefusedTitle('loft')).toBe('loft did not take the mode')
    expect(POWER_REFUSED_TITLE).toBe('Not every Spark took the mode')
  })
})

describe('whether a member console shows the "ask the controller" banner', () => {
  it('shows for a member', () => {
    expect(shouldShowMemberBanner(summary({ role: 'member' }))).toBe(true)
  })

  it('stays off for the controller', () => {
    expect(shouldShowMemberBanner(summary({ role: 'controller' }))).toBe(false)
  })

  it('stays off for a standalone Spark', () => {
    expect(shouldShowMemberBanner(summary({ role: 'standalone' }))).toBe(false)
  })

  it('stays off without a summary', () => {
    expect(shouldShowMemberBanner(null)).toBe(false)
  })
})

describe('which placement owns a model on a Spark', () => {
  it('keys every placement node by its Spark and model', () => {
    const index = deploymentIndex([deployment()])
    expect(index.get(deploymentKey('node-loft', 'qwen36-35b-a3b-nvfp4-1s'))?.deployment_id).toBe('deployment_one')
    expect(index.size).toBe(1)
  })

  it('keeps the newest placement of the same model on the same Spark', () => {
    const index = deploymentIndex([
      deployment({ deployment_id: 'deployment_new', created_at: '2026-08-23T11:00:00Z' }),
      deployment({ deployment_id: 'deployment_old', created_at: '2026-08-23T08:00:00Z' }),
    ])
    expect(index.get(deploymentKey('node-loft', 'qwen36-35b-a3b-nvfp4-1s'))?.deployment_id).toBe('deployment_new')
  })

  // The manager revives an adopted record with an update, which keeps the
  // first creation time. So a record the fleet still holds can be older than
  // one it let go of, and the older live record is the one that owns the row.
  it('keeps a live placement that is older than a removed one', () => {
    const index = deploymentIndex([
      deployment({ deployment_id: 'deployment_cleared', state: 'removed', created_at: '2026-08-23T11:00:00Z' }),
      deployment({ deployment_id: 'deployment_revived', state: 'running', created_at: '2026-08-23T08:00:00Z' }),
    ])
    expect(index.get(deploymentKey('node-loft', 'qwen36-35b-a3b-nvfp4-1s'))?.deployment_id)
      .toBe('deployment_revived')
  })

  it('keeps the newest of two removed placements', () => {
    const index = deploymentIndex([
      deployment({ deployment_id: 'deployment_old', state: 'removed', created_at: '2026-08-23T08:00:00Z' }),
      deployment({ deployment_id: 'deployment_new', state: 'removed', created_at: '2026-08-23T11:00:00Z' }),
    ])
    expect(index.get(deploymentKey('node-loft', 'qwen36-35b-a3b-nvfp4-1s'))?.deployment_id).toBe('deployment_new')
  })

  // rowPlacement hides a removed record from the row, but the record has to
  // reach the index first: the Clear tool reads it there.
  it('keeps a removed placement when the pair has no other', () => {
    const index = deploymentIndex([deployment({ deployment_id: 'deployment_cleared', state: 'removed' })])
    const kept = index.get(deploymentKey('node-loft', 'qwen36-35b-a3b-nvfp4-1s'))
    expect(kept?.deployment_id).toBe('deployment_cleared')
    expect(kept?.state).toBe('removed')
  })

  it('keeps what the controller last saw of the placement, not only its id', () => {
    const index = deploymentIndex([deployment({ stale: true })])
    expect(index.get(deploymentKey('node-loft', 'qwen36-35b-a3b-nvfp4-1s'))?.stale).toBe(true)
  })

  it('answers with an empty index when the fleet holds no placement', () => {
    expect(deploymentIndex([]).size).toBe(0)
  })
})

describe('what one read of the fleet placements leaves the table with', () => {
  const key = deploymentKey('node-loft', 'qwen36-35b-a3b-nvfp4-1s')
  const adopted = deployment({ deployment_id: 'deployment_adopted' })
  const held = new Map([[key, adopted]])

  it('keeps a record this console acted on that the read does not carry', () => {
    const merged = mergePlacements(deploymentIndex([]), held, new Set(['deployment_adopted']))
    expect(merged.get(key)?.deployment_id).toBe('deployment_adopted')
  })

  it('lets the read speak for a record this console never touched', () => {
    expect(mergePlacements(deploymentIndex([]), held, new Set(['deployment_other'])).size).toBe(0)
    expect(mergePlacements(deploymentIndex([]), held, new Set()).size).toBe(0)
  })

  it('prefers what the read carries for the same Spark and model', () => {
    const listed = deployment({ deployment_id: 'deployment_listed' })
    const merged = mergePlacements(deploymentIndex([listed]), held, new Set(['deployment_adopted']))
    expect(merged.get(key)?.deployment_id).toBe('deployment_listed')
    expect(merged.size).toBe(1)
  })

  it('keeps nothing twice when the read already carries that record', () => {
    const merged = mergePlacements(deploymentIndex([adopted]), held, new Set(['deployment_adopted']))
    expect(merged.size).toBe(1)
    expect(merged.get(key)?.deployment_id).toBe('deployment_adopted')
  })

  it('leaves every other Spark in the read alone', () => {
    const elsewhere = deployment({
      deployment_id: 'deployment_shed',
      nodes: [{ deployment_id: 'deployment_shed', node_id: 'node-shed', node_role: 'owner', rank: 0, reservation_id: '' }],
    })
    const merged = mergePlacements(deploymentIndex([elsewhere]), held, new Set(['deployment_adopted']))
    expect([...merged.keys()].sort()).toEqual(
      [key, deploymentKey('node-shed', 'qwen36-35b-a3b-nvfp4-1s')].sort(),
    )
  })
})

describe('where one row action goes', () => {
  const index = (...views: FleetDeploymentView[]) => deploymentIndex(views)
  const loft = (recipeID: string): ActionTarget =>
    ({ nodeID: 'node-loft', recipeID, isSelf: false })
  // The Spark that holds the model, as the table already built its row.
  const loftRow = (overrides: Partial<FleetRow> = {}): FleetRow => ({
    nodeID: 'node-loft',
    displayName: 'loft',
    isSelf: false,
    role: 'member',
    consoleURL: 'http://loft.local:7070',
    power: { mode: 'full', failure: '' },
    installedModels: [{ recipe_id: 'qwen36-35b-a3b-nvfp4-1s', recipe_version: 3, status: 'ready', active: true }],
    status: { word: 'Serving', dot: 'on' },
    answering: true,
    ...overrides,
  })
  const held = (recipeID: string, host: FleetRow = loftRow()): ActionTarget => ({ ...loft(recipeID), host })

  it('leaves the Spark this console runs on with its own calls', () => {
    const route = rowActionRoute({ nodeID: 'node-lead', recipeID: 'anything', isSelf: true }, new Map(), 'stop')
    expect(route).toEqual({ where: 'local' })
  })

  it('sends another Spark through the placement that owns the model', () => {
    const route = rowActionRoute(loft('qwen36-35b-a3b-nvfp4-1s'), index(deployment()), 'stop')
    expect(route).toEqual({
      where: 'fleet',
      deploymentID: 'deployment_one',
      path: '/api/v1/fleet/deployments/deployment_one/stop',
    })
  })

  it('names every action the fleet API accepts', () => {
    for (const action of FLEET_DEPLOYMENT_ACTIONS) {
      const route = rowActionRoute(loft('qwen36-35b-a3b-nvfp4-1s'), index(deployment()), action)
      expect(route).toMatchObject({ where: 'fleet', path: `/api/v1/fleet/deployments/deployment_one/${action}` })
    }
  })

  it('refuses an action the fleet API does not take', () => {
    const route = rowActionRoute(loft('qwen36-35b-a3b-nvfp4-1s'), index(deployment()), 'install')
    expect(route).toEqual({ where: 'none', reason: 'unsupported' })
  })

  it('refuses a model on another Spark no row speaks for', () => {
    const route = rowActionRoute(loft('qwen36-27b-nvfp4-1s'), index(deployment()), 'stop')
    expect(route).toEqual({ where: 'none', reason: 'no-placement' })
  })

  it('adopts a model another Spark runs that the fleet never placed', () => {
    const route = rowActionRoute(held('qwen36-35b-a3b-nvfp4-1s'), new Map(), 'stop')
    expect(route).toEqual({ where: 'adopt', nodeID: 'node-loft', recipeID: 'qwen36-35b-a3b-nvfp4-1s' })
  })

  it('offers every action the fleet API takes on a model it has not placed yet', () => {
    for (const action of FLEET_DEPLOYMENT_ACTIONS) {
      expect(rowActionRoute(held('qwen36-35b-a3b-nvfp4-1s'), new Map(), action)).toMatchObject({ where: 'adopt' })
    }
  })

  it('uses the placement it already has rather than adopting again', () => {
    const route = rowActionRoute(held('qwen36-35b-a3b-nvfp4-1s'), index(deployment()), 'stop')
    expect(route).toMatchObject({ where: 'fleet', deploymentID: 'deployment_one' })
  })

  it('refuses to adopt onto a Spark that has stopped answering', () => {
    const route = rowActionRoute(held('qwen36-35b-a3b-nvfp4-1s', loftRow({ answering: false })), new Map(), 'stop')
    expect(route).toEqual({ where: 'none', reason: 'not-answering' })
  })

  it('refuses to adopt onto a Spark added by address that never joined', () => {
    const shed = loftRow({ legacyPeerOnly: true, answering: false })
    expect(rowActionRoute(held('qwen36-35b-a3b-nvfp4-1s', shed), new Map(), 'stop'))
      .toEqual({ where: 'none', reason: 'no-placement' })
  })

  it('refuses to adopt a model that Spark does not report', () => {
    const empty = loftRow({ installedModels: [] })
    expect(rowActionRoute(held('qwen36-35b-a3b-nvfp4-1s', empty), new Map(), 'stop'))
      .toEqual({ where: 'none', reason: 'no-placement' })
  })

  it('refuses an action the fleet API does not take before adopting anything', () => {
    expect(rowActionRoute(held('qwen36-35b-a3b-nvfp4-1s'), new Map(), 'install'))
      .toEqual({ where: 'none', reason: 'unsupported' })
  })

  it('leaves the Spark this console runs on alone even with a row in hand', () => {
    const target: ActionTarget = { ...held('qwen36-35b-a3b-nvfp4-1s'), isSelf: true }
    expect(rowActionRoute(target, new Map(), 'stop')).toEqual({ where: 'local' })
  })

  it('names the fleet API once for both halves of the flow', () => {
    expect(ADOPT_PATH).toBe('/api/v1/fleet/deployments/adopt')
    expect(deploymentActionPath('deployment_one', 'stop')).toBe('/api/v1/fleet/deployments/deployment_one/stop')
  })

  it('refuses a placement whose Spark has stopped answering', () => {
    const route = rowActionRoute(loft('qwen36-35b-a3b-nvfp4-1s'), index(deployment({ stale: true })), 'stop')
    expect(route).toEqual({ where: 'none', reason: 'not-answering' })
  })

  it('escapes a placement id before it reaches a path', () => {
    const route = rowActionRoute(
      loft('qwen36-35b-a3b-nvfp4-1s'),
      index(deployment({ deployment_id: 'deployment one/two' })),
      'stop',
    )
    expect(route).toMatchObject({ path: '/api/v1/fleet/deployments/deployment%20one%2Ftwo/stop' })
  })

  it('reads the placement itself only for another Spark', () => {
    const placements = index(deployment())
    expect(rowPlacement(loft('qwen36-35b-a3b-nvfp4-1s'), placements)?.deployment_id).toBe('deployment_one')
    expect(rowPlacement(
      { nodeID: 'node-loft', recipeID: 'qwen36-35b-a3b-nvfp4-1s', isSelf: true },
      placements,
    )).toBeUndefined()
  })
})

describe('whether a placement is already working', () => {
  const job = (id: string, state: string): Job => ({
    id, kind: 'stop', recipe_id: 'qwen36-35b-a3b-nvfp4-1s', state, created_at: '', updated_at: '', steps: [],
  })

  it('leaves a Spark this console holds no placement for alone', () => {
    expect(placementBusy(undefined, undefined)).toBe(false)
    expect(placementBusy(undefined, 'job-one')).toBe(false)
  })

  it('is free when the controller last read a finished job', () => {
    expect(placementBusy(deployment({ job: job('job-one', 'ready') }), undefined)).toBe(false)
  })

  it('is busy while the controller reads a job that is still running', () => {
    expect(placementBusy(deployment({ job: job('job-one', 'starting') }), undefined)).toBe(true)
  })

  it('is busy from the moment an action is accepted, before any new read', () => {
    // The placement still carries the job it had before the click.
    expect(placementBusy(deployment({ job: job('job-one', 'ready') }), 'job-two')).toBe(true)
  })

  it('is free again once the controller reports the started job finished', () => {
    expect(placementBusy(deployment({ job: job('job-two', 'ready') }), 'job-two')).toBe(false)
  })

  it('is busy while the started job is still running', () => {
    expect(placementBusy(deployment({ job: job('job-two', 'stopping') }), 'job-two')).toBe(true)
  })

  it('is busy when a started action left the placement with no job to read', () => {
    expect(placementBusy(deployment({ job: undefined }), 'job-two')).toBe(true)
  })
})

describe('why a row action was refused', () => {
  it('has a line for every reason a route can give', () => {
    for (const reason of ['no-placement', 'not-answering', 'unsupported'] as const) {
      expect(ACTION_REFUSAL[reason].length).toBeGreaterThan(0)
    }
  })

  it('says a Spark does not answer in the words the row uses', () => {
    expect(ACTION_REFUSAL['not-answering']).toContain('does not answer')
  })

  it('has a line for an adoption that answered with no record', () => {
    expect(NO_PLACEMENT_BACK).toBe('The controller gave no placement back.')
  })
})

describe('the Sparks named on a model row', () => {
  const snapshot = (recipeID: string, active = false): FleetModelSnapshot =>
    ({ recipe_id: recipeID, recipe_version: 1, status: active ? 'ready' : 'stopped', active })

  const spark = (name: string, held: FleetModelSnapshot[]): FleetRow => ({
    nodeID: `node-${name}`,
    displayName: name,
    isSelf: false,
    role: 'member',
    consoleURL: `http://${name}.local:7070`,
    power: { mode: 'full', failure: '' },
    installedModels: held,
    serving: held.find(model => model.active),
    status: { word: 'Idle', dot: '' },
    answering: true,
  })

  it('names each Spark that holds a one-Spark model', () => {
    const sparks = [
      spark('attic', [snapshot('qwen36-27b-nvfp4-1s')]),
      spark('loft', [snapshot('qwen36-27b-nvfp4-1s', true)]),
    ]
    expect(modelChips(sparks, 'qwen36-27b-nvfp4-1s', 1)).toEqual([
      { key: 'node-attic', name: 'attic', live: false },
      { key: 'node-loft', name: 'loft', live: true },
    ])
  })

  // A model that spans the pair is on both machines at once, and "both" is
  // what the owner of two Sparks calls that.
  it('says both for a model that needs the pair', () => {
    const sparks = [
      spark('attic', [snapshot('deepseek-v4-flash-0731-2s')]),
      spark('loft', [snapshot('deepseek-v4-flash-0731-2s')]),
    ]
    expect(modelChips(sparks, 'deepseek-v4-flash-0731-2s', 2)).toEqual([
      { key: 'topology', name: BOTH_SPARKS_CHIP, live: false },
    ])
  })

  // Three or more machines have no such word, so the chip counts them.
  it('counts the machines past a pair', () => {
    const sparks = [spark('attic', [snapshot('inkling-small-nvfp4-2s')])]
    expect(modelChips(sparks, 'inkling-small-nvfp4-2s', 3)).toEqual([
      { key: 'topology', name: '3 Sparks', live: false },
    ])
  })

  it('lights the counted chip while the model serves', () => {
    const sparks = [spark('attic', [snapshot('deepseek-v4-flash-0731-2s', true)])]
    expect(modelChips(sparks, 'deepseek-v4-flash-0731-2s', 2)).toEqual([
      { key: 'topology', name: BOTH_SPARKS_CHIP, live: true },
    ])
  })

  it('names nothing for a model no Spark holds', () => {
    expect(modelChips([spark('attic', [snapshot('qwen36-27b-nvfp4-1s')])], 'deepseek-v4-flash-0731-2s', 2)).toEqual([])
  })

  it('leaves a Spark that only holds another model out', () => {
    const sparks = [
      spark('attic', [snapshot('qwen36-27b-nvfp4-1s')]),
      spark('loft', [snapshot('qwen36-35b-a3b-nvfp4-1s')]),
    ]
    expect(modelChips(sparks, 'qwen36-27b-nvfp4-1s', 1)).toEqual([
      { key: 'node-attic', name: 'attic', live: false },
    ])
  })

  // The chip carries the short name, which is the name the owner already uses
  // for the machine.
  it('names each Spark short', () => {
    const sparks = [
      spark('edgexpert-2051', [snapshot('qwen36-27b-nvfp4-1s')]),
      spark('edgexpert-37c4', [snapshot('qwen36-27b-nvfp4-1s', true)]),
    ]
    expect(modelChips(sparks, 'qwen36-27b-nvfp4-1s', 1)).toEqual([
      { key: 'node-edgexpert-2051', name: '2051', live: false },
      { key: 'node-edgexpert-37c4', name: '37c4', live: true },
    ])
  })
})

// The name a chip, a state line and the band call a Spark by. The fleet strip
// over the table keeps whole names; everything under it is short, because the
// same name is repeated on every row.
describe('the short name of a Spark', () => {
  const fleet = ['edgexpert-2051', 'edgexpert-37c4', 'spark-f1cc']

  it('keeps the part after the last dash', () => {
    expect(shortSparkName('edgexpert-37c4', fleet)).toBe('37c4')
    expect(shortSparkName('spark-f1cc', fleet)).toBe('f1cc')
  })

  it('leaves a name with no dash whole', () => {
    expect(shortSparkName('attic', ['attic', 'loft'])).toBe('attic')
    // A name that only ends or begins with a dash has no part to take.
    expect(shortSparkName('spark-', ['spark-'])).toBe('spark-')
    expect(shortSparkName('-f1cc', ['-f1cc'])).toBe('-f1cc')
  })

  // A short name two machines would share names neither of them, so both keep
  // the whole name rather than the console guessing which one is meant.
  it('keeps both names whole when the short forms would collide', () => {
    const clashing = ['edgexpert-37c4', 'spark-37c4', 'edgexpert-2051']
    expect(shortSparkName('edgexpert-37c4', clashing)).toBe('edgexpert-37c4')
    expect(shortSparkName('spark-37c4', clashing)).toBe('spark-37c4')
    // The Spark that shares its short name with nobody still reads short.
    expect(shortSparkName('edgexpert-2051', clashing)).toBe('2051')
  })
})

// The band over the table. A model that serves is the page's answer, so it
// leaves the table; everything else stays a row.
describe('which models stand over the table', () => {
  const model = (id: string, here: boolean, elsewhere: boolean) =>
    ({ id, serving: { here, elsewhere } })
  const idle = (id: string) => model(id, false, false)

  it('lifts the model this Spark serves out of the table', () => {
    const split = splitServing([idle('one'), model('two', true, false), idle('three')])
    expect(split.bands.map(item => item.id)).toEqual(['two'])
    expect(split.rows.map(item => item.id)).toEqual(['one', 'three'])
  })

  it('lifts a model that serves on another Spark out of the table too', () => {
    const split = splitServing([idle('one'), model('two', false, true)])
    expect(split.bands.map(item => item.id)).toEqual(['two'])
    expect(split.rows.map(item => item.id)).toEqual(['one'])
  })

  it('stands nothing over a table where nothing serves', () => {
    const split = splitServing([idle('one'), idle('two')])
    expect(split.bands).toEqual([])
    expect(split.rows.map(item => item.id)).toEqual(['one', 'two'])
  })

  // Two Sparks can each serve one model. Both stand over the table, in the
  // order the table held them, and neither is repeated below.
  it('stands one band over the table for each serving model', () => {
    const split = splitServing([
      model('one', true, false), idle('two'), model('three', false, true),
    ])
    expect(split.bands.map(item => item.id)).toEqual(['one', 'three'])
    expect(split.rows.map(item => item.id)).toEqual(['two'])
  })

  it('names the pair, a larger set, and one machine', () => {
    expect(servingPlace(2, '2051')).toBe(BOTH_SPARKS)
    expect(servingPlace(3, '2051')).toBe('3 Sparks')
    expect(servingPlace(1, '37c4')).toBe('37c4')
    // A console that can name no Spark says nothing about where it runs.
    expect(servingPlace(1, '')).toBe('')
  })

  // The band states which machine runs the model. That is a claim about the
  // fleet, so the two machines must never be swapped.
  it('names this Spark while this Spark serves, and the other one otherwise', () => {
    const here = { onHost: false, here: true, elsewhere: false }
    const there = { onHost: false, here: false, elsewhere: true }
    expect(servingSparkName(here, 'f1cc', '37c4')).toBe('f1cc')
    expect(servingSparkName(there, 'f1cc', '37c4')).toBe('37c4')
  })

  // A model another Spark in the fleet holds runs there whatever else is true,
  // because this Spark holds no copy of it to serve.
  it('names the host Spark for a model this one does not hold', () => {
    expect(servingSparkName({ onHost: true, here: false, elsewhere: true }, 'f1cc', '37c4')).toBe('37c4')
    expect(servingSparkName({ onHost: true, here: false, elsewhere: false }, 'f1cc', '37c4')).toBe('37c4')
  })

  it('names no machine while nothing serves', () => {
    expect(servingSparkName({ onHost: false, here: false, elsewhere: false }, 'f1cc', '37c4')).toBe('')
  })
})

// The number in the speed cell, and the mark that says where it came from.
describe('the speed one model shows', () => {
  it('shows the measurement this Spark made, to one decimal', () => {
    expect(speedText(22.24, 80, true)).toBe('22.2')
    // A measurement outranks the community figure, and needs no mark.
    expect(isTypical(speedText(22.24, 80, true))).toBe(false)
  })

  // A community report is not a measurement, and the mark is the whole of what
  // says so. Losing it would make a borrowed number read as this Spark's own.
  it('marks a community report with a tilde', () => {
    expect(speedText(undefined, 80, true)).toBe('~80')
    expect(isTypical(speedText(undefined, 80, true))).toBe(true)
  })

  it('says n/a rather than nothing, and never guesses', () => {
    expect(speedText(undefined, undefined, true)).toBe(NO_SPEED)
    // A model that generates media serves no tokens at all, whatever figures
    // are held for it.
    expect(speedText(9.5, 80, false)).toBe(NO_SPEED)
    expect(isTypical(NO_SPEED)).toBe(false)
  })

  // The note under the table explains the mark, so it is shown only while the
  // mark is on screen. The gate reads the very string the cell draws.
  it('gates the note under the table on the mark itself', () => {
    expect(isTypical(speedText(undefined, 19.4, true))).toBe(true)
    expect(isTypical(speedText(38, undefined, true))).toBe(false)
    expect(isTypical(speedText(undefined, undefined, false))).toBe(false)
  })
})

// Orange means one thing on this console: the primary action. The page holds
// one of them, on the band, and every action in the table below is a ghost.
describe('which button wears the orange pill', () => {
  it('gives the band the primary pill and the row a ghost', () => {
    expect(openPillClass(true)).toBe(PRIMARY_PILL)
    expect(openPillClass(false)).toBe(GHOST_PILL)
  })

  it('keeps the two pills apart', () => {
    expect(PRIMARY_PILL).not.toBe(GHOST_PILL)
  })
})

// The one line a row says about its own state, in place of what the model is.
describe('the state line under a model name', () => {
  it('says nothing at all about an installed or a catalog model', () => {
    expect(rowStateLine('Installed', '')).toBeNull()
    expect(rowStateLine('Not installed', '')).toBeNull()
  })

  // The ordinary two-Spark row: this console holds no copy, another Spark
  // holds a stopped one. The chip beside the name already says which Spark,
  // and the tab already says the model is installed somewhere, so the line
  // says neither and the model's own spec line stands.
  it('gives the line back to the spec for a model another Spark holds', () => {
    expect(rowStateLine('Installed', 'on 37c4')).toBeNull()
    expect(rowStateLine('Not installed', 'on 37c4')).toBeNull()
  })

  // Both machines hold it and both are quiet about it. Two quiet words are
  // still nothing to say.
  it('says nothing when both machines have it installed', () => {
    expect(rowStateLine('Installed', 'Installed on 37c4')).toBeNull()
  })

  // A quiet word on the other Spark never takes the line from this Spark's own
  // state either.
  it('drops a quiet word from the other Spark', () => {
    expect(rowStateLine('Failed', 'Installed on 37c4')).toEqual({ text: 'Failed', warn: true })
  })

  it('names the Spark a failure happened on, in amber', () => {
    expect(rowStateLine('Failed', 'on 37c4')).toEqual({ text: 'Failed on 37c4', warn: true })
    expect(rowStateLine('Failed', '')).toEqual({ text: 'Failed', warn: true })
  })

  it('reads a revoked recipe and a silent Spark as warnings too', () => {
    expect(rowStateLine('Revoked', '')).toEqual({ text: 'Revoked', warn: true })
    expect(rowStateLine('No answer', 'on 37c4')).toEqual({ text: 'No answer on 37c4', warn: true })
  })

  it('says work under way quietly', () => {
    expect(rowStateLine('Installing', 'on 37c4')).toEqual({ text: 'Installing on 37c4', warn: false })
    expect(rowStateLine('Starting', '')).toEqual({ text: 'Starting', warn: false })
    expect(rowStateLine('Recovering', '')).toEqual({ text: 'Recovering', warn: false })
  })

  // A model installed here says nothing about itself, and the other Spark's
  // own word takes the line instead.
  it('gives the line to the other Spark while this one has nothing to say', () => {
    expect(rowStateLine('Installed', 'Installing on 37c4'))
      .toEqual({ text: 'Installing on 37c4', warn: false })
  })

  // Both machines have something to say, so both are said, and neither word is
  // run into the other.
  it('keeps two states apart', () => {
    expect(rowStateLine('Failed', 'Installing on 37c4'))
      .toEqual({ text: 'Failed · Installing on 37c4', warn: true })
  })
})

describe('what each candidate machine has free', () => {
  const plan = (): PlacementPlan => ({
    recipe_id: 'qwen36-35b-a3b-nvfp4-1s',
    recipe_version: 3,
    recipe_fingerprint: 'fingerprint',
    recommended_node_id: 'node-loft',
    candidates: [
      { node_id: 'node-lead', display_name: 'attic', eligible: true },
      { node_id: 'node-loft', display_name: 'loft', eligible: false, reason: 'The fleet shows this Spark as not answering recently, so it cannot take a model now.' },
    ],
  })

  it('joins memory and disk by node id', () => {
    const joined = joinCandidatesWithInventory(plan(), summary({ nodes: [node(), member()] }))
    expect(joined[0]).toMatchObject({
      node_id: 'node-lead',
      memoryAvailableBytes: 18_000_000_000,
      storageAvailableBytes: 1_100_000_000_000,
    })
    expect(joined[1].memoryAvailableBytes).toBe(26_000_000_000)
    expect(joined[1].reason).toBe('The fleet shows this Spark as not answering recently, so it cannot take a model now.')
  })

  it('leaves a machine that reported nothing absent', () => {
    const joined = joinCandidatesWithInventory(plan(), summary({ nodes: [node({ inventory: undefined })] }))
    expect(joined[0].memoryAvailableBytes).toBeUndefined()
    expect(joined[1].storageAvailableBytes).toBeUndefined()
  })

  it('says nothing without a plan', () => {
    expect(joinCandidatesWithInventory(null, summary())).toEqual([])
  })
})

// ---- Where a new install runs ------------------------------------------------
// Every function below reads one resolved list, so these build that list the
// same way the dialog does: a plan from the controller, the membership summary
// behind it, and the rows the fleet table kept.

describe('the Run on list, resolved against the fleet table', () => {
  const RECIPE = 'qwen36-35b-a3b-nvfp4-1s'

  const plan = (overrides: Partial<PlacementPlan> = {}): PlacementPlan => ({
    recipe_id: RECIPE,
    recipe_version: 3,
    recipe_fingerprint: 'fingerprint',
    recommended_node_id: 'node-loft',
    candidates: [
      { node_id: 'node-lead', display_name: 'attic', eligible: true },
      { node_id: 'node-loft', display_name: 'loft', eligible: true },
    ],
    ...overrides,
  })

  const bothNodes = () => summary({ nodes: [node(), member()] })
  // The rows the table keeps for that same summary, which is what the dialog
  // reads: the controller's own row is the self row.
  const rows = (nodes = bothNodes()) => fleetRows(nodes, [], 'http://attic.local:7070', NOW)
  const targets = (over: Partial<PlacementPlan> = {}, nodes = bothNodes()) =>
    placementTargets(plan(over), nodes, rows(nodes), 'node-lead')

  it('names each Spark with what it has free', () => {
    const options = placementOptions(targets(), 'node-loft')
    expect(options[0]).toEqual({
      key: 'node-lead', name: 'attic', eligible: true,
      note: '18.0 GB memory free · 1.1 TB disk free',
    })
    expect(options[1].note).toBe('26.0 GB memory free · 1.1 TB disk free')
  })

  it('reads a machine that reported nothing as n/a rather than as empty', () => {
    const nodes = summary({ nodes: [node({ inventory: undefined }), member()] })
    expect(placementOptions(targets({}, nodes), 'node-loft')[0].note)
      .toBe('n/a memory free · n/a disk free')
  })

  it('marks the Spark this console runs on, and only that one', () => {
    const list = targets()
    expect(list.map(item => [item.nodeID, item.isSelf])).toEqual([
      ['node-lead', true], ['node-loft', false],
    ])
  })

  it('carries the version that Spark already holds of this same model', () => {
    const nodes = summary({
      nodes: [node(), member({ installed_models: [
        { recipe_id: RECIPE, recipe_version: 2, status: 'ready', active: false },
        { recipe_id: 'other', recipe_version: 9, status: 'ready', active: false },
      ] })],
    })
    const list = targets({}, nodes)
    expect(list[1].installedVersion).toBe(2)
    expect(list[0].installedVersion).toBeUndefined()
  })

  it('carries the model that Spark holds active, from the plan', () => {
    const current: FleetModelSnapshot =
      { recipe_id: 'minimax-h3', recipe_version: 1, status: 'starting', active: true }
    const list = targets({
      candidates: [
        { node_id: 'node-lead', display_name: 'attic', eligible: true },
        { node_id: 'node-loft', display_name: 'loft', eligible: true, current_model: current },
      ],
    })
    expect(list[1].currentModel).toEqual(current)
  })

  it('offers Choose for me last, in the words the dialog uses', () => {
    const options = placementOptions(targets(), 'node-loft')
    expect(options[options.length - 1]).toEqual({
      key: CHOOSE_FOR_ME, name: CHOOSE_FOR_ME_NAME, note: CHOOSE_FOR_ME_NOTE, eligible: true,
    })
    expect(CHOOSE_FOR_ME_NOTE).toBe('Basement picks the Spark with room.')
  })

  it('keeps a refused Spark on the list, dead, in the plan’s own words', () => {
    const reason = 'The fleet shows this Spark as not answering recently, so it cannot take a model now.'
    const options = placementOptions(targets({
      recommended_node_id: 'node-lead',
      candidates: [
        { node_id: 'node-lead', display_name: 'attic', eligible: true },
        { node_id: 'node-loft', display_name: 'loft', eligible: false, reason },
      ],
    }), 'node-lead')
    expect(options[1]).toEqual({ key: 'node-loft', name: 'loft', note: reason, eligible: false })
  })

  it('says something about a refusal that arrived with no reason', () => {
    const options = placementOptions(targets({
      recommended_node_id: 'node-lead',
      candidates: [
        { node_id: 'node-lead', display_name: 'attic', eligible: true },
        { node_id: 'node-loft', display_name: 'loft', eligible: false },
      ],
    }), 'node-lead')
    expect(options[1].note).toBe(PLACEMENT_REFUSED)
  })

  // The table drops a node with no console URL and de-duplicates two that
  // share one, so the plan can name a Spark no row speaks for. The dialog must
  // not offer it: the facts it would show and the machine it would install on
  // would be two different Sparks.
  it('refuses a Spark the fleet table kept no row for', () => {
    const nodes = summary({ nodes: [node(), member({ console_url: '' })] })
    const list = targets({}, nodes)
    expect(list[1]).toMatchObject({ nodeID: 'node-loft', eligible: false, reason: NO_FLEET_ROW })
    expect(placementOptions(list, 'node-loft').map(option => option.key))
      .toEqual(['node-lead', 'node-loft'])
  })

  it('refuses a Spark whose row another row already answers on', () => {
    const nodes = summary({ nodes: [node(), member({ console_url: 'http://attic.local:7070' })] })
    expect(targets({}, nodes)[1]).toMatchObject({ eligible: false, reason: NO_FLEET_ROW })
  })

  it('still reaches its own Spark when the table kept no row for it', () => {
    const nodes = summary({ nodes: [node({ console_url: '' }), member()] })
    expect(targets({}, nodes)[0]).toMatchObject({ nodeID: 'node-lead', isSelf: true, eligible: true })
  })

  it('does not offer to choose when no Spark can take the model', () => {
    const list = targets({
      recommended_node_id: '',
      candidates: [{ node_id: 'node-loft', display_name: 'loft', eligible: false, reason: 'busy' }],
    })
    expect(placementOptions(list, '').map(option => option.key)).toEqual(['node-loft'])
  })

  it('does not offer to choose a Spark the plan itself refused', () => {
    const list = targets({
      recommended_node_id: 'node-loft',
      candidates: [{ node_id: 'node-loft', display_name: 'loft', eligible: false, reason: 'busy' }],
    })
    expect(placementOptions(list, 'node-loft')).toHaveLength(1)
  })

  it('says nothing at all without a plan', () => {
    expect(placementTargets(null, summary(), rows(), 'node-lead')).toEqual([])
    expect(placementOptions([], undefined)).toEqual([])
  })

  it('formats one machine on its own', () => {
    expect(machineNote({ memoryAvailableBytes: 26_000_000_000 }))
      .toBe('26.0 GB memory free · n/a disk free')
  })
})

describe('which Spark the dialog opens on', () => {
  const target = (overrides: Partial<PlacementTarget>): PlacementTarget => ({
    nodeID: 'node-lead', name: 'attic', isSelf: false, eligible: true, reason: '', ...overrides,
  })

  it('opens on the Spark the controller recommends', () => {
    const list = [target({}), target({ nodeID: 'node-loft', name: 'loft' })]
    expect(initialPlacement(list, 'node-loft')).toBe('node-loft')
  })

  it('takes the first Spark that could take it when nothing is recommended', () => {
    const list = [
      target({ eligible: false, reason: 'busy' }),
      target({ nodeID: 'node-loft', name: 'loft' }),
    ]
    expect(initialPlacement(list, undefined)).toBe('node-loft')
  })

  it('ignores a recommendation that is itself refused', () => {
    const list = [
      target({ eligible: false, reason: 'busy' }),
      target({ nodeID: 'node-loft', name: 'loft' }),
    ]
    expect(initialPlacement(list, 'node-lead')).toBe('node-loft')
  })

  it('opens on nothing when no Spark can take the model', () => {
    expect(initialPlacement([target({ eligible: false, reason: 'busy' })], 'node-lead')).toBe('')
    expect(initialPlacement([], 'node-lead')).toBe('')
  })

  it('resolves Choose for me to the recommendation at the moment it is used', () => {
    const list = [target({}), target({ nodeID: 'node-loft', name: 'loft' })]
    expect(placedTarget(CHOOSE_FOR_ME, list, 'node-loft')?.nodeID).toBe('node-loft')
    expect(placedTarget(CHOOSE_FOR_ME, list, undefined)).toBeUndefined()
    expect(placedTarget('node-lead', list, 'node-loft')?.nodeID).toBe('node-lead')
    expect(placedTarget('node-shed', list, 'node-loft')).toBeUndefined()
  })
})

describe('where a confirmed install is sent', () => {
  const target = (overrides: Partial<PlacementTarget>): PlacementTarget => ({
    nodeID: 'node-lead', name: 'attic', isSelf: false, eligible: true, reason: '', ...overrides,
  })
  const list = (): PlacementTarget[] => [
    target({ isSelf: true }),
    target({ nodeID: 'node-loft', name: 'loft' }),
    target({ nodeID: 'node-shed', name: 'shed', eligible: false, reason: 'busy' }),
  ]

  it('keeps this Spark on the install call it has always used', () => {
    expect(installRoute('node-lead', list(), 'node-loft')).toEqual({ where: 'local' })
  })

  it('sends another Spark through the fleet, with the row the dialog showed', () => {
    const route = installRoute('node-loft', list(), 'node-loft')
    expect(route.where).toBe('fleet')
    expect(route.where === 'fleet' && route.target.name).toBe('loft')
  })

  it('sends Choose for me to the recommended Spark', () => {
    const route = installRoute(CHOOSE_FOR_ME, list(), 'node-loft')
    expect(route.where === 'fleet' && route.target.nodeID).toBe('node-loft')
  })

  it('sends Choose for me nowhere else when this Spark is the recommended one', () => {
    expect(installRoute(CHOOSE_FOR_ME, list(), 'node-lead')).toEqual({ where: 'local' })
  })

  it('sends nothing for a refused, unknown or unpicked Spark', () => {
    expect(installRoute('node-shed', list(), 'node-loft')).toEqual({ where: 'none' })
    expect(installRoute('node-attic', list(), 'node-loft')).toEqual({ where: 'none' })
    expect(installRoute('', list(), 'node-loft')).toEqual({ where: 'none' })
    expect(installRoute(CHOOSE_FOR_ME, list(), undefined)).toEqual({ where: 'none' })
    expect(installRoute('node-loft', [], 'node-loft')).toEqual({ where: 'none' })
  })

  // The refusal above is what keeps the dialog and the request on one machine:
  // a Spark with no fleet row is refused, so it can never be routed to while
  // the dialog shows this Spark's own facts.
  it('sends nothing to a Spark the fleet table kept no row for', () => {
    const unreachable = [target({ nodeID: 'node-loft', eligible: false, reason: NO_FLEET_ROW })]
    expect(installRoute('node-loft', unreachable, 'node-loft')).toEqual({ where: 'none' })
  })

  it('names the Spark beside every confirmation the local install sends', () => {
    expect(fleetInstallRequest('qwen36-27b-nvfp4-1s', 'node-loft', {
      confirmed: true, accept_licence: true, confirm_territory_eligibility: false, activate: true,
    })).toEqual({
      recipe_id: 'qwen36-27b-nvfp4-1s',
      node_id: 'node-loft',
      confirmed: true,
      accept_licence: true,
      confirm_territory_eligibility: false,
      activate: true,
    })
  })
})

describe('what the dialog says about the Spark it targets', () => {
  const target = (overrides: Partial<PlacementTarget>): PlacementTarget => ({
    nodeID: 'node-loft', name: 'loft', isSelf: false, eligible: true, reason: '', ...overrides,
  })

  it('reads an older version on that Spark as an update', () => {
    expect(placementVerb(target({ installedVersion: 2 }), 3)).toBe('Update')
  })

  it('reads the same version, another model, or nothing held as an install', () => {
    expect(placementVerb(target({ installedVersion: 3 }), 3)).toBe('Install')
    expect(placementVerb(target({ installedVersion: 4 }), 3)).toBe('Install')
    expect(placementVerb(target({}), 3)).toBe('Install')
  })

  it('names the model that has to stop on that Spark', () => {
    const current: FleetModelSnapshot =
      { recipe_id: 'minimax-h3', recipe_version: 1, status: 'ready', active: true }
    expect(placementSwitchFrom(target({ currentModel: current }))).toBe('minimax-h3')
  })

  // A model that is still starting holds the machine just as firmly as one
  // that has finished. Reading only the serving fact would miss it, and the
  // dialog would promise that nothing stops.
  it('names a model that is active but still starting', () => {
    const starting: FleetModelSnapshot =
      { recipe_id: 'minimax-h3', recipe_version: 1, status: 'starting', active: true }
    expect(placementSwitchFrom(target({ currentModel: starting }))).toBe('minimax-h3')
  })

  it('names this same model when that Spark is the one holding it active', () => {
    const same: FleetModelSnapshot =
      { recipe_id: 'qwen36-27b-nvfp4-1s', recipe_version: 2, status: 'ready', active: true }
    expect(placementSwitchFrom(target({ currentModel: same }))).toBe('qwen36-27b-nvfp4-1s')
  })

  it('names nothing when that Spark holds nothing active', () => {
    expect(placementSwitchFrom(target({}))).toBeUndefined()
    expect(placementSwitchFrom(undefined)).toBeUndefined()
  })
})

describe('a model the fleet is still working on', () => {
  // The one set the rows read, so this asserts about the rows themselves and
  // not about a copy of them.
  const DISRUPTIVE = DISRUPTIVE_KINDS

  const job = (id: string, kind: string, state: string): Job =>
    ({ id, kind, recipe_id: 'qwen36-35b-a3b-nvfp4-1s', state, created_at: '', updated_at: '', steps: [] })

  const placed = (deployment: FleetDeploymentView): Map<string, FleetDeploymentView> =>
    new Map([[deploymentKey(deployment.owner_node_id, deployment.recipe_id), deployment]])

  // The exact state a remote install is in between the click and the finish:
  // the record exists and its job is running, and no Spark names the model in
  // a heartbeat yet.
  const installing = deployment({ job: job('job-install', 'install', 'running') })

  it('finds the placement that is still installing', () => {
    expect(workingPlacement(placed(installing), new Map(), installing.recipe_id, DISRUPTIVE))
      .toBe(installing)
  })

  it('finds it without any Spark holding the model', () => {
    // fleetRows never sees this model: the target Spark installed_models is
    // empty until the install lands. The record is the only signal there is.
    const sparks = fleetRows(summary({ nodes: [node(), member()] }), [], 'http://attic.local:7070', NOW)
    expect(sparks.every(spark => spark.installedModels.length === 0)).toBe(true)
    expect(workingPlacement(placed(installing), new Map(), installing.recipe_id, DISRUPTIVE)).toBe(installing)
  })

  it('lets go once that job is finished and this console has seen it', () => {
    const done = deployment({ job: job('job-install', 'install', 'ready') })
    const started = new Map([[done.deployment_id, 'job-install']])
    expect(workingPlacement(placed(done), started, done.recipe_id, DISRUPTIVE)).toBeUndefined()
  })

  it('holds on while the poll still reports the job this console started', () => {
    const stale = deployment({ job: job('job-old', 'install', 'ready') })
    const started = new Map([[stale.deployment_id, 'job-install']])
    expect(workingPlacement(placed(stale), started, stale.recipe_id, DISRUPTIVE)).toBe(stale)
  })

  it('ignores work on another model', () => {
    expect(workingPlacement(placed(installing), new Map(), 'qwen36-27b-nvfp4-1s', DISRUPTIVE)).toBeUndefined()
    expect(workingPlacement(new Map(), new Map(), installing.recipe_id, DISRUPTIVE)).toBeUndefined()
  })

  it('leaves the row alone for a measurement on another Spark', () => {
    const measuring = deployment({ job: job('job-bench', 'benchmark', 'running') })
    expect(workingPlacement(placed(measuring), new Map(), measuring.recipe_id, DISRUPTIVE))
      .toBeUndefined()
  })

  // Adoption only writes down a model that Spark already runs. It starts
  // nothing, so it must never take a row's buttons away, even in the moment
  // between the record being written and the carrier job reading terminal.
  it('leaves the row alone while the fleet records a model that already runs', () => {
    expect(DISRUPTIVE.has('adopt')).toBe(false)
    const adopting = deployment({ job: job('job-adopt', 'adopt', 'queued') })
    expect(workingPlacement(placed(adopting), new Map(), adopting.recipe_id, DISRUPTIVE)).toBeUndefined()
  })

  it('locks the row for a placement it has read no job kind for', () => {
    const unread = deployment({ job: undefined })
    const started = new Map([[unread.deployment_id, 'job-install']])
    expect(workingPlacement(placed(unread), started, unread.recipe_id, DISRUPTIVE)).toBe(unread)
  })

  it('says what that Spark is doing, from the job it runs', () => {
    expect(placementWord(installing)).toBe('Installing')
    expect(placementWord(deployment({ job: job('j', 'start', 'running') }))).toBe('Starting')
    expect(placementWord(deployment({ job: job('j', 'stop', 'running') }))).toBe('Stopping')
    expect(placementWord(deployment({ job: job('j', 'remove', 'running') }))).toBe('Removing')
  })

  it('never invents a word for work of a kind it does not know', () => {
    expect(placementWord(deployment({ job: job('j', 'benchmark', 'running') }))).toBe('Working')
    expect(placementWord(deployment({ job: undefined }))).toBe('Working')
    expect(placementWord(undefined)).toBe('')
  })

  // The install dialog asks a different question of the same placements: not
  // "is anything working on this model" but "which Sparks is it working on",
  // so that the "Run on" list can refuse those and only those.
  it('names every Spark the fleet is working on this model on', () => {
    const elsewhere = deployment({
      deployment_id: 'deployment_two', owner_node_id: 'node-lead',
      job: job('job-install-two', 'install', 'running'),
    })
    const placements = new Map([
      [deploymentKey(installing.owner_node_id, installing.recipe_id), installing],
      [deploymentKey(elsewhere.owner_node_id, elsewhere.recipe_id), elsewhere],
    ])
    expect(workingNodes(placements, new Map(), installing.recipe_id, DISRUPTIVE))
      .toEqual(new Set(['node-loft', 'node-lead']))
  })

  it('names no Spark for a model nothing is working on', () => {
    const done = deployment({ job: job('job-install', 'install', 'ready') })
    const started = new Map([[done.deployment_id, 'job-install']])
    expect(workingNodes(placed(done), started, done.recipe_id, DISRUPTIVE)).toEqual(new Set())
    expect(workingNodes(placed(installing), new Map(), 'qwen36-27b-nvfp4-1s', DISRUPTIVE))
      .toEqual(new Set())
  })

  it('leaves a Spark alone for a measurement, exactly as the row lock does', () => {
    const measuring = deployment({ job: job('job-bench', 'benchmark', 'running') })
    expect(workingNodes(placed(measuring), new Map(), measuring.recipe_id, DISRUPTIVE))
      .toEqual(new Set())
  })
})

// The double-install hole: the plan reads heartbeats, and a Spark names a
// model in a heartbeat only once its install has finished. Between the click
// and the finish the plan still offers that machine, so a second install of
// one model onto one Spark was one click away. The manager refuses a second
// record now; the list refuses it first, and says why.
describe('a Spark the fleet is already working on this model on', () => {
  const RECIPE = 'qwen36-35b-a3b-nvfp4-1s'

  const plan = (): PlacementPlan => ({
    recipe_id: RECIPE,
    recipe_version: 3,
    recipe_fingerprint: 'fingerprint',
    recommended_node_id: 'node-loft',
    candidates: [
      { node_id: 'node-lead', display_name: 'attic', eligible: true },
      { node_id: 'node-loft', display_name: 'loft', eligible: true },
    ],
  })

  const nodes = () => summary({ nodes: [node(), member()] })
  const targets = (busy: Set<string>) =>
    placementTargets(plan(), nodes(), fleetRows(nodes(), [], 'http://attic.local:7070', NOW), 'node-lead', busy)

  it('marks that Spark dead in the Run on list, and says why', () => {
    const list = targets(new Set(['node-loft']))
    expect(list[0]).toMatchObject({ nodeID: 'node-lead', eligible: true, reason: '' })
    expect(list[1]).toMatchObject({ nodeID: 'node-loft', eligible: false, reason: PLACEMENT_WORKING })
    expect(placementOptions(list, 'node-loft')[1])
      .toEqual({ key: 'node-loft', name: 'loft', note: PLACEMENT_WORKING, eligible: false })
  })

  it('refuses the install that names it, however it was picked', () => {
    const list = targets(new Set(['node-loft']))
    expect(installRoute('node-loft', list, 'node-loft')).toEqual({ where: 'none' })
    // "Choose for me" resolves to the recommendation, so it must not offer a
    // way around the refusal either, and it is not offered at all.
    expect(installRoute(CHOOSE_FOR_ME, list, 'node-loft')).toEqual({ where: 'none' })
    expect(placementOptions(list, 'node-loft').map(option => option.key))
      .toEqual(['node-lead', 'node-loft'])
    // The dialog opens on the Spark that can still take the model.
    expect(initialPlacement(list, 'node-loft')).toBe('node-lead')
  })

  it('leaves every other Spark exactly as it was', () => {
    expect(targets(new Set()).map(target => target.eligible)).toEqual([true, true])
    expect(targets(new Set(['node-other'])).map(target => target.eligible)).toEqual([true, true])
  })

  // The controller knows more about that machine than this console does, so
  // when both have something to say, the plan's own words are kept.
  it('keeps the plan’s own reason for a Spark the plan itself refused', () => {
    const refused = plan()
    refused.candidates[1] = {
      node_id: 'node-loft', display_name: 'loft', eligible: false, reason: PLACEMENT_REFUSED,
    }
    const list = placementTargets(
      refused, nodes(), fleetRows(nodes(), [], 'http://attic.local:7070', NOW), 'node-lead',
      new Set(['node-loft']),
    )
    expect(list[1].reason).toBe(PLACEMENT_REFUSED)
  })
})

// A record the controller can no longer read pins its row to "No answer" and
// kills every button on it. Ending the record is the way out, and the words
// say what really happens: the model goes with it.
describe('clearing a record the fleet can no longer read', () => {
  // The manager never deletes a placement row, it marks it removed, so the
  // read keeps reporting a record the fleet has let go of. Reading it as an
  // owner would leave the row pinned to whatever that record last said and
  // would keep adopt-on-demand from writing a fresh one. This is what gives a
  // cleared row back.
  it('gives the row back once the record reads removed', () => {
    const target: ActionTarget = { nodeID: 'node-loft', recipeID: 'qwen36-35b-a3b-nvfp4-1s', isSelf: false }
    const cleared = deploymentIndex([deployment({ state: 'removed', stale: true })])
    expect(rowPlacement(target, cleared)).toBeUndefined()
    // With no owner, the row asks the Spark itself again, which is what lets
    // adopt-on-demand rebuild the record.
    const host: FleetRow = {
      nodeID: 'node-loft', displayName: 'loft', isSelf: false, role: 'member',
      consoleURL: 'http://loft.local:7070',
      installedModels: [{ recipe_id: 'qwen36-35b-a3b-nvfp4-1s', recipe_version: 3, status: 'ready', active: true }],
      status: { word: 'Serving', dot: 'on' }, answering: true, power: { mode: 'full', failure: '' },
    }
    expect(rowActionRoute({ ...target, host }, cleared, 'stop'))
      .toEqual({ where: 'adopt', nodeID: 'node-loft', recipeID: 'qwen36-35b-a3b-nvfp4-1s' })
  })

  it('still reads a record the fleet has not let go of', () => {
    const target: ActionTarget = { nodeID: 'node-loft', recipeID: 'qwen36-35b-a3b-nvfp4-1s', isSelf: false }
    expect(rowPlacement(target, deploymentIndex([deployment({ state: 'running' })]))?.deployment_id)
      .toBe('deployment_one')
  })

  it('names where the fleet ends a record it cannot act on', () => {
    expect(releasePath('deployment_one')).toBe('/api/v1/fleet/deployments/deployment_one/release')
    expect(releasePath('a/b')).toBe('/api/v1/fleet/deployments/a%2Fb/release')
  })

  it('says what it is and what it does', () => {
    expect(CLEAR_RECORD).toBe('Clear')
    expect(CLEAR_RECORD_CONFIRM).toBe('Clear record')
    expect(clearRecordTitle('Qwen3.6 35B')).toBe('Clear the record for Qwen3.6 35B?')
    expect(clearRecordBody('loft')).toBe(
      'The fleet cannot read this model on loft. Basement asks loft to remove the model. ' +
      'This ends the record, and the downloaded files stay.',
    )
  })

  it('names the Spark rather than promising something about every Spark', () => {
    expect(clearRecordBody('loft')).toContain('loft')
    expect(clearRecordBody('shed')).not.toContain('loft')
  })
})

describe('what locks one row', () => {
  const working = deployment({ state: 'installing' })

  it('locks a model this Spark does not hold while the fleet installs it', () => {
    expect(recipeBusy(false, false, working)).toBe(true)
  })

  // A model installed here is its own. A remote install of the same recipe
  // downloads other files onto another machine: it does not stop this one,
  // start it, or touch what it serves, so Stop, Update, Open and the row's
  // tools stay live.
  it('leaves a model this Spark holds alone while the fleet installs it', () => {
    expect(recipeBusy(false, true, working)).toBe(false)
  })

  it('locks either row while this Spark is doing the work', () => {
    expect(recipeBusy(true, true, undefined)).toBe(true)
    expect(recipeBusy(true, false, undefined)).toBe(true)
  })

  it('locks nothing when neither Spark is working', () => {
    expect(recipeBusy(false, false, undefined)).toBe(false)
    expect(recipeBusy(false, true, undefined)).toBe(false)
  })
})

describe('which tab a model sits under', () => {
  const snapshot = (recipeID: string): FleetModelSnapshot =>
    ({ recipe_id: recipeID, recipe_version: 1, status: 'stopped', active: false })

  const row = (name: string, holds: FleetModelSnapshot[] = []): FleetRow => ({
    nodeID: `node-${name}`,
    displayName: name,
    isSelf: false,
    role: 'member',
    consoleURL: `http://${name}.local:7070`,
    power: { mode: 'full', failure: '' },
    installedModels: holds,
    status: { word: 'Idle', dot: '' },
    answering: true,
  })

  // A Spark added by address only reports nothing to this console, so its row
  // carries no model list at all.
  const byAddress = (name: string): FleetRow => ({ ...row(name), legacyPeerOnly: true })

  // The catalog groups by the lab that made the model, so a recipe here
  // carries the same model_by field the manager sends.
  const recipe = (id: string, model_by: string) => ({ id, model_by })

  const fast = recipe('qwen36-35b-a3b-nvfp4-1s', 'Qwen team, Alibaba')
  const coder = recipe('qwen36-27b-nvfp4-1s', 'Qwen team, Alibaba')
  const agent = recipe('laguna-s-2-1-nvfp4-dflash-1s', 'poolside')
  const flagship = recipe('deepseek-v4-flash-0731-2s', 'DeepSeek')
  const catalogue = [fast, coder, agent, flagship]

  // What the catalog pane draws: one divider for each lab, and the models
  // under it, in the order the split hands them over.
  const shelves = <T extends { id: string }>(labs: LabGroup<T>[]) =>
    labs.map(group => [group.label, group.models.map(model => model.id)])

  const none: ReadonlySet<string> = new Set()

  it('puts a model this Spark holds on the first tab', () => {
    const split = splitModels(catalogue, new Set([fast.id]), none, [])
    expect(split.held).toEqual([fast])
    expect(shelves(split.labs)).toEqual([
      ['Qwen · Alibaba', [coder.id]],
      ['poolside', [agent.id]],
      ['DeepSeek', [flagship.id]],
    ])
    expect(split.catalogCount).toBe(3)
  })

  it('puts a model another Spark in the fleet holds on the first tab', () => {
    const split = splitModels(catalogue, none, none, [row('attic'), row('loft', [snapshot(coder.id)])])
    expect(split.held).toEqual([coder])
    expect(shelves(split.labs)).toEqual([
      ['Qwen · Alibaba', [fast.id]],
      ['poolside', [agent.id]],
      ['DeepSeek', [flagship.id]],
    ])
  })

  // A Spark added by address is in no fleet, so its row reports nothing. Its
  // own summary is what the row on screen already reads, and the tab has to
  // agree with the row: one cannot say "Installed on shed" under the other
  // saying "not installed".
  it('puts a model the paired Spark holds on the first tab', () => {
    const split = splitModels(catalogue, none, new Set([agent.id]), [byAddress('shed')])
    expect(split.held).toEqual([agent])
    expect(shelves(split.labs)).toEqual([
      ['Qwen · Alibaba', [fast.id, coder.id]],
      ['DeepSeek', [flagship.id]],
    ])
    expect(split.catalogCount).toBe(3)
  })

  it('puts a model that spans two Sparks on the first tab', () => {
    const sparks = [row('attic', [snapshot(flagship.id)]), row('loft', [snapshot(flagship.id)])]
    const split = splitModels(catalogue, none, none, sparks)
    expect(split.held).toEqual([flagship])
    expect(shelves(split.labs)).toEqual([
      ['Qwen · Alibaba', [fast.id, coder.id]],
      ['poolside', [agent.id]],
    ])
    expect(split.catalogCount).toBe(3)
  })

  it('names a model once, whichever Spark holds it', () => {
    const sparks = [row('attic', [snapshot(fast.id)]), row('loft', [snapshot(fast.id)])]
    const split = splitModels(catalogue, new Set([fast.id]), new Set([fast.id]), sparks)
    expect(split.held).toEqual([fast])
    expect(split.held.length + split.catalogCount).toBe(catalogue.length)
  })

  it('keeps the order it was given inside each group', () => {
    const split = splitModels(catalogue, none, none, [])
    expect(split.held).toEqual([])
    expect(shelves(split.labs)).toEqual([
      ['Qwen · Alibaba', [fast.id, coder.id]],
      ['poolside', [agent.id]],
      ['DeepSeek', [flagship.id]],
    ])
    expect(split.catalogCount).toBe(4)
  })

  // The obliterated build names its ablation author in the same field as the
  // model's maker. It is still a Qwen model and it reads under the Qwen
  // divider, not under a shelf of its own.
  it('reads both forms of the Qwen team name as one lab', () => {
    const obliterated = recipe(
      'qwen38-27b-obliterated-q8-0-1s',
      'Qwen team, Alibaba; abliteration by OBLITERATUS',
    )
    const split = splitModels([fast, obliterated], none, none, [])
    expect(shelves(split.labs)).toEqual([['Qwen · Alibaba', [fast.id, obliterated.id]]])
  })

  // model_by is free text a feed can deliver in any case. One lab written two
  // ways draws one divider, under the first spelling the catalog gave it.
  it('draws one divider for a lab written two ways', () => {
    const shouty = recipe('deepseek-v5-2s', 'DEEPSEEK')
    const split = splitModels([flagship, shouty], none, none, [])
    expect(shelves(split.labs)).toEqual([['DeepSeek', [flagship.id, shouty.id]]])
  })

  // A recipe that names no maker still has to sit somewhere, and it sits
  // under the name it does declare rather than under a lab that has not
  // claimed it.
  it('falls back to the publisher, then to Community', () => {
    const published = { id: 'published-1s', publisher: 'Comfy-Org' }
    const anonymous = { id: 'anonymous-1s' }
    const split = splitModels([published, anonymous], none, none, [])
    expect(shelves(split.labs)).toEqual([
      ['Comfy-Org', [published.id]],
      ['Community', [anonymous.id]],
    ])
  })

  // sortCatalog hands the catalog over with each lab's models already
  // together. A list that arrives in any other order still gets one divider
  // for each lab, in the order each lab first appears.
  it('gives a lab one group even when its models arrive apart', () => {
    const split = splitModels([fast, agent, coder], none, none, [])
    expect(shelves(split.labs)).toEqual([
      ['Qwen · Alibaba', [fast.id, coder.id]],
      ['poolside', [agent.id]],
    ])
  })

  // The first run: nothing is installed on any machine this console speaks
  // for, so the first tab is empty. The view reads that same emptiness, and
  // shows the hero and one plain list instead of tabs.
  it('holds nothing on the first tab before anything is installed', () => {
    expect(splitModels(catalogue, none, none, [row('attic'), byAddress('shed')]).held).toEqual([])
  })

  it('leaves the catalog empty once every model is on a Spark', () => {
    const split = splitModels(catalogue, new Set(catalogue.map(item => item.id)), none, [])
    expect(split.held).toEqual(catalogue)
    expect(split.catalogCount).toBe(0)
    expect(split.labs).toEqual([])
  })

  it('reads a Spark added by address through its own summary, not its fleet row', () => {
    expect(heldSomewhere(fast.id, none, none, [byAddress('shed')])).toBe(false)
    expect(heldSomewhere(fast.id, none, new Set([fast.id]), [byAddress('shed')])).toBe(true)
    expect(heldSomewhere(fast.id, none, new Set([coder.id]), [byAddress('shed')])).toBe(false)
  })

  it('reads this Spark and every fleet Spark as well', () => {
    expect(heldSomewhere(fast.id, new Set([fast.id]), none, [])).toBe(true)
    expect(heldSomewhere(fast.id, none, none, [row('loft', [snapshot(fast.id)])])).toBe(true)
    expect(heldSomewhere(fast.id, none, none, [row('loft', [snapshot(coder.id)])])).toBe(false)
  })

  it('says Spark for one machine and Sparks for more', () => {
    expect(heldTabLabel(0)).toBe(ONE_SPARK_TAB)
    expect(heldTabLabel(1)).toBe(ONE_SPARK_TAB)
    expect(heldTabLabel(2)).toBe(MANY_SPARKS_TAB)
    expect(heldTabLabel(3)).toBe(MANY_SPARKS_TAB)
  })

  it('says each label in the approved words', () => {
    expect(ONE_SPARK_TAB).toBe('On your Spark')
    expect(MANY_SPARKS_TAB).toBe('On your Sparks')
    expect(CATALOG_TAB).toBe('Catalog')
    expect(CATALOG_EMPTY).toBe('Every model is installed.')
  })
})
