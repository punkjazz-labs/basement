package recipe

type Recipe struct {
	SchemaVersion int    `yaml:"schema_version" json:"schema_version"`
	ID            string `yaml:"id" json:"id"`
	Version       int    `yaml:"version" json:"version"`
	DisplayName   string `yaml:"display_name" json:"display_name"`
	Publisher     string `yaml:"publisher" json:"publisher"`
	// Attribution is three separate facts: the lab that made the model, and
	// the author of this serving recipe. The weights builder is derived from
	// the artifact repository owner. Publisher remains the display byline.
	ModelBy  string `yaml:"model_by" json:"model_by"`
	RecipeBy string `yaml:"recipe_by" json:"recipe_by"`
	// ModelReleased is a maintainer-researched display string (e.g. "May
	// 2026"), not a computed value. Absent means unknown; the console must
	// show n/a rather than guess.
	ModelReleased string       `yaml:"model_released,omitempty" json:"model_released,omitempty"`
	Trust         string       `yaml:"trust" json:"trust"`
	Verification  string       `yaml:"verification" json:"verification"`
	Source        Source       `yaml:"source" json:"source"`
	Topology      Topology     `yaml:"topology" json:"topology"`
	Runtime       Runtime      `yaml:"runtime" json:"runtime"`
	Artifacts     []Artifact   `yaml:"artifacts" json:"artifacts"`
	Requirements  Requirements `yaml:"requirements" json:"requirements"`
	Service       Service      `yaml:"service" json:"service"`
	Operations    []Operation  `yaml:"operations" json:"operations"`
	Uninstall     []Operation  `yaml:"uninstall" json:"uninstall"`
	// MemoryModel is measured or derived by maintainers during recipe
	// qualification; the console shows memory estimates only when this block
	// is present. Absent means the recipe has no memory estimate yet, not
	// zero footprint.
	MemoryModel *MemoryModel `yaml:"memory_model,omitempty" json:"memory_model,omitempty"`
}

// MemoryModel documents a recipe's memory footprint for the fleet fit
// calculator. weights_bytes equals the primary artifact's expected_bytes
// unless overridden. kv_bytes_per_token is derived from the model's public
// config: layers x kv_heads x head_dim x 2 (K and V) x dtype bytes, honoring
// the recipe's kv_cache_dtype. runtime_overhead_bytes (engine, CUDA graphs,
// activations) is measured on hardware at qualification time.
type MemoryModel struct {
	WeightsBytes         int64 `yaml:"weights_bytes" json:"weights_bytes"`
	KVBytesPerToken      int64 `yaml:"kv_bytes_per_token" json:"kv_bytes_per_token"`
	RuntimeOverheadBytes int64 `yaml:"runtime_overhead_bytes" json:"runtime_overhead_bytes"`
}

type Source struct {
	URL      string `yaml:"url" json:"url"`
	Revision string `yaml:"revision" json:"revision"`
}

type Topology struct {
	SparkCount int `yaml:"spark_count" json:"spark_count"`
	// Interconnect is required when SparkCount is above 1 and forbidden
	// otherwise. Device names and GID indices are facts of a particular
	// cabling, so they are declared by the recipe and never inferred.
	Interconnect *Interconnect `yaml:"interconnect,omitempty" json:"interconnect,omitempty"`
}

// Interconnect describes the fabric two Sparks are cabled with and the
// environment each rank needs to use it. SharedEnvironment applies to every
// node; HeadEnvironment and WorkerEnvironment are the per-rank additions.
type Interconnect struct {
	Kind              string            `yaml:"kind" json:"kind"`
	MasterPort        int               `yaml:"master_port" json:"master_port"`
	SharedEnvironment map[string]string `yaml:"shared_environment" json:"shared_environment"`
	HeadEnvironment   map[string]string `yaml:"head_environment" json:"head_environment"`
	WorkerEnvironment map[string]string `yaml:"worker_environment" json:"worker_environment"`
}

// Distributed reports whether this recipe serves one model across more than
// one Spark. Everything shipped today is single-node, so this is false.
func (r Recipe) Distributed() bool { return r.Topology.SparkCount > 1 }

// SocketInterface is the network interface NCCL and Gloo are pinned to. It
// is also the interface whose local address becomes --master-addr on the
// head, so a recipe without it cannot describe a two-node launch.
func (t Topology) SocketInterface() string {
	if t.Interconnect == nil {
		return ""
	}
	return t.Interconnect.SharedEnvironment["NCCL_SOCKET_IFNAME"]
}

