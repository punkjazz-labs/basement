package operations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/punkjazz-labs/basement/internal/recipe"
	"github.com/punkjazz-labs/basement/internal/recipe/recipetest"
)

func TestComfyUICommandNamesOnlyDirectoriesBasementOwns(t *testing.T) {
	r := recipetest.Media()
	entrypoint, args, err := runtimeCommand(r, Placement{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(entrypoint, " ") != "python3 main.py" {
		t.Fatalf("entrypoint=%v", entrypoint)
	}
	want := []string{
		"--listen", "0.0.0.0",
		"--port", "8188",
		"--output-directory", "/output",
		"--input-directory", "/input",
		"--temp-directory", "/tmp",
		"--user-directory", "/root/.cache/comfyui-user",
		"--extra-model-paths-config", "/root/.cache/comfyui-model-paths.yaml",
		"--disable-auto-launch",
	}
	if strings.Join(args, " ") != strings.Join(want, " ") {
		t.Fatalf("args=%v\nwant %v", args, want)
	}
}

// TestComfyUIRefusesASecondSpark keeps the kind honest about what it can do.
// A media recipe cannot declare two Sparks (the validator refuses it), so
// this can only be reached by a Recipe built in code, and it must not
// silently launch a single-node server instead.
func TestComfyUIRefusesASecondSpark(t *testing.T) {
	r := recipetest.Media()
	if _, _, err := runtimeCommand(r, Placement{Role: RoleWorker, NodeCount: 2}); err == nil {
		t.Fatal("a distributed placement must be refused")
	}
	missing := recipetest.Media()
	missing.Service.ComfyUI = nil
	if _, _, err := runtimeCommand(missing, Placement{}); err == nil {
		t.Fatal("a comfyui recipe with no service block must be refused")
	}
}

// TestComfyUIModelPathsFollowThePinnedFiles proves the folder map is derived
// from the weights the recipe actually pins, so it can never name a directory
// the download did not fetch.
func TestComfyUIModelPathsFollowThePinnedFiles(t *testing.T) {
	document := comfyUIModelPaths(recipetest.Media())
	want := "basement:\n  diffusion_models: /model/diffusion_models\n  text_encoders: /model/text_encoders\n  vae: /model/vae\n"
	if document != want {
		t.Fatalf("model paths document:\n%s\nwant:\n%s", document, want)
	}
	empty := recipetest.Copy(recipetest.Media())
	empty.Artifacts[0].Files = nil
	if got := comfyUIModelPaths(empty); got != "basement:\n" {
		t.Fatalf("an artifact pinning nothing must name no folders: %q", got)
	}
}

func TestMediaContainerGetsItsOutputAndInputMounts(t *testing.T) {
	executor := &HostExecutor{dataDir: "/data"}
	r := recipetest.Media()
	mounts := executor.expectedMounts(r)
	if mounts["/output"] != "/data/generations/media-test-1s" {
		t.Fatalf("output mount=%q", mounts["/output"])
	}
	if mounts["/input"] != "/data/generations/media-test-1s/.input" {
		t.Fatalf("input mount=%q", mounts["/input"])
	}
	if mounts["/model"] == "" || mounts["/root/.cache"] == "" {
		t.Fatalf("the existing mounts must survive: %#v", mounts)
	}
	// A text recipe gets nothing new.
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	text := executor.expectedMounts(recipes[0])
	if len(text) != len(recipes[0].Artifacts)+1 {
		t.Fatalf("a text recipe's mounts changed: %#v", text)
	}
}

func TestMediaHealthWaitUsesTheQueueRoute(t *testing.T) {
	if got := healthPath(recipetest.Media()); got != "/queue" {
		t.Fatalf("healthPath(comfyui)=%s", got)
	}
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	if got := healthPath(recipes[0]); got != "/health" {
		t.Fatalf("healthPath(text)=%s", got)
	}
}

func TestMediaMemoryPlanIsTheRecipesOwnTotal(t *testing.T) {
	plan, err := memoryPlan(recipetest.Media())
	if err != nil {
		t.Fatal(err)
	}
	if plan.budgetBytes != 50_000_000_000 || plan.utilization != 0 {
		t.Fatalf("plan=%#v; a media model claims bytes, not a share of the device", plan)
	}
	if plan.kvCacheDType != "" || plan.maxModelLen != 0 {
		t.Fatalf("a media model has no KV cache and no context length: %#v", plan)
	}
	if plan.maxConcurrentRequests != 1 {
		t.Fatalf("maxConcurrentRequests=%d, want the recipe's concurrent_generations", plan.maxConcurrentRequests)
	}
	unqualified := recipetest.Copy(recipetest.Media())
	unqualified.MemoryModel = nil
	if _, err := memoryPlan(unqualified); err == nil {
		t.Fatal("a media recipe with no memory model must have no plan")
	}
}

// fakeComfyUI is the test double for the runtime's HTTP API. It answers the
// four routes basement drives and writes whatever files the test says a run
// produced, into the directory the container's output mount points at.
type fakeComfyUI struct {
	server *httptest.Server
	// pending is how many history polls answer "not there yet" before the
	// run appears, so a test can watch a generation actually wait.
	pending int
	// completed and status are what history eventually reports.
	completed bool
	status    string
	// writes are files the run produces, relative to the output root, with
	// their contents. An entry reported but not written is how the
	// "no output file" failure is staged.
	writes     map[string]string
	reports    []string
	outputRoot string
	submitted  []json.RawMessage
	polls      int
}

func newFakeComfyUI(t *testing.T, outputRoot string) *fakeComfyUI {
	t.Helper()
	fake := &fakeComfyUI{completed: true, status: "success", writes: map[string]string{}, outputRoot: outputRoot}
	mux := http.NewServeMux()
	mux.HandleFunc("/queue", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"queue_running": []any{}, "queue_pending": []any{}})
	})
	mux.HandleFunc("/prompt", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Prompt json.RawMessage `json:"prompt"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		fake.submitted = append(fake.submitted, body.Prompt)
		_ = json.NewEncoder(w).Encode(map[string]any{"prompt_id": "p1"})
	})
	mux.HandleFunc("/history/", func(w http.ResponseWriter, _ *http.Request) {
		fake.polls++
		if fake.polls <= fake.pending {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		for name, body := range fake.writes {
			path := filepath.Join(fake.outputRoot, filepath.FromSlash(name))
			_ = os.MkdirAll(filepath.Dir(path), 0o750)
			_ = os.WriteFile(path, []byte(body), 0o640)
		}
		images := make([]map[string]string, 0, len(fake.reports))
		for _, name := range fake.reports {
			images = append(images, map[string]string{"filename": filepath.Base(name), "subfolder": filepath.Dir(name), "type": "output"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"p1": map[string]any{
				"status":  map[string]any{"status_str": fake.status, "completed": fake.completed, "messages": []any{}},
				"outputs": map[string]any{"6": map[string]any{"gifs": images}},
			},
		})
	})
	mux.HandleFunc("/interrupt", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

// port is what a recipe has to publish for the manager's own loopback dial to
// land on this fake.
func (f *fakeComfyUI) port(t *testing.T) int {
	t.Helper()
	value, err := strconv.Atoi(f.server.URL[strings.LastIndex(f.server.URL, ":")+1:])
	if err != nil {
		t.Fatal(err)
	}
	return value
}

// mediaExecutor wires a HostExecutor to a fake runtime on a temporary data
// directory, and returns the recipe pointed at it.
func mediaExecutor(t *testing.T) (*HostExecutor, recipe.Recipe, *fakeComfyUI) {
	t.Helper()
	recipetest.WithTextToVideoGraph(t)
	dataDir := t.TempDir()
	r := recipetest.Copy(recipetest.Media())
	executor := &HostExecutor{dataDir: dataDir, http: &http.Client{Timeout: 5 * time.Second}}
	root := executor.GenerationRoot(r)
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatal(err)
	}
	fake := newFakeComfyUI(t, root)
	r.Service.DefaultHostPort = fake.port(t)
	return executor, r, fake
}

func TestVerifyMediaGenerationProducesAFile(t *testing.T) {
	previous := GenerationPollInterval
	t.Cleanup(func() { GenerationPollInterval = previous })
	GenerationPollInterval = time.Millisecond

	executor, r, fake := mediaExecutor(t)
	fake.pending = 2
	fake.writes["basement_00001_.mp4"] = "video bytes"
	fake.reports = []string{"basement_00001_.mp4"}

	receipt, err := executor.Execute(context.Background(), Execution{}, recipe.Operation{Type: "verify_media_generation"}, r, nil)
	if err != nil {
		t.Fatalf("verify_media_generation: %v", err)
	}
	// The proof is the smallest legal generation: minimum duration, default
	// short edge, fixed seed.
	if receipt["blocks"] != 1 || receipt["frames"] != 22 || receipt["width"] != 768 || receipt["height"] != 768 || receipt["seed"] != mediaVerificationSeed {
		t.Fatalf("verification did not use the smallest legal generation: %#v", receipt)
	}
	files, _ := receipt["output_files"].([]string)
	if len(files) != 1 || filepath.Base(files[0]) != "basement_00001_.mp4" {
		t.Fatalf("output_files=%#v", receipt["output_files"])
	}
	// The file moved into the verification's own directory and is not empty.
	info, err := os.Stat(files[0])
	if err != nil || info.Size() == 0 {
		t.Fatalf("stat %s: %v", files[0], err)
	}
	if filepath.Base(filepath.Dir(files[0])) != "install-verification" {
		t.Fatalf("the proof was left in the shared output directory: %s", files[0])
	}
	if receipt["bytes"].(int64) != int64(len("video bytes")) {
		t.Fatalf("bytes=%#v", receipt["bytes"])
	}
	// The workflow reached the runtime rendered, with no token left in it.
	if len(fake.submitted) != 1 || strings.Contains(string(fake.submitted[0]), "{{") {
		t.Fatalf("submitted workflow=%s", fake.submitted)
	}
	// And nothing about the workflow is in the receipt.
	encoded, _ := json.Marshal(receipt)
	if strings.Contains(string(encoded), "class_type") || strings.Contains(string(encoded), "KSampler") {
		t.Fatalf("the workflow leaked into a receipt: %s", encoded)
	}
}

func TestVerifyMediaGenerationFailsWhenTheRuntimeReportsAnError(t *testing.T) {
	previous := GenerationPollInterval
	t.Cleanup(func() { GenerationPollInterval = previous })
	GenerationPollInterval = time.Millisecond

	executor, r, fake := mediaExecutor(t)
	fake.completed = false
	fake.status = "error"

	_, err := executor.Execute(context.Background(), Execution{}, recipe.Operation{Type: "verify_media_generation"}, r, nil)
	if err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("err=%v, want the runtime's own failure", err)
	}
}

// TestVerifyMediaGenerationFailsWhenNoFileAppears is the case a weaker check
// would wave through: the runtime says the workflow ran and wrote nothing.
func TestVerifyMediaGenerationFailsWhenNoFileAppears(t *testing.T) {
	previous := GenerationPollInterval
	t.Cleanup(func() { GenerationPollInterval = previous })
	GenerationPollInterval = time.Millisecond

	t.Run("nothing reported", func(t *testing.T) {
		executor, r, _ := mediaExecutor(t)
		_, err := executor.Execute(context.Background(), Execution{}, recipe.Operation{Type: "verify_media_generation"}, r, nil)
		if err == nil || !strings.Contains(err.Error(), "without writing an output file") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("reported but never written", func(t *testing.T) {
		executor, r, fake := mediaExecutor(t)
		fake.reports = []string{"basement_00001_.mp4"}
		_, err := executor.Execute(context.Background(), Execution{}, recipe.Operation{Type: "verify_media_generation"}, r, nil)
		if err == nil || !strings.Contains(err.Error(), "no such file was written") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("written empty", func(t *testing.T) {
		executor, r, fake := mediaExecutor(t)
		fake.writes["basement_00001_.mp4"] = ""
		fake.reports = []string{"basement_00001_.mp4"}
		_, err := executor.Execute(context.Background(), Execution{}, recipe.Operation{Type: "verify_media_generation"}, r, nil)
		if err == nil || !strings.Contains(err.Error(), "empty file") {
			t.Fatalf("err=%v", err)
		}
	})
}

// TestMediaGenerationCompletionChecksTheProofOnDisk proves a resumed job does
// not regenerate an hour-long clip, and does not accept a receipt whose files
// have since gone.
func TestMediaGenerationCompletionChecksTheProofOnDisk(t *testing.T) {
	executor := &HostExecutor{dataDir: t.TempDir()}
	proof := filepath.Join(executor.dataDir, "proof.mp4")
	if err := os.WriteFile(proof, []byte("bytes"), 0o640); err != nil {
		t.Fatal(err)
	}
	receipt, _ := json.Marshal(map[string]any{"output_files": []string{proof}, "bytes": 5})
	op := recipe.Operation{Type: "verify_media_generation"}
	if !executor.Completed(context.Background(), Execution{}, op, recipetest.Media(), receipt) {
		t.Fatal("a receipt whose file is on disk must count as proved")
	}
	if err := os.Remove(proof); err != nil {
		t.Fatal(err)
	}
	if executor.Completed(context.Background(), Execution{}, op, recipetest.Media(), receipt) {
		t.Fatal("a receipt whose file is gone must not count as proved")
	}
	for _, bad := range []string{`{}`, `{"output_files":[],"bytes":5}`, `not json`} {
		if executor.Completed(context.Background(), Execution{}, op, recipetest.Media(), json.RawMessage(bad)) {
			t.Fatalf("receipt %q must not count as proved", bad)
		}
	}
}

// TestComfyUIRuntimeStateIsWrittenBeforeTheContainer proves the two things a
// media runtime needs to start are put in the container's writable bind, and
// that no other kind gets them.
func TestComfyUIRuntimeStateIsWrittenBeforeTheContainer(t *testing.T) {
	executor := &HostExecutor{dataDir: t.TempDir()}
	cache := t.TempDir()
	if err := executor.writeComfyUIRuntimeState(recipetest.Media(), cache); err != nil {
		t.Fatal(err)
	}
	document, err := os.ReadFile(filepath.Join(cache, "comfyui-model-paths.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(document), "diffusion_models: /model/diffusion_models") {
		t.Fatalf("model paths document:\n%s", document)
	}
	if info, err := os.Stat(filepath.Join(cache, "comfyui-user")); err != nil || !info.IsDir() {
		t.Fatalf("the runtime's own state directory is missing: %v", err)
	}
	bare := t.TempDir()
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.writeComfyUIRuntimeState(recipes[0], bare); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(bare)
	if err != nil || len(entries) != 0 {
		t.Fatalf("a text recipe must get nothing here: %#v", entries)
	}
}

// TestHistoryCollectsOnlyOutputTypeFiles keeps a temp preview from being
// mistaken for a result.
func TestHistoryCollectsOnlyOutputTypeFiles(t *testing.T) {
	body := `{"p1":{"status":{"status_str":"success","completed":true},"outputs":{
	  "6":{"gifs":[{"filename":"clip.mp4","subfolder":"","type":"output"}]},
	  "7":{"images":[{"filename":"preview.png","subfolder":"","type":"temp"}]},
	  "8":{"text":["not a file list"]}}}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	entry, err := NewComfyUIClient(server.URL).History(context.Background(), "p1")
	if err != nil {
		t.Fatal(err)
	}
	if !entry.Present || !entry.Completed || strings.Join(entry.Outputs, ",") != "clip.mp4" {
		t.Fatalf("entry=%#v", entry)
	}
}

func TestComfyUIProgressParsingUsesPromptScopedEventShapes(t *testing.T) {
	for _, test := range []struct {
		name    string
		message string
		want    GenerationStepProgress
		ok      bool
	}{
		{
			name:    "executing node",
			message: `{"type":"executing","data":{"node":"14","display_node":"14","prompt_id":"p1"}}`,
			want:    GenerationStepProgress{Node: "14"}, ok: true,
		},
		{
			name:    "sampler progress",
			message: `{"type":"progress","data":{"value":7,"max":20,"prompt_id":"p1","node":"14"}}`,
			want:    GenerationStepProgress{Value: 7, Max: 20, Node: "14"}, ok: true,
		},
		{
			name:    "different prompt",
			message: `{"type":"progress","data":{"value":8,"max":20,"prompt_id":"p2","node":"14"}}`,
		},
		{
			name:    "execution start is not progress",
			message: `{"type":"execution_start","data":{"prompt_id":"p1"}}`,
		},
		{
			name:    "execution success is not an outcome",
			message: `{"type":"execution_success","data":{"prompt_id":"p1","timestamp":1754301600000}}`,
		},
		{
			name:    "execution error is not an outcome",
			message: `{"type":"execution_error","data":{"prompt_id":"p1","node_id":"14","node_type":"SamplerCustomAdvanced","exception_message":"failed"}}`,
		},
		{
			name:    "binary preview is ignored",
			message: "\x00\x01preview",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, ok := parseComfyUIProgress([]byte(test.message), "p1")
			if ok != test.ok || got != test.want {
				t.Fatalf("progress=%#v ok=%t, want %#v ok=%t", got, ok, test.want, test.ok)
			}
		})
	}
}

