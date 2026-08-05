package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/punkjazz-labs/basement/internal/operations"
	"github.com/punkjazz-labs/basement/internal/recipe"
	"github.com/punkjazz-labs/basement/internal/store"
)

// Generations are not jobs. A job is an install: planned, rollback-capable,
// receipted, and it changes what this machine has on it. A generation is a
// request against a model that is already running, so it gets a small surface
// of its own rather than borrowing the job engine's transactional machinery
// for something that neither installs nor removes anything.

// MaxPromptLength bounds the prompt a request may carry. It is small enough
// that the field cannot be used to write megabytes into the database one
// request at a time, and large enough for the prompts video models are
// actually written for: a shot-by-shot description with per-subject
// definitions, timings and dialogue runs to several thousand characters, and
// the first limit here was 2000, which cut one in half.
//
// It is exported because the console reads it from the generation config and
// counts against it. A second copy of the number in the browser is a copy
// that can disagree with this one, and the way it disagrees is a prompt the
// field accepts and the server then refuses.
const MaxPromptLength = 8000

// maxSeed is the largest seed a request may name. It is the largest integer
// JSON carries exactly, so a seed recorded in the gallery is the seed that
// reaches the workflow, and a seed copied back out of a response reproduces
// the same clip.
const maxSeed = 1<<53 - 1

// generationQueue holds the requests waiting for the device. ComfyUI's own
// queue would accept them all, but a Spark runs one generation at a time (see
// concurrent_generations, pinned at 1), and a queue basement owns is the one
// that can tell a caller its position and cancel something that has not
// started yet.
type generationQueue struct {
	mu sync.Mutex
	// pending is submission order. First in, first generated: a queue that
	// reordered would make the position basement reports a guess.
	pending []string
	// live carries the running generation's elapsed time and runtime queue,
	// which are only meaningful while the process that measured them runs.
	// ComfyUI's reported step progress is persisted on the generation row.
	live map[string]liveGeneration
	// cancels the running generation. Present for at most one id at a time.
	cancels map[string]context.CancelFunc
	// wake is signalled on every enqueue; buffered by one so a submission
	// never blocks on a worker that is already awake.
	wake chan struct{}
	// subscribers are the open generation SSE streams. Their one-slot signals
	// coalesce bursts of sampler steps; every receiver reads a fresh store
	// snapshot, so a coalesced terminal change cannot be lost.
	subscribers      map[uint64]chan struct{}
	nextSubscriberID uint64
	once             sync.Once
}

// liveGeneration is what basement can honestly say about a generation while
// it runs: how long it has been going, and what the runtime says is in its
// own queue. There is no percentage here. ComfyUI publishes step progress on
// its websocket only, and a percentage derived from anything else would be a
// number basement made up.
type liveGeneration struct {
	Elapsed time.Duration
	Queue   operations.QueueState
}

func newGenerationQueue() *generationQueue {
	return &generationQueue{
		live: map[string]liveGeneration{}, cancels: map[string]context.CancelFunc{}, wake: make(chan struct{}, 1),
		subscribers: map[uint64]chan struct{}{},
	}
}

func (q *generationQueue) enqueue(id string) int {
	q.mu.Lock()
	q.pending = append(q.pending, id)
	position := len(q.pending)
	q.changedLocked()
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return position
}

func (q *generationQueue) next() (string, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) == 0 {
		return "", false
	}
	id := q.pending[0]
	q.pending = q.pending[1:]
	return id, true
}

// position is where a queued generation sits, counting from 1. A generation
// that is running or finished is not in the queue and reports 0.
func (q *generationQueue) position(id string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	for index, pending := range q.pending {
		if pending == id {
			return index + 1
		}
	}
	return 0
}

// withdraw removes a generation that has not started. It reports false for
// one that is running or already gone, so the caller knows it has to
// interrupt the runtime instead.
func (q *generationQueue) withdraw(id string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for index, pending := range q.pending {
		if pending != id {
			continue
		}
		q.pending = append(q.pending[:index], q.pending[index+1:]...)
		q.changedLocked()
		return true
	}
	return false
}

