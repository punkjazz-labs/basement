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
	// writablePathPattern admits an absolute path of ordinary name components.
	// A leading dot is allowed because these are dot-directories by nature
	// (/root/.tilelang); "." and ".." are not, which the cleanliness check
	// beside it refuses.
	writablePathPattern = regexp.MustCompile(`^(/[A-Za-z0-9][A-Za-z0-9._-]*|/\.[A-Za-z0-9][A-Za-z0-9._-]*)+$`)
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
	problems = append(problems, writablePathProblems(r)...)
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
		problems = append(problems, artifactFileProblems(artifact, prefix)...)
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

// artifactFileProblems validates per-file pinning. An artifact that declares
// no files keeps today's whole-snapshot behaviour and is not checked here at
// all, so no existing recipe changes meaning. An artifact that declares files
// must account for every byte it claims: the declared sizes sum to
// expected_bytes exactly, so the disk guardrail and the download verify the
// same number and neither is left rounding.
func artifactFileProblems(artifact Artifact, prefix string) []string {
	if len(artifact.Files) == 0 {
		return nil
	}
	var problems []string
	seen := make(map[string]bool, len(artifact.Files))
	var total int64
	for _, file := range artifact.Files {
		if err := validateArtifactFileName(file.Name); err != nil {
			problems = append(problems, prefix+" file name is unsafe")
			continue
		}
		if seen[file.Name] {
			problems = append(problems, prefix+" declares "+file.Name+" more than once")
			continue
		}
		seen[file.Name] = true
		if file.ExpectedBytes <= 0 {
			problems = append(problems, prefix+" file "+file.Name+" must declare positive expected_bytes")
			continue
		}
		total += file.ExpectedBytes
	}
	if len(problems) == 0 && total != artifact.ExpectedBytes {
		problems = append(problems, fmt.Sprintf("%s files total %d bytes but expected_bytes is %d", prefix, total, artifact.ExpectedBytes))
	}
	return problems
}

// maxWritablePaths bounds how many private tmpfs mounts one recipe may ask
// for. Each is unified memory the model does not get, and a runtime that needs
// more than a handful of writable cache roots is a runtime nobody has
// qualified.
const maxWritablePaths = 4

// maxWritablePathLength bounds one path, so a recipe cannot hand the daemon a
// mount point of unbounded length.
const maxWritablePathLength = 128

// writablePathProblems validates runtime.writable_paths. Each entry is mounted
// as its own tmpfs inside the container, which makes it the only part of the
// schema that can change what the container's filesystem means, so it is held
// to the layout the manager owns: a writable path may not be the root, may not
// take over the scratch tmpfs, and may not sit anywhere near a mount that is
// already there. A writable tmpfs over /model would hide the weights behind an
// empty directory; one over /root/.cache would throw away the compilation
// cache on every restart; one containing either would do the same from above.
// A recipe declaring none keeps exactly the filesystem it has always had.
func writablePathProblems(r Recipe) []string {
	paths := r.Runtime.WritablePaths
	if len(paths) == 0 {
		return nil
	}
	var problems []string
	if len(paths) > maxWritablePaths {
		problems = append(problems, fmt.Sprintf("runtime writable_paths must declare at most %d paths", maxWritablePaths))
	}
	reserved := []string{TempMountPath, CacheMountPath}
	for _, artifact := range r.Artifacts {
		reserved = append(reserved, ArtifactMountPath(artifact.Role))
	}
	seen := make(map[string]bool, len(paths))
	for _, writable := range paths {
		if writable == "/" {
			problems = append(problems, "runtime writable_path must not be the container root")
			continue
		}
		if len(writable) > maxWritablePathLength || !writablePathPattern.MatchString(writable) || path.Clean(writable) != writable {
			problems = append(problems, "runtime writable_path "+writable+" must be an absolute container path with no trailing slash, relative segment or unusual character")
			continue
		}
		if seen[writable] {
			problems = append(problems, "runtime writable_path "+writable+" is declared more than once")
			continue
		}
		seen[writable] = true
		for _, occupied := range reserved {
			if pathsOverlap(writable, occupied) {
				problems = append(problems, "runtime writable_path "+writable+" collides with the container's "+occupied+" mount")
			}
		}
		for other := range seen {
			if other != writable && pathsOverlap(writable, other) {
				problems = append(problems, "runtime writable_path "+writable+" is inside "+other)
			}
		}
	}
	return problems
}

