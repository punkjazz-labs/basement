import { describe, expect, it } from 'vitest'
import { updatePlan, type Recipe, type StorageInfo } from './api'

const GB = 1_000_000_000

const target = (extra: Partial<Recipe> = {}): Recipe => ({
  id: 'laguna-s-2-1-nvfp4-dflash-1s',
  version: 4,
  display_name: 'Laguna S 2.1',
  publisher: 'poolside',
  trust: 'verified',
  verification: 'maintainer',
  source: { url: 'https://example.invalid', revision: 'src' },
  topology: { spark_count: 1 },
  artifacts: [
    { role: 'primary', repository: 'poolside/Laguna-S-2.1-NVFP4', revision: 'rev-new', expected_bytes: 60 * GB, licence: 'MIT', licence_url: 'https://example.invalid/l' },
  ],
  requirements: {
    per_node_minimum_memory_bytes: 1, per_node_memory_reserve_bytes: 1, safety_margin_bytes: 1,
    secrets: [], required_licence_acceptance: true,
  },
  service: { internal_port: 8000, default_host_port: 8000, served_model_id: 'laguna', vllm: { max_model_len: 262144 } },
  runtime: { kind: 'vllm', image: 'ghcr.io/example/vllm', digest: 'sha256:new', start_timeout_minutes: 20 },
  artifact_bytes: 60 * GB,
  required_bytes: 80 * GB,
  revoked: false,
  ...extra,
})

const storage = (extra: Partial<StorageInfo> = {}): StorageInfo => ({
  data_dir: '/var/lib/basement',
  storage_total: 4_000 * GB,
  storage_available: 2_000 * GB,
  total_managed_bytes: 0,
  database_bytes: 0,
  artifacts: [],
  caches: [],
  images: [],
  ...extra,
})

describe('updatePlan', () => {
  it('carries both version numbers', () => {
    const plan = updatePlan(3, target(), storage())
    expect(plan.from).toBe(3)
    expect(plan.to).toBe(4)
  })

  it('reuses weights when the pinned revision is already on disk', () => {
    const plan = updatePlan(3, target(), storage({
      artifacts: [{ repository: 'poolside/Laguna-S-2.1-NVFP4', revision: 'rev-new', bytes: 60 * GB, recipe_ids: ['laguna-s-2-1-nvfp4-dflash-1s'] }],
    }))
    expect(plan.weightsPresent).toBe(true)
    expect(plan.bytesToFetch).toBe(0)
  })

  it('does not count another revision of the same repository as these weights', () => {
    const plan = updatePlan(3, target(), storage({
      artifacts: [{ repository: 'poolside/Laguna-S-2.1-NVFP4', revision: 'rev-old', bytes: 60 * GB, recipe_ids: ['laguna-s-2-1-nvfp4-dflash-1s'] }],
    }))
    expect(plan.weightsPresent).toBe(false)
    expect(plan.bytesToFetch).toBe(60 * GB)
  })

  it('reports what is left to fetch for a partial download', () => {
    const plan = updatePlan(3, target(), storage({
      artifacts: [{ repository: 'poolside/Laguna-S-2.1-NVFP4', revision: 'rev-new', bytes: 20 * GB, recipe_ids: ['laguna-s-2-1-nvfp4-dflash-1s'] }],
    }))
    expect(plan.weightsPresent).toBe(false)
    expect(plan.bytesToFetch).toBe(40 * GB)
  })

  it('reports the pinned runtime image as present only on an exact reference match', () => {
    const present = updatePlan(3, target(), storage({
      images: [{ reference: 'ghcr.io/example/vllm@sha256:new', bytes: 9 * GB, recipe_ids: ['laguna-s-2-1-nvfp4-dflash-1s'] }],
    }))
    expect(present.imagePresent).toBe(true)
    const other = updatePlan(3, target(), storage({
      images: [{ reference: 'ghcr.io/example/vllm@sha256:old', bytes: 9 * GB, recipe_ids: ['laguna-s-2-1-nvfp4-dflash-1s'] }],
    }))
    expect(other.imagePresent).toBe(false)
  })

  it('says unknown rather than absent while storage has not answered', () => {
    const plan = updatePlan(3, target(), null)
    expect(plan.weightsPresent).toBeNull()
    expect(plan.imagePresent).toBeNull()
    expect(plan.bytesToFetch).toBe(0)
  })

  it('reads the context length from whichever runtime block the recipe carries', () => {
    expect(updatePlan(3, target(), null).contextLength).toBe(262144)
    const sglang = target({
      service: { internal_port: 8000, default_host_port: 8000, served_model_id: 'laguna', sglang: { context_length: 131072, quantization: 'fp8' } },
      runtime: { kind: 'sglang', image: 'ghcr.io/example/sglang', digest: 'sha256:new', start_timeout_minutes: 20 },
    })
    const plan = updatePlan(3, sglang, null)
    expect(plan.contextLength).toBe(131072)
    expect(plan.quantization).toBe('fp8')
    expect(plan.runtimeKind).toBe('sglang')
  })

  it('never claims a runtime image state when the recipe pins no digest', () => {
    const plan = updatePlan(3, target({ runtime: { kind: 'vllm', start_timeout_minutes: 20 } }), storage())
    expect(plan.imagePresent).toBeNull()
  })
})
