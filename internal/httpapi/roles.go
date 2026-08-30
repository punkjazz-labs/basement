package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/punkjazz-labs/basement/internal/recipe"
	"github.com/punkjazz-labs/basement/internal/store"
)

// Roles name a job rather than a model. A client sets model: "role/fast" once
// and never edits its configuration again; the owner decides in the console
// which installed model answers to that name today. Resolution happens on the
// same stable, authenticated /v1 endpoint ADR 0007 established, so a role
// changes nothing about how a client connects (ADR 0015).
const roleModelPrefix = "role/"

// defaultRoleName is the role an app gets for free. While it has no
// assignment it resolves to whatever model is serving, which is exactly what
// a request naming a concrete model already gets, so an app can point at it
// before any role has been set up and never meet a configuration error.
// Assigned, it behaves like every other role, switching models when it has to.
const defaultRoleName = "standard"

// rolePeekChunk is how much of a request body is read before the first look
// for its model field, doubling from there. rolePeekLimit is where reading
// stops: a JSON request whose model field is further in than this cannot be
// routed by name, and is refused in plain language rather than sent to
// whichever model happens to be serving.
const (
	rolePeekChunk = 64 << 10
	rolePeekStep  = 4 << 20
	rolePeekLimit = 32_000_000
)

// defaultRoleActivationTimeout matches operations' default health-wait budget
// for recipes that do not set runtime.start_timeout_minutes. A role request
// uses the recipe's explicit budget when it has one, so it cannot give up
// while the engine is still within the runtime's own startup deadline.
const defaultRoleActivationTimeout = 20 * time.Minute

func roleActivationTimeout(target recipe.Recipe) time.Duration {
	minutes := target.Runtime.StartTimeoutMinutes
	if minutes <= 0 {
		return defaultRoleActivationTimeout
	}
	return time.Duration(minutes) * time.Minute
}

const roleActivationPoll = 250 * time.Millisecond

// switchDrainTimeout is how long a switch waits for requests already let
// through to another model to reach that model before going ahead anyway. It
// is generous next to a time to first token and short next to the switch it
// delays, because the job asking for it is already committed.
const switchDrainTimeout = 30 * time.Second

// roles is the console's view of which model answers to which name. The
// listing needs a console session like every other read; assigning is a
// mutation and takes the same CSRF gate as the rest. No Idempotency-Key is
// consumed here because an assignment is not a job: sending the same one
// twice leaves exactly the same row, so a retried click is already safe.
//
// Only assignments are listed. The named roles the console always shows
// (standard, fast, reasoning, vision) are console copy rather than rows, so a
// role nobody has assigned yet is an absent name rather than a row pointing
// at nothing.
func (s *Server) roles(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if _, ok := s.auth.Authenticate(r); !ok {
			writeError(w, http.StatusUnauthorized, errors.New("authentication required"))
			return
		}
		list, err := s.store.Roles(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, list)
	case http.MethodPost:
		if err := s.auth.AuthorizeMutation(r); err != nil {
			writeError(w, http.StatusForbidden, err)
			return
		}
		s.assignRole(w, r)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) assignRole(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Role     string `json:"role"`
		RecipeID string `json:"recipe_id"`
	}
	if err := decodeBody(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	// A role that points at something this Spark cannot serve is a promise
	// broken later, at the moment a client's request arrives, which is the
	// worst possible time to learn about it.
	model, err := s.store.Model(r.Context(), request.RecipeID)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusConflict, errors.New("that model is not installed on this Spark, so install it from the Models page before giving it a role"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("basement could not read its own list of installed models: %w", err))
		return
	}
	if _, ok := s.pinnedOrEffective(model.RecipeID, model.RecipeVersion); !ok {
		writeError(w, http.StatusConflict, errors.New("basement no longer has a recipe for that model, so it cannot be given a role"))
		return
	}
	role, err := s.store.AssignRole(r.Context(), request.Role, model.RecipeID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, role)
}

func (s *Server) roleAction(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/api/v1/roles/")
	if name == "" || name == "." || strings.Contains(name, "/") {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodDelete {
		methodNotAllowed(w)
		return
	}
	if err := s.auth.AuthorizeMutation(r); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	if err := s.store.ClearRole(r.Context(), name); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, errors.New("no model is assigned to that role"))
			return
		}
		writeError(w, http.StatusInternalServerError, fmt.Errorf("basement could not update its own role list: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cleared": true})
}