func (q *generationQueue) setLive(id string, live liveGeneration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.live[id] = live
	q.changedLocked()
}

func (q *generationQueue) liveState(id string) (liveGeneration, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	live, ok := q.live[id]
	return live, ok
}

func (q *generationQueue) hold(id string, cancel context.CancelFunc) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.cancels[id] = cancel
}

func (q *generationQueue) release(id string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.cancels, id)
	delete(q.live, id)
	q.changedLocked()
}

func (q *generationQueue) changed() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.changedLocked()
}

func (q *generationQueue) changedLocked() {
	for _, subscriber := range q.subscribers {
		select {
		case subscriber <- struct{}{}:
		default:
		}
	}
}

func (q *generationQueue) subscribe() (<-chan struct{}, func()) {
	q.mu.Lock()
	q.nextSubscriberID++
	id := q.nextSubscriberID
	updates := make(chan struct{}, 1)
	q.subscribers[id] = updates
	q.mu.Unlock()
	return updates, func() {
		q.mu.Lock()
		delete(q.subscribers, id)
		q.mu.Unlock()
	}
}

func (q *generationQueue) subscriberCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.subscribers)
}

// interrupt stops a running generation, and reports whether there was one.
func (q *generationQueue) interrupt(id string) bool {
	q.mu.Lock()
	cancel, running := q.cancels[id]
	q.mu.Unlock()
	if running {
		cancel()
	}
	return running
}

// generateHandler is POST /api/v1/generate: validate against the recipe,
// record the request, and put it in the queue. It answers with the
// generation's id and its place in line, never with the workflow.
func (s *Server) generateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if err := s.auth.AuthorizeMutation(r); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	s.updateAdmissionMu.Lock()
	defer s.updateAdmissionMu.Unlock()
	if s.refuseMutationDuringUpdate(w) {
		return
	}
	var request struct {
		ModelID string `json:"model_id"`
		Mode    string `json:"mode"`
		Prompt  string `json:"prompt"`
		Blocks  int    `json:"blocks"`
		Width   int    `json:"width"`
		Height  int    `json:"height"`
		Seed    *int64 `json:"seed"`
	}
	if err := decodeBody(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	target, config, err := s.mediaTarget(r.Context(), request.ModelID)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	prompt := strings.TrimSpace(request.Prompt)
	if prompt == "" {
		writeError(w, http.StatusBadRequest, errors.New("a prompt is required"))
		return
	}
	// Counted in characters, not bytes, because that is what the message
	// says and what the console counts. A prompt of accented text or curly
	// quotes is longer in bytes than anything the user can see, and a limit
	// that moves with the encoding is one nobody can write to.
	if length := utf8.RuneCountInString(prompt); length > MaxPromptLength {
		writeError(w, http.StatusBadRequest, fmt.Errorf("a prompt is at most %d characters, and this one is %d", MaxPromptLength, length))
		return
	}
	if err := validateMode(request.Mode, config); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if request.Blocks < config.MinBlocks || request.Blocks > config.MaxBlocks {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%s generates between %d and %d blocks of duration, and this request asked for %d",
			target.DisplayName, config.MinBlocks, config.MaxBlocks, request.Blocks))
		return
	}
	shortEdge, err := validateCanvas(request.Width, request.Height, target, config)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	seed, err := resolveSeed(request.Seed)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	record, err := s.store.CreateGeneration(r.Context(), store.Generation{
		RecipeID: target.ID, Mode: request.Mode, Prompt: prompt,
		Blocks: request.Blocks, ShortEdge: shortEdge,
		Width: request.Width, Height: request.Height,
		Frames: config.Frames(request.Blocks), Seed: seed,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	position := s.generations.enqueue(record.ID)
	s.startGenerationWorker()
	writeJSON(w, http.StatusAccepted, map[string]any{
		"generation_id": record.ID, "queue_position": position,
		"seed": seed, "frames": record.Frames, "width": record.Width, "height": record.Height,
		"seconds": config.Seconds(request.Blocks),
	})
}

// mediaTarget resolves the model a request names and refuses, in one plain
// sentence the console can show as it stands, anything that is not a media
// model running right now. All three refusals are conflicts rather than bad
// requests: the request is well formed, the machine is simply not in a state
// to answer it.
func (s *Server) mediaTarget(ctx context.Context, modelID string) (recipe.Recipe, recipe.ComfyUIConfig, error) {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return recipe.Recipe{}, recipe.ComfyUIConfig{}, errors.New("a model id is required")
	}
	active, ok := s.activeReadyRecipe(ctx)
	if !ok {
		return recipe.Recipe{}, recipe.ComfyUIConfig{}, errors.New("no model is running on this Spark, so start the media model from the Models page first")
	}
	if active.ID != modelID {
		return recipe.Recipe{}, recipe.ComfyUIConfig{}, fmt.Errorf("%s is the model running on this Spark right now, so switch to %s from the Models page before generating with it", active.DisplayName, modelID)
	}
	config, media := active.MediaGeneration()
	if !media {
		return recipe.Recipe{}, recipe.ComfyUIConfig{}, fmt.Errorf("%s is a text model, so it cannot generate video or images", active.DisplayName)
	}
	return active, config, nil
}

