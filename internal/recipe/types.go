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
	Role              string `yaml:"role" json:"role"`
	Repository        string `yaml:"repository" json:"repository"`
	Revision          string `yaml:"revision" json:"revision"`
	ExpectedBytes     int64  `yaml:"expected_bytes" json:"expected_bytes"`
	Licence           string `yaml:"licence" json:"licence"`
	LicenceURL        string `yaml:"licence_url" json:"licence_url"`
	LicenceRepository string `yaml:"licence_repository,omitempty" json:"licence_repository,omitempty"`
	LicenceRevision   string `yaml:"licence_revision,omitempty" json:"licence_revision,omitempty"`
	// LicenceTerritoryExclusions names the territories this artifact's
	// licence excludes, in the licence's own words, copied rather than
	// composed, the same rule already enforced for Licence and LicenceURL.
	// Absent (nil) means the licence carries no territorial restriction;
	// present means the console must show this list and the install API
	// must require a second, separate confirmation
	// (confirm_territory_eligibility) before install can proceed. It carries
	// no meaning without Licence and LicenceURL, which every artifact
	// already requires unconditionally.
	LicenceTerritoryExclusions []string `yaml:"licence_territory_exclusions,omitempty" json:"licence_territory_exclusions,omitempty"`
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
	ComfyUI         *ComfyUIConfig  `yaml:"comfyui,omitempty" json:"comfyui,omitempty"`
}

// ComfyUIConfig is what a media recipe pins about the model it serves. Every
// numeric limit here is a fact about the model rather than a preference: the
// console reads them to build its controls, and the generation API validates
// against them, so neither can ever offer or accept a size or a duration the
// model does not take.
//
// The graphs are the exception to the "recipes carry no runtime program"
// rule, and they are held to it in a different way: they are pinned files
// shipped inside basement, not text a recipe writes, and they never appear in
// an API response. A recipe names them; it cannot supply them.
type ComfyUIConfig struct {
	// Graphs maps a generation mode onto a workflow file under
	// internal/recipe/graphs. The mode names are a closed set (see
	// allowedGenerationModes) because each one is a code path in the
	// generation driver, not a label.
	Graphs map[string]string `yaml:"graphs" json:"graphs"`
	// OutputDirectory and InputDirectory are absolute container paths.
	// basement owns both: it bind-mounts host directories under its data
	// directory onto them, reads results out of the first, and stages source
	// images into the second.
	OutputDirectory string `yaml:"output_directory" json:"output_directory"`
	InputDirectory  string `yaml:"input_directory" json:"input_directory"`
	// The canvas. DefaultShortEdge is what the console offers first;
	// MaxShortEdge and MaxLongEdge bound what the model accepts. All three
	// are multiples of CanvasMultiple, because the model's own canvas is.
	DefaultShortEdge int `yaml:"default_short_edge" json:"default_short_edge"`
	MaxShortEdge     int `yaml:"max_short_edge" json:"max_short_edge"`
	MaxLongEdge      int `yaml:"max_long_edge" json:"max_long_edge"`
	// The duration grid: frames = FrameBlock*blocks + FrameOffset, played at
	// FramesPerSecond. A duration is expressed in blocks everywhere inside
	// basement and converted to seconds only for display, so a request can
	// never name a frame count the model would round away from.
	FrameBlock      int `yaml:"frame_block" json:"frame_block"`
	FrameOffset     int `yaml:"frame_offset" json:"frame_offset"`
	FramesPerSecond int `yaml:"frames_per_second" json:"frames_per_second"`
	MinBlocks       int `yaml:"min_blocks" json:"min_blocks"`
	MaxBlocks       int `yaml:"max_blocks" json:"max_blocks"`
	DefaultBlocks   int `yaml:"default_blocks" json:"default_blocks"`
	// SamplerSteps is how many denoising steps a real generation runs, and
	// VerificationSamplerSteps is how many the install and start proof runs.
	// They are separate numbers because they answer different questions. A
	// generation is asked for a good clip; the proof is asked only whether the
	// weights loaded and the graph produced a file, and on H3 a step costs
	// about forty-six seconds, so proving that at full quality spent a quarter
	// of an hour making a video nobody would ever see.
	//
	// Both substitute into the same pinned graph through GraphStepsToken, so
	// the proof walks the same loaders, samplers, VAEs and save node a
	// generation does. That is the whole point: a cheaper proof that skipped
	// part of the path would stop being evidence for the part it skipped.
	SamplerSteps             int `yaml:"sampler_steps" json:"sampler_steps"`
	VerificationSamplerSteps int `yaml:"verification_sampler_steps" json:"verification_sampler_steps"`
	// ConcurrentGenerations is how many generations may run at once. It is
	// pinned at 1 (see validateComfyUI): a generation on a GB10 holds the
	// whole device for minutes at a time, and nothing has qualified a second
	// one running beside it. Requests beyond that are queued in order, never
	// refused.
	ConcurrentGenerations int `yaml:"concurrent_generations" json:"concurrent_generations"`
	// SizeWaits carries measured wait factors per canvas short edge, so the
	// console can scale a shown wait estimate by how much longer a larger
	// canvas actually took on real hardware, rather than showing the same
	// estimate at every size. It is optional: a recipe with no measurements
	// yet simply omits it, and the console falls back to whatever it does
	// today for that case.
	SizeWaits []SizeWait `yaml:"size_waits,omitempty" json:"size_waits,omitempty"`
}

