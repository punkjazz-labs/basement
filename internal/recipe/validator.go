package recipe

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

var (
	recipeIDPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,79}$`)
	digestPattern     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	revisionPattern   = regexp.MustCompile(`^[0-9a-f]{40}$`)
	repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)
	imagePattern      = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?::[0-9]+)?(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)+$`)
	// parserNamePattern admits an empty value and otherwise a plain token: the
	// runtime resolves the name against its own parser registry, and this only
	// guarantees the value cannot be read as another command-line option.
	parserNamePattern = regexp.MustCompile(`^([a-z0-9][a-z0-9_.-]*)?$`)
)

var allowedOperations = map[string]bool{
	"verify_architecture": true, "verify_dgx_spark": true, "verify_memory_capacity": true, "verify_memory": true, "verify_disk": true,
	"verify_port": true, "verify_docker": true, "verify_nvidia_runtime": true,
	"verify_artifact_access": true,
	"pull_image":             true, "download_artifact": true, "write_generated_config": true,
	"create_container": true, "start_container": true, "stop_container": true,
	"remove_container": true, "wait_http": true, "verify_openai_inference": true,
	"remove_artifact_if_unshared": true,
}

func DecodeStrict(data []byte) (Recipe, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var r Recipe
	if err := decoder.Decode(&r); err != nil {
		return Recipe{}, fmt.Errorf("decode recipe: %w", err)
	}
	if err := Validate(r); err != nil {
		return Recipe{}, err
	}
	return r, nil
}