func validateMode(mode string, config recipe.ComfyUIConfig) error {
	if _, ok := config.Graphs[mode]; !ok {
		modes := make([]string, 0, len(config.Graphs))
		for name := range config.Graphs {
			modes = append(modes, name)
		}
		return fmt.Errorf("mode must be one of: %s", strings.Join(modes, ", "))
	}
	if mode == recipe.ModeImageToVideo {
		// The graph for this mode substitutes a source image file name, and
		// nothing yet puts a file where it could be substituted from. Saying
		// so is better than accepting a request that would fail inside the
		// runtime with a message about a node.
		return errors.New("generating from a source image is not available yet, so use text_to_video")
	}
	return nil
}

// validateCanvas holds a requested size to the model's own canvas. It never
// rounds and never clamps: a request off the grid is answered with the rule it
// broke, because a server that silently changed the size would return a clip
// that is not the one that was asked for.
func validateCanvas(width, height int, target recipe.Recipe, config recipe.ComfyUIConfig) (int, error) {
	if width <= 0 || width%recipe.CanvasMultiple != 0 {
		return 0, fmt.Errorf("width must be a positive multiple of %d, and this request asked for %d", recipe.CanvasMultiple, width)
	}
	if height <= 0 || height%recipe.CanvasMultiple != 0 {
		return 0, fmt.Errorf("height must be a positive multiple of %d, and this request asked for %d", recipe.CanvasMultiple, height)
	}
	shortEdge, longEdge := width, height
	if shortEdge > longEdge {
		shortEdge, longEdge = longEdge, shortEdge
	}
	if shortEdge > config.MaxShortEdge {
		return 0, fmt.Errorf("%s generates a short edge of at most %d pixels, and this request asked for %d", target.DisplayName, config.MaxShortEdge, shortEdge)
	}
	if longEdge > config.MaxLongEdge {
		return 0, fmt.Errorf("%s generates a long edge of at most %d pixels, and this request asked for %d", target.DisplayName, config.MaxLongEdge, longEdge)
	}
	return shortEdge, nil
}

// resolveSeed takes the request's seed or makes one. A seed basement chose is
// recorded exactly like one the user chose, so every result in the gallery
// can be generated again.
func resolveSeed(requested *int64) (int64, error) {
	if requested != nil {
		if *requested < 0 || *requested > maxSeed {
			return 0, fmt.Errorf("seed must be between 0 and %d", int64(maxSeed))
		}
		return *requested, nil
	}
	value, err := rand.Int(rand.Reader, big.NewInt(maxSeed))
	if err != nil {
		return 0, err
	}
	return value.Int64(), nil
}

// startGenerationWorker brings the queue's single worker up the first time
// anything is submitted. It is started lazily rather than at boot because a
// Spark with no media model installed should not carry a goroutine waiting
// for work that can never arrive.
func (s *Server) startGenerationWorker() {
	s.generations.once.Do(func() { go s.serveGenerations() })
}