type Runtime struct {
	Kind           string            `yaml:"kind" json:"kind"`
	Image          string            `yaml:"image" json:"image"`
	Digest         string            `yaml:"digest" json:"digest"`
	ImageBytes     int64             `yaml:"image_bytes" json:"image_bytes"`
	ImageDiskBytes int64             `yaml:"image_disk_bytes" json:"image_disk_bytes"`
	Environment    map[string]string `yaml:"environment" json:"environment"`
	ShmBytes       int64             `yaml:"shm_bytes" json:"shm_bytes"`
	MemoryLock     bool              `yaml:"memory_lock" json:"memory_lock"`
	IPCLock        bool              `yaml:"ipc_lock" json:"ipc_lock"`
	// StartTimeoutMinutes bounds how long the health wait gives a first start
	// before failing; the console's own copy derives from this value so it
	// never promises a number the wait does not honour. 0 means the default
	// of 20 minutes.
	StartTimeoutMinutes int `yaml:"start_timeout_minutes" json:"start_timeout_minutes"`
	// WritablePaths are absolute container paths a runtime must be able to
	// write before it can serve at all. The container's root filesystem is
	// read only and its scratch tmpfs is noexec, which is right for everything
	// that ships today and wrong for a runtime that compiles kernels at import
	// time: tilelang writes its JIT cache to /root/.tilelang while the module
	// is being imported, so on a read-only root vLLM raises OSError and the
	// worker dies — after the weights have loaded. Pointing such a cache at
	// /tmp does not help either, because the compiler writes a shared object
	// and then loads it, which noexec forbids.
	//
	// Each entry becomes its own small tmpfs, writable and nosuid but not
	// noexec. The size is the manager's choice rather than the recipe's: on a
	// GB10 a tmpfs spends the same unified memory the model is loading into,
	// so a recipe names the paths and the manager bounds them. Absent means
	// the container gets exactly the filesystem it always had.
	WritablePaths []string `yaml:"writable_paths,omitempty" json:"writable_paths,omitempty"`
}

// The container filesystem layout every recipe is served under. It is declared
// beside the schema because two parts of the program need the same answer: the
// container builder mounts these paths, and the validator has to know which
// paths a recipe may not claim as writable. A second copy of this layout would
// drift from the first without anything failing.
const (
	// CacheMountPath is the one writable bind mount: the compilation cache,
	// which is kept on disk so it survives restarts.
	CacheMountPath = "/root/.cache"
	// TempMountPath is the container's scratch tmpfs. Nothing loads code from
	// it, so it is mounted noexec.
	TempMountPath = "/tmp"
	// PrimaryMountPath is where the model itself is mounted, read only.
	PrimaryMountPath = "/model"
)

// ArtifactMountPath is where the artifact with this role is mounted inside the
// container. The primary artifact is the model and keeps its own path; every
// other role is mounted under its own name.
func ArtifactMountPath(role string) string {
	if role == "primary" {
		return PrimaryMountPath
	}
	return "/" + role
}

func (r Runtime) Reference() string { return r.Image + "@" + r.Digest }

type Artifact struct {
	Role          string `yaml:"role" json:"role"`
	Repository    string `yaml:"repository" json:"repository"`
	Revision      string `yaml:"revision" json:"revision"`
	ExpectedBytes int64  `yaml:"expected_bytes" json:"expected_bytes"`
	Licence       string `yaml:"licence" json:"licence"`
	LicenceURL    string `yaml:"licence_url" json:"licence_url"`
	// Files narrows the artifact to named files inside the pinned revision.
	// Absent means the whole repository snapshot, exactly as before; present
	// means these files and only these, each verified against its own pinned
	// size and the repository's own hash. This strengthens pinning rather
	// than relaxing it: a snapshot total is one number for a whole tree,
	// while a file list is one number per file. It exists because a GGUF
	// repository publishes every quantization of a model side by side, so the
	// one a recipe serves is a small part of a tree nothing should download.
	Files []ArtifactFile `yaml:"files,omitempty" json:"files,omitempty"`
}

// ArtifactFile pins one file inside an artifact's repository revision. Name
// is the repository-relative path exactly as the revision lists it, and
// ExpectedBytes is that file's own size, not a share of a total.
type ArtifactFile struct {
	Name          string `yaml:"name" json:"name"`
	ExpectedBytes int64  `yaml:"expected_bytes" json:"expected_bytes"`
}

