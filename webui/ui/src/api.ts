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

// MemoryModel is present only for recipes maintainers have qualified for the
// memory-fit calculator (see webui/ui/src/memory.ts). Absent means unknown,
// not zero footprint.
export interface MemoryModel {
  weights_bytes: number
  kv_bytes_per_token: number
  runtime_overhead_bytes: number
}

export interface Recipe {
  id: string
  version: number
  display_name: string
  publisher: string
  model_by?: string
  recipe_by?: string
  model_released?: string
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
  // Exactly one of vllm/sglang is present, named by runtime.kind. Only the
  // settings a person would feel are declared here; the wire object carries
  // the recipe's whole serving block.
  service: {
    internal_port: number
    default_host_port: number
    served_model_id: string
    vllm?: { max_model_len?: number; tensor_parallel_size?: number }
    sglang?: { context_length?: number; tensor_parallel_size?: number; quantization?: string }
  }
  // image and digest together are the pinned runtime reference, the same
  // string /api/v1/storage reports for an image already pulled here.
  runtime: { kind?: string; image?: string; digest?: string; start_timeout_minutes: number }
  memory_model?: MemoryModel
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

// What one model has served on this Spark since basement started counting.
// Only models basement has taken a reading for appear at all: a model it has
// never served here has no entry, which is not the same as a zero.
export interface ModelTokenUsage {
  recipe_id: string
  prompt_tokens: number
  generation_tokens: number
  first_counted_at: string
  updated_at: string
}

export interface TokenUsage {
  models: ModelTokenUsage[]
  totals: { prompt_tokens: number; generation_tokens: number }
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
  gpu_power_draw_watts: number
  gpu_clock_mhz: number
  gpu_temperature_c: number
  storage_total: number
  storage_available: number
  active_model?: {
    recipe_id: string
    served_model_id: string
    runtime_kind?: string
    // Every field is optional: each runtime publishes its own subset, and a
    // series this runtime does not expose stays absent rather than zero.
    runtime_metrics?: {
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
  images: { reference: string; bytes: number; recipe_ids: string[] }[]
}

export interface APIKey {
  id: string
  name: string
  created_at: string
  last_used_at?: string
}

export interface Peer {
  id: string
  name: string
  base_url: string
}

// ---- Finding and adopting a second Spark ------------------------------------
// One machine the network sweep answered for. gb10_hint and basement are the
// sweep's own findings, never a guess made here: they rank the list the
// console shows and never remove anything from it.
export interface FleetCandidate {
  name: string
  address: string
  gb10_hint: boolean
  basement: { base_url: string } | null
}

// One line of the adoption run, as the manager stored it. The six keys are
// connect, verify, install, start, pair and peer, always all six and always
// in that order; key is what logic reads and label is what a person reads,
// so the wording stays the backend's to change.
export interface AdoptStep {
  key: string
  label: string
  state: string
  detail?: string
}

export interface AdoptResult {
  // The adopted Spark, as either the stored peer or just its name.
  peer?: Peer | string
  console_url?: string
  alt_url?: string
  // Where the new Spark's console asks for the pairing token. It has no
  // token-in-URL form, so the owner types the token into the page.
  owner_pairing_url?: string
  // A durable shared secret, not a one-shot code. It is shown so it can be
  // copied and typed, and it never travels in a link from here.
  owner_pairing_token?: string
}

export interface AdoptStatus {
  state: 'idle' | 'running' | 'succeeded' | 'failed' | string
  address?: string
  started_at?: string
  finished_at?: string
  steps?: AdoptStep[]
  // Extra lines the run wrote as it went. The most recent one is the only
  // one the console shows.
  progress?: string[]
  error?: string
  result?: AdoptResult
}

// Sparks already running basement first, then likely GB10s, then everything
// else. Sort is stable, so the sweep's own order survives inside each group.
export const candidateRank = (candidate: FleetCandidate): number =>
  candidate.basement ? 0 : candidate.gb10_hint ? 1 : 2

export const rankCandidates = (candidates: FleetCandidate[]): FleetCandidate[] =>
  [...candidates].sort((left, right) => candidateRank(left) - candidateRank(right))

// The name to greet the new Spark by, whichever shape the result carries.
export const adoptedName = (result?: AdoptResult): string => {
  if (typeof result?.peer === 'string') return result.peer
  return result?.peer?.name ?? ''
}

// Adoption takes a bare host: no scheme, no port, no path. The sweep already
// answers with one, so this only guards what a person or a prefill could
// hand over. An IPv6 literal in brackets keeps its colons; a bare one is
// left alone rather than cut at its first colon.
export function bareHost(value: string): string {
  let host = value.trim()
  const scheme = host.indexOf('://')
  if (scheme !== -1) host = host.slice(scheme + 3)
  const credentials = host.lastIndexOf('@')
  if (credentials !== -1) host = host.slice(credentials + 1)
  host = host.split('/')[0].split('?')[0].split('#')[0]
  if (host.startsWith('[')) {
    const end = host.indexOf(']')
    return end === -1 ? host : host.slice(0, end + 1)
  }
  const port = host.indexOf(':')
  if (port === -1) return host
  // Two or more colons and no brackets: an unbracketed IPv6 literal, which
  // has no port to strip.
  return host.indexOf(':', port + 1) === -1 ? host.slice(0, port) : host
}

// A merged read of a peer's own system/models/telemetry endpoints. Any of
// the three is absent when the peer could not be reached in time.
export interface PeerSummary {
  reachable: boolean
  system?: SystemInfo
  models?: InstalledModel[]
  telemetry?: Telemetry
}

// The peer's installed models. The summary carries the peer's own
// /api/v1/models body; the same list also rides inside its system payload,
// so either read is the peer's own answer, never an inference.
export const peerModelList = (summary?: PeerSummary | null): InstalledModel[] =>
  summary?.models ?? summary?.system?.installed_models ?? []

// The word a model's row shows for its own state, on this Spark or another.
// Only statuses the manager actually stores are named; anything else is
// shown as it arrived rather than guessed at.
export function modelStateWord(model: InstalledModel): string {
  if (model.active && model.status === 'ready') return 'Serving'
  switch (model.status) {
    case 'ready':
    case 'stopped':
      return 'Installed'
    case 'starting':
      return 'Starting'
    case 'switching':
      return 'Switching'
    case 'recovering':
      return 'Recovering'
    case 'failed':
      return 'Failed'
    default:
      return model.status
  }
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

// Thrown when fetch itself rejects rather than answering with a status: the
// browser never got a response, so the manager either never saw this request
// or never got the chance to answer it. Every cause the browser can hit here
// (the machine off, Wi-Fi dropped, the manager mid-restart, this tab offline)
// collapses into the same opaque rejection, so the message does not guess
// which one it was. It also does not promise the request went unhandled: a
// manager can act on a request and lose the connection before answering, and
// from here those two look identical.
export class OfflineError extends Error {
  constructor() {
    super('Cannot reach this Spark. It may be offline, or its manager may be restarting. Try again once it answers.')
    this.name = 'OfflineError'
  }
}

// A caller passing its own AbortSignal wants a cancelled request reported as
// exactly that, not folded into "the machine is unreachable" — the two have
// nothing to do with each other and calling code may want to ignore an abort
// entirely.
const isAbort = (error: unknown): boolean =>
  typeof error === 'object' && error !== null && (error as { name?: unknown }).name === 'AbortError'

export async function api<T>(path: string, options: RequestInit = {}): Promise<T> {
  const headers = new Headers(options.headers)
  if (options.method && options.method !== 'GET') {
    headers.set('X-CSRF-Token', csrf)
    headers.set('Content-Type', 'application/json')
  }
  let response: Response
  try {
    response = await fetch(path, { ...options, headers })
  } catch (cause) {
    if (isAbort(cause)) throw cause
    throw new OfflineError()
  }
  const body = await response.json().catch(() => ({ error: response.statusText }))
  if (!response.ok) throw new ApiError(response.status, (body as { error?: string }).error ?? response.statusText)
  return body as T
}

// crypto.randomUUID exists only in secure contexts (https / localhost); the
// console is typically served over plain http on the LAN, so build the UUID
// from getRandomValues, which works everywhere.
const randomUUID = (): string => {
  if (typeof crypto.randomUUID === 'function') return crypto.randomUUID()
  const bytes = crypto.getRandomValues(new Uint8Array(16))
  bytes[6] = (bytes[6] & 0x0f) | 0x40
  bytes[8] = (bytes[8] & 0x3f) | 0x80
  const hex = [...bytes].map(byte => byte.toString(16).padStart(2, '0')).join('')
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`
}

export const idempotency = () => ({ 'Idempotency-Key': randomUUID() })

export function formatBytes(value?: number): string {
  if (!value) return 'n/a'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let unit = 0
  let amount = value
  while (amount >= 1000 && unit < units.length - 1) {
    amount /= 1000
    unit += 1
  }
  return `${amount.toFixed(amount >= 100 || unit <= 1 ? 0 : 1)} ${units[unit]}`
}

// Token counts reach the billions, so they are shown short. The exact
// number always travels with them in a title attribute, so the rounding
// never hides the real figure.
export function formatTokens(value: number): string {
  if (value < 1000) return String(Math.round(value))
  const units = ['K', 'M', 'B', 'T']
  let unit = -1
  let amount = value
  while (amount >= 1000 && unit < units.length - 1) {
    amount /= 1000
    unit += 1
  }
  return `${amount.toFixed(amount >= 100 ? 0 : 1)}${units[unit]}`
}

export async function copyText(value: string): Promise<void> {
  if (navigator.clipboard && window.isSecureContext) return navigator.clipboard.writeText(value)
  // A console reached over plain HTTP on a LAN address is not a secure
  // context, so this fallback is the everyday path here, not an edge case.
  // The holder must go inside the open modal dialog when there is one: a
  // modal makes the rest of the document inert, and an inert textarea
  // cannot be selected, so the copy would silently take nothing.
  const host = document.querySelector('dialog[open]') ?? document.body
  const holder = document.createElement('textarea')
  holder.value = value
  holder.setAttribute('readonly', '')
  holder.style.position = 'fixed'
  holder.style.top = '0'
  holder.style.left = '0'
  holder.style.width = '1px'
  holder.style.height = '1px'
  holder.style.padding = '0'
  holder.style.border = 'none'
  holder.style.outline = 'none'
  holder.style.boxShadow = 'none'
  holder.style.background = 'transparent'
  host.appendChild(holder)
  const selection = document.getSelection()
  const previous = selection && selection.rangeCount > 0 ? selection.getRangeAt(0) : null
  holder.focus({ preventScroll: true })
  holder.setSelectionRange(0, holder.value.length)
  try {
    if (!document.execCommand('copy')) throw new Error('Copy is unavailable in this browser context.')
  } finally {
    holder.remove()
    if (previous && selection) {
      selection.removeAllRanges()
      selection.addRange(previous)
    }
  }
}

export const terminal = (state: string) => ['ready', 'failed', 'cancelled', 'stopped', 'removed'].includes(state)

// startTimeoutMinutes mirrors the backend fallback in
// internal/operations/host.go startTimeout: 0 or unset means the default of
// 20 minutes. The console derives every first-start time claim from this so
// it never promises a number the health wait does not honour.
export function startTimeoutMinutes(recipe?: Recipe): number {
  const minutes = recipe?.runtime.start_timeout_minutes
  return minutes && minutes > 0 ? minutes : 20
}

// UpdatePlan answers "what am I getting?" for a model that is already
// installed at an older recipe version, using only facts this console can
// actually check: the two version numbers, what /api/v1/storage reports is
// on this disk right now, and what the new recipe pins.
//
// What it deliberately does NOT contain is a diff of serving settings. The
// manager keeps a full recipe per (id, version) in memory
// (recipefeed.Fetcher.all, read through recipe.FindVersion), but that
// history only ever holds versions this build has seen: the embedded set of
// the running binary, plus anything fetched from the signed index. An update
// that arrives by upgrading basement itself replaces the embedded recipe, so
// the version the user installed is genuinely gone and no honest
// setting-by-setting comparison is possible. The dialog says what is known
// and does not imply a changelog exists.
export interface UpdatePlan {
  from: number
  to: number
  // Null while /api/v1/storage has not answered yet: unknown, not "absent".
  weightsPresent: boolean | null
  bytesToFetch: number
  imagePresent: boolean | null
  contextLength?: number
  sparkCount: number
  runtimeKind?: string
  quantization?: string
}

// An artifact directory is treated as complete at 99% of the recipe's
// expected bytes, the same threshold the Models view uses to tell a kept
// download from a partial one.
const COMPLETE_FRACTION = 0.99

export function updatePlan(installedVersion: number, target: Recipe, storage: StorageInfo | null): UpdatePlan {
  const plan: UpdatePlan = {
    from: installedVersion,
    to: target.version,
    weightsPresent: null,
    bytesToFetch: 0,
    imagePresent: null,
    contextLength: target.service.vllm?.max_model_len ?? target.service.sglang?.context_length,
    sparkCount: target.topology.spark_count,
    runtimeKind: target.runtime.kind,
    quantization: target.service.sglang?.quantization,
  }
  if (!storage) return plan
  // Matched on repository AND revision, so weights left over from another
  // version of the same recipe never count as this version's.
  let complete = true
  for (const artifact of target.artifacts) {
    const onDisk = storage.artifacts.find(
      entry => entry.repository === artifact.repository && entry.revision === artifact.revision,
    )?.bytes ?? 0
    if (onDisk < artifact.expected_bytes * COMPLETE_FRACTION) complete = false
    plan.bytesToFetch += Math.max(artifact.expected_bytes - onDisk, 0)
  }
  plan.weightsPresent = complete
  if (target.runtime.image && target.runtime.digest) {
    const reference = `${target.runtime.image}@${target.runtime.digest}`
    plan.imagePresent = storage.images.some(image => image.reference === reference)
  }
  return plan
}

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
  verify_dgx_spark: 'Detect GB10 hardware',
  verify_memory_capacity: 'Check memory capacity',
  verify_disk: 'Reserve disk space',
  verify_port: 'Check endpoint port',
  verify_docker: 'Check Docker',
  verify_nvidia_runtime: 'Check NVIDIA runtime',
  verify_artifact_access: 'Check model access',
  // The cable check runs on the head, which dials the second Spark over the
  // link; the peer preflight runs the other node's own guardrails. stepCopy
  // adds which Spark each was recorded against.
  verify_fabric: 'Check the cable between Sparks',
  verify_peer_node: 'Check hardware and memory',
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
  teardown_stop_container: 'Stop model service',
}

// A step is stored as "[rollback_]operation[:role]". Anything deciding what
// a step IS — which phase it belongs to, which progress shape its receipt
// carries — wants the bare operation, or a two-Spark job matches nothing.
export const stepOperation = (operation: string): string =>
  operation.replace(/^rollback_/, '').split(':')[0]

// A two-Spark job runs the same operation once per node and records each as
// "operation:role". Nothing is invented here: the role shown is the one the
// step was stored under.
export function stepCopy(operation: string): string {
  const [name, role] = operation.split(':')
  const label = operationCopy[name] ?? name
  if (!role) return label
  return role === 'worker' ? `${label} (second Spark)` : `${label} (this Spark)`
}
