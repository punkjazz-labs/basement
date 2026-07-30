package recipe

type Recipe struct {
	SchemaVersion int          `yaml:"schema_version" json:"schema_version"`
	ID            string       `yaml:"id" json:"id"`
	Version       int          `yaml:"version" json:"version"`
	DisplayName   string       `yaml:"display_name" json:"display_name"`
	Publisher     string       `yaml:"publisher" json:"publisher"`
	Trust         string       `yaml:"trust" json:"trust"`
	Verification  string       `yaml:"verification" json:"verification"`
	Topology      Topology     `yaml:"topology" json:"topology"`
	Runtime       Runtime      `yaml:"runtime" json:"runtime"`
	Artifacts     []Artifact   `yaml:"artifacts" json:"artifacts"`
	Requirements  Requirements `yaml:"requirements" json:"requirements"`
	Service       Service      `yaml:"service" json:"service"`
	Operations    []Operation  `yaml:"operations" json:"operations"`
	Uninstall     []Operation  `yaml:"uninstall" json:"uninstall"`
}

type Topology struct {
	SparkCount int `yaml:"spark_count" json:"spark_count"`
}

type Runtime struct {
	Kind       string `yaml:"kind" json:"kind"`
	Image      string `yaml:"image" json:"image"`
	Digest     string `yaml:"digest" json:"digest"`
	ImageBytes int64  `yaml:"image_bytes" json:"image_bytes"`
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
	SafetyMarginBytes     int64    `yaml:"safety_margin_bytes" json:"safety_margin_bytes"`
	Secrets               []string `yaml:"secrets" json:"secrets"`
	RequiredLicenceAccept bool     `yaml:"required_licence_acceptance" json:"required_licence_acceptance"`
}

type Service struct {
	InternalPort    int        `yaml:"internal_port" json:"internal_port"`
	DefaultHostPort int        `yaml:"default_host_port" json:"default_host_port"`
	ServedModelID   string     `yaml:"served_model_id" json:"served_model_id"`
	VLLM            VLLMConfig `yaml:"vllm" json:"vllm"`
}

type VLLMConfig struct {
	TensorParallelSize int    `yaml:"tensor_parallel_size" json:"tensor_parallel_size"`
	KVCacheDType       string `yaml:"kv_cache_dtype" json:"kv_cache_dtype"`
	AttentionBackend   string `yaml:"attention_backend" json:"attention_backend"`
	MoEBackend         string `yaml:"moe_backend" json:"moe_backend"`
	GPUMemoryUtil      string `yaml:"gpu_memory_utilization" json:"gpu_memory_utilization"`
	MaxModelLen        int    `yaml:"max_model_len" json:"max_model_len"`
	MaxNumSeqs         int    `yaml:"max_num_seqs" json:"max_num_seqs"`
	MaxBatchedTokens   int    `yaml:"max_num_batched_tokens" json:"max_num_batched_tokens"`
	SpeculativeMethod  string `yaml:"speculative_method" json:"speculative_method"`
	SpeculativeTokens  int    `yaml:"speculative_tokens" json:"speculative_tokens"`
	SpeculativeMoE     string `yaml:"speculative_moe_backend" json:"speculative_moe_backend"`
	ReasoningParser    string `yaml:"reasoning_parser" json:"reasoning_parser"`
	ToolCallParser     string `yaml:"tool_call_parser" json:"tool_call_parser"`
	LoadFormat         string `yaml:"load_format" json:"load_format"`
	TrustRemoteCode    bool   `yaml:"trust_remote_code" json:"trust_remote_code"`
	ChunkedPrefill     bool   `yaml:"chunked_prefill" json:"chunked_prefill"`
	AsyncScheduling    bool   `yaml:"async_scheduling" json:"async_scheduling"`
	PrefixCaching      bool   `yaml:"prefix_caching" json:"prefix_caching"`
	AutoToolChoice     bool   `yaml:"auto_tool_choice" json:"auto_tool_choice"`
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
	return r.TotalArtifactBytes() + r.Runtime.ImageBytes + r.Requirements.SafetyMarginBytes
}