func (s *Server) serveGenerations() {
	for {
		select {
		case <-s.closing:
			return
		case <-s.generations.wake:
		}
		for {
			id, ok := s.generations.next()
			if !ok {
				break
			}
			select {
			case <-s.closing:
				// The queue row survives; Open marks it interrupted on the
				// next start rather than leaving it reading as running.
				return
			default:
			}
			s.runOneGeneration(id)
		}
	}
}

// runOneGeneration drives a single request all the way through: render the
// pinned workflow, submit it, wait, and record what came back. Every failure
// is written to the row, so a generation that went wrong says so in the
// gallery instead of disappearing.
func (s *Server) runOneGeneration(id string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The shutdown signal reaches a running generation the same way a cancel
	// does; without this a stop would wait out a generation that can take an
	// hour.
	go func() {
		select {
		case <-s.closing:
			cancel()
		case <-ctx.Done():
		}
	}()
	s.generations.hold(id, cancel)
	defer s.generations.release(id)

	record, err := s.store.Generation(ctx, id)
	if err != nil {
		return
	}
	if err := s.store.StartGeneration(ctx, id); err != nil {
		// Not queued any more: cancelled or deleted between submission and
		// this worker picking it up. Nothing to run and nothing to report.
		return
	}
	s.generations.changed()
	fail := func(status, message string) {
		failCtx, failCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer failCancel()
		if s.store.FailGeneration(failCtx, id, status, message) == nil {
			s.generations.changed()
		}
	}
	// The exact installed version, never the catalog's current entry: the
	// running container is the one that was installed, and its port and its
	// workflow both come from that version.
	installed, err := s.store.Model(ctx, record.RecipeID)
	if err != nil {
		fail("failed", "the model this generation was made with is no longer installed on this Spark")
		return
	}
	target, ok := s.pinnedOrEffective(record.RecipeID, installed.RecipeVersion)
	if !ok {
		fail("failed", "basement no longer has a recipe for the model this generation was made with")
		return
	}
	config, media := target.MediaGeneration()
	if !media {
		fail("failed", "the model this generation was made with is not a media model")
		return
	}
	raw, err := recipe.Graph(config.Graphs[record.Mode])
	if err != nil {
		fail("failed", err.Error())
		return
	}
	graph, err := recipe.RenderGraph(raw, recipe.GraphInputs{
		Prompt: record.Prompt, Seed: record.Seed, Frames: record.Frames,
		Width: record.Width, Height: record.Height, Steps: config.SamplerSteps,
	})
	if err != nil {
		fail("failed", err.Error())
		return
	}
	root := operations.GenerationRoot(s.dataDir, target.ID)
	client := operations.NewComfyUIClient(fmt.Sprintf("http://127.0.0.1:%d", target.Service.DefaultHostPort))
	progress := func(update operations.GenerationProgressUpdate) error {
		live, _ := s.generations.liveState(id)
		live.Elapsed = update.Elapsed
		if update.Queue != nil {
			live.Queue = *update.Queue
		}
		if update.Step != nil {
			if err := s.store.UpdateGenerationProgress(ctx, id, update.Step.Value, update.Step.Max, update.Step.Node); err != nil {
				return err
			}
		}
		s.generations.setLive(id, live)
		return nil
	}
	// An allowed generation has no elapsed-time abort. H3's measured points
	// do not bound every canvas and duration combination the recipe permits,
	// so a finite prediction here could discard hours of legitimate work.
	// The user can still cancel explicitly, and server shutdown cancels too.
	outcome, err := operations.RunGeneration(ctx, client, graph, root, filepath.Join(root, id), progress)
	if err != nil {
		if ctx.Err() != nil {
			// A cancelled generation asks the runtime to stop too, on a
			// context of its own: the one this ran under is already dead.
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = client.Interrupt(stopCtx)
			stopCancel()
			fail("cancelled", "this generation was cancelled; no result was saved and any partial work was lost")
			return
		}
		fail("failed", err.Error())
		return
	}
	// One request, one result: the pinned workflows each write a single file,
	// and the first is the one the gallery plays.
	completeCtx, completeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer completeCancel()
	if s.store.CompleteGeneration(completeCtx, id, outcome.Files[0], outcome.Bytes) == nil {
		s.generations.changed()
	}
}

