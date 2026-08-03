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
}

func (r Runtime) Reference() string { return r.Image + "@" + r.Digest }

type Artifact struct {
	Role          string `yaml:"role" json:"role"`
	Repository    string `yaml:"repository" json:"repository"`
	Revision      string `yaml:"revision" json:"revision"`
	ExpectedBytes int64  `yaml:"expected_bytes" json:"expected_bytes"`
	Licence       string `yaml:"licence" json:"licence"`
	LicenceURL    string `yaml:"licence_url" json:"licence_url"`
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
	InternalPort    int           `yaml:"internal_port" json:"internal_port"`
	DefaultHostPort int           `yaml:"default_host_port" json:"default_host_port"`
	ServedModelID   string        `yaml:"served_model_id" json:"served_model_id"`
	VLLM            *VLLMConfig   `yaml:"vllm,omitempty" json:"vllm,omitempty"`
	SGLang          *SGLangConfig `yaml:"sglang,omitempty" json:"sglang,omitempty"`
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

// MemoryFraction reports the device memory fraction the active runtime block
// pins, as written in the recipe. The second result is false when the recipe
// carries no block for the given kind.
func (s Service) MemoryFraction(kind string) (string, bool) {
	switch {
	case kind == "vllm" && s.VLLM != nil:
		return s.VLLM.GPUMemoryUtil, true
	case kind == "sglang" && s.SGLang != nil:
		return s.SGLang.MemFractionStatic, true
	}
	return "", false
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
