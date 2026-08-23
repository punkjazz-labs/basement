import { describe, expect, it } from 'vitest'
import type {
  FleetDeploymentView, FleetNodeSummary, FleetSummary, NodeInventory, Peer, PlacementPlan,
} from './api'
import { deploymentIndex, deploymentKey, fleetRows, joinCandidatesWithInventory } from './fleetModels'

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

const node = (overrides: Partial<FleetNodeSummary> = {}): FleetNodeSummary => ({
  node_id: 'node-lead',
  display_name: 'attic',
  role: 'controller',
  status: 'fresh',
  console_url: 'http://attic.local:7070',
  node_url: 'https://attic.local:7071',
  manager_version: 'v0.5.16',
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
    const rows = fleetRows(summary({ nodes: [node(), member()] }), [], 'http://attic.local:7070')
    expect(rows.map(row => [row.displayName, row.isSelf])).toEqual([['attic', true], ['loft', false]])
  })

  it('carries what each Spark reported about itself', () => {
    const rows = fleetRows(summary({ nodes: [member()] }), [], 'http://attic.local:7070')
    expect(rows[0].inventory?.memory_available_bytes).toBe(26_000_000_000)
    expect(rows[0].inventory?.storage_available_bytes).toBe(1_100_000_000_000)
    expect(rows[0].status).toEqual({ word: 'Idle', dot: '' })
  })

  it('reads the model a Spark serves and the ones it only holds', () => {
    const installed = [
      { recipe_id: 'qwen36-27b-nvfp4-1s', recipe_version: 2, status: 'stopped', active: false },
      { recipe_id: 'qwen36-35b-a3b-nvfp4-1s', recipe_version: 3, status: 'ready', active: true },
    ]
    const rows = fleetRows(summary({ nodes: [member({ installed_models: installed })] }), [], '')
    expect(rows[0].installedModels).toHaveLength(2)
    expect(rows[0].serving?.recipe_id).toBe('qwen36-35b-a3b-nvfp4-1s')
    expect(rows[0].status).toEqual({ word: 'Serving', dot: 'on' })
  })

  it('keeps one row for a Spark that is both a peer and a member', () => {
    const rows = fleetRows(
      summary({ nodes: [node(), member()] }),
      [peer('loft', 'http://loft.local:7070/')],
      'http://attic.local:7070',
    )
    expect(rows.map(row => row.displayName)).toEqual(['attic', 'loft'])
    expect(rows[1].legacyPeerOnly).toBeUndefined()
    expect(rows[1].inventory).toBeDefined()
  })

  it('keeps a Spark added by address that never joined the fleet', () => {
    const rows = fleetRows(summary(), [peer('shed', 'http://shed.local:7070')], 'http://attic.local:7070')
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
    const rows = fleetRows(summary({ nodes: [node(), member({ console_url: '' })] }), [], '')
    expect(rows.map(row => row.nodeID)).toEqual(['node-lead'])
  })

  it('says nothing without a fleet summary', () => {
    expect(fleetRows(null, [], 'http://attic.local:7070')).toEqual([])
  })
})

describe('which placement owns a model on a Spark', () => {
  it('keys every placement node by its Spark and model', () => {
    const index = deploymentIndex([deployment()])
    expect(index.get(deploymentKey('node-loft', 'qwen36-35b-a3b-nvfp4-1s'))).toBe('deployment_one')
    expect(index.size).toBe(1)
  })

  it('keeps the newest placement of the same model on the same Spark', () => {
    const index = deploymentIndex([
      deployment({ deployment_id: 'deployment_new', created_at: '2026-08-23T11:00:00Z' }),
      deployment({ deployment_id: 'deployment_old', created_at: '2026-08-23T08:00:00Z' }),
    ])
    expect(index.get(deploymentKey('node-loft', 'qwen36-35b-a3b-nvfp4-1s'))).toBe('deployment_new')
  })

  it('answers with an empty index when the fleet holds no placement', () => {
    expect(deploymentIndex([]).size).toBe(0)
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