// SizeWait is one measured canvas size and the multiple of the fastest
// measured wait it cost. ShortEdge matches the console's own canvas control,
// which is expressed by short edge everywhere else in this struct. Factor is
// that size's wall time divided by the fastest measured size's wall time, so
// a factor of 1 marks the fastest size and every other factor says how many
// times as long the rest took.
type SizeWait struct {
	ShortEdge int     `yaml:"short_edge" json:"short_edge"`
	Factor    float64 `yaml:"factor" json:"factor"`
}

// CanvasMultiple is the grid every generated dimension sits on. It is the
// model's own constraint, not a rounding convenience, which is why a request
// off the grid is refused rather than snapped to it.
const CanvasMultiple = 32

// Frames is how many frames a duration of blocks generates. blocks is
// validated against MinBlocks and MaxBlocks before it reaches here.
func (c ComfyUIConfig) Frames(blocks int) int { return c.FrameBlock*blocks + c.FrameOffset }

// Seconds is the wall-clock length of a generation of blocks, for display
// only. It is derived from the frame grid, never stated separately, so the
// number the console shows and the frames the model makes cannot disagree.
func (c ComfyUIConfig) Seconds(blocks int) float64 {
	if c.FramesPerSecond <= 0 {
		return 0
	}
	return float64(c.Frames(blocks)) / float64(c.FramesPerSecond)
}