// roleProblem is one refusal, in the OpenAI error shape /v1 clients parse.
type roleProblem struct {
	status  int
	kind    string
	message string
}

// ---- Serving gate ----------------------------------------------------------
//
// This Spark serves one model at a time (ADR 0003), so a request naming a role
// whose model is not the one running has to switch models before it can be
// answered. A switch must never overlap a request that has been let through to
// a different model but has not reached that model yet: stopping that model
// now answers the request with a bad gateway, a truncated stream, or another
// model's output.
//
// Which switch does not matter. A request naming a role, the console's Start
// button and an install that activates what it downloaded all stop one model
// and start another, so all three announce themselves here, through the
// engine's switch guard (engine.SetSwitchGuard), at the moment they begin
// changing what serves and not before. That is why the gate takes no part in
// creating jobs: it guards the switch itself, wherever it was asked for.
//
// A request holds the gate from the moment it is let through until the
// runtime's response headers come back, which is as far as a hold can usefully
// reach: holding for a whole streamed answer would let one long stream block
// every switch, and once the headers are back the request is committed to the
// model that produced them.
type servingGate struct {
	mu sync.Mutex
	// waiters are woken on every state change; each is closed exactly once.
	waiters []chan struct{}
	// switching is set while a switch runs; queued counts the requests
	// waiting to start one. New requests hold off while either is set, so a
	// steady stream of requests for the model that is serving right now can
	// never starve a switch away from it.
	switching bool
	queued    int
	// inflight counts, per recipe, the requests let through and still waiting
	// for their response headers.
	inflight map[string]int
}

func newServingGate() *servingGate { return &servingGate{inflight: map[string]int{}} }

// serveHold is what one request carries between being let through and reaching
// the runtime. release is safe to call more than once, and on a nil hold,
// which is what a request that never took one has.
type serveHold struct {
	gate     *servingGate
	recipeID string
	once     sync.Once
}

func (h *serveHold) release() {
	if h == nil {
		return
	}
	h.once.Do(func() { h.gate.done(h.recipeID) })
}

// waiter and wake are the gate's condition variable. sync.Cond cannot be
// waited on with a context, and every wait here is bounded by the request that
// is waiting, so waiters are plain channels closed on each state change.
func (g *servingGate) waiter() chan struct{} {
	signal := make(chan struct{})
	g.waiters = append(g.waiters, signal)
	return signal
}

func (g *servingGate) wake() {
	for _, signal := range g.waiters {
		close(signal)
	}
	g.waiters = nil
}

func (g *servingGate) busy() bool { return g.switching || g.queued > 0 }

func (g *servingGate) othersInflight(recipeID string) int {
	total := 0
	for id, count := range g.inflight {
		if id != recipeID {
			total += count
		}
	}
	return total
}

// tryAdmit lets a request through to whichever model resolve names, without
// waiting: a switch in progress means there is nothing serving to be let
// through to, which is the same answer this endpoint gave before roles
// existed. Refusing while a switch is merely queued is what keeps a busy
// endpoint from starving that switch forever.
func (g *servingGate) tryAdmit(resolve func() (recipe.Recipe, bool)) (recipe.Recipe, *serveHold, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.busy() {
		return recipe.Recipe{}, nil, false
	}
	target, ok := resolve()
	if !ok {
		return recipe.Recipe{}, nil, false
	}
	g.inflight[target.ID]++
	return target, &serveHold{gate: g, recipeID: target.ID}, true
}