type Requirements struct {
	Architecture          string   `yaml:"architecture" json:"architecture"`
	DGXSpark              bool     `yaml:"dgx_spark" json:"dgx_spark"`
	Docker                bool     `yaml:"docker" json:"docker"`
	NvidiaRuntime         bool     `yaml:"nvidia_container_runtime" json:"nvidia_container_runtime"`
	MinimumMemoryBytes    int64    `yaml:"per_node_minimum_memory_bytes" json:"per_node_minimum_memory_bytes"`
	MemoryReserveBytes    int64    `yaml:"per_node_memory_reserve_bytes" json:"per_node_memory_reserve_bytes"`
	SafetyMarginBytes     int64    `yaml:"safety_margin_bytes" json:"safety_margin_bytes"`
	Secrets               []string `yaml:"secrets" json:"secrets"`
	RequiredLicenceAccept bool     `yaml:"required_licence_acceptance" json:"required_licence_acceptance"`
}

// Service carries exactly one per-runtime block, and it must be the block
// named by runtime.kind. A recipe that sets both, or neither, is rejected:
// the command, memory model, health path and metric prefixes all follow from
// the kind, so an ambiguous recipe has no single meaning.
type Service struct {
	InternalPort    int             `yaml:"internal_port" json:"internal_port"`
	DefaultHostPort int             `yaml:"default_host_port" json:"default_host_port"`
	ServedModelID   string          `yaml:"served_model_id" json:"served_model_id"`
	VLLM            *VLLMConfig     `yaml:"vllm,omitempty" json:"vllm,omitempty"`
	SGLang          *SGLangConfig   `yaml:"sglang,omitempty" json:"sglang,omitempty"`
	LlamaCpp        *LlamaCppConfig `yaml:"llamacpp,omitempty" json:"llamacpp,omitempty"`
}

type VLLMConfig struct {
	TensorParallelSize   int                 `yaml:"tensor_parallel_size" json:"tensor_parallel_size"`
	KVCacheDType         string              `yaml:"kv_cache_dtype" json:"kv_cache_dtype"`
	AttentionBackend     string              `yaml:"attention_backend" json:"attention_backend"`
	MoEBackend           string              `yaml:"moe_backend" json:"moe_backend"`
	LinearBackend        string              `yaml:"linear_backend" json:"linear_backend"`
	GPUMemoryUtil        string              `yaml:"gpu_memory_utilization" json:"gpu_memory_utilization"`
	MaxModelLen          int                 `yaml:"max_model_len" json:"max_model_len"`
	MaxNumSeqs           int                 `yaml:"max_num_seqs" json:"max_num_seqs"`
	MaxBatchedTokens     int                 `yaml:"max_num_batched_tokens" json:"max_num_batched_tokens"`
	MultimodalImageLimit int                 `yaml:"multimodal_image_limit" json:"multimodal_image_limit"`
	SpeculativeMethod    string              `yaml:"speculative_method" json:"speculative_method"`
	SpeculativeTokens    int                 `yaml:"speculative_tokens" json:"speculative_tokens"`
	SpeculativeMoE       string              `yaml:"speculative_moe_backend" json:"speculative_moe_backend"`
	SpeculativeModelRole string              `yaml:"speculative_model_role" json:"speculative_model_role"`
	ReasoningParser      string              `yaml:"reasoning_parser" json:"reasoning_parser"`
	ToolCallParser       string              `yaml:"tool_call_parser" json:"tool_call_parser"`
	LoadFormat           string              `yaml:"load_format" json:"load_format"`
	ChatTemplateFile     string              `yaml:"chat_template_file" json:"chat_template_file"`
	ChatTemplate         ChatTemplateOptions `yaml:"chat_template" json:"chat_template"`
	Generation           GenerationConfig    `yaml:"generation" json:"generation"`
	TrustRemoteCode      bool                `yaml:"trust_remote_code" json:"trust_remote_code"`
	ChunkedPrefill       bool                `yaml:"chunked_prefill" json:"chunked_prefill"`
	AsyncScheduling      bool                `yaml:"async_scheduling" json:"async_scheduling"`
	PrefixCaching        bool                `yaml:"prefix_caching" json:"prefix_caching"`
	AutoToolChoice       bool                `yaml:"auto_tool_choice" json:"auto_tool_choice"`
	// DisableQuantFusions turns off exactly two of vLLM's default-on
	// compilation passes, fuse_norm_quant and fuse_act_quant, by emitting one
	// fixed --compilation-config document. It is a boolean rather than a
	// compilation-config field because a recipe must not be able to hand the
	// runtime arbitrary compiler configuration: the only expressible outcome
	// is the documented workaround for the passes that garble DeepSeek V4
	// Flash output on SM121 (vllm-project/vllm issue 50773). Absent, which is
	// every recipe that shipped before this field, means no
	// --compilation-config flag at all and vLLM's own defaults stand.
	DisableQuantFusions bool `yaml:"disable_quant_fusions,omitempty" json:"disable_quant_fusions,omitempty"`
}

