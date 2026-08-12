package operations

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/punkjazz-labs/basement/internal/recipe"
	"golang.org/x/net/websocket"
)

// ComfyUIClient speaks the handful of ComfyUI HTTP routes basement drives. It
// is deliberately small: submit a pinned workflow, ask what happened to it,
// read the queue, interrupt. There is no general proxy here and there is not
// going to be one — ComfyUI's own port is bound to loopback and its web app is
// never exposed, so every route this manager uses is one written down here.
type ComfyUIClient struct {
	baseURL string
	http    *http.Client
}

// NewComfyUIClient dials a running model's loopback port. baseURL carries no
// trailing slash.
func NewComfyUIClient(baseURL string) *ComfyUIClient {
	return &ComfyUIClient{baseURL: strings.TrimSuffix(baseURL, "/"), http: &http.Client{Timeout: 60 * time.Second}}
}

// QueueState is what ComfyUI reports about work in flight. It is also the
// health check: /queue is a documented route that answers with JSON, where /
// serves the web app basement never shows anyone.
type QueueState struct {
	Running int `json:"running"`
	Pending int `json:"pending"`
}

func (c *ComfyUIClient) Queue(ctx context.Context) (QueueState, error) {
	var body struct {
		Running []json.RawMessage `json:"queue_running"`
		Pending []json.RawMessage `json:"queue_pending"`
	}
	if err := c.get(ctx, "/queue", &body); err != nil {
		return QueueState{}, err
	}
	return QueueState{Running: len(body.Running), Pending: len(body.Pending)}, nil
}