// admit waits for the gate to be quiet and then asks resolve, under the lock,
// whether the model this request wants is already serving. Holding the lock
// across the question is what makes the answer usable: no switch can be
// running at that moment, so the model cannot stop serving between the answer
// and the admission. A false answer means the caller has to bring the model
// up itself.
func (g *servingGate) admit(ctx context.Context, recipeID string, resolve func() bool) (*serveHold, bool, error) {
	g.mu.Lock()
	for g.busy() {
		signal := g.waiter()
		g.mu.Unlock()
		select {
		case <-signal:
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
		g.mu.Lock()
	}
	if !resolve() {
		g.mu.Unlock()
		return nil, false, nil
	}
	g.inflight[recipeID]++
	g.mu.Unlock()
	return &serveHold{gate: g, recipeID: recipeID}, true, nil
}

// startSwitch marks a switch as running and waits, up to the caller's
// deadline, for the requests already let through to other models to reach
// them. Marking it queued first is what stops new requests being let through
// while this waits, so a busy endpoint cannot starve a switch away from the
// model it is busy with.
//
// The wait is bounded and the switch then proceeds either way. The job on the
// other end of this call already holds the engine's runtime lock and is
// committed to switching; a request that was let through and never reached its
// runtime must not be able to hold up what somebody asked for. Exclusion
// between switches is the runtime lock's job, not this flag's: only one job
// holds it, so only one switch is ever announced here at a time.
func (g *servingGate) startSwitch(ctx context.Context, recipeID string) {
	g.mu.Lock()
	g.queued++
	for g.switching || g.othersInflight(recipeID) > 0 {
		signal := g.waiter()
		g.mu.Unlock()
		expired := false
		select {
		case <-signal:
		case <-ctx.Done():
			expired = true
		}
		g.mu.Lock()
		if expired {
			break
		}
	}
	g.queued--
	g.switching = true
	g.wake()
	g.mu.Unlock()
}

func (g *servingGate) endSwitch() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.switching = false
	g.wake()
}

func (g *servingGate) done(recipeID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.inflight[recipeID] <= 1 {
		delete(g.inflight, recipeID)
	} else {
		g.inflight[recipeID]--
	}
	g.wake()
}

// ---- Request routing -------------------------------------------------------

// roleRoute is what a role name resolved to. follow marks the default role
// while it has no assignment: it serves whatever is already running and never
// starts anything, so its model is resolved at admission rather than pinned
// here.
type roleRoute struct {
	target recipe.Recipe
	follow bool
}

// inferenceTarget decides which installed model answers one /v1 request,
// holds the request until that model is serving, and leaves r.Body ready to
// forward. The returned hold must be released once the runtime has answered.
//
// A request naming a concrete model is untouched, exactly as before roles
// existed: the body is put back byte for byte and the serving model answers
// it. A request naming role/<name> is resolved to the assigned model, that
// model is brought up if it is not the one serving, and the model field is
// rewritten to the real served model id so the runtime sees a name it knows.
// Only the request is ever rewritten; the response is proxied untouched.
func (s *Server) inferenceTarget(w http.ResponseWriter, r *http.Request) (recipe.Recipe, *serveHold, bool) {
	head, rest, model, start, end, unreachable := peekModelField(r)
	if unreachable {
		restoreBody(r, head, rest)
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "invalid_request_error",
			fmt.Sprintf("basement reads the first %d MB of a request to find its model field, and this request does not name a model within that. Put the model field near the start of the body.", rolePeekLimit/1_000_000))
		return recipe.Recipe{}, nil, false
	}
	name, isRole := strings.CutPrefix(model, roleModelPrefix)
	if !isRole {
		restoreBody(r, head, rest)
		target, hold, ok := s.admitToServingModel()
		if !ok {
			writeOpenAIError(w, http.StatusServiceUnavailable, "model_not_ready", "no model is active and ready; start one from the basement console")
			return recipe.Recipe{}, nil, false
		}
		return target, hold, true
	}
	route, problem := s.roleRoute(r.Context(), name)
	var hold *serveHold
	if problem == nil {
		if route.follow {
			var ok bool
			route.target, hold, ok = s.admitToServingModel()
			if !ok {
				problem = &roleProblem{http.StatusServiceUnavailable, "model_not_ready",
					fmt.Sprintf("no model is active and ready, and %s%s follows whatever is serving. Start a model from the basement console, or assign one to %s%s on the Roles page.", roleModelPrefix, defaultRoleName, roleModelPrefix, defaultRoleName)}
			}
		} else {
			hold, problem = s.holdForServing(r.Context(), route.target)
		}
	}
	if problem != nil {
		restoreBody(r, head, rest)
		writeOpenAIError(w, problem.status, problem.kind, problem.message)
		return recipe.Recipe{}, nil, false
	}
	replaceModelField(r, head, rest, start, end, route.target.Service.ServedModelID)
	return route.target, hold, true
}

// admitToServingModel lets a request through to whatever model is serving
// right now, which is what a request naming a concrete model has always been
// answered by.
func (s *Server) admitToServingModel() (recipe.Recipe, *serveHold, bool) {
	// The store read runs under the gate's lock and takes its own background
	// context: a request that has gone away is answered by the write failing
	// later, never by leaving the gate held on a dead context.
	return s.gate.tryAdmit(func() (recipe.Recipe, bool) { return s.activeReadyRecipe(context.Background()) })
}