// listGenerations is GET /api/v1/generations: the gallery, newest first.
func (s *Server) listGenerations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	records, err := s.store.Generations(r.Context(), 0)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	views := make([]map[string]any, 0, len(records))
	for _, record := range records {
		views = append(views, s.generationView(record))
	}
	writeJSON(w, http.StatusOK, views)
}

// generationView is one generation as the console reads it. The workflow is
// not in it and never will be: the graph is an implementation detail the user
// never sees, never edits and is never asked about. The output path is not in
// it either — a console that had it could only use it to build a link the
// file endpoint already provides.
func (s *Server) generationView(record store.Generation) map[string]any {
	view := map[string]any{
		"id": record.ID, "model_id": record.RecipeID, "mode": record.Mode, "prompt": record.Prompt,
		"blocks": record.Blocks, "short_edge": record.ShortEdge, "width": record.Width, "height": record.Height,
		"frames": record.Frames, "seed": record.Seed, "status": record.Status,
		"created_at": record.CreatedAt,
	}
	if record.Error != "" {
		view["error"] = record.Error
	}
	if record.StartedAt != "" {
		view["started_at"] = record.StartedAt
	}
	if record.FinishedAt != "" {
		view["finished_at"] = record.FinishedAt
	}
	if record.Bytes > 0 {
		view["bytes"] = record.Bytes
	}
	if record.Status == "completed" {
		view["file_url"] = "/api/v1/generations/" + record.ID + "/file"
	}
	if record.Status == "queued" {
		if position := s.generations.position(record.ID); position > 0 {
			view["queue_position"] = position
		}
	}
	if record.Status == "running" && record.ProgressMax > 0 {
		view["progress_value"] = record.ProgressValue
		view["progress_max"] = record.ProgressMax
	}
	if record.Status == "running" && record.ProgressPhase != "" {
		view["progress_phase"] = record.ProgressPhase
	}
	if live, ok := s.generations.liveState(record.ID); ok {
		view["elapsed_seconds"] = int64(live.Elapsed.Seconds())
		view["runtime_queue_running"] = live.Queue.Running
		view["runtime_queue_pending"] = live.Queue.Pending
	}
	return view
}

// generationEvents is one authenticated stream for the whole Generate view.
// It sends the same newest-first snapshot as the gallery endpoint whenever
// progress or state changes. An empty snapshot is an explicit idle state, so
// a new tab never waits silently for a generation that does not exist.
func (s *Server) generationEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, errors.New("streaming is unavailable"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	updates, unsubscribe := s.generations.subscribe()
	defer unsubscribe()
	last := ""
	writeSnapshot := func() bool {
		records, err := s.store.Generations(r.Context(), 0)
		if err != nil {
			return false
		}
		views := make([]map[string]any, 0, len(records))
		for _, record := range records {
			views = append(views, s.generationView(record))
		}
		body, err := json.Marshal(map[string]any{"generations": views})
		if err != nil {
			return false
		}
		current := string(body)
		if current == last {
			return true
		}
		if _, err := fmt.Fprintf(w, "event: generation\ndata: %s\n\n", body); err != nil {
			return false
		}
		flusher.Flush()
		last = current
		return true
	}
	if !writeSnapshot() {
		return
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.closing:
			return
		case <-updates:
			if !writeSnapshot() {
				return
			}
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// generationAction routes everything under /api/v1/generations/{id}.
func (s *Server) generationAction(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(path.Clean(r.URL.Path), "/api/v1/generations/")
	parts := strings.Split(trimmed, "/")
	if len(parts) < 1 || parts[0] == "" || parts[0] == "." {
		http.NotFound(w, r)
		return
	}
	id := parts[0]
	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		s.showGeneration(w, r, id)
	case len(parts) == 1 && r.Method == http.MethodDelete:
		s.deleteGeneration(w, r, id)
	case len(parts) == 2 && parts[1] == "file" && r.Method == http.MethodGet:
		s.serveGenerationFile(w, r, id)
	case len(parts) == 2 && parts[1] == "cancel" && r.Method == http.MethodPost:
		s.cancelGeneration(w, r, id)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) showGeneration(w http.ResponseWriter, r *http.Request, id string) {
	record, err := s.store.Generation(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, errors.New("no generation with that id"))
		return
	}
	writeJSON(w, http.StatusOK, s.generationView(record))
}

// serveGenerationFile streams a result off local disk. The path comes from
// the store, and it is still checked against the directory basement owns
// before it is opened: a row is data this process wrote, but it is data, and
// serving a file by a path out of a database without checking where it points
// is how a file server becomes a file server for the whole machine.
func (s *Server) serveGenerationFile(w http.ResponseWriter, r *http.Request, id string) {
	record, err := s.store.Generation(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, errors.New("no generation with that id"))
		return
	}
	if record.Status != "completed" || record.OutputPath == "" {
		writeError(w, http.StatusConflict, errors.New("this generation has not produced a file"))
		return
	}
	root := filepath.Join(s.dataDir, "generations")
	target, err := containedPath(root, record.OutputPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("this generation's file is not where basement keeps generations"))
		return
	}
	file, err := os.Open(target)
	if err != nil {
		writeError(w, http.StatusNotFound, errors.New("this generation's file is no longer on disk"))
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		writeError(w, http.StatusNotFound, errors.New("this generation's file is no longer on disk"))
		return
	}
	// Served as a download rather than inline: the console plays it from a
	// blob it fetched itself, and nothing here should be able to render in
	// the page's own origin.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(target)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, filepath.Base(target), info.ModTime(), file)
}

