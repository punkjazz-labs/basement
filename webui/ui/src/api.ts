// Typed client for the manager's local API. All mutations carry the CSRF
// token from pairing; the session cookie rides along automatically.

export interface Artifact {
  role: string
  repository: string
  revision: string
  expected_bytes: number
  licence: string
  licence_url: string
}

export interface Recipe {
  id: string
  version: number
  display_name: string
  publisher: string
  trust: string
  verification: string
  source: { url: string; revision: string }
  topology: { spark_count: number }
  artifacts: Artifact[]
  requirements: {
    per_node_minimum_memory_bytes: number
    per_node_memory_reserve_bytes: number
    safety_margin_bytes: number
    secrets: string[]
    required_licence_acceptance: boolean
  }
  service: { internal_port: number; default_host_port: number; served_model_id: string }
  artifact_bytes: number
  required_bytes: number
}

export interface InstalledModel {
  recipe_id: string
  recipe_version: number
  status: string
  active: boolean
  updated_at: string
  tokens_per_second?: number
  time_to_first_token_ms?: number
  measured_at?: string
}

export interface Step {
  index: number
  operation: string
  state: string
  receipt?: Record<string, unknown>
  error?: string
}

export interface Job {
  id: string
  kind: string
  recipe_id: string
  state: string
  error?: string
  created_at: string
  updated_at: string
  steps: Step[]
}

export interface ManagedNode {
  hostname: string
  product_name: string
  dgx_spark: boolean
  local: boolean
  ready: boolean
}

export interface SystemInfo {
  hostname: string
  product_name: string
  architecture: string
  os: string
  dgx_spark: boolean
  memory_total_bytes: number
  memory_available_bytes: number
  storage_total_bytes: number
  storage_available_bytes: number
  blocking_conditions?: string[]
  manager_version: string
  installed_models: InstalledModel[]
  hardware_scope: { mode: string; detected_spark_count: number; managed_nodes: ManagedNode[] }
}

export interface PreflightCheck {
  operation: string
  ok: boolean
  error?: string
  receipt?: Record<string, unknown> & {
    reclaim_candidates?: { recipe_id: string; display_name: string; bytes: number; active: boolean }[]
  }
}

export interface Preflight {
  recipe_id: string
  ready: boolean
  checks: PreflightCheck[]
  licence_accepted: boolean
  secrets: Record<string, boolean>
}

export interface Telemetry {
  sampled_at: string
  memory_total: number
  memory_available: number
  gpu_memory_total: number
  gpu_memory_free: number
  storage_total: number
  storage_available: number
  active_model?: {
    recipe_id: string
    served_model_id: string
    vllm?: {
      requests_running?: number
      requests_waiting?: number
      kv_cache_usage?: number
      prompt_tokens_total?: number
      generation_tokens_total?: number
    }
  }
}

export interface StorageInfo {
  data_dir: string
  storage_total: number
  storage_available: number
  total_managed_bytes: number
  database_bytes: number
  artifacts: { repository: string; revision: string; bytes: number; recipe_ids: string[] }[]
  caches: { recipe_id: string; bytes: number }[]
}

export interface APIKey {
  id: string
  name: string
  created_at: string
  last_used_at?: string
}

export interface UpdateInfo {
  current_version: string
  latest_version?: string
  update_available: boolean
  checked: boolean
  release_url?: string
  note?: string
}

let csrf = ''
export const setCSRF = (token: string) => {
  csrf = token
}

export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

export async function api<T>(path: string, options: RequestInit = {}): Promise<T> {
  const headers = new Headers(options.headers)
  if (options.method && options.method !== 'GET') {
    headers.set('X-CSRF-Token', csrf)
    headers.set('Content-Type', 'application/json')
  }
  const response = await fetch(path, { ...options, headers })
  const body = await response.json().catch(() => ({ error: response.statusText }))
  if (!response.ok) throw new ApiError(response.status, (body as { error?: string }).error ?? response.statusText)
  return body as T
}

export const idempotency = () => ({ 'Idempotency-Key': crypto.randomUUID() })

export function formatBytes(value?: number): string {
  if (!value) return '—'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let unit = 0
  let amount = value
  while (amount >= 1000 && unit < units.length - 1) {
    amount /= 1000
    unit += 1
  }
  return `${amount.toFixed(amount >= 100 || unit <= 1 ? 0 : 1)} ${units[unit]}`
}

export async function copyText(value: string): Promise<void> {
  if (navigator.clipboard && window.isSecureContext) return navigator.clipboard.writeText(value)
  const holder = document.createElement('textarea')
  holder.value = value
  holder.setAttribute('readonly', '')
  holder.style.position = 'fixed'
  holder.style.opacity = '0'
  document.body.appendChild(holder)
  holder.select()
  try {
    if (!document.execCommand('copy')) throw new Error('Copy is unavailable in this browser context.')
  } finally {
    holder.remove()
  }
}

export const terminal = (state: string) => ['ready', 'failed', 'cancelled', 'stopped', 'removed'].includes(state)

export const stateCopy: Record<string, string> = {
  queued: 'Queued',
  preflighting: 'Checking system',
  downloading_runtime: 'Preparing runtime',
  downloading_models: 'Downloading model',
  configuring: 'Configuring',
  checking_memory: 'Reserving memory',
  starting: 'Starting model',
  stopping: 'Stopping model',
  verifying_health: 'Checking health',
  verifying_inference: 'Testing inference',
  benchmarking: 'Measuring speed',
  removing: 'Removing model',
  ready: 'Ready',
  stopped: 'Stopped',
  removed: 'Removed',
  failed: 'Failed',
  cancelled: 'Cancelled',
  cancelling: 'Cancelling',
  rolling_back: 'Restoring previous model',
  interrupted: 'Interrupted',
}

export const operationCopy: Record<string, string> = {
  verify_architecture: 'Check system architecture',
  verify_dgx_spark: 'Detect DGX Spark',
  verify_memory_capacity: 'Check memory capacity',
  verify_disk: 'Reserve disk space',
  verify_port: 'Check endpoint port',
  verify_docker: 'Check Docker',
  verify_nvidia_runtime: 'Check NVIDIA runtime',
  verify_artifact_access: 'Check model access',
  pull_image: 'Prepare vLLM runtime',
  download_artifact: 'Download model files',
  write_generated_config: 'Write runtime configuration',
  create_container: 'Create model service',
  stop_container: 'Stop active model',
  verify_memory: 'Reserve runtime memory',
  start_container: 'Start model service',
  wait_http: 'Verify health endpoint',
  verify_openai_inference: 'Run inference test',
  measure_throughput: 'Measure real speed',
  remove_container: 'Remove model service',
  remove_artifact_if_unshared: 'Remove model files',
}