// roleRoute resolves a role name to the exact installed recipe version that
// must answer for it, never the catalog's current entry for that ID, for the
// same reason activeReadyRecipe does not: the running container is the one
// that was installed.
func (s *Server) roleRoute(ctx context.Context, name string) (roleRoute, *roleProblem) {
	clean, err := store.NormalizeRoleName(name)
	if err != nil {
		return roleRoute{}, &roleProblem{http.StatusNotFound, "model_not_found",
			"that is not a role name basement can read. A role is written as role/fast, using lowercase letters, numbers and dashes."}
	}
	role, err := s.store.Role(ctx, clean)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if clean == defaultRoleName {
			return roleRoute{follow: true}, nil
		}
		return roleRoute{}, &roleProblem{http.StatusNotFound, "model_not_found",
			fmt.Sprintf("no model is assigned to %s%s yet. Open the Roles page in the basement console and assign one.", roleModelPrefix, clean)}
	case err != nil:
		return roleRoute{}, &roleProblem{http.StatusInternalServerError, "server_error",
			"basement could not read its own role list: " + err.Error()}
	}
	model, err := s.store.Model(ctx, role.RecipeID)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return roleRoute{}, &roleProblem{http.StatusServiceUnavailable, "model_not_found",
			fmt.Sprintf("the model assigned to %s%s is not installed on this Spark. Open the Roles page in the basement console and assign a model that is installed.", roleModelPrefix, clean)}
	case err != nil:
		return roleRoute{}, &roleProblem{http.StatusInternalServerError, "server_error",
			"basement could not read its own list of installed models: " + err.Error()}
	}
	target, ok := s.pinnedOrEffective(model.RecipeID, model.RecipeVersion)
	if !ok {
		return roleRoute{}, &roleProblem{http.StatusServiceUnavailable, "model_not_found",
			fmt.Sprintf("basement no longer has a recipe for the model assigned to %s%s. Open the Roles page in the basement console and assign another model.", roleModelPrefix, clean)}
	}
	return roleRoute{target: target}, nil
}

// holdSwitch is what the engine's switch guard is bound to: it is called by
// whichever job is about to change what this machine serves, and the function
// it returns is called when that job is done changing it.
func (s *Server) holdSwitch(ctx context.Context, recipeID string) func() {
	drainCtx, cancel := context.WithTimeout(ctx, switchDrainTimeout)
	defer cancel()
	s.gate.startSwitch(drainCtx, recipeID)
	return s.gate.endSwitch
}

// holdForServing returns once target is the model this Spark is serving and
// this request has been let through to it, starting the model first when it is
// not already running.
//
// Admission is asked for again after the model comes up rather than assumed,
// because a switch queued behind this one can take the model away in between.
// That is also why this loops: each turn either gets the request through or
// leaves it waiting on a switch that is really happening, and the recipe's
// startup budget bounds the whole thing.
//
// Two requests for the same role cost one switch, not two: whoever arrives
// second either finds the model serving, or joins the start job already
// running rather than asking for a second one. Two requests for two different
// roles are both served, one after the other, each paying for its own switch.
//
// The switch itself runs in the engine on its own context, so a client that
// hangs up mid-switch leaves the model coming up rather than half started, and
// the next request joins that same job.
func (s *Server) holdForServing(ctx context.Context, target recipe.Recipe) (*serveHold, *roleProblem) {
	waitCtx, cancel := context.WithTimeout(ctx, roleActivationTimeout(target))
	defer cancel()
	for {
		hold, admitted, err := s.gate.admit(waitCtx, target.ID, func() bool { return s.servingNow(waitCtx, target.ID) })
		if err != nil {
			return nil, activationTimedOut(target)
		}
		if admitted {
			return hold, nil
		}
		if problem := s.activate(waitCtx, target); problem != nil {
			return nil, problem
		}
	}
}

func (s *Server) activate(ctx context.Context, target recipe.Recipe) *roleProblem {
	if s.servingNow(ctx, target.ID) {
		return nil
	}
	job, problem := s.startJobFor(ctx, target)
	if problem != nil {
		return problem
	}
	return s.waitForServing(ctx, target, job.ID)
}