func (s *Server) cancelGeneration(w http.ResponseWriter, r *http.Request, id string) {
	if err := s.auth.AuthorizeMutation(r); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	record, err := s.store.Generation(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, errors.New("no generation with that id"))
		return
	}
	switch record.Status {
	case "queued":
		if !s.generations.withdraw(id) {
			// It left the queue between the read and now, which means a
			// worker has it; interrupting is the right answer either way.
			s.generations.interrupt(id)
			writeJSON(w, http.StatusOK, map[string]any{"cancelled": true})
			return
		}
		if err := s.store.FailGeneration(r.Context(), id, "cancelled", "this generation was cancelled before it started"); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		s.generations.changed()
	case "running":
		if !s.generations.interrupt(id) {
			writeError(w, http.StatusConflict, errors.New("this generation is not running on this manager, so there is nothing to cancel"))
			return
		}
	default:
		writeError(w, http.StatusConflict, errors.New("this generation has already finished"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cancelled": true})
}

// deleteGeneration removes the row and the files together, so the gallery and
// the disk never disagree. A generation that is still running is refused
// rather than pulled out from under the worker holding it.
func (s *Server) deleteGeneration(w http.ResponseWriter, r *http.Request, id string) {
	if err := s.auth.AuthorizeMutation(r); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	record, err := s.store.Generation(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, errors.New("no generation with that id"))
		return
	}
	if record.Status == "running" {
		writeError(w, http.StatusConflict, errors.New("this generation is still running, so cancel it first"))
		return
	}
	if s.generations.withdraw(id) {
		_ = s.store.FailGeneration(r.Context(), id, "cancelled", "this generation was deleted before it started")
	}
	root := filepath.Join(s.dataDir, "generations")
	// The directory is named by the generation id, which basement generated;
	// it is still resolved against the root it must be inside, so a delete
	// can never reach outside the generations tree.
	target, err := containedPath(root, filepath.Join(operations.GenerationRoot(s.dataDir, record.RecipeID), id))
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("this generation's files are not where basement keeps generations"))
		return
	}
	reclaimed := dirBytes(target)
	if err := os.RemoveAll(target); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.store.DeleteGeneration(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.generations.changed()
	writeJSON(w, http.StatusOK, map[string]any{"removed": true, "reclaimed_bytes": reclaimed})
}

// containedPath resolves candidate and confirms it sits inside root. Both are
// cleaned and made absolute first, so /data/generations-elsewhere is not
// inside /data/generations.
func containedPath(root, candidate string) (string, error) {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	absoluteCandidate, err := filepath.Abs(candidate)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(absoluteRoot, absoluteCandidate)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", errors.New("path escapes the managed root")
	}
	return absoluteCandidate, nil
}
