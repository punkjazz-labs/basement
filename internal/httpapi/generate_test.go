package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/punkjazz-labs/basement/internal/auth"
	"github.com/punkjazz-labs/basement/internal/engine"
	"github.com/punkjazz-labs/basement/internal/operations"
	"github.com/punkjazz-labs/basement/internal/recipe"
	"github.com/punkjazz-labs/basement/internal/recipe/recipetest"
	"github.com/punkjazz-labs/basement/internal/store"
)

// mediaRuntime is the fake ComfyUI the generation API drives. It writes one
// file per accepted workflow into the output directory the container mount
// points at, and can be held so a test can watch the queue behave.
type mediaRuntime struct {
	mu         sync.Mutex
	server     *httptest.Server
	outputRoot string
	blocked    bool
	// prompts is the order workflows actually reached the runtime in, read
	// out of the rendered graph. It is what proves the queue keeps its word.
	prompts []string
	// files maps a prompt id onto the file that run produced.
	files      map[string]string
	interrupts int
	next       int
	// silent leaves the run producing nothing, so a failing generation can
	// be observed end to end.
	silent bool
}

func newMediaRuntime(t *testing.T, outputRoot string) *mediaRuntime {
	t.Helper()
	runtime := &mediaRuntime{outputRoot: outputRoot, files: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/queue", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"queue_running": []any{}, "queue_pending": []any{}})
	})
	mux.HandleFunc("/prompt", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Prompt map[string]map[string]any `json:"prompt"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		runtime.mu.Lock()
		runtime.next++
		id := "p" + strconv.Itoa(runtime.next)
		if node, ok := body.Prompt["4"]; ok {
			if inputs, ok := node["inputs"].(map[string]any); ok {
				runtime.prompts = append(runtime.prompts, inputs["text"].(string))
			}
		}
		runtime.files[id] = id + ".mp4"
		runtime.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"prompt_id": id})
	})
	mux.HandleFunc("/history/", func(w http.ResponseWriter, r *http.Request) {
		id := filepath.Base(r.URL.Path)
		runtime.mu.Lock()
		blocked, silent := runtime.blocked, runtime.silent
		name := runtime.files[id]
		runtime.mu.Unlock()
		if blocked {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		outputs := map[string]any{}
		if !silent {
			_ = os.MkdirAll(runtime.outputRoot, 0o750)
			_ = os.WriteFile(filepath.Join(runtime.outputRoot, name), []byte("clip "+id), 0o640)
			outputs["6"] = map[string]any{"gifs": []map[string]string{{"filename": name, "subfolder": "", "type": "output"}}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			id: map[string]any{
				"status":  map[string]any{"status_str": "success", "completed": true, "messages": []any{}},
				"outputs": outputs,
			},
		})
	})
	mux.HandleFunc("/interrupt", func(w http.ResponseWriter, _ *http.Request) {
		runtime.mu.Lock()
		runtime.interrupts++
		runtime.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	runtime.server = httptest.NewServer(mux)
	t.Cleanup(runtime.server.Close)
	return runtime
}

func (m *mediaRuntime) hold(blocked bool) {
	m.mu.Lock()
	m.blocked = blocked
	m.mu.Unlock()
}

func (m *mediaRuntime) order() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.prompts...)
}

// mediaConsole is a paired console with one media model installed, active and
// ready, and a fake runtime behind it.
type mediaConsole struct {
	server  *httptest.Server
	manager *Server
	cookies []*http.Cookie
	headers map[string]string
	recipe  recipe.Recipe
	runtime *mediaRuntime
	store   *store.Store
	dataDir string
}

func newMediaConsole(t *testing.T) *mediaConsole {
	t.Helper()
	recipetest.WithTextToVideoGraph(t)
	previousInterval := operations.GenerationPollInterval
	t.Cleanup(func() { operations.GenerationPollInterval = previousInterval })
	operations.GenerationPollInterval = 5 * time.Millisecond

	dataDir := t.TempDir()
	database, err := store.Open(filepath.Join(dataDir, "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	authManager, err := auth.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	media := recipetest.Copy(recipetest.Media())
	root := operations.GenerationRoot(dataDir, media.ID)
	runtime := newMediaRuntime(t, root)
	port, err := strconv.Atoi(runtime.server.URL[strings.LastIndex(runtime.server.URL, ":")+1:])
	if err != nil {
		t.Fatal(err)
	}
	media.Service.DefaultHostPort = port
	if err := recipe.Validate(media); err != nil {
		t.Fatalf("the media fixture must stay a valid recipe: %v", err)
	}
	recipes := []recipe.Recipe{media}
	executor := &apiExecutor{done: map[string]bool{}}
	runner := engine.New(database, executor, recipes)
	srv := New("test-version", dataDir, authManager, database, readyInventory{}, executor, runner, recipes)
	server := httptest.NewServer(srv.Handler())
	t.Cleanup(server.Close)
	t.Cleanup(srv.Close)

	if err := database.ActivateExclusively(t.Context(), store.InstalledModel{
		RecipeID: media.ID, RecipeVersion: media.Version, Status: "ready", ArtifactPath: filepath.Join(dataDir, "artifacts"), ContainerID: "c1",
	}); err != nil {
		t.Fatal(err)
	}

	tokenBytes, err := os.ReadFile(authManager.PairingTokenPath())
	if err != nil {
		t.Fatal(err)
	}
	paired := doRequest(t, http.MethodPost, server.URL+"/api/v1/auth/pair",
		`{"token":"`+strings.TrimSpace(string(tokenBytes))+`"}`, nil, map[string]string{"Origin": server.URL})
	var pairResult struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.NewDecoder(paired.Body).Decode(&pairResult); err != nil {
		t.Fatal(err)
	}
	cookies := paired.Cookies()
	paired.Body.Close()
	return &mediaConsole{
		server: server, manager: srv, cookies: cookies, recipe: media, runtime: runtime, store: database, dataDir: dataDir,
		headers: map[string]string{"Origin": server.URL, "X-CSRF-Token": pairResult.CSRF},
	}
}

func (c *mediaConsole) generate(t *testing.T, body string) (int, map[string]any) {
	t.Helper()
	response := doRequest(t, http.MethodPost, c.server.URL+"/api/v1/generate", body, c.cookies, c.headers)
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return response.StatusCode, decoded
}

func (c *mediaConsole) show(t *testing.T, id string) map[string]any {
	t.Helper()
	response := doRequest(t, http.MethodGet, c.server.URL+"/api/v1/generations/"+id, "", c.cookies, nil)
	defer response.Body.Close()
	var decoded map[string]any
	_ = json.NewDecoder(response.Body).Decode(&decoded)
	return decoded
}

// awaitStatus waits for a generation to reach one of the given states. Every
// wait in these tests is bounded; a generation that never gets there is a
// failure, not a hang.
func (c *mediaConsole) awaitStatus(t *testing.T, id string, states ...string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		view := c.show(t, id)
		for _, state := range states {
			if view["status"] == state {
				return view
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("generation %s never reached %v; last view %#v", id, states, c.show(t, id))
	return nil
}

const goodRequest = `{"model_id":"media-test-1s","mode":"text_to_video","prompt":"a quiet room","blocks":1,"width":1152,"height":768}`

func TestGenerateValidatesAgainstTheRecipe(t *testing.T) {
	console := newMediaConsole(t)
	tests := []struct {
		name string
		body string
		want string
	}{
		{"no prompt", `{"model_id":"media-test-1s","mode":"text_to_video","prompt":"   ","blocks":1,"width":1152,"height":768}`, "a prompt is required"},
		{"prompt too long", `{"model_id":"media-test-1s","mode":"text_to_video","prompt":"` + strings.Repeat("x", 2001) + `","blocks":1,"width":1152,"height":768}`, "at most 2000 characters"},
		{"unknown mode", `{"model_id":"media-test-1s","mode":"text_to_music","prompt":"x","blocks":1,"width":1152,"height":768}`, "mode must be one of: text_to_video"},
		{"source image mode", `{"model_id":"media-test-1s","mode":"image_to_video","prompt":"x","blocks":1,"width":1152,"height":768}`, "mode must be one of: text_to_video"},
		{"blocks below the minimum", `{"model_id":"media-test-1s","mode":"text_to_video","prompt":"x","blocks":0,"width":1152,"height":768}`, "between 1 and 21 blocks"},
		{"blocks above the maximum", `{"model_id":"media-test-1s","mode":"text_to_video","prompt":"x","blocks":22,"width":1152,"height":768}`, "between 1 and 21 blocks"},
		{"width off the grid", `{"model_id":"media-test-1s","mode":"text_to_video","prompt":"x","blocks":1,"width":1150,"height":768}`, "width must be a positive multiple of 32, and this request asked for 1150"},
		{"height off the grid", `{"model_id":"media-test-1s","mode":"text_to_video","prompt":"x","blocks":1,"width":1152,"height":767}`, "height must be a positive multiple of 32, and this request asked for 767"},
		{"short edge beyond the canvas", `{"model_id":"media-test-1s","mode":"text_to_video","prompt":"x","blocks":1,"width":1024,"height":1024}`, "short edge of at most 768 pixels, and this request asked for 1024"},
		{"long edge beyond the canvas", `{"model_id":"media-test-1s","mode":"text_to_video","prompt":"x","blocks":1,"width":1376,"height":768}`, "long edge of at most 1344 pixels, and this request asked for 1376"},
		{"old short edge field", `{"model_id":"media-test-1s","mode":"text_to_video","prompt":"x","blocks":1,"short_edge":768}`, `unknown field "short_edge"`},
		{"negative seed", `{"model_id":"media-test-1s","mode":"text_to_video","prompt":"x","blocks":1,"width":1152,"height":768,"seed":-1}`, "seed must be between"},
		{"seed beyond what JSON carries", `{"model_id":"media-test-1s","mode":"text_to_video","prompt":"x","blocks":1,"width":1152,"height":768,"seed":9007199254740993}`, "seed must be between"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status, body := console.generate(t, test.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status=%d body=%#v", status, body)
			}
			message, _ := body["error"].(string)
			if !strings.Contains(message, test.want) {
				t.Fatalf("error=%q, want it to name %q", message, test.want)
			}
		})
	}
	// Nothing was recorded: a refused request is not a generation.
	records, err := console.store.Generations(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("a refused request was recorded: %#v", records)
	}
}

func TestValidateCanvas(t *testing.T) {
	target := recipetest.Media()
	config, ok := target.MediaGeneration()
	if !ok {
		t.Fatal("media fixture has no generation config")
	}
	for _, test := range []struct {
		name          string
		width, height int
		wantShort     int
		wantError     string
	}{
		{name: "width must be positive and on the grid", width: -32, height: 768, wantError: "width must be a positive multiple of 32, and this request asked for -32"},
		{name: "height must be positive and on the grid", width: 768, height: 1150, wantError: "height must be a positive multiple of 32, and this request asked for 1150"},
		{name: "short edge is bounded", width: 1024, height: 1024, wantError: "short edge of at most 768 pixels, and this request asked for 1024"},
		{name: "long edge is bounded", width: 768, height: 1376, wantError: "long edge of at most 1344 pixels, and this request asked for 1376"},
		{name: "landscape is valid", width: 1152, height: 768, wantShort: 768},
		{name: "portrait is valid", width: 768, height: 1152, wantShort: 768},
	} {
		t.Run(test.name, func(t *testing.T) {
			shortEdge, err := validateCanvas(test.width, test.height, target, config)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("error=%v, want it to name %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if shortEdge != test.wantShort {
				t.Fatalf("short edge=%d, want %d", shortEdge, test.wantShort)
			}
		})
	}
}

func TestGenerateAcceptsBothCanvasOrientations(t *testing.T) {
	console := newMediaConsole(t)
	for _, test := range []struct {
		name          string
		width, height int
	}{
		{name: "landscape", width: 1152, height: 768},
		{name: "portrait", width: 768, height: 1152},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := `{"model_id":"media-test-1s","mode":"text_to_video","prompt":"` + test.name + `","blocks":1,"width":` + strconv.Itoa(test.width) + `,"height":` + strconv.Itoa(test.height) + `}`
			status, accepted := console.generate(t, body)
			if status != http.StatusAccepted {
				t.Fatalf("status=%d body=%#v", status, accepted)
			}
			if accepted["width"] != float64(test.width) || accepted["height"] != float64(test.height) {
				t.Fatalf("accepted canvas=%#vx%#v, want %dx%d", accepted["width"], accepted["height"], test.width, test.height)
			}
			record, err := console.store.Generation(t.Context(), accepted["generation_id"].(string))
			if err != nil {
				t.Fatal(err)
			}
			if record.ShortEdge != 768 || record.Width != test.width || record.Height != test.height {
				t.Fatalf("stored canvas=%dx%d short_edge=%d", record.Width, record.Height, record.ShortEdge)
			}
		})
	}
}

func TestRecipeCatalogReportsMediaGenerationConfigOnlyForMedia(t *testing.T) {
	textRecipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	media := recipetest.Media()
	text := textRecipes[0]
	server := &Server{}
	server.SetRecipes([]recipe.Recipe{media, text}, []recipe.Recipe{media, text})

	request := httptest.NewRequest(http.MethodGet, "/api/v1/recipes", nil)
	response := httptest.NewRecorder()
	server.listRecipes(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	type mediaGeneration struct {
		Modes                 []string `json:"modes"`
		DefaultShortEdge      int      `json:"default_short_edge"`
		MaxShortEdge          int      `json:"max_short_edge"`
		MaxLongEdge           int      `json:"max_long_edge"`
		CanvasMultiple        int      `json:"canvas_multiple"`
		FrameBlock            int      `json:"frame_block"`
		FrameOffset           int      `json:"frame_offset"`
		FramesPerSecond       int      `json:"frames_per_second"`
		MinBlocks             int      `json:"min_blocks"`
		MaxBlocks             int      `json:"max_blocks"`
		DefaultBlocks         int      `json:"default_blocks"`
		ConcurrentGenerations int      `json:"concurrent_generations"`
	}
	var body []struct {
		ID              string          `json:"id"`
		MediaGeneration json.RawMessage `json:"media_generation"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	var sawMedia, sawText bool
	for _, item := range body {
		switch item.ID {
		case media.ID:
			sawMedia = true
			if len(item.MediaGeneration) == 0 {
				t.Fatal("media recipe has no media_generation block")
			}
			if strings.Contains(string(item.MediaGeneration), "t2v.json") {
				t.Fatalf("media_generation exposed a graph file name: %s", item.MediaGeneration)
			}
			var config mediaGeneration
			if err := json.Unmarshal(item.MediaGeneration, &config); err != nil {
				t.Fatal(err)
			}
			if len(config.Modes) != 1 || config.Modes[0] != recipe.ModeTextToVideo {
				t.Fatalf("modes=%v", config.Modes)
			}
			if config.DefaultShortEdge != 768 || config.MaxShortEdge != 768 || config.MaxLongEdge != 1344 || config.CanvasMultiple != recipe.CanvasMultiple ||
				config.FrameBlock != 17 || config.FrameOffset != 5 || config.FramesPerSecond != 24 || config.MinBlocks != 1 || config.MaxBlocks != 21 ||
				config.DefaultBlocks != 7 || config.ConcurrentGenerations != 1 {
				t.Fatalf("media_generation=%#v", config)
			}
		case text.ID:
			sawText = true
			if len(item.MediaGeneration) != 0 {
				t.Fatalf("text recipe gained media_generation: %#v", item.MediaGeneration)
			}
		}
	}
	if !sawMedia || !sawText {
		t.Fatalf("catalog omitted fixture recipes: media=%t text=%t", sawMedia, sawText)
	}
}

