package attached

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/punkjazz-labs/basement/internal/redact"
)

const (
	StatusUnknown   = "unknown"
	StatusHealthy   = "healthy"
	StatusUnhealthy = "unhealthy"

	// ReloadInterval bounds how long an edited file waits to be read again
	// when the edit did not change the directory itself. Adding, removing or
	// renaming a file is noticed within a second.
	ReloadInterval = 60 * time.Second
	ProbeTimeout   = 5 * time.Second
	tick           = time.Second
)

// Health is what the last check of one service found. It lives in memory
// only: a restarted manager says unknown until it has checked again, which
// is the truth.
type Health struct {
	Status        string `json:"status"`
	LastCheckedAt string `json:"last_checked_at,omitempty"`
	LatencyMS     int64  `json:"latency_ms"`
	LastError     string `json:"last_error,omitempty"`
}

// Entry is one valid declaration and its live health.
type Entry struct {
	File        string  `json:"file"`
	Declaration Service `json:"declaration"`
	Health      Health  `json:"health"`
}

// Problem is a file in the directory that could not be used, and why. It
// keeps no Spark off limits: a declaration that cannot be read cannot say
// which Sparks it means.
type Problem struct {
	File  string `json:"file"`
	Error string `json:"error"`
}

type Snapshot struct {
	Services []Entry   `json:"services"`
	Problems []Problem `json:"errors"`
}

type declared struct {
	file    string
	service Service
}

type probeState struct {
	// url is the health URL this state measured. A declaration whose URL
	// changed starts again from unknown rather than inherit an answer that
	// came from somewhere else.
	url     string
	health  Health
	due     time.Time
	running bool
}

// Registry holds the declarations read from one directory and the health of
// each. Production builds one per manager; it is safe for concurrent use.
type Registry struct {
	dir    string
	logger *slog.Logger
	client *http.Client
	now    func() time.Time
	// timeout bounds one check. Production keeps ProbeTimeout; tests shorten
	// it so a check that gets no answer does not cost them five seconds.
	timeout time.Duration

	mu       sync.Mutex
	services []declared
	problems []Problem
	probes   map[string]*probeState
	// logged keeps the problem last written to the log per file, so a file
	// that stays broken is reported once rather than every minute.
	logged     map[string]string
	dirSeen    bool
	dirModTime time.Time
	loadedAt   time.Time
}

func NewRegistry(dataDir string, logger *slog.Logger) *Registry {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// The check goes to the health URL and nowhere else: no proxy from the
	// environment, no redirect, and no idle connection held open against a
	// service basement does not own.
	transport.Proxy = nil
	transport.DisableKeepAlives = true
	return &Registry{
		dir:    filepath.Join(dataDir, DirectoryName),
		logger: logger,
		client: &http.Client{
			Transport:     transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		now:     time.Now,
		timeout: ProbeTimeout,
		probes:  map[string]*probeState{},
		logged:  map[string]string{},
	}
}

func (r *Registry) Directory() string { return r.dir }

// Reload reads the directory again. A missing directory means the feature is
// off and is not a problem; a file that cannot be used is reported and
// skipped, and never stops the manager.
func (r *Registry) Reload() {
	services, problems, seen, modTime := r.read()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dirSeen, r.dirModTime, r.loadedAt = seen, modTime, r.now()
	before := serviceIDs(r.services)
	r.services, r.problems = services, problems
	now := r.now()
	present := map[string]bool{}
	for _, item := range services {
		present[item.service.ID] = true
		state, ok := r.probes[item.service.ID]
		if !ok || state.url != item.service.HealthURL {
			r.probes[item.service.ID] = &probeState{url: item.service.HealthURL, health: Health{Status: StatusUnknown}, due: now}
		}
	}
	for id := range r.probes {
		if !present[id] {
			delete(r.probes, id)
		}
	}
	reported := map[string]bool{}
	for _, problem := range problems {
		reported[problem.File] = true
		if r.logged[problem.File] != problem.Error {
			r.logger.Warn("attached service declaration refused", "file", problem.File, "error", problem.Error)
			r.logged[problem.File] = problem.Error
		}
	}
	for file := range r.logged {
		if !reported[file] {
			delete(r.logged, file)
		}
	}
	if after := serviceIDs(services); after != before {
		r.logger.Info("attached services declared", "directory", r.dir, "services", after)
	}
}

func serviceIDs(services []declared) string {
	ids := make([]string, 0, len(services))
	for _, item := range services {
		ids = append(ids, item.service.ID)
	}
	return strings.Join(ids, ",")
}

func (r *Registry) read() ([]declared, []Problem, bool, time.Time) {
	info, err := os.Stat(r.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, false, time.Time{}
	}
	if err == nil && !info.IsDir() {
		err = errors.New("it is not a directory")
	}
	if err != nil {
		return nil, []Problem{{File: DirectoryName, Error: "the directory could not be read: " + err.Error()}}, true, time.Time{}
	}
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return nil, []Problem{{File: DirectoryName, Error: "the directory could not be read: " + err.Error()}}, true, info.ModTime()
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var services []declared
	var problems []Problem
	owner := map[string]string{}
	for _, entry := range entries {
		name := entry.Name()
		// Editors and package managers leave dotfiles and backups beside the
		// files they write. Only *.yaml is a declaration.
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".yaml") {
			continue
		}
		service, err := readDeclaration(filepath.Join(r.dir, name))
		if err != nil {
			problems = append(problems, Problem{File: name, Error: err.Error()})
			continue
		}
		if first, taken := owner[service.ID]; taken {
			problems = append(problems, Problem{File: name, Error: fmt.Sprintf("id %s is already declared in %s", service.ID, first)})
			continue
		}
		owner[service.ID] = name
		services = append(services, declared{file: name, service: service})
	}
	return services, problems, true, info.ModTime()
}