// SGLangConfig mirrors the subset of sglang.launch_server arguments a recipe
// is allowed to pin. Zero and empty fields are omitted from the command line,
// leaving the runtime's own default in place.
type SGLangConfig struct {
	TensorParallelSize int `yaml:"tensor_parallel_size" json:"tensor_parallel_size"`
	// MemFractionStatic stays a string for the same reason vLLM's
	// gpu_memory_utilization does: the recipe's exact decimal reaches the
	// runtime unrounded.
	MemFractionStatic         string `yaml:"mem_fraction_static" json:"mem_fraction_static"`
	ContextLength             int    `yaml:"context_length" json:"context_length"`
	MaxRunningRequests        int    `yaml:"max_running_requests" json:"max_running_requests"`
	Quantization              string `yaml:"quantization" json:"quantization"`
	KVCacheDType              string `yaml:"kv_cache_dtype" json:"kv_cache_dtype"`
	SpeculativeAlgorithm      string `yaml:"speculative_algorithm" json:"speculative_algorithm"`
	SpeculativeNumDraftTokens int    `yaml:"speculative_num_draft_tokens" json:"speculative_num_draft_tokens"`
	// SpeculativeModelRole names a declared non-primary artifact role; the
	// draft model is served from that artifact's mount, never from a path the
	// recipe writes by hand.
	SpeculativeModelRole string `yaml:"speculative_model_role" json:"speculative_model_role"`
	ChatTemplateFile     string `yaml:"chat_template_file" json:"chat_template_file"`
	ToolCallParser       string `yaml:"tool_call_parser" json:"tool_call_parser"`
	ReasoningParser      string `yaml:"reasoning_parser" json:"reasoning_parser"`
	AttentionBackend     string `yaml:"attention_backend" json:"attention_backend"`
}

// LlamaCppConfig mirrors the subset of llama-server arguments a recipe is
// allowed to pin. Zero and empty fields are omitted from the command line,
// leaving the runtime's own default in place. Draft-model flags are
// deliberately absent: speculative decoding is not part of this kind's first
// phase, and a flag the manager cannot qualify is a flag it must not accept.
type LlamaCppConfig struct {
	// ModelFile names the GGUF inside the primary artifact mount. Unlike vLLM
	// and SGLang, which are handed a repository snapshot directory, llama.cpp
	// is handed one file; a split quantization names its first shard and
	// llama.cpp opens the rest itself. The name must be one of the files the
	// primary artifact pins, so the server is never pointed at a path the
	// download never fetched.
	ModelFile string `yaml:"model_file" json:"model_file"`
	// ContextSize is --ctx-size, the total context llama-server allocates KV
	// cache for. llama-server divides it among the parallel slots, so this is
	// the whole server's budget rather than a per-request limit.
	ContextSize int `yaml:"context_size" json:"context_size"`
	// GPULayers is --n-gpu-layers. A GB10 has one unified memory pool, so a
	// recipe that means "all of them" says so with a count above the model's
	// layer count; 0 leaves the flag off and llama.cpp decides.
	GPULayers int `yaml:"gpu_layers" json:"gpu_layers"`
	// Parallel is --parallel, the number of request slots served at once.
	Parallel int `yaml:"parallel" json:"parallel"`
	// FlashAttention is --flash-attn, pinned as the runtime's own tri-state
	// (on, off, auto) rather than as a boolean, because "the recipe did not
	// pin it" and "the recipe pinned off" are different decisions and only
	// the second belongs on the command line.
	FlashAttention string `yaml:"flash_attention" json:"flash_attention"`
	// Jinja turns on --jinja, which makes llama-server render the chat
	// template carried in the GGUF metadata instead of its built-in default.
	Jinja bool `yaml:"jinja" json:"jinja"`
	// ChatTemplateFile overrides that template with a file inside the primary
	// artifact mount, the same way the other kinds do.
	ChatTemplateFile string `yaml:"chat_template_file" json:"chat_template_file"`
}