// TestGenerateRefusesModelsThatCannotAnswer covers the three conflicts: a
// model that is not the one running, a text model, and nothing running at all.
func TestGenerateRefusesModelsThatCannotAnswer(t *testing.T) {
	console := newMediaConsole(t)
	status, body := console.generate(t, `{"model_id":"some-other-model","mode":"text_to_video","prompt":"x","blocks":1,"width":1152,"height":768}`)
	if status != http.StatusConflict || !strings.Contains(body["error"].(string), "switch to some-other-model") {
		t.Fatalf("status=%d body=%#v", status, body)
	}
	if err := console.store.SetModelState(t.Context(), console.recipe.ID, "stopped", false); err != nil {
		t.Fatal(err)
	}
	status, body = console.generate(t, goodRequest)
	if status != http.StatusConflict || !strings.Contains(body["error"].(string), "no model is running") {
		t.Fatalf("status=%d body=%#v", status, body)
	}
}

func TestGenerateRefusesATextModel(t *testing.T) {
	console := newMediaConsole(t)
	// The catalogue entry for the running model becomes a text recipe: the
	// request is well formed and the machine simply cannot answer it.
	text, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	swapped := text[0]
	swapped.ID = console.recipe.ID
	swapped.Version = console.recipe.Version
	// Swapping the catalogue is how the running model becomes a text model
	// without reinstalling anything.
	console.manager.SetRecipes([]recipe.Recipe{swapped}, []recipe.Recipe{swapped})
	status, body := console.generate(t, goodRequest)
	if status != http.StatusConflict || !strings.Contains(body["error"].(string), "is a text model") {
		t.Fatalf("status=%d body=%#v", status, body)
	}
}