type scriptedProgressSocket struct {
	mu       sync.Mutex
	messages [][]byte
	closed   bool
}

func (s *scriptedProgressSocket) Receive() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.messages) == 0 {
		return nil, io.EOF
	}
	message := s.messages[0]
	s.messages = s.messages[1:]
	return message, nil
}

func (s *scriptedProgressSocket) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}

func holdProgressSeams(t *testing.T) {
	t.Helper()
	previousDial := dialComfyUIProgress
	previousReconnect := GenerationProgressReconnectInterval
	t.Cleanup(func() {
		dialComfyUIProgress = previousDial
		GenerationProgressReconnectInterval = previousReconnect
	})
}

func TestComfyUIProgressReconnectsAfterTheSocketDrops(t *testing.T) {
	holdProgressSeams(t)
	GenerationProgressReconnectInterval = time.Millisecond
	first := &scriptedProgressSocket{messages: [][]byte{
		[]byte(`{"type":"progress","data":{"value":1,"max":20,"prompt_id":"p1","node":"14"}}`),
	}}
	second := &scriptedProgressSocket{messages: [][]byte{
		[]byte(`{"type":"progress","data":{"value":2,"max":20,"prompt_id":"p1","node":"14"}}`),
	}}
	var dials atomic.Int32
	dialComfyUIProgress = func(_ context.Context, endpoint, _ string) (comfyUIProgressSocket, error) {
		if !strings.Contains(endpoint, "/ws?clientId=basement-test") {
			t.Errorf("endpoint=%q", endpoint)
		}
		if dials.Add(1) == 1 {
			return first, nil
		}
		return second, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	updates := make(chan GenerationStepProgress, 4)
	done := make(chan struct{})
	go func() {
		NewComfyUIClient("http://127.0.0.1:8188").watchProgress(ctx, "basement-test", "p1", func(step GenerationStepProgress) error {
			updates <- step
			return nil
		})
		close(done)
	}()
	for _, want := range []int64{1, 2} {
		select {
		case got := <-updates:
			if got.Value != want || got.Max != 20 {
				t.Fatalf("progress=%#v, want value %d of 20", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("progress value %d did not arrive", want)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("progress watcher did not stop")
	}
	if dials.Load() < 2 {
		t.Fatalf("dials=%d, want a reconnect", dials.Load())
	}
}

func TestFailingProgressSocketDoesNotFailTheGeneration(t *testing.T) {
	holdProgressSeams(t)
	previousPoll := GenerationPollInterval
	t.Cleanup(func() { GenerationPollInterval = previousPoll })
	GenerationPollInterval = time.Millisecond
	GenerationProgressReconnectInterval = time.Millisecond
	var dials atomic.Int32
	dialComfyUIProgress = func(context.Context, string, string) (comfyUIProgressSocket, error) {
		dials.Add(1)
		return nil, errors.New("socket unavailable")
	}

	outputRoot := t.TempDir()
	var polls atomic.Int32
	client := &ComfyUIClient{baseURL: "http://runtime.test", http: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := "{}"
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/prompt":
			body = `{"prompt_id":"p1"}`
		case request.Method == http.MethodGet && request.URL.Path == "/queue":
			body = `{"queue_running":[],"queue_pending":[]}`
		case request.Method == http.MethodGet && request.URL.Path == "/history/p1":
			if polls.Add(1) > 10 {
				if err := os.WriteFile(filepath.Join(outputRoot, "clip.mp4"), []byte("video bytes"), 0o640); err != nil {
					return nil, err
				}
				body = `{"p1":{"status":{"status_str":"success","completed":true,"messages":[]},"outputs":{"6":{"gifs":[{"filename":"clip.mp4","subfolder":"","type":"output"}]}}}}`
			}
		default:
			return nil, fmt.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})}}
	outcome, err := RunGeneration(
		context.Background(), client, []byte(`{"1":{"class_type":"Test"}}`),
		outputRoot, filepath.Join(t.TempDir(), "generation"), time.Second,
		func(GenerationProgressUpdate) error { return nil },
	)
	if err != nil {
		t.Fatalf("generation failed with its optional socket: %v", err)
	}
	if len(outcome.Files) != 1 || outcome.Bytes == 0 {
		t.Fatalf("outcome=%#v", outcome)
	}
	if dials.Load() == 0 {
		t.Fatal("the failing progress socket was not attempted")
	}
}
