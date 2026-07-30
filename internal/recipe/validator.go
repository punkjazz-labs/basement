package recipe

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var (
	recipeIDPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,79}$`)
	digestPattern     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	revisionPattern   = regexp.MustCompile(`^[0-9a-f]{40}$`)
	repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

var allowedOperations = map[string]bool{
	"verify_architecture": true, "verify_dgx_spark": true, "verify_disk": true,
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
	if r.Trust != "runonspark-candidate" && r.Trust != "runonspark-verified" {
		problems = append(problems, "trust is invalid")
	}
	if r.Verification != "candidate" && r.Verification != "dgx-spark-verified" {
		problems = append(problems, "verification is invalid")
	}
	if r.Trust == "runonspark-verified" && r.Verification != "dgx-spark-verified" {
		problems = append(problems, "verified trust requires real DGX verification")
	}
	if r.Topology.SparkCount != 1 {
		problems = append(problems, "only one Spark is supported")
	}
	if r.Runtime.Kind != "vllm" {
		problems = append(problems, "runtime kind must be vllm")
	}
	if !repositoryPattern.MatchString(r.Runtime.Image) {
		problems = append(problems, "runtime image must be a two-part repository without a tag")
	}
	if !digestPattern.MatchString(r.Runtime.Digest) {
		problems = append(problems, "runtime digest must be an immutable sha256")
	}
	if r.Runtime.ImageBytes <= 0 {
		problems = append(problems, "runtime image_bytes must be positive")
	}
	if len(r.Artifacts) == 0 {
		problems = append(problems, "at least one artifact is required")
	}
	for i, artifact := range r.Artifacts {
		prefix := fmt.Sprintf("artifact[%d]", i)
		if artifact.Role == "" || !repositoryPattern.MatchString(artifact.Repository) {
			problems = append(problems, prefix+" role/repository are invalid")
		}
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
		}
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
	if len(r.Artifacts) > 0 && r.Service.ServedModelID != r.Artifacts[0].Repository {
		problems = append(problems, "served_model_id must identify the pinned primary artifact")
	}
	if err := validateVLLM(r.Service.VLLM); err != nil {
		problems = append(problems, err.Error())
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
	installSequence := []string{"verify_architecture", "verify_dgx_spark", "verify_disk", "verify_port", "verify_docker", "verify_nvidia_runtime", "verify_artifact_access", "pull_image", "download_artifact", "write_generated_config", "create_container", "start_container", "wait_http", "verify_openai_inference"}
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

func validateVLLM(v VLLMConfig) error {
	if v.TensorParallelSize != 1 || v.MaxModelLen <= 0 || v.MaxNumSeqs <= 0 || v.MaxBatchedTokens <= 0 {
		return errors.New("vllm numeric settings are invalid")
	}
	allowed := map[string]map[string]bool{
		"kv": {"fp8": true}, "attention": {"flashinfer": true}, "moe": {"marlin": true},
		"spec_method": {"mtp": true}, "spec_moe": {"triton": true}, "reasoning": {"qwen3": true},
		"tool": {"qwen3_xml": true}, "load": {"fastsafetensors": true},
	}
	if !allowed["kv"][v.KVCacheDType] || !allowed["attention"][v.AttentionBackend] || !allowed["moe"][v.MoEBackend] ||
		!allowed["spec_method"][v.SpeculativeMethod] || !allowed["spec_moe"][v.SpeculativeMoE] ||
		!allowed["reasoning"][v.ReasoningParser] || !allowed["tool"][v.ToolCallParser] || !allowed["load"][v.LoadFormat] {
		return errors.New("vllm setting is outside the recipe policy")
	}
	if v.GPUMemoryUtil != "0.4" || v.SpeculativeTokens != 3 {
		return errors.New("vllm resource settings are outside the verified candidate")
	}
	return nil
}