func TestGenerateRunsAndServesTheResult(t *testing.T) {
	console := newMediaConsole(t)
	status, accepted := console.generate(t, goodRequest)
	if status != http.StatusAccepted {
		t.Fatalf("status=%d body=%#v", status, accepted)
	}
	id := accepted["generation_id"].(string)
	// A seed the caller did not choose is generated and reported, so the clip
	// can be made again.
	seed, ok := accepted["seed"].(float64)
	if !ok || seed <= 0 {
		t.Fatalf("no seed was recorded: %#v", accepted)
	}
	if accepted["frames"] != float64(22) {
		t.Fatalf("frames=%#v, want the recipe's own grid", accepted["frames"])
	}
	if accepted["width"] != float64(1152) || accepted["height"] != float64(768) {
		t.Fatalf("accepted canvas=%#vx%#v", accepted["width"], accepted["height"])
	}
	view := console.awaitStatus(t, id, "completed", "failed")
	if view["status"] != "completed" {
		t.Fatalf("generation did not complete: %#v", view)
	}
	if view["seed"].(float64) != seed {
		t.Fatalf("the recorded seed changed: %#v", view["seed"])
	}
	if view["short_edge"] != float64(768) || view["width"] != float64(1152) || view["height"] != float64(768) {
		t.Fatalf("recorded canvas=%#vx%#v short_edge=%#v", view["width"], view["height"], view["short_edge"])
	}
	if view["file_url"] != "/api/v1/generations/"+id+"/file" {
		t.Fatalf("file_url=%#v", view["file_url"])
	}
	// The workflow is nowhere in the response.
	encoded, _ := json.Marshal(view)
	if strings.Contains(string(encoded), "class_type") || strings.Contains(string(encoded), "KSampler") || strings.Contains(string(encoded), "output_path") {
		t.Fatalf("a workflow or a host path leaked into the API: %s", encoded)
	}

	file := doRequest(t, http.MethodGet, console.server.URL+"/api/v1/generations/"+id+"/file", "", console.cookies, nil)
	defer file.Body.Close()
	if file.StatusCode != http.StatusOK {
		t.Fatalf("file status=%d", file.StatusCode)
	}
	bytes, _ := io.ReadAll(file.Body)
	if len(bytes) == 0 || !strings.HasPrefix(string(bytes), "clip ") {
		t.Fatalf("file body=%q", bytes)
	}
	if !strings.Contains(file.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("a result must be served as a download: %q", file.Header.Get("Content-Disposition"))
	}

	// Deleting takes the row and the files together.
	removed := doRequest(t, http.MethodDelete, console.server.URL+"/api/v1/generations/"+id, "", console.cookies, console.headers)
	removedBody, _ := io.ReadAll(removed.Body)
	removed.Body.Close()
	if removed.StatusCode != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", removed.StatusCode, removedBody)
	}
	if _, err := os.Stat(filepath.Join(operations.GenerationRoot(console.dataDir, console.recipe.ID), id)); !os.IsNotExist(err) {
		t.Fatalf("the generation's files survived the delete: %v", err)
	}
	if _, err := console.store.Generation(t.Context(), id); err == nil {
		t.Fatal("the generation's row survived the delete")
	}
}