// MemoryFraction reports the device memory fraction the active runtime block
// pins, as written in the recipe. The second result is false when the recipe
// carries no block for the given kind — including llama.cpp, which has no
// such knob at all; see PlannedMemoryBytes.
func (s Service) MemoryFraction(kind string) (string, bool) {
	switch {
	case kind == "vllm" && s.VLLM != nil:
		return s.VLLM.GPUMemoryUtil, true
	case kind == "sglang" && s.SGLang != nil:
		return s.SGLang.MemFractionStatic, true
	}
	return "", false
}

// PlannedMemoryBytes reports the device memory a llama.cpp recipe holds, as
// an absolute number: the mapped weights, the KV cache for the whole pinned
// context, and the runtime overhead measured at qualification.
//
// vLLM and SGLang are told what share of the device to claim and then fill
// it; llama.cpp is told nothing of the sort. It maps the GGUF and sizes the
// cache from --ctx-size, so its footprint follows from the recipe's own
// figures and is knowable only when the recipe states a memory_model. The
// second result is false for every other kind, and for a llama.cpp recipe
// with no memory model — which the validator refuses.
// The figures come from a recipe file, which is data this process did not
// write, so the arithmetic is checked rather than assumed. An overflowing
// product wraps to a small positive number, and a small positive number is
// exactly what an OOM guardrail waves through, so overflow reports "no
// footprint" instead of a footprint that flatters the recipe.
func (r Recipe) PlannedMemoryBytes() (int64, bool) {
	if r.Runtime.Kind != "llamacpp" || r.Service.LlamaCpp == nil || r.MemoryModel == nil {
		return 0, false
	}
	m := *r.MemoryModel
	context := int64(r.Service.LlamaCpp.ContextSize)
	if m.WeightsBytes < 0 || m.KVBytesPerToken < 0 || m.RuntimeOverheadBytes < 0 || context < 0 {
		return 0, false
	}
	kv, ok := multiplyWithoutOverflow(m.KVBytesPerToken, context)
	if !ok {
		return 0, false
	}
	total, ok := addWithoutOverflow(m.WeightsBytes, kv)
	if !ok {
		return 0, false
	}
	return addWithoutOverflow(total, m.RuntimeOverheadBytes)
}

// multiplyWithoutOverflow and addWithoutOverflow report false rather than
// wrapping. Both take non-negative operands only, which is what every caller
// here has already established.
func multiplyWithoutOverflow(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	product := a * b
	if product/b != a {
		return 0, false
	}
	return product, true
}

func addWithoutOverflow(a, b int64) (int64, bool) {
	sum := a + b
	if sum < a {
		return 0, false
	}
	return sum, true
}

type ChatTemplateOptions struct {
	EnableThinking   bool `yaml:"enable_thinking" json:"enable_thinking"`
	PreserveThinking bool `yaml:"preserve_thinking" json:"preserve_thinking"`
}

type GenerationConfig struct {
	Temperature       *float64 `yaml:"temperature" json:"temperature,omitempty"`
	TopP              *float64 `yaml:"top_p" json:"top_p,omitempty"`
	TopK              *int     `yaml:"top_k" json:"top_k,omitempty"`
	MinP              *float64 `yaml:"min_p" json:"min_p,omitempty"`
	PresencePenalty   *float64 `yaml:"presence_penalty" json:"presence_penalty,omitempty"`
	RepetitionPenalty *float64 `yaml:"repetition_penalty" json:"repetition_penalty,omitempty"`
}

func (r Recipe) ArtifactIndex(role string) (int, bool) {
	for index, artifact := range r.Artifacts {
		if artifact.Role == role {
			return index, true
		}
	}
	return 0, false
}

type Operation struct {
	Type string `yaml:"type" json:"type"`
}

func (r Recipe) TotalArtifactBytes() int64 {
	var total int64
	for _, artifact := range r.Artifacts {
		total += artifact.ExpectedBytes
	}
	return total
}

func (r Recipe) RequiredBytes() int64 {
	return r.TotalArtifactBytes() + r.Runtime.ImageDiskBytes + r.Requirements.SafetyMarginBytes
}