func Validate(r Recipe) error {
	var problems []string
	if r.SchemaVersion != 1 {
		problems = append(problems, "schema_version must be 1")
	}
	if !recipeIDPattern.MatchString(r.ID) {
		problems = append(problems, "id is invalid")
	}
	if r.Version < 1 {
		problems = append(problems, "version must be positive")
	}
	if strings.TrimSpace(r.DisplayName) == "" || strings.TrimSpace(r.Publisher) == "" {
		problems = append(problems, "display_name and publisher are required")
	}
	if r.Trust != "basement-candidate" && r.Trust != "basement-verified" {
		problems = append(problems, "trust is invalid")
	}
	if r.Verification != "candidate" && r.Verification != "dgx-spark-verified" {
		problems = append(problems, "verification is invalid")
	}
	if r.Trust == "basement-verified" && r.Verification != "dgx-spark-verified" {
		problems = append(problems, "verified trust requires real DGX verification")
	}
	sourceURL, sourceErr := url.Parse(r.Source.URL)
	if sourceErr != nil || sourceURL.Scheme != "https" || (sourceURL.Host != "github.com" && sourceURL.Host != "huggingface.co") || !revisionPattern.MatchString(r.Source.Revision) {
		problems = append(problems, "source must use an approved HTTPS URL and immutable revision")
	}
	problems = append(problems, topologyProblems(r.Topology)...)
	if !allowedRuntimeKinds[r.Runtime.Kind] {
		problems = append(problems, "runtime kind must be one of: "+strings.Join(runtimeKindNames(), ", "))
	}
	if !imagePattern.MatchString(r.Runtime.Image) {
		problems = append(problems, "runtime image must be a repository path without a tag")
	}
	if !digestPattern.MatchString(r.Runtime.Digest) {
		problems = append(problems, "runtime digest must be an immutable sha256")
	}
	if r.Runtime.ImageBytes <= 0 {
		problems = append(problems, "runtime image_bytes must be positive")
	}
	if r.Runtime.ImageDiskBytes < r.Runtime.ImageBytes || r.Runtime.ImageDiskBytes > r.Runtime.ImageBytes*10 {
		problems = append(problems, "runtime image_disk_bytes must safely bound expanded image storage")
	}
	if r.Runtime.ShmBytes < 0 || r.Runtime.ShmBytes > 64<<30 {
		problems = append(problems, "runtime shm_bytes is outside the safe limit")
	}
	allowedEnvironment := map[string]map[string]bool{
		"CUTE_DSL_ARCH":      {"sm_121a": true},
		"VLLM_TARGET_DEVICE": {"cuda": true},
		"MAX_JOBS":           {"4": true},
	}
	for name, value := range r.Runtime.Environment {
		if !allowedEnvironment[name][value] {
			problems = append(problems, "runtime environment is outside the allowlist")
		}
	}
	if len(r.Artifacts) == 0 {
		problems = append(problems, "at least one artifact is required")
	}
	roles := map[string]bool{}
	for i, artifact := range r.Artifacts {
		prefix := fmt.Sprintf("artifact[%d]", i)
		if !recipeIDPattern.MatchString(artifact.Role) || roles[artifact.Role] || !repositoryPattern.MatchString(artifact.Repository) {
			problems = append(problems, prefix+" role/repository are invalid")
		}
		roles[artifact.Role] = true
		if !revisionPattern.MatchString(artifact.Revision) {
			problems = append(problems, prefix+" revision must be a 40-character immutable commit")
		}
		if artifact.ExpectedBytes <= 0 {
			problems = append(problems, prefix+" expected_bytes must be positive")
		}
		if artifact.Licence == "" || artifact.LicenceURL == "" {
			problems = append(problems, prefix+" licence metadata is required")
		}
		licenceURL, err := url.Parse(artifact.LicenceURL)
		if err != nil || licenceURL.Scheme != "https" || licenceURL.Host != "huggingface.co" {
			problems = append(problems, prefix+" licence_url must be an HTTPS Hugging Face URL")
		} else if licenceURL.Path != "/"+artifact.Repository && !strings.HasPrefix(licenceURL.Path, "/"+artifact.Repository+"/") {
			problems = append(problems, prefix+" licence_url must reference the artifact's own repository")
		}
	}
	if !roles["primary"] {
		problems = append(problems, "artifacts must contain one primary role")
	}
	if r.Requirements.Architecture != "aarch64" {
		problems = append(problems, "architecture must be aarch64")
	}
	if !r.Requirements.DGXSpark || !r.Requirements.Docker || !r.Requirements.NvidiaRuntime {
		problems = append(problems, "DGX Spark, Docker, and NVIDIA runtime requirements must be explicit")
	}
	if r.Requirements.SafetyMarginBytes <= 0 {
		problems = append(problems, "a positive disk safety margin is required")
	}
	if r.Requirements.MinimumMemoryBytes <= 0 || r.Requirements.MemoryReserveBytes <= 0 || r.Requirements.MemoryReserveBytes >= r.Requirements.MinimumMemoryBytes {
		problems = append(problems, "positive per-node memory capacity and reserve are required")
	}
	seenSecrets := map[string]bool{}
	for _, secret := range r.Requirements.Secrets {
		if secret != "HF_TOKEN" || seenSecrets[secret] {
			problems = append(problems, "secret references must be unique and allowlisted")
		}
		seenSecrets[secret] = true
	}
	if r.Service.InternalPort != 8000 || r.Service.DefaultHostPort < 1024 || r.Service.DefaultHostPort > 65535 {
		problems = append(problems, "service ports are invalid")
	}
	if r.Service.ServedModelID == "" || filepath.IsAbs(r.Service.ServedModelID) || strings.Contains(r.Service.ServedModelID, "..") {
		problems = append(problems, "served_model_id is unsafe")
	}
	if primaryIndex, ok := r.ArtifactIndex("primary"); ok && r.Service.ServedModelID != r.Artifacts[primaryIndex].Repository {
		problems = append(problems, "served_model_id must identify the pinned primary artifact")
	}
	problems = append(problems, validateRuntimeBlocks(r, roles, r.Topology.SparkCount)...)
	if fraction, ok := r.Service.MemoryFraction(r.Runtime.Kind); ok {
		if util, err := strconv.ParseFloat(fraction, 64); err == nil && r.Requirements.MinimumMemoryBytes > 0 {
			nominalHeadroom := int64(math.Floor(float64(r.Requirements.MinimumMemoryBytes) * (1 - util)))
			if nominalHeadroom < r.Requirements.MemoryReserveBytes {
				problems = append(problems, r.Runtime.Kind+" device memory fraction does not preserve the per-node memory reserve")
			}
		}
	}
	if len(r.Operations) == 0 || len(r.Uninstall) == 0 {
		problems = append(problems, "operations and uninstall operations are required")
	}
	for _, operation := range append(append([]Operation{}, r.Operations...), r.Uninstall...) {
		if !allowedOperations[operation.Type] {
			problems = append(problems, "unknown operation: "+operation.Type)
		}
		if operation.Type == "run_shell" {
			problems = append(problems, "run_shell is forbidden")
		}
	}
	installSequence := []string{"verify_architecture", "verify_dgx_spark", "verify_memory_capacity", "verify_disk", "verify_port", "verify_docker", "verify_nvidia_runtime", "verify_artifact_access", "pull_image", "download_artifact", "write_generated_config", "create_container", "verify_memory", "start_container", "wait_http", "verify_openai_inference"}
	uninstallSequence := []string{"stop_container", "remove_container", "remove_artifact_if_unshared"}
	if !operationSequenceEqual(r.Operations, installSequence) {
		problems = append(problems, "install operations must use the complete verified lifecycle in order")
	}
	if !operationSequenceEqual(r.Uninstall, uninstallSequence) {
		problems = append(problems, "uninstall operations must use the complete safe lifecycle in order")
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func operationSequenceEqual(operations []Operation, expected []string) bool {
	if len(operations) != len(expected) {
		return false
	}
	for index := range expected {
		if operations[index].Type != expected[index] {
			return false
		}
	}
	return true
}

// allowedRuntimeKinds grows one kind at a time, and only once the manager can
// build that runtime's command, memory model, health wait and metric mapping.
var allowedRuntimeKinds = map[string]bool{"vllm": true, "sglang": true}

func runtimeKindNames() []string {
	names := make([]string, 0, len(allowedRuntimeKinds))
	for kind := range allowedRuntimeKinds {
		names = append(names, kind)
	}
	sort.Strings(names)
	return names
}

// validateRuntimeBlocks enforces the one-block rule and validates whichever
// block the recipe declares, so a wrong-kind block still reports its own
// content errors instead of hiding behind the kind mismatch.
func validateRuntimeBlocks(r Recipe, roles map[string]bool, sparkCount int) []string {
	var problems []string
	declared := make([]string, 0, 2)
	if r.Service.VLLM != nil {
		declared = append(declared, "service.vllm")
	}
	if r.Service.SGLang != nil {
		declared = append(declared, "service.sglang")
	}
	switch {
	case len(declared) == 0:
		problems = append(problems, "service must declare exactly one runtime block: service.vllm or service.sglang")
	case len(declared) > 1:
		problems = append(problems, "service declares "+strings.Join(declared, " and ")+"; keep only the block that matches runtime.kind")
	case allowedRuntimeKinds[r.Runtime.Kind] && declared[0] != "service."+r.Runtime.Kind:
		problems = append(problems, "runtime kind "+r.Runtime.Kind+" requires service."+r.Runtime.Kind+", but the recipe declares "+declared[0])
	}
	if r.Service.VLLM != nil {
		if err := validateVLLM(*r.Service.VLLM, roles, sparkCount); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if r.Service.SGLang != nil {
		if err := validateSGLang(*r.Service.SGLang, roles, sparkCount); err != nil {
			problems = append(problems, err.Error())
		}
	}
	return problems
}

// interconnectEnvironmentAllowlist is the entire environment a topology block
// may set. Every name is an NCCL, Gloo or PyTorch distributed control, so a
// recipe cannot smuggle arbitrary process environment into a container
// through the topology block.
var interconnectEnvironmentAllowlist = map[string]bool{
	"NCCL_IB_DISABLE": true, "NCCL_IB_HCA": true, "NCCL_IB_GID_INDEX": true,
	"NCCL_SOCKET_IFNAME": true, "GLOO_SOCKET_IFNAME": true, "TP_SOCKET_IFNAME": true,
	"NCCL_IGNORE_CPU_AFFINITY": true, "NCCL_DEBUG": true, "NCCL_P2P_DISABLE": true,
	"NCCL_NET_GDR_LEVEL": true, "NCCL_CROSS_NIC": true,
}

var interconnectValuePattern = regexp.MustCompile(`^[A-Za-z0-9_.:,/-]{1,64}$`)

// topologyProblems admits a two-Spark recipe only when it carries a complete
// interconnect description. A single-Spark recipe stays valid exactly as it
// was and must not carry one, so nothing can quietly half-declare a fabric.
func topologyProblems(t Topology) []string {
	var problems []string
	if t.SparkCount != 1 && t.SparkCount != 2 {
		return append(problems, "spark_count must be 1 or 2")
	}
	if t.SparkCount == 1 {
		if t.Interconnect != nil {
			problems = append(problems, "a single-Spark recipe must not declare topology.interconnect")
		}
		return problems
	}
	if t.Interconnect == nil {
		return append(problems, "a two-Spark recipe must declare topology.interconnect")
	}
	link := *t.Interconnect
	if link.Kind != "connectx7" {
		problems = append(problems, "topology.interconnect.kind must be connectx7")
	}
	if link.MasterPort < 1024 || link.MasterPort > 65535 {
		problems = append(problems, "topology.interconnect.master_port must be a non-privileged port")
	}
	for _, environment := range []map[string]string{link.SharedEnvironment, link.HeadEnvironment, link.WorkerEnvironment} {
		for name, value := range environment {
			if !interconnectEnvironmentAllowlist[name] || !interconnectValuePattern.MatchString(value) {
				problems = append(problems, "topology.interconnect environment is outside the allowlist")
			}
		}
	}
	// The head resolves --master-addr from this interface, and both ranks
	// pin their transports to it; without it there is no two-node launch.
	if t.SocketInterface() == "" {
		problems = append(problems, "topology.interconnect.shared_environment must set NCCL_SOCKET_IFNAME")
	}
	return problems
}

func validateVLLM(v VLLMConfig, roles map[string]bool, sparkCount int) error {
	// Tensor parallelism spans the whole topology: one rank per Spark.
	if v.TensorParallelSize != sparkCount || v.MaxModelLen <= 0 || v.MaxNumSeqs <= 0 || v.MaxBatchedTokens < 0 || v.MultimodalImageLimit < 0 || v.MultimodalImageLimit > 8 {
		return errors.New("vllm numeric settings are invalid")
	}
	allowed := map[string]map[string]bool{
		"kv": {"": true, "fp8": true}, "attention": {"": true, "flashinfer": true},
		"moe": {"": true, "auto": true, "marlin": true}, "linear": {"": true, "flashinfer_b12x": true},
		"spec_method": {"": true, "mtp": true, "dflash": true}, "spec_moe": {"": true, "triton": true},
		"reasoning": {"qwen3": true, "poolside_v1": true},
		"tool":      {"qwen3_xml": true, "qwen3_coder": true, "poolside_v1": true},
		"load":      {"": true, "fastsafetensors": true},
	}
	if !allowed["kv"][v.KVCacheDType] || !allowed["attention"][v.AttentionBackend] || !allowed["moe"][v.MoEBackend] ||
		!allowed["linear"][v.LinearBackend] ||
		!allowed["spec_method"][v.SpeculativeMethod] || !allowed["spec_moe"][v.SpeculativeMoE] ||
		!allowed["reasoning"][v.ReasoningParser] || !allowed["tool"][v.ToolCallParser] || !allowed["load"][v.LoadFormat] {
		return errors.New("vllm setting is outside the recipe policy")
	}
	util, err := strconv.ParseFloat(v.GPUMemoryUtil, 64)
	if err != nil || util <= 0 || util > 0.95 || v.SpeculativeTokens < 0 || v.SpeculativeTokens > 32 {
		return errors.New("vllm resource settings are outside the verified candidate")
	}
	// An empty method means the model has no speculative decoding; a recipe
	// must not carry draft settings it cannot honour.
	if v.SpeculativeMethod == "" && (v.SpeculativeTokens != 0 || v.SpeculativeModelRole != "" || v.SpeculativeMoE != "") {
		return errors.New("speculative settings require a speculative method")
	}
	if v.SpeculativeMethod != "" && v.SpeculativeTokens <= 0 {
		return errors.New("a speculative method requires a positive draft token count")
	}
	if v.SpeculativeMethod == "mtp" && v.SpeculativeModelRole != "" {
		return errors.New("MTP must not reference a separate speculative model")
	}
	if v.SpeculativeMethod == "dflash" && (v.SpeculativeModelRole == "" || !roles[v.SpeculativeModelRole] || v.SpeculativeModelRole == "primary") {
		return errors.New("DFlash must reference a declared non-primary artifact role")
	}
	if err := validateChatTemplateFile(v.ChatTemplateFile); err != nil {
		return err
	}
	if err := validateGeneration(v.Generation); err != nil {
		return err
	}
	return nil
}

// validateSGLang mirrors the vLLM policy: every value a recipe pins is a
// value the maintainers have seen work, and the list widens only when a
// recipe is qualified. Choice lists come from sglang.launch_server's own
// server arguments; the parser names are resolved by the runtime against its
// registry, so they are checked as plain tokens instead of an invented list.
func validateSGLang(s SGLangConfig, roles map[string]bool, sparkCount int) error {
	// Tensor parallelism spans the whole topology: one rank per Spark.
	if s.TensorParallelSize != sparkCount || s.ContextLength <= 0 || s.MaxRunningRequests <= 0 {
		return errors.New("sglang numeric settings are invalid")
	}
	allowed := map[string]map[string]bool{
		"quantization": {"": true, "fp8": true, "modelopt": true, "modelopt_fp8": true, "modelopt_fp4": true, "nvfp4_online": true},
		"kv":           {"": true, "auto": true, "fp8_e5m2": true, "fp8_e4m3": true, "bf16": true, "nvfp4": true},
		"attention":    {"": true, "flashinfer": true, "triton": true, "torch_native": true, "fa3": true},
		"spec":         {"": true, "EAGLE": true, "EAGLE3": true, "NEXTN": true, "STANDALONE": true, "NGRAM": true},
	}
	if !allowed["quantization"][s.Quantization] || !allowed["kv"][s.KVCacheDType] ||
		!allowed["attention"][s.AttentionBackend] || !allowed["spec"][s.SpeculativeAlgorithm] {
		return errors.New("sglang setting is outside the recipe policy")
	}
	if !parserNamePattern.MatchString(s.ToolCallParser) || !parserNamePattern.MatchString(s.ReasoningParser) {
		return errors.New("sglang parser names must be plain lowercase identifiers")
	}
	fraction, err := strconv.ParseFloat(s.MemFractionStatic, 64)
	if err != nil || fraction <= 0 || fraction > 0.95 {
		return errors.New("sglang mem_fraction_static must be a decimal above 0 and no greater than 0.95")
	}
	if s.SpeculativeAlgorithm == "" && (s.SpeculativeNumDraftTokens != 0 || s.SpeculativeModelRole != "") {
		return errors.New("sglang speculative settings require speculative_algorithm")
	}
	if s.SpeculativeAlgorithm != "" && (s.SpeculativeNumDraftTokens <= 0 || s.SpeculativeNumDraftTokens > 32) {
		return errors.New("sglang speculative_num_draft_tokens must be between 1 and 32")
	}
	if s.SpeculativeModelRole != "" && (!roles[s.SpeculativeModelRole] || s.SpeculativeModelRole == "primary") {
		return errors.New("sglang speculative_model_role must name a declared non-primary artifact role")
	}
	return validateChatTemplateFile(s.ChatTemplateFile)
}

// validateChatTemplateFile keeps the template inside the primary artifact
// mount: the value is joined to that mount path, so an absolute or climbing
// path would read a file the recipe never pinned.
func validateChatTemplateFile(file string) error {
	if file == "" {
		return nil
	}
	clean := path.Clean(file)
	if filepath.IsAbs(file) || strings.Contains(file, "\\") || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return errors.New("chat template file is unsafe")
	}
	return nil
}

func validateGeneration(g GenerationConfig) error {
	if g.Temperature == nil || g.TopP == nil || *g.Temperature < 0 || *g.Temperature > 2 || *g.TopP <= 0 || *g.TopP > 1 {
		return errors.New("generation settings must include safe temperature and top_p values")
	}
	if g.TopK != nil && (*g.TopK < 0 || *g.TopK > 1000) {
		return errors.New("generation top_k is outside the safe range")
	}
	if g.MinP != nil && (*g.MinP < 0 || *g.MinP > 1) {
		return errors.New("generation min_p is outside the safe range")
	}
	if g.PresencePenalty != nil && (*g.PresencePenalty < -2 || *g.PresencePenalty > 2) {
		return errors.New("generation presence penalty is outside the safe range")
	}
	if g.RepetitionPenalty != nil && (*g.RepetitionPenalty <= 0 || *g.RepetitionPenalty > 2) {
		return errors.New("generation repetition penalty is outside the safe range")
	}
	return nil
}
