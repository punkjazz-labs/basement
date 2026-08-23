import { describe, expect, it } from 'vitest'
import type {
  FleetDeploymentView, FleetModelSnapshot, FleetNodeSummary, FleetSummary, Job, NodeInventory, Peer, PlacementPlan,
} from './api'
import {
  deploymentActionPath, deploymentIndex, deploymentKey, fleetRows, joinCandidatesWithInventory,
  mergePlacements, modelChips, placementBusy, rowActionRoute, rowPlacement,
  ACTION_REFUSAL, ADOPT_PATH, FLEET_DEPLOYMENT_ACTIONS, NO_PLACEMENT_BACK,
  type ActionTarget, type FleetRow,
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
    consoleURL: 'http://loft.local:7070',
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

  it('says a Spark is not answering in the words the row uses', () => {
    expect(ACTION_REFUSAL['not-answering']).toContain('not answering')
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
    consoleURL: `http://${name}.local:7070`,
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

  it('counts the machines for a model that needs more than one Spark', () => {
    const sparks = [
      spark('attic', [snapshot('deepseek-v4-flash-0731-2s')]),
      spark('loft', [snapshot('deepseek-v4-flash-0731-2s')]),
    ]
    expect(modelChips(sparks, 'deepseek-v4-flash-0731-2s', 2)).toEqual([
      { key: 'topology', name: '2 Sparks', live: false },
    ])
  })

  it('lights the counted chip while the model serves', () => {
    const sparks = [spark('attic', [snapshot('deepseek-v4-flash-0731-2s', true)])]
    expect(modelChips(sparks, 'deepseek-v4-flash-0731-2s', 2)).toEqual([
      { key: 'topology', name: '2 Sparks', live: true },
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
})

describe('what each candidate machine has free', () => {
  const plan = (): PlacementPlan => ({
    recipe_id: 'qwen36-35b-a3b-nvfp4-1s',
    recipe_version: 3,
    recipe_fingerprint: 'fingerprint',
    recommended_node_id: 'node-loft',
    candidates: [
      { node_id: 'node-lead', display_name: 'attic', eligible: true },
      { node_id: 'node-loft', display_name: 'loft', eligible: false, reason: 'the node is stale and cannot accept a placement' },
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
    expect(joined[1].reason).toBe('the node is stale and cannot accept a placement')
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