// pathsOverlap reports whether two absolute container paths are the same path
// or one contains the other. Both are already clean, so the comparison is by
// component and /modelling is not inside /model.
func pathsOverlap(a, b string) bool {
	return a == b || strings.HasPrefix(a, withTrailingSlash(b)) || strings.HasPrefix(b, withTrailingSlash(a))
}

func withTrailingSlash(p string) string {
	if strings.HasSuffix(p, "/") {
		return p
	}
	return p + "/"
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
var allowedRuntimeKinds = map[string]bool{"vllm": true, "sglang": true, "llamacpp": true}

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
	declared := make([]string, 0, 3)
	if r.Service.VLLM != nil {
		declared = append(declared, "service.vllm")
	}
	if r.Service.SGLang != nil {
		declared = append(declared, "service.sglang")
	}
	if r.Service.LlamaCpp != nil {
		declared = append(declared, "service.llamacpp")
	}
	switch {
	case len(declared) == 0:
		problems = append(problems, "service must declare exactly one runtime block: service."+strings.Join(runtimeKindNames(), ", service."))
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
	if r.Service.LlamaCpp != nil {
		problems = append(problems, llamaCppProblems(r, sparkCount)...)
	}
	return append(problems, chatTemplateReachabilityProblems(r)...)
}

// chatTemplateReachabilityProblems is the other half of the chat template
// check. Each block validates its own template for path safety; this asks
// whether the file will be on disk at all. When the primary artifact pins
// files, the download fetches those and nothing else, so a template outside
// that list is a file the runtime is told to read and the manager never
// fetches. That fails the pre-start file check on a clean install, and on a
// machine where someone once put a file there by hand it is worse: the
// runtime would consume unpinned, unverified bytes. An artifact pinning no
// files downloads the whole snapshot and is not checked here, so nothing
// that shipped before per-file pinning changes meaning.
func chatTemplateReachabilityProblems(r Recipe) []string {
	template := ""
	switch {
	case r.Service.VLLM != nil:
		template = r.Service.VLLM.ChatTemplateFile
	case r.Service.SGLang != nil:
		template = r.Service.SGLang.ChatTemplateFile
	case r.Service.LlamaCpp != nil:
		template = r.Service.LlamaCpp.ChatTemplateFile
	}
	index, ok := r.ArtifactIndex("primary")
	if template == "" || !ok || len(r.Artifacts[index].Files) == 0 {
		return nil
	}
	if primaryPinsFile(r, template) {
		return nil
	}
	return []string{"chat_template_file " + template + " is not one of the files the primary artifact pins, so nothing will download it"}
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

// llamaCppProblems validates the llama.cpp block against the whole recipe,
// not just against itself: the model file has to be one of the files the
// primary artifact pins, and the footprint has to come from a memory model,
// because llama.cpp has no memory-fraction knob to bound it with.
func llamaCppProblems(r Recipe, sparkCount int) []string {
	var problems []string
	l := *r.Service.LlamaCpp
	// llama.cpp can spread one model across machines with its RPC backend.
	// That is not this phase, nothing here builds those ranks, and a recipe
	// that asked for it would silently run on one node instead.
	if sparkCount != 1 {
		problems = append(problems, "llama.cpp recipes run on a single Spark; serving one model across two Sparks with llama.cpp is not supported yet")
	}
	if err := validateArtifactFileName(l.ModelFile); err != nil {
		problems = append(problems, "llamacpp model_file is unsafe")
	} else if !strings.HasSuffix(l.ModelFile, ".gguf") {
		problems = append(problems, "llamacpp model_file must name a .gguf file inside the primary artifact")
	} else if !primaryPinsFile(r, l.ModelFile) {
		problems = append(problems, "llamacpp model_file "+l.ModelFile+" is not one of the files the primary artifact pins")
	}
	if l.ContextSize <= 0 {
		problems = append(problems, "llamacpp context_size must be positive")
	}
	// A GB10 holds every layer in one unified pool, so a recipe either says
	// how many layers to offload or leaves the flag off entirely. A negative
	// count is neither.
	if l.GPULayers < 0 {
		problems = append(problems, "llamacpp gpu_layers must not be negative")
	}
	if l.Parallel <= 0 || l.Parallel > 64 {
		problems = append(problems, "llamacpp parallel must be between 1 and 64")
	}
	if !allowedFlashAttention[l.FlashAttention] {
		problems = append(problems, "llamacpp flash_attention must be one of: on, off, auto")
	}
	if err := validateChatTemplateFile(l.ChatTemplateFile); err != nil {
		problems = append(problems, err.Error())
	}
	return append(problems, llamaCppMemoryProblems(r)...)
}

// allowedFlashAttention is llama-server's own tri-state for --flash-attn.
// The empty value is "the recipe pinned nothing", which keeps the flag off
// the command line so llama.cpp's own default applies.
var allowedFlashAttention = map[string]bool{"": true, "on": true, "off": true, "auto": true}

// llamaCppMemoryProblems is the llama.cpp counterpart of the device memory
// fraction check the other kinds get. Those kinds state a share of the
// device and the check confirms the leftover covers the host reserve. Here
// the footprint is an absolute number the recipe computes, so the check
// confirms that number plus the reserve fits inside the memory the recipe
// requires a node to have. Without a memory model there is no number at all,
// and an unbounded runtime on a machine with no discrete VRAM to fail into
// takes the host down with it, so the model is required rather than optional.
func llamaCppMemoryProblems(r Recipe) []string {
	if r.MemoryModel == nil {
		return []string{"a llamacpp recipe must declare memory_model; llama.cpp claims an absolute footprint rather than a share of the device"}
	}
	var problems []string
	m := *r.MemoryModel
	// A KV cost of zero would make a long context look free, which no
	// attention model is, so it is refused rather than treated as unstated.
	if m.WeightsBytes <= 0 || m.KVBytesPerToken <= 0 || m.RuntimeOverheadBytes < 0 {
		problems = append(problems, "llamacpp memory_model must state positive weights and KV figures and a non-negative overhead")
	}
	if primaryIndex, ok := r.ArtifactIndex("primary"); ok && m.WeightsBytes != r.Artifacts[primaryIndex].ExpectedBytes {
		problems = append(problems, "llamacpp memory_model weights_bytes must equal the primary artifact's expected_bytes")
	}
	if len(problems) > 0 {
		return problems
	}
	planned, ok := r.PlannedMemoryBytes()
	if !ok {
		// The figures are individually sane and still do not add up, which
		// means the total overflowed. A wrapped total reads as a tiny
		// footprint and would sail through every check below it.
		return append(problems, "llamacpp memory_model and context_size multiply out to a footprint too large to represent")
	}
	// Written as a subtraction rather than planned+reserve: the reserve is
	// already known to be below the minimum, so the right-hand side cannot
	// overflow the way a sum against an attacker-chosen planned total could.
	if r.Requirements.MinimumMemoryBytes > 0 && planned > r.Requirements.MinimumMemoryBytes-r.Requirements.MemoryReserveBytes {
		problems = append(problems, "llamacpp planned memory does not preserve the per-node memory reserve")
	}
	return problems
}

// primaryPinsFile reports whether the primary artifact pins the named file.
// An artifact that pins no files at all downloads a whole snapshot, and a
// GGUF quantization is never a whole snapshot, so that case is a mismatch
// too rather than a free pass.
func primaryPinsFile(r Recipe, name string) bool {
	index, ok := r.ArtifactIndex("primary")
	if !ok {
		return false
	}
	for _, file := range r.Artifacts[index].Files {
		if file.Name == name {
			return true
		}
	}
	return false
}

// validateArtifactFileName keeps a pinned file inside the artifact it belongs
// to. The name is joined to the download target and to the container mount,
// so an absolute or climbing path would write and then read outside both.
func validateArtifactFileName(name string) error {
	if name == "" {
		return errors.New("artifact file name is empty")
	}
	clean := path.Clean(name)
	if filepath.IsAbs(name) || strings.Contains(name, "\\") || clean != name || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return errors.New("artifact file name is unsafe")
	}
	return nil
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
