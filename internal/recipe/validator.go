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
	if r.Topology.SparkCount != 1 {
		problems = append(problems, "only one Spark is supported")
	}
	if r.Runtime.Kind != "vllm" {
		problems = append(problems, "runtime kind must be vllm")
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
	if err := validateVLLM(r.Service.VLLM, roles); err != nil {
		problems = append(problems, err.Error())
	}
	if util, err := strconv.ParseFloat(r.Service.VLLM.GPUMemoryUtil, 64); err == nil && r.Requirements.MinimumMemoryBytes > 0 {
		nominalHeadroom := int64(math.Floor(float64(r.Requirements.MinimumMemoryBytes) * (1 - util)))
		if nominalHeadroom < r.Requirements.MemoryReserveBytes {
			problems = append(problems, "vllm GPU utilization does not preserve the per-node memory reserve")
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

func validateVLLM(v VLLMConfig, roles map[string]bool) error {
	if v.TensorParallelSize != 1 || v.MaxModelLen <= 0 || v.MaxNumSeqs <= 0 || v.MaxBatchedTokens < 0 || v.MultimodalImageLimit < 0 || v.MultimodalImageLimit > 8 {
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
	if v.ChatTemplateFile != "" {
		clean := path.Clean(v.ChatTemplateFile)
		if filepath.IsAbs(v.ChatTemplateFile) || strings.Contains(v.ChatTemplateFile, "\\") || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
			return errors.New("chat template file is unsafe")
		}
	}
	if err := validateGeneration(v.Generation); err != nil {
		return err
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