// startJobFor joins the start job already bringing this model up when there is
// one, rather than asking for a second. A client that hung up mid-switch, or
// an owner who pressed Start in the console a moment ago, leaves a job running
// that this request wants exactly the same outcome from; the store is what
// says so, so this holds across a manager restart too.
//
// Looking and then creating is one step here, not two: several requests for a
// role can arrive in the same instant, and each would otherwise look before
// any of them had created anything and all of them would create one.
func (s *Server) startJobFor(ctx context.Context, target recipe.Recipe) (store.Job, *roleProblem) {
	s.activationMu.Lock()
	defer s.activationMu.Unlock()
	if job, ok := s.runningStart(ctx, target.ID); ok {
		return job, nil
	}
	job, created, err := s.store.CreateJob(ctx, "start", target.ID, fmt.Sprintf("role-activation-%d", time.Now().UnixNano()), map[string]any{"role_activation": true})
	if err != nil {
		return store.Job{}, &roleProblem{http.StatusServiceUnavailable, "model_activation_failed",
			fmt.Sprintf("basement could not start %s for this request: %s", target.DisplayName, err.Error())}
	}
	if created || job.State == "failed" || job.State == "interrupted" {
		s.engine.Start(job.ID)
	}
	return job, nil
}

// runningStart finds a start job for this model that is still on its way to
// serving. A job interrupted by a manager restart, or one being cancelled, is
// not one to wait on: nothing is driving the first, and the second is on its
// way to stopping.
func (s *Server) runningStart(ctx context.Context, recipeID string) (store.Job, bool) {
	jobs, err := s.store.ListJobs(ctx, 100)
	if err != nil {
		return store.Job{}, false
	}
	for _, job := range jobs {
		if job.Kind != "start" || job.RecipeID != recipeID {
			continue
		}
		if terminal(job.State) || job.State == "interrupted" || job.State == "cancelling" {
			continue
		}
		return job, true
	}
	return store.Job{}, false
}

func (s *Server) waitForServing(ctx context.Context, target recipe.Recipe, jobID string) *roleProblem {
	ticker := time.NewTicker(roleActivationPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return activationTimedOut(target)
		case <-s.closing:
			return activationStopped(target)
		case <-ticker.C:
		}
		job, err := s.store.GetJob(ctx, jobID)
		if err != nil {
			continue
		}
		if !terminal(job.State) {
			continue
		}
		if s.servingNow(ctx, target.ID) {
			return nil
		}
		reason := strings.TrimSpace(job.Error)
		if reason == "" {
			reason = "the model did not come up"
		}
		return &roleProblem{http.StatusServiceUnavailable, "model_activation_failed",
			fmt.Sprintf("basement could not start %s for this request: %s. The Activity page in the basement console has the full job.", target.DisplayName, reason)}
	}
}

func (s *Server) servingNow(ctx context.Context, recipeID string) bool {
	active, ok := s.activeReadyRecipe(ctx)
	return ok && active.ID == recipeID
}

func activationTimedOut(target recipe.Recipe) *roleProblem {
	timeout := roleActivationTimeout(target)
	return &roleProblem{http.StatusGatewayTimeout, "model_activation_timeout",
		fmt.Sprintf("%s did not finish loading within %d minutes, so this request was not answered. The Activity page in the basement console shows how the start is going.", target.DisplayName, int(timeout/time.Minute))}
}

func activationStopped(target recipe.Recipe) *roleProblem {
	return &roleProblem{http.StatusServiceUnavailable, "model_activation_failed",
		fmt.Sprintf("this Spark's manager is restarting, so %s was not started for this request. Try again once it is back.", target.DisplayName)}
}

// ---- Reading and rewriting the model field ---------------------------------

// bodyReader forwards a request body that has already been partly read, and
// closes the original body underneath it.
type bodyReader struct {
	io.Reader
	io.Closer
}

// peekModelField reads a JSON request body until it has found the top-level
// model field, and hands back everything it read so the request can still be
// forwarded byte for byte. start and end are that field's value inside head,
// which is what rewriting it needs.
//
// unreachable is set when reading stopped at rolePeekLimit without finding a
// model field in a body that is a JSON object. Sending such a request on would
// quietly answer it with whichever model happened to be running, so the caller
// refuses it instead. A body that is not JSON at all (a multipart upload, say)
// names no model here by construction and is forwarded as it is.
func peekModelField(r *http.Request) (head []byte, rest io.ReadCloser, model string, start, end int, unreachable bool) {
	if r.Body == nil {
		return nil, http.NoBody, "", 0, 0, false
	}
	return peekModelFieldLimit(r.Body, rolePeekLimit)
}