// TestGenerationsQueueInOrder is the concurrency contract: one Spark runs one
// generation at a time, and the rest wait in the order they were asked for
// rather than being refused.
func TestGenerationsQueueInOrder(t *testing.T) {
	console := newMediaConsole(t)
	console.runtime.hold(true)

	first := ""
	rest := make([]string, 0, 2)
	for index, prompt := range []string{"first", "second", "third"} {
		status, accepted := console.generate(t,
			`{"model_id":"media-test-1s","mode":"text_to_video","prompt":"`+prompt+`","blocks":1,"width":1152,"height":768,"seed":`+strconv.Itoa(index+1)+`}`)
		if status != http.StatusAccepted {
			t.Fatalf("%s status=%d body=%#v", prompt, status, accepted)
		}
		if index == 0 {
			first = accepted["generation_id"].(string)
			continue
		}
		rest = append(rest, accepted["generation_id"].(string))
		// The queue position is basement's own, counted from the front.
		if position := accepted["queue_position"]; position != float64(index) {
			t.Fatalf("%s queue_position=%#v, want %d", prompt, position, index)
		}
	}
	console.awaitStatus(t, first, "running")
	for offset, id := range rest {
		view := console.show(t, id)
		if view["status"] != "queued" {
			t.Fatalf("a second generation started while the first was running: %#v", view)
		}
		if view["queue_position"] != float64(offset+1) {
			t.Fatalf("queued generation reports position %#v, want %d", view["queue_position"], offset+1)
		}
	}
	console.runtime.hold(false)
	for _, id := range append([]string{first}, rest...) {
		if view := console.awaitStatus(t, id, "completed", "failed"); view["status"] != "completed" {
			t.Fatalf("generation %s did not complete: %#v", id, view)
		}
	}
	if got := strings.Join(console.runtime.order(), " "); got != "first second third" {
		t.Fatalf("the runtime saw %q; the queue must keep submission order", got)
	}
}