// Submit posts a rendered workflow and returns the prompt id ComfyUI tracks
// it under. The graph is the caller's already-rendered document; nothing here
// edits it, and it is never echoed back to any caller.
func (c *ComfyUIClient) Submit(ctx context.Context, graph []byte, clientID string) (string, error) {
	payload, err := json.Marshal(map[string]any{"prompt": json.RawMessage(graph), "client_id": clientID})
	if err != nil {
		return "", fmt.Errorf("encode generation request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/prompt", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("submit generation: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("read generation response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// ComfyUI answers a workflow it cannot run with a structured error
		// naming the node. That text is about a graph basement pinned, not
		// about anything the user typed, so it is carried into the job
		// receipt where a maintainer can read it.
		return "", fmt.Errorf("the media runtime refused the workflow (%s): %s", resp.Status, truncateForError(raw, 2048))
	}
	var accepted struct {
		PromptID string `json:"prompt_id"`
	}
	if err := json.Unmarshal(raw, &accepted); err != nil || accepted.PromptID == "" {
		return "", fmt.Errorf("the media runtime accepted the workflow without naming it; body: %s", truncateForError(raw, 2048))
	}
	return accepted.PromptID, nil
}

// HistoryEntry is one finished or failed generation as ComfyUI records it.
// Present is false while the prompt is still queued or running: ComfyUI moves
// a prompt into its history only once it is done with it either way.
type HistoryEntry struct {
	Present   bool
	Completed bool
	Status    string
	// Error is ComfyUI's own message when the run failed, already trimmed to
	// something a receipt can carry.
	Error string
	// Outputs are the files the workflow wrote, in the order ComfyUI listed
	// them, as paths relative to the output directory.
	Outputs []string
}

func (c *ComfyUIClient) History(ctx context.Context, promptID string) (HistoryEntry, error) {
	if promptID == "" {
		return HistoryEntry{}, errors.New("a prompt id is required")
	}
	var body map[string]historyRecord
	if err := c.get(ctx, "/history/"+promptID, &body); err != nil {
		return HistoryEntry{}, err
	}
	record, present := body[promptID]
	if !present {
		return HistoryEntry{Present: false}, nil
	}
	entry := HistoryEntry{Present: true, Completed: record.Status.Completed, Status: record.Status.StatusStr}
	if !record.Status.Completed {
		entry.Error = record.problem()
	}
	entry.Outputs = record.outputFiles()
	return entry, nil
}

func (c *ComfyUIClient) Interrupt(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/interrupt", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("interrupt generation: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("the media runtime refused to interrupt (%s)", resp.Status)
	}
	return nil
}

func (c *ComfyUIClient) get(ctx context.Context, path string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("the media runtime returned %s for %s", resp.Status, path)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(target)
}

// historyRecord is the shape of one entry in ComfyUI's history. Outputs are
// keyed by node id and every node type names its files differently (images,
// gifs, audio, and whatever a future node calls them), so the files are
// gathered structurally rather than by a list of key names this manager would
// have to keep in step with ComfyUI's node catalogue.
type historyRecord struct {
	Status struct {
		StatusStr string            `json:"status_str"`
		Completed bool              `json:"completed"`
		Messages  []json.RawMessage `json:"messages"`
	} `json:"status"`
	Outputs map[string]map[string]json.RawMessage `json:"outputs"`
}

func (h historyRecord) problem() string {
	if h.Status.StatusStr != "" && h.Status.StatusStr != "success" {
		detail := ""
		for _, message := range h.Status.Messages {
			detail += " " + truncateForError(message, 512)
		}
		return strings.TrimSpace(h.Status.StatusStr + detail)
	}
	return ""
}

// outputFiles collects every saved artefact the run reported, as a path
// relative to the output directory. Only entries ComfyUI marked as living in
// the output tree are taken: a node that reports a temp preview is reporting
// something that is not a result.
func (h historyRecord) outputFiles() []string {
	var files []string
	for _, node := range h.Outputs {
		for _, value := range node {
			var entries []struct {
				Filename  string `json:"filename"`
				Subfolder string `json:"subfolder"`
				Type      string `json:"type"`
			}
			if json.Unmarshal(value, &entries) != nil {
				continue
			}
			for _, entry := range entries {
				if entry.Filename == "" || entry.Type != "output" {
					continue
				}
				files = append(files, filepath.ToSlash(filepath.Join(entry.Subfolder, entry.Filename)))
			}
		}
	}
	// ComfyUI keys outputs by node id in a map, so the order Go walks them in
	// is not the order the workflow wrote them. Sorting makes one run's
	// receipt reproducible instead of shuffling between identical runs.
	sort.Strings(files)
	return files
}

// GenerationOutcome is one completed generation: what ComfyUI called it, what
// it wrote, and how long it took on this machine. Duration is measured here
// rather than reported by the runtime, so the number the gallery shows is
// this Spark's own.
type GenerationOutcome struct {
	PromptID string
	Files    []string
	Bytes    int64
	Duration time.Duration
}

// GenerationPollInterval paces the history poll. A generation on this
// hardware runs for minutes at a minimum, so a slow poll costs nothing and a
// fast one would be hundreds of pointless round trips per clip. Production
// never reassigns it; tests that drive a fake runtime shorten it so a queue
// ordering test does not have to sit out real seconds.
var GenerationPollInterval = 2 * time.Second

// GenerationStallTimeout bounds how long a generation may go with nothing
// observably advancing: no new websocket progress event and no change in the
// runtime's queue. It is deliberately not an elapsed-time deadline. H3's
// measured points cannot bound every canvas and duration the recipe permits,
// and a wrong finite guess would discard hours of legitimate work, but a run
// that advances nothing is not working, it is wedged. The margin is set by
// the slowest silently-running node a legitimate run contains: a QHD VAE
// decode reports no step progress at all while it works, so the bound must
// comfortably hold the longest such gap, not the longest generation.
var GenerationStallTimeout = 30 * time.Minute

// GenerationProgressReconnectInterval paces reconnects after ComfyUI's
// optional progress socket drops. Production never reassigns it; tests make
// it short so proving a reconnect does not have to wait on a real backoff.
var GenerationProgressReconnectInterval = 250 * time.Millisecond

// GenerationStepProgress is the sampler progress ComfyUI reported. Max zero
// means it reported only an executing node, so no percentage is known yet.
// Node is ComfyUI's own node identifier, not a phase name basement invented.
type GenerationStepProgress struct {
	Value int64
	Max   int64
	Node  string
}

// GenerationProgressUpdate is either a history-poll heartbeat or websocket
// enrichment. Queue is present only for a poll and Step only for a websocket
// execution event, so a caller never mistakes an absent reading for zero.
type GenerationProgressUpdate struct {
	Elapsed time.Duration
	Queue   *QueueState
	Step    *GenerationStepProgress
}

// GenerationProgress is called while a generation runs. An error from a poll
// callback still stops the operation as before. An error from a websocket
// callback only stops that enrichment attempt: the socket is never allowed
// to decide whether the generation succeeded.
type GenerationProgress func(update GenerationProgressUpdate) error

type comfyUIProgressSocket interface {
	Receive() ([]byte, error)
	Close() error
}

type netProgressSocket struct{ conn *websocket.Conn }

func (s netProgressSocket) Receive() ([]byte, error) {
	var message []byte
	err := websocket.Message.Receive(s.conn, &message)
	return message, err
}

func (s netProgressSocket) Close() error { return s.conn.Close() }

// Production never reassigns this. Tests replace it with deterministic
// sockets so reconnects and permanent failures need no listener or runtime.
var dialComfyUIProgress = func(ctx context.Context, endpoint, origin string) (comfyUIProgressSocket, error) {
	config, err := websocket.NewConfig(endpoint, origin)
	if err != nil {
		return nil, err
	}
	conn, err := config.DialContext(ctx)
	if err != nil {
		return nil, err
	}
	conn.MaxPayloadBytes = 8 << 20
	return netProgressSocket{conn: conn}, nil
}

var comfyUIClientSequence atomic.Uint64

func nextComfyUIClientID() string {
	return fmt.Sprintf("basement-%d", comfyUIClientSequence.Add(1))
}

func (c *ComfyUIClient) progressEndpoint(clientID string) (string, error) {
	endpoint, err := url.Parse(c.baseURL)
	if err != nil {
		return "", err
	}
	switch endpoint.Scheme {
	case "http":
		endpoint.Scheme = "ws"
	case "https":
		endpoint.Scheme = "wss"
	default:
		return "", fmt.Errorf("the media runtime URL uses unsupported scheme %q", endpoint.Scheme)
	}
	endpoint.Path = strings.TrimSuffix(endpoint.Path, "/") + "/ws"
	query := endpoint.Query()
	query.Set("clientId", clientID)
	endpoint.RawQuery = query.Encode()
	return endpoint.String(), nil
}

// watchProgress reconnects for as long as the history poll is running. A
// malformed event, a binary preview, a refused handshake and a dropped socket
// are all progress failures only: they are ignored or retried, never returned
// to RunGeneration.
func (c *ComfyUIClient) watchProgress(ctx context.Context, clientID, promptID string, report func(GenerationStepProgress) error) {
	endpoint, err := c.progressEndpoint(clientID)
	if err != nil {
		return
	}
	for {
		dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		socket, err := dialComfyUIProgress(dialCtx, endpoint, c.baseURL)
		cancel()
		if err == nil {
			stopClosing := context.AfterFunc(ctx, func() { _ = socket.Close() })
			for {
				message, receiveErr := socket.Receive()
				if receiveErr != nil {
					break
				}
				step, ok := parseComfyUIProgress(message, promptID)
				if ok && report(step) != nil {
					break
				}
			}
			stopClosing()
			_ = socket.Close()
		}
		timer := time.NewTimer(GenerationProgressReconnectInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
	}
}

// parseComfyUIProgress follows ComfyUI's {"type":...,"data":...} websocket
// envelope. execution_start/success/error are deliberately not outcomes here:
// /history remains the only authority for success and failure.
func parseComfyUIProgress(message []byte, promptID string) (GenerationStepProgress, bool) {
	var event struct {
		Type string `json:"type"`
		Data struct {
			PromptID string          `json:"prompt_id"`
			Node     json.RawMessage `json:"node"`
			Value    *int64          `json:"value"`
			Max      *int64          `json:"max"`
		} `json:"data"`
	}
	if json.Unmarshal(message, &event) != nil || event.Data.PromptID != promptID {
		return GenerationStepProgress{}, false
	}
	var node string
	_ = json.Unmarshal(event.Data.Node, &node)
	switch event.Type {
	case "executing":
		if node == "" {
			return GenerationStepProgress{}, false
		}
		return GenerationStepProgress{Node: node}, true
	case "progress":
		if event.Data.Value == nil || event.Data.Max == nil || *event.Data.Max <= 0 || *event.Data.Value < 0 || *event.Data.Value > *event.Data.Max {
			return GenerationStepProgress{}, false
		}
		return GenerationStepProgress{Value: *event.Data.Value, Max: *event.Data.Max, Node: node}, true
	default:
		return GenerationStepProgress{}, false
	}
}

// RunGeneration submits a rendered workflow, waits for it, and moves what the
// run produced into destination. It is the one driver both the install
// verification and the generation API go through, so a generation started by
// either is proved the same way: files on disk, non-empty, inside the
// directory basement owns.
//
// outputRoot is the host side of the container's output directory; ComfyUI
// writes there under names of its own choosing, and this moves the results
// into the per-generation directory afterwards rather than trying to make
// ComfyUI name them. There is deliberately no elapsed-time deadline here;
// the only automatic bound is GenerationStallTimeout, which measures
// advancement rather than duration. The recipe validates what may be
// requested, while explicit cancellation or the caller's context decides
// when work must otherwise stop.
func RunGeneration(ctx context.Context, client *ComfyUIClient, graph []byte, outputRoot, destination string, progress GenerationProgress) (GenerationOutcome, error) {
	started := time.Now()
	clientID := nextComfyUIClientID()
	promptID, err := client.Submit(ctx, graph, clientID)
	if err != nil {
		return GenerationOutcome{}, err
	}
	// lastAdvance holds the elapsed offset of the newest observable
	// advancement: a websocket progress event that differs from the one
	// before it, or a change in the runtime's queue. Submission counts as
	// the first advancement.
	var lastAdvance atomic.Int64
	advance := func() { lastAdvance.Store(int64(time.Since(started))) }
	progressCtx, stopProgress := context.WithCancel(ctx)
	progressDone := make(chan struct{})
	go func() {
		defer close(progressDone)
		// The watcher runs even with no progress callback: a verification
		// run needs its stall clock reset by real step events too.
		var previous GenerationStepProgress
		var seen bool
		client.watchProgress(progressCtx, clientID, promptID, func(step GenerationStepProgress) error {
			if !seen || step != previous {
				previous, seen = step, true
				advance()
			}
			if progress == nil {
				return nil
			}
			return progress(GenerationProgressUpdate{Elapsed: time.Since(started), Step: &step})
		})
	}()
	defer func() {
		stopProgress()
		<-progressDone
	}()
	var previousQueue QueueState
	var queueSeen bool
	for {
		entry, err := client.History(ctx, promptID)
		if err != nil {
			return GenerationOutcome{}, err
		}
		if entry.Present {
			if !entry.Completed {
				detail := entry.Error
				if detail == "" {
					detail = "the media runtime reported no reason"
				}
				return GenerationOutcome{}, fmt.Errorf("the generation did not finish: %s", detail)
			}
			outcome, err := collectGeneration(promptID, entry.Outputs, outputRoot, destination)
			if err != nil {
				return GenerationOutcome{}, err
			}
			outcome.Duration = time.Since(started)
			return outcome, nil
		}
		if queue, queueErr := client.Queue(ctx); queueErr == nil {
			if !queueSeen || queue != previousQueue {
				previousQueue, queueSeen = queue, true
				advance()
			}
			if progress != nil {
				if err := progress(GenerationProgressUpdate{Elapsed: time.Since(started), Queue: &queue}); err != nil {
					return GenerationOutcome{}, err
				}
			}
		}
		if stall := GenerationStallTimeout; stall > 0 && time.Since(started)-time.Duration(lastAdvance.Load()) > stall {
			// The runtime is holding the model's queue without doing
			// anything. Asking it to stop is best effort: a wedge deep
			// enough to ignore the interrupt is settled by restarting the
			// model, and this error says so.
			interruptCtx, cancelInterrupt := context.WithTimeout(ctx, 10*time.Second)
			_ = client.Interrupt(interruptCtx)
			cancelInterrupt()
			return GenerationOutcome{}, fmt.Errorf("nothing advanced for %s, so the generation was stopped as stuck; if this repeats, stop and start the model", stall)
		}
		select {
		case <-ctx.Done():
			return GenerationOutcome{}, ctx.Err()
		case <-time.After(GenerationPollInterval):
		}
	}
}

// collectGeneration moves the run's files out of the shared output directory
// into this generation's own. A run that reported files it did not write, or
// wrote empty ones, is a failure and not a result: the whole point of this
// step is that something playable exists on disk afterwards.
func collectGeneration(promptID string, reported []string, outputRoot, destination string) (GenerationOutcome, error) {
	if len(reported) == 0 {
		return GenerationOutcome{}, errors.New("the generation finished without writing an output file")
	}
	if err := os.MkdirAll(destination, 0o750); err != nil {
		return GenerationOutcome{}, err
	}
	outcome := GenerationOutcome{PromptID: promptID}
	for _, name := range reported {
		// The names come from the runtime, so they are joined the same
		// careful way a downloaded file name is: inside the directory
		// basement owns, or not at all.
		source, err := safeJoin(outputRoot, name)
		if err != nil {
			return GenerationOutcome{}, err
		}
		info, err := os.Stat(source)
		if err != nil || !info.Mode().IsRegular() {
			return GenerationOutcome{}, fmt.Errorf("the generation reported %s but no such file was written", filepath.Base(name))
		}
		if info.Size() == 0 {
			return GenerationOutcome{}, fmt.Errorf("the generation wrote %s as an empty file", filepath.Base(name))
		}
		target := filepath.Join(destination, filepath.Base(name))
		if err := os.Rename(source, target); err != nil {
			return GenerationOutcome{}, fmt.Errorf("move the generated file into place: %w", err)
		}
		outcome.Files = append(outcome.Files, target)
		outcome.Bytes += info.Size()
	}
	return outcome, nil
}

// comfyUIModelPathsFile is where basement writes the model folder map the
// runtime reads. It sits in the container's one writable persistent bind
// because ComfyUI has no command-line flag per model folder: the documented
// way to point it at weights outside its own tree is an extra model paths
// file, and the file has to be somewhere the read-only root filesystem does
// not forbid. It is rewritten every time a container is created, so it can
// never describe a layout the recipe has since moved off.
const comfyUIModelPathsFile = recipe.CacheMountPath + "/comfyui-model-paths.yaml"

// comfyUIUserDirectory is where ComfyUI keeps its own state. The container
// root is read-only and basement never exposes the web app, so this is a
// scratch directory the runtime needs to start rather than anything a user
// sees.
const comfyUIUserDirectory = recipe.CacheMountPath + "/comfyui-user"

// comfyUIEnvironment steers the image's own cache and home directories into
// the one writable persistent mount. The image points TMPDIR and every
// library cache at /root/comfyui-temp and its home at /root/comfyui-user,
// and both sit on the read-only root filesystem basement creates. That is not
// a slow path that degrades: torch._dynamo creates its inductor cache while
// the module is still being imported, so the container exits before ComfyUI
// binds a port at all, and the install fails at the health wait with a stack
// trace instead of a reason.
//
// A tmpfs would also satisfy the writes, but these are compilation caches for
// a twenty gigabyte model on a machine whose unified memory is already the
// scarce resource. Sending them to the persistent cache costs no memory and
// lets a second start reuse what the first one compiled.
//
// TRITON_CACHE_DIR is deliberately absent: every runtime kind already gets
// the same redirect for it, and one rule for one cache is easier to keep true
// than two. A recipe that names any of these itself still wins.
func comfyUIEnvironment() map[string]string {
	return map[string]string{
		"TMPDIR":                  recipe.CacheMountPath + "/comfyui-temp",
		"XDG_CACHE_HOME":          recipe.CacheMountPath + "/comfyui-temp/cache",
		"CUDA_CACHE_PATH":         recipe.CacheMountPath + "/comfyui-temp/cuda-cache",
		"HF_HOME":                 recipe.CacheMountPath + "/comfyui-temp/huggingface",
		"NUMBA_CACHE_DIR":         recipe.CacheMountPath + "/comfyui-temp/numba-cache",
		"TORCH_HOME":              recipe.CacheMountPath + "/comfyui-temp/torch",
		"TORCHINDUCTOR_CACHE_DIR": recipe.CacheMountPath + "/comfyui-temp/torchinductor",
		"HOME":                    comfyUIUserDirectory,
		"XDG_CONFIG_HOME":         comfyUIUserDirectory + "/config",
		"XDG_DATA_HOME":           comfyUIUserDirectory + "/data",
	}
}

// comfyUIModelPaths renders the extra model paths document for r: one entry
// per model folder the primary artifact pins, each pointing at that folder
// inside the read-only artifact mount. The folders come from the pinned file
// names, so the document can never name a directory the download did not
// fetch, and the validator has already held every one of them to ComfyUI's
// own folder vocabulary.
func comfyUIModelPaths(r recipe.Recipe) string {
	index, ok := r.ArtifactIndex("primary")
	if !ok {
		return ""
	}
	mount := artifactMountPath("primary")
	folders := map[string]bool{}
	for _, file := range r.Artifacts[index].Files {
		if folder, _, nested := strings.Cut(file.Name, "/"); nested {
			folders[folder] = true
		}
	}
	names := make([]string, 0, len(folders))
	for folder := range folders {
		names = append(names, folder)
	}
	sort.Strings(names)
	var document strings.Builder
	document.WriteString("basement:\n")
	for _, folder := range names {
		document.WriteString("  " + folder + ": " + mount + "/" + folder + "\n")
	}
	return document.String()
}

// comfyUIArgs builds the ComfyUI command line. Every directory it names is
// one basement mounted or created; nothing here is a path a recipe wrote by
// hand, and there is no flag that would let a recipe reach the network or the
// host filesystem.
func comfyUIArgs(r recipe.Recipe, placement Placement) []string {
	c, ok := r.MediaGeneration()
	if !ok {
		return nil
	}
	serveHost, servePort := serveEndpointArgs(r, placement)
	return []string{
		"--listen", serveHost,
		"--port", fmt.Sprint(servePort),
		"--output-directory", c.OutputDirectory,
		"--input-directory", c.InputDirectory,
		"--temp-directory", recipe.TempMountPath,
		"--user-directory", comfyUIUserDirectory,
		"--extra-model-paths-config", comfyUIModelPathsFile,
		// The web app is never opened and never reachable; a runtime that
		// tries to launch a browser in a container just fails noisily.
		"--disable-auto-launch",
	}
}
