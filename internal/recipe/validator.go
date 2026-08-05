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
	"verify_media_generation":     true,
	"remove_artifact_if_unshared": true,
}

// InferenceVerification names the operation that proves a freshly installed
// model really produces output, for the runtime kind given. Every install
// ends in one of these and no install may end without one: it is the step
// that separates "the container answered a health check" from "the model
// works". A media model produces no tokens and has no OpenAI endpoint, so it
// is proved by generating the smallest clip its recipe allows instead.
func InferenceVerification(kind string) string {
	if kind == "comfyui" {
		return "verify_media_generation"
	}
	return "verify_openai_inference"
}

// serviceInternalPort is the port a runtime's server binds inside its
// container. It is fixed per kind rather than free, so a recipe cannot point
// the health wait, the proxy and the container's published port at three
// different numbers. vLLM, SGLang and llama-server are all launched on 8000
// by their argument builders; ComfyUI binds its own documented 8188, which is
// also why a media model never contends with the text endpoint.
func serviceInternalPort(kind string) int {
	if kind == "comfyui" {
		return 8188
	}
	return 8000
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
		// Opt-out only: breakable CUDA graphs accumulate allocations during
		// long decodes on GB10 until the driver reports out of memory, so a
		// recipe may turn them off but never force them on.
		"VLLM_USE_BREAKABLE_CUDAGRAPH": {"0": true},
		// Opt-in only, and only to the GB10 target. A runtime image built
		// before GB10 defaults these arch lists to the previous Blackwell
		// generation, and anything it compiles at run time then targets that
		// instead of the machine it is running on.
		"TORCH_CUDA_ARCH_LIST":      {"12.1a": true},
		"FLASHINFER_CUDA_ARCH_LIST": {"12.1a": true},
		"VLLM_USE_B12X_MOE":         {"1": true},
	}
	// TMPDIR may only point at a surface the recipe itself declares writable:
	// runtimes whose JIT caches load compiled objects need an exec-friendly
	// temp dir, and the declared writable paths are exactly those mounts.
	writable := map[string]bool{}
	for _, path := range r.Runtime.WritablePaths {
		writable[path] = true
	}
	for name, value := range r.Runtime.Environment {
		if name == "TMPDIR" {
			if !writable[value] {
				problems = append(problems, "runtime environment TMPDIR must name one of the recipe's writable paths")
			}
			continue
		}
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
		} else if artifact.LicenceRepository == "" {
			if licenceURL.Path != "/"+artifact.Repository && !strings.HasPrefix(licenceURL.Path, "/"+artifact.Repository+"/") {
				problems = append(problems, prefix+" licence_url must reference the artifact's own repository")
			}
		} else if artifact.LicenceRevision != "" && !licenceURLReferencesRevision(licenceURL.Path, artifact.LicenceRepository, artifact.LicenceRevision) {
			problems = append(problems, prefix+" licence_url must reference licence_repository at licence_revision")
		}
		if (artifact.LicenceRepository == "") != (artifact.LicenceRevision == "") {
			problems = append(problems, prefix+" licence_repository and licence_revision must be set together")
		}
		if artifact.LicenceRepository != "" && !repositoryPattern.MatchString(artifact.LicenceRepository) {
			problems = append(problems, prefix+" licence_repository is invalid")
		}
		if artifact.LicenceRevision != "" && !revisionPattern.MatchString(artifact.LicenceRevision) {
			problems = append(problems, prefix+" licence_revision must be a 40-character immutable commit")
		}
		if artifact.LicenceTerritoryExclusions != nil {
			blank := false
			for _, exclusion := range artifact.LicenceTerritoryExclusions {
				if strings.TrimSpace(exclusion) == "" {
					blank = true
				}
			}
			if len(artifact.LicenceTerritoryExclusions) == 0 || blank {
				problems = append(problems, prefix+" licence_territory_exclusions must be a non-empty list of non-blank strings")
			}
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
	if r.Service.InternalPort != serviceInternalPort(r.Runtime.Kind) || r.Service.DefaultHostPort < 1024 || r.Service.DefaultHostPort > 65535 {
		problems = append(problems, fmt.Sprintf("service ports are invalid; a %s recipe serves on internal_port %d", r.Runtime.Kind, serviceInternalPort(r.Runtime.Kind)))
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
	installSequence := []string{"verify_architecture", "verify_dgx_spark", "verify_memory_capacity", "verify_disk", "verify_port", "verify_docker", "verify_nvidia_runtime", "verify_artifact_access", "pull_image", "download_artifact", "write_generated_config", "create_container", "verify_memory", "start_container", "wait_http", InferenceVerification(r.Runtime.Kind)}
	uninstallSequence := []string{"stop_container", "remove_container", "remove_artifact_if_unshared"}
	if !operationSequenceEqual(r.Operations, installSequence) {
		problems = append(problems, "install operations must use the complete verified lifecycle in order, ending in "+InferenceVerification(r.Runtime.Kind))
	}
	if !operationSequenceEqual(r.Uninstall, uninstallSequence) {
		problems = append(problems, "uninstall operations must use the complete safe lifecycle in order")
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func licenceURLReferencesRevision(urlPath, repository, revision string) bool {
	for _, form := range []string{"blob", "resolve", "raw"} {
		prefix := "/" + repository + "/" + form + "/" + revision + "/"
		if strings.HasPrefix(urlPath, prefix) && len(urlPath) > len(prefix) {
			return true
		}
	}
	return false
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
var allowedRuntimeKinds = map[string]bool{"vllm": true, "sglang": true, "llamacpp": true, "comfyui": true}

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
	declared := make([]string, 0, 4)
	if r.Service.VLLM != nil {
		declared = append(declared, "service.vllm")
	}
	if r.Service.SGLang != nil {
		declared = append(declared, "service.sglang")
	}
	if r.Service.LlamaCpp != nil {
		declared = append(declared, "service.llamacpp")
	}
	if r.Service.ComfyUI != nil {
		declared = append(declared, "service.comfyui")
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
	if r.Service.ComfyUI != nil {
		problems = append(problems, comfyUIProblems(r, sparkCount)...)
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

// allowedVLLMBlockSizes is the set of KV cache block sizes a recipe may pin.
// 0 means the recipe pinned nothing and vLLM keeps its own default.
var allowedVLLMBlockSizes = map[int]bool{0: true, 16: true, 32: true, 64: true, 128: true, 256: true}

// maxVLLMCUDAGraphCaptureSize bounds the largest batch a recipe may ask vLLM
// to capture a CUDA graph for. Every captured size holds device memory on a
// machine with no separate VRAM to fail into, so the schema caps the number a
// recipe can name rather than trusting it.
const maxVLLMCUDAGraphCaptureSize = 1024

func validateVLLM(v VLLMConfig, roles map[string]bool, sparkCount int) error {
	// Tensor parallelism spans the whole topology: one rank per Spark.
	if v.TensorParallelSize != sparkCount || v.MaxModelLen <= 0 || v.MaxNumSeqs <= 0 || v.MaxBatchedTokens < 0 || v.MultimodalImageLimit < 0 || v.MultimodalImageLimit > 8 {
		return errors.New("vllm numeric settings are invalid")
	}
	// Every value here is one a maintainer has seen a runtime image accept for
	// a recipe in this pack. nvfp4_ds_mla, flashinfer_b12x as a MoE backend,
	// the dspark speculative method and the deepseek_v4 parsers and tokenizer
	// mode are not stock vLLM: they exist in the GB10 build the two-Spark
	// DeepSeek V4 Flash recipe pins, and a recipe that names them on an image
	// without them fails at start rather than silently serving something else.
	allowed := map[string]map[string]bool{
		"kv": {"": true, "fp8": true, "nvfp4_ds_mla": true}, "attention": {"": true, "flashinfer": true},
		"moe": {"": true, "auto": true, "marlin": true, "flashinfer_b12x": true}, "linear": {"": true, "flashinfer_b12x": true},
		"spec_method": {"": true, "mtp": true, "dflash": true, "dspark": true}, "spec_moe": {"": true, "triton": true},
		"spec_draft_sample": {"": true, "probabilistic": true},
		"reasoning":         {"qwen3": true, "poolside_v1": true, "deepseek_v4": true},
		"tool":              {"qwen3_xml": true, "qwen3_coder": true, "poolside_v1": true, "deepseek_v4": true},
		"load":              {"": true, "fastsafetensors": true},
		"tokenizer":         {"": true, "auto": true, "deepseek_v4": true},
	}
	// disable_quant_fusions carries no allowlist because its type is one: the
	// field is a boolean, so the only compilation configuration a recipe can
	// ask for is the single fixed document the argument builder holds. The
	// strictness that matters for it is the decoder's, which rejects any
	// neighbouring key this schema does not name.
	if !allowed["kv"][v.KVCacheDType] || !allowed["attention"][v.AttentionBackend] || !allowed["moe"][v.MoEBackend] ||
		!allowed["linear"][v.LinearBackend] ||
		!allowed["spec_method"][v.SpeculativeMethod] || !allowed["spec_moe"][v.SpeculativeMoE] ||
		!allowed["spec_draft_sample"][v.SpeculativeDraftSampleMethod] ||
		!allowed["reasoning"][v.ReasoningParser] || !allowed["tool"][v.ToolCallParser] || !allowed["load"][v.LoadFormat] ||
		!allowed["tokenizer"][v.TokenizerMode] {
		return errors.New("vllm setting is outside the recipe policy")
	}
	if !allowedVLLMBlockSizes[v.BlockSize] {
		return errors.New("vllm block_size must be unset or one of 16, 32, 64, 128, 256")
	}
	// A capture size below max_num_seqs would leave the busiest batch this
	// recipe admits running eager while smaller ones run captured, which is
	// the opposite of what bounding the ladder is for.
	if v.MaxCUDAGraphCaptureSize < 0 || v.MaxCUDAGraphCaptureSize > maxVLLMCUDAGraphCaptureSize ||
		(v.MaxCUDAGraphCaptureSize > 0 && v.MaxCUDAGraphCaptureSize < v.MaxNumSeqs) {
		return errors.New("vllm max_cudagraph_capture_size must be unset, at least max_num_seqs, and no more than " + strconv.Itoa(maxVLLMCUDAGraphCaptureSize))
	}
	util, err := strconv.ParseFloat(v.GPUMemoryUtil, 64)
	if err != nil || util <= 0 || util > 0.95 || v.SpeculativeTokens < 0 || v.SpeculativeTokens > 32 {
		return errors.New("vllm resource settings are outside the verified candidate")
	}
	// An empty method means the model has no speculative decoding; a recipe
	// must not carry draft settings it cannot honour.
	if v.SpeculativeMethod == "" && (v.SpeculativeTokens != 0 || v.SpeculativeModelRole != "" || v.SpeculativeMoE != "" || v.SpeculativeDraftSampleMethod != "") {
		return errors.New("speculative settings require a speculative method")
	}
	if v.SpeculativeMethod != "" && v.SpeculativeTokens <= 0 {
		return errors.New("a speculative method requires a positive draft token count")
	}
	// MTP and DSpark both draft from heads carried by the served checkpoint
	// itself, so there is no second model for them to point at.
	if (v.SpeculativeMethod == "mtp" || v.SpeculativeMethod == "dspark") && v.SpeculativeModelRole != "" {
		return errors.New("MTP and DSpark draft from the served model's own heads and must not reference a separate speculative model")
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

// comfyUIModelFolders is the complete set of top-level directories a media
// recipe's weights may sit in. ComfyUI resolves a checkpoint by folder name,
// so these names are load-bearing rather than descriptive: a file under a
// folder ComfyUI does not know is a file no workflow can ever reference.
// The list widens when a recipe is qualified that needs a folder on it, the
// same way every other allowlist in this file does.
var comfyUIModelFolders = map[string]bool{
	"diffusion_models": true, "text_encoders": true, "vae": true,
	"clip": true, "clip_vision": true, "loras": true, "controlnet": true,
	"upscale_models": true,
}

// maxComfyUICanvasEdge bounds the largest dimension a recipe may declare.
// The canvas figures reach the console as the controls it offers and the
// generation API as the sizes it accepts, and an unbounded edge on a machine
// with one unified memory pool is an out-of-memory the user asked for by
// moving a slider.
const maxComfyUICanvasEdge = 4096

// maxComfyUIBlocks bounds the longest duration a recipe may declare, on the
// same reasoning as the canvas: a block is 17-odd frames of video and every
// one of them is minutes of wall clock on this hardware.
const maxComfyUIBlocks = 256

// comfyUIProblems validates the media block against the whole recipe. Like
// llama.cpp it is checked against more than itself: the graphs it names have
// to be files this binary actually ships, the directories it claims have to
// be ones the container layout leaves free, and the footprint has to come
// from a memory model, because ComfyUI is handed no share of the device to
// bound it with.
func comfyUIProblems(r Recipe, sparkCount int) []string {
	var problems []string
	c := *r.Service.ComfyUI
	// ComfyUI serves one machine. Nothing here builds a second rank, and a
	// recipe that asked for one would quietly run on one node instead.
	if sparkCount != 1 {
		problems = append(problems, "comfyui recipes run on a single Spark; serving one media model across two Sparks is not supported")
	}
	problems = append(problems, comfyUIGraphProblems(c)...)
	problems = append(problems, comfyUIDirectoryProblems(r, c)...)
	problems = append(problems, comfyUICanvasProblems(c)...)
	problems = append(problems, comfyUIDurationProblems(c)...)
	if c.ConcurrentGenerations != 1 {
		// Stated as a schema rule rather than left to the API: a second
		// generation beside the first has never been measured on this
		// hardware, and the generation queue is written against this number.
		problems = append(problems, "comfyui concurrent_generations must be 1; further requests are queued in order rather than run beside the first")
	}
	problems = append(problems, comfyUIWeightsProblems(r)...)
	return append(problems, comfyUIMemoryProblems(r)...)
}

func comfyUIGraphProblems(c ComfyUIConfig) []string {
	var problems []string
	if len(c.Graphs) == 0 {
		return []string{"comfyui graphs must name at least one workflow"}
	}
	if _, ok := c.Graphs[ModeTextToVideo]; !ok {
		// The install verification generates through this mode, so a recipe
		// without it could not be proved to work at all.
		problems = append(problems, "comfyui graphs must include "+ModeTextToVideo)
	}
	modes := make([]string, 0, len(c.Graphs))
	for mode := range c.Graphs {
		modes = append(modes, mode)
	}
	sort.Strings(modes)
	for _, mode := range modes {
		name := c.Graphs[mode]
		if !allowedGenerationModes[mode] {
			problems = append(problems, "comfyui graph mode "+mode+" is not one of: "+strings.Join(GenerationModeNames(), ", "))
			continue
		}
		raw, err := Graph(name)
		if err != nil {
			problems = append(problems, "comfyui graph for "+mode+": "+err.Error())
			continue
		}
		tokens, err := GraphTokens(raw)
		if err != nil {
			problems = append(problems, "comfyui graph "+name+": "+err.Error())
			continue
		}
		required := RequiredGraphTokens(mode)
		sort.Strings(required)
		if strings.Join(tokens, " ") != strings.Join(required, " ") {
			problems = append(problems, fmt.Sprintf("comfyui graph %s carries %s but %s requires exactly %s", name, joinOrNone(tokens), mode, strings.Join(required, " ")))
		}
		subfolders, err := GraphSaveSubfolders(raw)
		if err != nil {
			problems = append(problems, "comfyui graph "+name+": "+err.Error())
			continue
		}
		for _, prefix := range subfolders {
			problems = append(problems, fmt.Sprintf("comfyui graph %s saves into %q; a filename_prefix must not name a subfolder, because the runtime creates it as root inside the output directory and the manager can then never move the result out", name, prefix))
		}
	}
	return problems
}

func joinOrNone(tokens []string) string {
	if len(tokens) == 0 {
		return "no substitutable tokens"
	}
	return strings.Join(tokens, " ")
}

// comfyUIDirectoryProblems holds the two directories basement bind-mounts
// into the container to the same rules writable_paths is held to. They are
// mounted read-write over whatever the image has there, so one over /model
// would hide the weights and one over /root/.cache would throw away the
// compilation cache on every restart.
func comfyUIDirectoryProblems(r Recipe, c ComfyUIConfig) []string {
	var problems []string
	reserved := []string{TempMountPath, CacheMountPath}
	for _, artifact := range r.Artifacts {
		reserved = append(reserved, ArtifactMountPath(artifact.Role))
	}
	for field, declared := range map[string]string{"output_directory": c.OutputDirectory, "input_directory": c.InputDirectory} {
		if declared == "" {
			problems = append(problems, "comfyui "+field+" is required")
			continue
		}
		if declared == "/" || len(declared) > maxWritablePathLength || !writablePathPattern.MatchString(declared) || path.Clean(declared) != declared {
			problems = append(problems, "comfyui "+field+" must be an absolute container path with no trailing slash, relative segment or unusual character")
			continue
		}
		for _, occupied := range append(reserved, r.Runtime.WritablePaths...) {
			if pathsOverlap(declared, occupied) {
				problems = append(problems, "comfyui "+field+" "+declared+" collides with the container's "+occupied+" mount")
			}
		}
	}
	if c.OutputDirectory != "" && c.InputDirectory != "" && pathsOverlap(c.OutputDirectory, c.InputDirectory) {
		// basement stages source images into one and reads results out of
		// the other; nested, a staged image would read back as a result.
		problems = append(problems, "comfyui output_directory and input_directory must be separate paths")
	}
	sort.Strings(problems)
	return problems
}

func comfyUICanvasProblems(c ComfyUIConfig) []string {
	var problems []string
	for field, edge := range map[string]int{"default_short_edge": c.DefaultShortEdge, "max_short_edge": c.MaxShortEdge, "max_long_edge": c.MaxLongEdge} {
		if edge <= 0 || edge > maxComfyUICanvasEdge || edge%CanvasMultiple != 0 {
			problems = append(problems, fmt.Sprintf("comfyui %s must be a positive multiple of %d no greater than %d", field, CanvasMultiple, maxComfyUICanvasEdge))
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		return problems
	}
	if c.DefaultShortEdge > c.MaxShortEdge {
		problems = append(problems, "comfyui default_short_edge must not exceed max_short_edge")
	}
	if c.MaxShortEdge > c.MaxLongEdge {
		problems = append(problems, "comfyui max_short_edge must not exceed max_long_edge")
	}
	return problems
}

func comfyUIDurationProblems(c ComfyUIConfig) []string {
	var problems []string
	if c.FrameBlock <= 0 {
		problems = append(problems, "comfyui frame_block must be positive; it is the frame count one block of duration adds")
	}
	if c.FrameOffset < 0 {
		problems = append(problems, "comfyui frame_offset must not be negative")
	}
	if c.FramesPerSecond <= 0 || c.FramesPerSecond > 240 {
		problems = append(problems, "comfyui frames_per_second must be between 1 and 240")
	}
	if c.MinBlocks < 1 || c.MaxBlocks > maxComfyUIBlocks || c.MinBlocks > c.MaxBlocks {
		problems = append(problems, fmt.Sprintf("comfyui min_blocks and max_blocks must satisfy 1 <= min_blocks <= max_blocks <= %d", maxComfyUIBlocks))
		return problems
	}
	if c.DefaultBlocks < c.MinBlocks || c.DefaultBlocks > c.MaxBlocks {
		problems = append(problems, "comfyui default_blocks must be between min_blocks and max_blocks")
	}
	return problems
}

// comfyUIWeightsProblems requires per-file pinning and holds every pinned
// file to ComfyUI's own model folder layout. A media repository publishes
// every variant of a model side by side (the H3 repackaging is hundreds of
// gigabytes), so a whole-snapshot download is not a thing a Spark could do;
// and a file outside a folder ComfyUI resolves is a file nothing can load.
func comfyUIWeightsProblems(r Recipe) []string {
	index, ok := r.ArtifactIndex("primary")
	if !ok {
		return nil
	}
	files := r.Artifacts[index].Files
	if len(files) == 0 {
		return []string{"a comfyui recipe must pin its weights file by file; a media repository holds every variant of the model side by side"}
	}
	var problems []string
	folders := map[string]bool{}
	for _, file := range files {
		folder, _, nested := strings.Cut(file.Name, "/")
		if !nested || !comfyUIModelFolders[folder] {
			problems = append(problems, "comfyui weights file "+file.Name+" must sit in one of ComfyUI's model folders: "+comfyUIModelFolderNames())
			continue
		}
		folders[folder] = true
	}
	if len(problems) == 0 && len(folders) == 0 {
		problems = append(problems, "comfyui weights pin no model folder")
	}
	return problems
}

func comfyUIModelFolderNames() string {
	names := make([]string, 0, len(comfyUIModelFolders))
	for name := range comfyUIModelFolders {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// comfyUIMemoryProblems is the media counterpart of the device memory
// fraction check. ComfyUI is handed no share of the device: it loads the
// weights it is asked for and holds them plus whatever the pipeline needs
// while a generation runs, so the footprint is an absolute number the recipe
// states and this confirms it leaves the host reserve intact. Without a
// memory model there is no number at all, and an unbounded runtime on a
// machine with no discrete VRAM to fail into takes the host down with it.
func comfyUIMemoryProblems(r Recipe) []string {
	if r.MemoryModel == nil {
		return []string{"a comfyui recipe must declare memory_model; ComfyUI claims an absolute footprint rather than a share of the device"}
	}
	var problems []string
	m := *r.MemoryModel
	if m.WeightsBytes <= 0 || m.RuntimeOverheadBytes < 0 {
		problems = append(problems, "comfyui memory_model must state positive weights and a non-negative runtime overhead")
	}
	// A diffusion pipeline keeps no KV cache, so a per-token figure here
	// would be a number nothing computes from and nothing spends.
	if m.KVBytesPerToken != 0 {
		problems = append(problems, "comfyui memory_model kv_bytes_per_token must be 0; a media model keeps no KV cache")
	}
	if primaryIndex, ok := r.ArtifactIndex("primary"); ok && m.WeightsBytes != r.Artifacts[primaryIndex].ExpectedBytes {
		problems = append(problems, "comfyui memory_model weights_bytes must equal the primary artifact's expected_bytes")
	}
	if len(problems) > 0 {
		return problems
	}
	planned, ok := r.PlannedMemoryBytes()
	if !ok {
		return append(problems, "comfyui memory_model adds up to a footprint too large to represent")
	}
	// Written as a subtraction for the same reason the llama.cpp check is:
	// the reserve is already known to be below the minimum, so the
	// right-hand side cannot overflow.
	if r.Requirements.MinimumMemoryBytes > 0 && planned > r.Requirements.MinimumMemoryBytes-r.Requirements.MemoryReserveBytes {
		problems = append(problems, "comfyui planned memory does not preserve the per-node memory reserve")
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