// TestQueuedGenerationCanBeCancelledBeforeItStarts proves a cancel of
// something that has not run never reaches the runtime, and that the queue
// closes up behind it.
func TestQueuedGenerationCanBeCancelledBeforeItStarts(t *testing.T) {
	console := newMediaConsole(t)
	console.runtime.hold(true)
	_, running := console.generate(t, `{"model_id":"media-test-1s","mode":"text_to_video","prompt":"first","blocks":1,"width":1152,"height":768}`)
	_, waiting := console.generate(t, `{"model_id":"media-test-1s","mode":"text_to_video","prompt":"second","blocks":1,"width":1152,"height":768}`)
	runningID, waitingID := running["generation_id"].(string), waiting["generation_id"].(string)
	console.awaitStatus(t, runningID, "running")

	cancelled := doRequest(t, http.MethodPost, console.server.URL+"/api/v1/generations/"+waitingID+"/cancel", "", console.cookies, console.headers)
	cancelled.Body.Close()
	if cancelled.StatusCode != http.StatusOK {
		t.Fatalf("cancel status=%d", cancelled.StatusCode)
	}
	view := console.show(t, waitingID)
	if view["status"] != "cancelled" || !strings.Contains(view["error"].(string), "before it started") {
		t.Fatalf("cancelled view=%#v", view)
	}
	console.runtime.hold(false)
	console.awaitStatus(t, runningID, "completed", "failed")
	if got := strings.Join(console.runtime.order(), " "); got != "first" {
		t.Fatalf("a cancelled generation still reached the runtime: %q", got)
	}
}