func readDeclaration(path string) (Service, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Service{}, fmt.Errorf("the file could not be read: %w", err)
	}
	if !info.Mode().IsRegular() {
		return Service{}, errors.New("the entry is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return Service{}, fmt.Errorf("the file could not be read: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxDeclarationBytes+1))
	if err != nil {
		return Service{}, fmt.Errorf("the file could not be read: %w", err)
	}
	return Decode(data)
}

// Snapshot is every declaration with its current health, and every file
// that was refused, in file name order.
func (r *Registry) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	snapshot := Snapshot{Services: make([]Entry, 0, len(r.services)), Problems: append([]Problem{}, r.problems...)}
	for _, item := range r.services {
		health := Health{Status: StatusUnknown}
		if state, ok := r.probes[item.service.ID]; ok {
			health = state.health
		}
		snapshot.Services = append(snapshot.Services, Entry{File: item.file, Declaration: item.service, Health: health})
	}
	return snapshot
}

// Holder names the service that keeps basement's own models off the node
// with this id or display name. It answers from the declarations alone, not
// from health: a service that stopped answering still holds the memory it
// was given until its owner says otherwise by removing the file.
func (r *Registry) Holder(nodeID, displayName string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, item := range r.services {
		if item.service.Names(nodeID, displayName) {
			return item.service.DisplayName, true
		}
	}
	return "", false
}

// Run re-reads the directory when it changes and at least every
// ReloadInterval, and checks each service on its own interval, until ctx
// ends. Reload should be called once before Run so that the declarations are
// in force before the manager admits any work.
func (r *Registry) Run(ctx context.Context) {
	var checks sync.WaitGroup
	defer checks.Wait()
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		if r.reloadDue() {
			r.Reload()
		}
		r.checkDue(ctx, &checks)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *Registry) reloadDue() bool {
	info, err := os.Stat(r.dir)
	seen := err == nil
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.loadedAt.IsZero() || r.now().Sub(r.loadedAt) >= ReloadInterval || seen != r.dirSeen {
		return true
	}
	return seen && !info.ModTime().Equal(r.dirModTime)
}

func (r *Registry) checkDue(ctx context.Context, checks *sync.WaitGroup) {
	type due struct{ id, url string }
	var work []due
	r.mu.Lock()
	now := r.now()
	for _, item := range r.services {
		state := r.probes[item.service.ID]
		if state == nil || state.running || now.Before(state.due) {
			continue
		}
		state.running = true
		state.due = now.Add(item.service.HealthInterval())
		work = append(work, due{id: item.service.ID, url: item.service.HealthURL})
	}
	r.mu.Unlock()
	for _, item := range work {
		checks.Add(1)
		go func() {
			defer checks.Done()
			r.record(ctx, item.id, item.url, r.check(ctx, item.url))
		}()
	}
}

// CheckNow checks every declared service once, in turn, and waits for the
// answers. Run is the production path; this one exists so a caller can
// state health deterministically.
func (r *Registry) CheckNow(ctx context.Context) {
	r.mu.Lock()
	targets := make([]declared, len(r.services))
	copy(targets, r.services)
	r.mu.Unlock()
	for _, item := range targets {
		r.record(ctx, item.service.ID, item.service.HealthURL, r.check(ctx, item.service.HealthURL))
	}
}

// record keeps a check's answer, unless the manager stopped while the check
// was in flight: a check cut short by shutdown measured nothing about the
// service, and must not be logged as if it had.
func (r *Registry) record(ctx context.Context, id, healthURL string, health Health) {
	r.mu.Lock()
	defer r.mu.Unlock()
	state, ok := r.probes[id]
	if !ok || state.url != healthURL {
		return
	}
	state.running = false
	if ctx.Err() != nil {
		return
	}
	previous := state.health.Status
	state.health = health
	if previous == health.Status {
		return
	}
	if health.Status == StatusHealthy {
		r.logger.Info("attached service healthy", "service", id)
	} else {
		r.logger.Warn("attached service unhealthy", "service", id, "error", health.LastError)
	}
}

// check is the only request basement ever sends an attached service: one
// GET of its health URL, bounded by ProbeTimeout, where HTTP 200 is healthy
// and every other answer, or none, is unhealthy.
func (r *Registry) check(ctx context.Context, healthURL string) Health {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	started := time.Now()
	health := Health{Status: StatusUnhealthy, LastCheckedAt: r.now().UTC().Format(time.RFC3339Nano)}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		health.LastError = redact.String(err.Error())
		return health
	}
	response, err := r.client.Do(request)
	health.LatencyMS = time.Since(started).Milliseconds()
	if err != nil {
		health.LastError = redact.String(checkFailure(ctx, err, r.timeout))
		return health
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	if response.StatusCode != http.StatusOK {
		health.LastError = fmt.Sprintf("the health check answered HTTP %d", response.StatusCode)
		return health
	}
	health.Status = StatusHealthy
	return health
}

func checkFailure(ctx context.Context, err error, timeout time.Duration) string {
	var failure *url.Error
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || (errors.As(err, &failure) && failure.Timeout()) {
		return "no answer within " + timeout.String()
	}
	if failure != nil && failure.Err != nil {
		return failure.Err.Error()
	}
	return err.Error()
}