func peekModelFieldLimit(body io.ReadCloser, limit int) (head []byte, rest io.ReadCloser, model string, start, end int, unreachable bool) {
	rest = body
	// Doubling chunks: a body that names its model first is read once and
	// parsed once, and a body that buries it stays within a handful of parses
	// however large it is. No read ever goes past the limit, so what the limit
	// says is what is read.
	size := rolePeekChunk
	for {
		if remaining := limit - len(head); remaining <= 0 {
			// One byte decides between a body that ended exactly at the limit,
			// which simply names no model, and one that continues past it,
			// whose model field is out of reach.
			probe, _ := io.ReadAll(io.LimitReader(body, 1))
			if len(probe) == 0 {
				return head, rest, "", 0, 0, false
			}
			head = append(head, probe...)
			return head, rest, "", 0, 0, couldNameAModelLater(head)
		} else if size > remaining {
			size = remaining
		}
		chunk, err := io.ReadAll(io.LimitReader(body, int64(size)))
		head = append(head, chunk...)
		if value, from, to, ok := modelFieldSpan(head); ok {
			return head, rest, value, from, to, false
		}
		if err != nil || len(chunk) < size {
			return head, rest, "", 0, 0, false // the whole body holds no model field
		}
		if size < rolePeekStep {
			size *= 2
		}
	}
}

// couldNameAModelLater reports whether the bytes read so far are a JSON object
// that is still open where the reading stopped, which is the only shape whose
// model field could have been just beyond the bound. Refusing on the first
// character alone would refuse an opaque body that merely starts with a brace,
// and that body has no model field to miss: it is forwarded, whatever its
// size, exactly as it was before roles existed.
func couldNameAModelLater(body []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(body))
	open, err := decoder.Token()
	if err != nil {
		return false
	}
	if delimiter, isDelimiter := open.(json.Delim); !isDelimiter || delimiter != '{' {
		return false
	}
	for {
		key, err := decoder.Token()
		if truncated(err) {
			return true
		}
		if err != nil {
			return false // malformed: this is not a request whose model field was missed
		}
		if delimiter, isDelimiter := key.(json.Delim); isDelimiter && delimiter == '}' {
			return false // a whole object, and it named no model
		}
		if _, isString := key.(string); !isString {
			return false
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return truncated(err)
		}
	}
}

func truncated(err error) bool {
	return errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)
}

func restoreBody(r *http.Request, head []byte, rest io.ReadCloser) {
	if len(head) == 0 {
		return
	}
	r.Body = bodyReader{Reader: io.MultiReader(bytes.NewReader(head), rest), Closer: rest}
}

// replaceModelField swaps the model value for the name the runtime actually
// serves. Only the already-read head is rewritten, so the rest of a long body
// is never held in memory to change a few bytes near its front.
func replaceModelField(r *http.Request, head []byte, rest io.ReadCloser, start, end int, replacement string) {
	encoded, err := json.Marshal(replacement)
	if err != nil {
		restoreBody(r, head, rest)
		return
	}
	rewritten := make([]byte, 0, len(head)-(end-start)+len(encoded))
	rewritten = append(rewritten, head[:start]...)
	rewritten = append(rewritten, encoded...)
	rewritten = append(rewritten, head[end:]...)
	// A chunked body carries no declared length to correct.
	if r.ContentLength > 0 {
		r.ContentLength += int64(len(rewritten) - len(head))
	}
	r.Body = bodyReader{Reader: io.MultiReader(bytes.NewReader(rewritten), rest), Closer: rest}
}

// modelFieldSpan finds the top-level model field of a JSON object and returns
// its string value and the exact byte range that value occupies. Only the top
// level is examined, so a "model" key inside a message or a tool definition is
// never mistaken for the one being addressed. Bytes that are not a JSON
// object, or that end before the field is reached, simply have no field.
func modelFieldSpan(body []byte) (value string, start, end int, ok bool) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	open, err := decoder.Token()
	if err != nil {
		return "", 0, 0, false
	}
	if delimiter, isDelimiter := open.(json.Delim); !isDelimiter || delimiter != '{' {
		return "", 0, 0, false
	}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return "", 0, 0, false
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return "", 0, 0, false
		}
		if name, isString := key.(string); !isString || name != "model" {
			continue
		}
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return "", 0, 0, false
		}
		finish := int(decoder.InputOffset())
		return text, finish - len(raw), finish, true
	}
	return "", 0, 0, false
}