// TestFailedGenerationSaysWhy keeps a run that produced nothing from
// disappearing out of the gallery.
func TestFailedGenerationSaysWhy(t *testing.T) {
	console := newMediaConsole(t)
	console.runtime.mu.Lock()
	console.runtime.silent = true
	console.runtime.mu.Unlock()
	_, accepted := console.generate(t, goodRequest)
	id := accepted["generation_id"].(string)
	view := console.awaitStatus(t, id, "failed", "completed")
	if view["status"] != "failed" || !strings.Contains(view["error"].(string), "without writing an output file") {
		t.Fatalf("view=%#v", view)
	}
	// There is no file to serve, and the endpoint says so rather than 404ing
	// on a path.
	response := doRequest(t, http.MethodGet, console.server.URL+"/api/v1/generations/"+id+"/file", "", console.cookies, nil)
	response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("file status=%d", response.StatusCode)
	}
}

func TestGenerationEndpointsRequireASession(t *testing.T) {
	console := newMediaConsole(t)
	for _, call := range []struct {
		method, path, body string
	}{
		{http.MethodPost, "/api/v1/generate", goodRequest},
		{http.MethodGet, "/api/v1/generations", ""},
		{http.MethodGet, "/api/v1/generations/gen_x", ""},
		{http.MethodGet, "/api/v1/generations/gen_x/file", ""},
		{http.MethodPost, "/api/v1/generations/gen_x/cancel", ""},
		{http.MethodDelete, "/api/v1/generations/gen_x", ""},
	} {
		response := doRequest(t, call.method, console.server.URL+call.path, call.body, nil, nil)
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s %s status=%d, want 401", call.method, call.path, response.StatusCode)
		}
	}
}

