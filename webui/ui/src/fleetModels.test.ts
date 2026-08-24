import { describe, expect, it } from 'vitest'
import type {
  FleetDeploymentView, FleetModelSnapshot, FleetNodeSummary, FleetSummary, Job, NodeInventory, Peer, PlacementPlan,
} from './api'
import {
  deploymentActionPath, deploymentIndex, deploymentKey, fleetInstallRequest, fleetRows,
  initialPlacement, installRoute, joinCandidatesWithInventory, machineNote, mergePlacements,
  modelChips, placedTarget, placementBusy, placementOptions, placementSwitchFrom, placementTargets,
  placementVerb, placementWord, recipeBusy, rowActionRoute, rowPlacement, shouldShowMemberBanner, workingPlacement,
  ACTION_REFUSAL, ADOPT_PATH, CHOOSE_FOR_ME, CHOOSE_FOR_ME_NAME, CHOOSE_FOR_ME_NOTE, DISRUPTIVE_KINDS,
  FLEET_DEPLOYMENT_ACTIONS, NO_FLEET_ROW, NO_PLACEMENT_BACK, PLACEMENT_REFUSED,
  type ActionTarget, type FleetRow, type PlacementTarget,
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
    const reason = 'the node is stale and cannot accept a placement'
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