// MediaGeneration reports the recipe's media configuration, and false for
// every recipe that serves text. It is how the API and the runtime adapter
// ask "is this a media model" without reading the kind string in two places.
func (r Recipe) MediaGeneration() (ComfyUIConfig, bool) {
	if r.Runtime.Kind != "comfyui" || r.Service.ComfyUI == nil {
		return ComfyUIConfig{}, false
	}
	return *r.Service.ComfyUI, true
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
	// BlockSize is --block-size, the token count in one KV cache block. It is
	// pinned rather than left to vLLM because a KV cache dtype and its block
	// size are one decision: the NVFP4 MLA cache path this recipe pack now
	// serves DeepSeek V4 Flash with runs at 256 in the deployment that
	// configuration comes from, and a mismatched block size is a different
	// allocator, not a tuning preference. Absent, which is every recipe before this
	// field, means no --block-size flag and vLLM's own default stands.
	BlockSize int `yaml:"block_size,omitempty" json:"block_size,omitempty"`
	// MaxCUDAGraphCaptureSize is --max-cudagraph-capture-size, the largest
	// batch vLLM captures a CUDA graph for. Left alone, vLLM derives a ladder
	// of capture sizes and holds a graph for each, and on a GB10 those graphs
	// are spent from the same unified pool the weights and the KV cache live
	// in. A recipe that knows the largest batch it can actually run says so and
	// stops paying for the rest. Absent means vLLM's own sizing, unchanged.
	MaxCUDAGraphCaptureSize int `yaml:"max_cudagraph_capture_size,omitempty" json:"max_cudagraph_capture_size,omitempty"`
	// TokenizerMode is --tokenizer-mode. It names a tokenizer implementation
	// the runtime image carries, not a file, so it is checked against the
	// modes this pack has a recipe for rather than passed through. Absent means
	// no flag and the runtime's default (auto) applies.
	TokenizerMode string `yaml:"tokenizer_mode,omitempty" json:"tokenizer_mode,omitempty"`
	// SpeculativeDraftSampleMethod becomes the draft_sample_method member of
	// the --speculative-config document. It is how the draft heads pick their
	// proposed tokens, and like every other speculative setting it is only
	// meaningful beside a speculative method.
	SpeculativeDraftSampleMethod string `yaml:"speculative_draft_sample_method,omitempty" json:"speculative_draft_sample_method,omitempty"`
	// FlashInferAutotune turns on --enable-flashinfer-autotune, which lets
	// FlashInfer pick each kernel's tiling by measuring at startup instead of
	// using its compiled-in default. It costs start time and writes to the
	// FlashInfer workspace, so a recipe asks for it deliberately.
	FlashInferAutotune bool `yaml:"enable_flashinfer_autotune,omitempty" json:"enable_flashinfer_autotune,omitempty"`
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
	// TrustRemoteCode turns on --trust-remote-code, letting SGLang import
	// model code the checkpoint ships instead of only its own registered
	// architectures. A hybrid Gated-DeltaNet model needs it because its
	// forward pass is not built into every runtime release.
	TrustRemoteCode bool `yaml:"trust_remote_code" json:"trust_remote_code"`
	// ChunkedPrefillSize is --chunked-prefill-size, the token budget one
	// prefill chunk may spend before it yields. 0 leaves the flag off and
	// SGLang's own default stands.
	ChunkedPrefillSize int `yaml:"chunked_prefill_size" json:"chunked_prefill_size"`
	// DisablePrefillCUDAGraph turns off CUDA graph capture for the prefill
	// path with --disable-prefill-cuda-graph. A hybrid attention model mixes
	// linear-attention and full-attention layers whose prefill shapes vary
	// more than a uniform-attention model's, which is the case a qualified
	// hybrid-attention launcher has needed this off for.
	DisablePrefillCUDAGraph bool `yaml:"disable_prefill_cuda_graph" json:"disable_prefill_cuda_graph"`
	// MambaSSMDType is --mamba-ssm-dtype, the numeric type the state-space
	// (linear-attention) layers keep their recurrent state in. It is separate
	// from kv_cache_dtype because a hybrid model's Mamba state and its
	// full-attention KV cache are two different memory pools with two
	// different precision needs.
	MambaSSMDType string `yaml:"mamba_ssm_dtype" json:"mamba_ssm_dtype"`
	// MambaFullMemoryRatio is --mamba-full-memory-ratio, the share of the
	// memory sized for full attention that Mamba's own state is allowed to
	// claim on top of it. It stays a string for the same reason
	// mem_fraction_static does: the recipe's exact decimal reaches the
	// runtime unrounded.
	MambaFullMemoryRatio string `yaml:"mamba_full_memory_ratio" json:"mamba_full_memory_ratio"`
	// MambaRadixCacheStrategy is --mamba-radix-cache-strategy, how SGLang
	// buffers Mamba state for reuse inside its radix cache.
	MambaRadixCacheStrategy string `yaml:"mamba_radix_cache_strategy" json:"mamba_radix_cache_strategy"`
	// MaxMambaCacheSize is --max-mamba-cache-size, the number of Mamba
	// recurrent states SGLang keeps resident at once. 0 leaves the flag off
	// and SGLang's own default stands.
	MaxMambaCacheSize int `yaml:"max_mamba_cache_size" json:"max_mamba_cache_size"`
	// SpeculativeNumSteps is --speculative-num-steps, how many draft steps
	// EAGLE-family speculation runs before verifying. It shapes a speculative
	// algorithm rather than standing on its own, so it means nothing without
	// one.
	SpeculativeNumSteps int `yaml:"speculative_num_steps" json:"speculative_num_steps"`
	// SpeculativeEagleTopK is --speculative-eagle-topk, the branching factor
	// EAGLE and EAGLE3 draft with at each step. Every other speculative
	// algorithm has no top-k to speak of.
	SpeculativeEagleTopK int `yaml:"speculative_eagle_topk" json:"speculative_eagle_topk"`
	// SamplingDefaults is --sampling-defaults, which source SGLang reads its
	// default sampling parameters from when a request states none of its own.
	SamplingDefaults string `yaml:"sampling_defaults" json:"sampling_defaults"`
	// PageSize is --page-size, the number of tokens one KV cache page holds.
	// A model served with compressed sparse attention cannot take the
	// runtime's own default: that attention path addresses its cache by page
	// and its qualified launcher pins 64 for exactly that reason. 0 leaves
	// the flag off and SGLang's own default stands.
	PageSize int `yaml:"page_size" json:"page_size"`
	// MambaTrackInterval is --mamba-track-interval, how far apart SGLang
	// keeps the Mamba states it can rewind a sequence to. The state is
	// addressed in whole cache pages, so a qualified launcher documents two
	// rules for it: the interval is a multiple of the page size, and it is
	// never smaller than the draft token count. The validator holds a recipe
	// to the first one, against the page size that recipe pins. 0 leaves the
	// flag off and SGLang's own default stands.
	MambaTrackInterval int `yaml:"mamba_track_interval" json:"mamba_track_interval"`
	// AllowAutoTruncate turns on --allow-auto-truncate, which makes SGLang
	// shorten a request that is longer than the context window instead of
	// refusing it.
	AllowAutoTruncate bool `yaml:"allow_auto_truncate" json:"allow_auto_truncate"`
	// PLEOffloadEmbedding turns on --ple-offload-embedding, which keeps the
	// checkpoint's per-layer embedding (PLE) n-gram tables in host memory
	// instead of in device memory. It exists for a model that carries 52 GB
	// of those tables in FP8. A GB10 spends one unified pool for both, and
	// the qualified launcher's own warning says what a device-resident fp16
	// copy of the tables costs there: it overcommits that pool and freezes
	// the whole node.
	PLEOffloadEmbedding bool `yaml:"ple_offload_embedding" json:"ple_offload_embedding"`
	// CudaGraphBSDecode is --cuda-graph-bs-decode, the batch sizes SGLang
	// captures decode CUDA graphs for, written as one increasing list of
	// space-separated numbers. A recipe pins it because the runtime's own
	// list is chosen for a larger device: on a GB10 the default list runs the
	// graph pool out of memory, so a qualified launcher names the few batch
	// sizes it really serves. Empty leaves the flag off and SGLang's own
	// default stands.
	CudaGraphBSDecode string `yaml:"cuda_graph_bs_decode" json:"cuda_graph_bs_decode"`
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

// PlannedMemoryBytes reports the device memory a recipe holds as an absolute
// number, for the kinds whose footprint is absolute: the mapped weights, the
// KV cache for the whole pinned context, and the runtime overhead measured at
// qualification.
//
// vLLM and SGLang are told what share of the device to claim and then fill
// it; llama.cpp is told nothing of the sort. It maps the GGUF and sizes the
// cache from --ctx-size, so its footprint follows from the recipe's own
// figures and is knowable only when the recipe states a memory_model. ComfyUI
// is the same shape with a simpler sum: it holds the weights it loads plus
// whatever the diffusion pipeline needs while a generation runs, and it keeps
// no KV cache at all, so its kv_bytes_per_token is zero by construction.
// The second result is false for every other kind, and for a recipe of these
// kinds with no memory model — which the validator refuses.
// The figures come from a recipe file, which is data this process did not
// write, so the arithmetic is checked rather than assumed. An overflowing
// product wraps to a small positive number, and a small positive number is
// exactly what an OOM guardrail waves through, so overflow reports "no
// footprint" instead of a footprint that flatters the recipe.
func (r Recipe) PlannedMemoryBytes() (int64, bool) {
	if r.Runtime.Kind == "comfyui" {
		if r.Service.ComfyUI == nil || r.MemoryModel == nil {
			return 0, false
		}
		m := *r.MemoryModel
		if m.WeightsBytes < 0 || m.RuntimeOverheadBytes < 0 {
			return 0, false
		}
		return addWithoutOverflow(m.WeightsBytes, m.RuntimeOverheadBytes)
	}
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

// RequiresTerritoryConfirmation reports whether any artifact carries a
// territory-exclusion licence term, in which case the install API must
// require confirm_territory_eligibility (section 5.3) before install can
// proceed.
func (r Recipe) RequiresTerritoryConfirmation() bool {
	for _, artifact := range r.Artifacts {
		if len(artifact.LicenceTerritoryExclusions) > 0 {
			return true
		}
	}
	return false
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