// TestMediaModelReportsNoTokenTelemetry is the honest-numbers rule at the API
// boundary: a diffusion model produces no tokens, so the console's telemetry
// tiles must see nothing to draw rather than zeros.
func TestMediaModelReportsNoTokenTelemetry(t *testing.T) {
	console := newMediaConsole(t)
	response := doRequest(t, http.MethodGet, console.server.URL+"/api/v1/telemetry", "", console.cookies, nil)
	defer response.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	active, ok := body["active_model"].(map[string]any)
	if !ok {
		t.Fatalf("telemetry did not report the running model: %#v", body)
	}
	if active["runtime_kind"] != "comfyui" {
		t.Fatalf("runtime_kind=%#v", active["runtime_kind"])
	}
	if _, present := active["runtime_metrics"]; present {
		t.Fatalf("a media model must publish no token metrics: %#v", active)
	}
	if names := runtimeMetricNames("comfyui"); names != nil {
		t.Fatalf("comfyui must map no Prometheus series, got %#v", names)
	}
}

// TestMediaModelIsNotReachableThroughTheTextEndpoint keeps /v1 meaning what
// it has always meant. A media runtime speaks nothing OpenAI-compatible, and
// proxying to it would put its own port behind an authenticated route it was
// never meant to answer on.
func TestMediaModelIsNotReachableThroughTheTextEndpoint(t *testing.T) {
	console := newMediaConsole(t)
	response := doRequest(t, http.MethodPost, console.server.URL+"/v1/chat/completions",
		`{"model":"Comfy-Org/Media-Test","messages":[]}`, console.cookies, console.headers)
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", response.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "does not answer on this endpoint") {
		t.Fatalf("body=%s", raw)
	}
	// The runtime never saw the request.
	if len(console.runtime.order()) != 0 {
		t.Fatalf("a text request reached the media runtime: %v", console.runtime.order())
	}
}

// TestMediaModelCannotBeBenchmarked is the same rule for the speed
// measurement: no tokens, no tokens-per-second figure.
func TestMediaModelCannotBeBenchmarked(t *testing.T) {
	console := newMediaConsole(t)
	headers := map[string]string{}
	for name, value := range console.headers {
		headers[name] = value
	}
	headers["Idempotency-Key"] = "bench-media"
	response := doRequest(t, http.MethodPost, console.server.URL+"/api/v1/models/"+console.recipe.ID+"/benchmark", `{}`, console.cookies, headers)
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusConflict || !strings.Contains(string(raw), "no tokens-per-second figure") {
		t.Fatalf("status=%d body=%s", response.StatusCode, raw)
	}
}
