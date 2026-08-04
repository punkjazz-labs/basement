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
	"github.com/punkjazz-labs/basement/internal/recipe"
	"github.com/punkjazz-labs/basement/internal/store"
)

// fakeRuntime stands in for the model container on its loopback port. It
// records what the proxy actually forwarded, which is the only way to prove
// the runtime is asked for a model id it knows rather than a role name.
type fakeRuntime struct {
	mu       sync.Mutex
	bodies   []string
	lengths  []int64
	response string
	// arrived is closed when the first request reaches the runtime, and hold
	// keeps that request from being answered until a test releases it. Both
	// nil means the runtime answers immediately.
	arrived  chan struct{}
	holdOnce sync.Once
	hold     chan struct{}
}

func (f *fakeRuntime) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.bodies = append(f.bodies, string(body))
	f.lengths = append(f.lengths, r.ContentLength)
	f.mu.Unlock()
	if f.hold != nil {
		f.holdOnce.Do(func() {
			close(f.arrived)
			<-f.hold
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": "chatcmpl-test", "note": f.response})
}

func (f *fakeRuntime) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func (f *fakeRuntime) lastBody(t *testing.T) (string, int64) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) == 0 {
		t.Fatal("the runtime was never reached")
	}
	return f.bodies[len(f.bodies)-1], f.lengths[len(f.lengths)-1]
}

// roleFixture is a manager with two installed single-Spark models whose
// service ports both point at one fake runtime, so a proxied request can be
// read back without a container. Hardware detection is the same stub the
// other API tests use (readyInventory), and every operation runs through the
// stub executor, so nothing here touches a real device.
type roleFixture struct {
	url     string
	runtime *fakeRuntime
	store   *store.Store
	server  *Server
	recipes []recipe.Recipe
	cookies []*http.Cookie
	csrf    string
	serving recipe.Recipe
	idle    recipe.Recipe
}

func newRoleFixture(t *testing.T) *roleFixture {
	t.Helper()
	return newRoleFixtureWith(t, nil)
}

// newRoleFixtureWith lets a test arrange the fake runtime before it starts,
// which is how a request can be caught while it is still inside the runtime.
func newRoleFixtureWith(t *testing.T, arrange func(*fakeRuntime)) *roleFixture {
	t.Helper()
	runtime := &fakeRuntime{response: "served"}
	if arrange != nil {
		arrange(runtime)
	}
	upstream := httptest.NewServer(http.HandlerFunc(runtime.handler))
	t.Cleanup(upstream.Close)
	port, err := strconv.Atoi(upstream.URL[strings.LastIndex(upstream.URL, ":")+1:])
	if err != nil {
		t.Fatal(err)
	}
	recipes := []recipe.Recipe{
		{
			ID: "serving-model", Version: 1, DisplayName: "Serving Model",
			Topology: recipe.Topology{SparkCount: 1},
			Runtime:  recipe.Runtime{Kind: "vllm"},
			Service:  recipe.Service{DefaultHostPort: port, ServedModelID: "publisher/serving-model-nvfp4"},
		},
		{
			ID: "idle-model", Version: 1, DisplayName: "Idle Model",
			Topology: recipe.Topology{SparkCount: 1},
			Runtime:  recipe.Runtime{Kind: "vllm"},
			Service:  recipe.Service{DefaultHostPort: port, ServedModelID: "publisher/idle-model-nvfp4"},
		},
	}

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
	executor := &apiExecutor{done: map[string]bool{}, running: true}
	runner := engine.New(database, executor, recipes)
	manager := New("test-version", dataDir, authManager, database, readyInventory{}, executor, runner, recipes)
	server := httptest.NewServer(manager.Handler())
	t.Cleanup(server.Close)

	ctx := t.Context()
	for _, item := range recipes {
		if err := database.SetInstalled(ctx, store.InstalledModel{RecipeID: item.ID, RecipeVersion: item.Version, Status: "stopped", ArtifactPath: "/managed/" + item.ID, ContainerID: "container-" + item.ID}); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.ActivateExclusively(ctx, store.InstalledModel{RecipeID: recipes[0].ID, RecipeVersion: 1, Status: "ready", ArtifactPath: "/managed/serving-model", ContainerID: "container-serving-model"}); err != nil {
		t.Fatal(err)
	}

	tokenBytes, err := os.ReadFile(authManager.PairingTokenPath())
	if err != nil {
		t.Fatal(err)
	}
	paired := doRequest(t, http.MethodPost, server.URL+"/api/v1/auth/pair", `{"token":"`+strings.TrimSpace(string(tokenBytes))+`"}`, nil, map[string]string{"Origin": server.URL})
	var result struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.NewDecoder(paired.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	cookies := paired.Cookies()
	paired.Body.Close()
	if len(cookies) == 0 || result.CSRF == "" {
		t.Fatal("pairing did not issue session and CSRF tokens")
	}
	return &roleFixture{
		url: server.URL, runtime: runtime, store: database, server: manager, recipes: recipes,
		cookies: cookies, csrf: result.CSRF, serving: recipes[0], idle: recipes[1],
	}
}

func (f *roleFixture) mutate(t *testing.T, method, path, body string) *http.Response {
	t.Helper()
	return doRequest(t, method, f.url+path, body, f.cookies, map[string]string{
		"Origin": f.url, "X-CSRF-Token": f.csrf, "Idempotency-Key": "role-test",
	})
}

func (f *roleFixture) infer(t *testing.T, body string) *http.Response {
	t.Helper()
	return doRequest(t, http.MethodPost, f.url+"/v1/chat/completions", body, f.cookies, nil)
}

// A client that sets model: "role/fast" must reach the assigned model, and
// the runtime must be asked for the model id it actually serves: it has never
// heard of a role. Everything else in the body has to arrive untouched.
func TestRoleRequestIsRewrittenToTheServedModelID(t *testing.T) {
	fixture := newRoleFixture(t)
	assigned := fixture.mutate(t, http.MethodPost, "/api/v1/roles", `{"role":"fast","recipe_id":"serving-model"}`)
	if assigned.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(assigned.Body)
		t.Fatalf("assign status=%d body=%s", assigned.StatusCode, data)
	}
	assigned.Body.Close()

	response := fixture.infer(t, `{"model":"role/fast","messages":[{"role":"user","content":"hello"}],"stream":false}`)
	if response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("inference status=%d body=%s", response.StatusCode, data)
	}
	response.Body.Close()

	forwarded, length := fixture.runtime.lastBody(t)
	want := `{"model":"publisher/serving-model-nvfp4","messages":[{"role":"user","content":"hello"}],"stream":false}`
	if forwarded != want {
		t.Fatalf("forwarded body=%s\nwant=%s", forwarded, want)
	}
	if length != int64(len(want)) {
		t.Fatalf("declared length=%d, want %d", length, len(want))
	}
}

// The role prefix is the only thing that changes behaviour. A request naming
// a model id is forwarded exactly as it arrived, byte for byte, as it was
// before roles existed.
func TestConcreteModelRequestsAreForwardedUnchanged(t *testing.T) {
	fixture := newRoleFixture(t)
	body := `{"messages":[{"role":"user","content":"a role/fast mention in the text"}],"model":"publisher/serving-model-nvfp4"}`
	response := fixture.infer(t, body)
	if response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("inference status=%d body=%s", response.StatusCode, data)
	}
	response.Body.Close()
	forwarded, length := fixture.runtime.lastBody(t)
	if forwarded != body {
		t.Fatalf("forwarded body=%s\nwant=%s", forwarded, body)
	}
	if length != int64(len(body)) {
		t.Fatalf("declared length=%d, want %d", length, len(body))
	}
}

// The point of a role: the model behind it does not have to be the one
// serving. The request holds while basement starts it, and is answered by
// that model once it is up.
func TestRoleRequestActivatesTheAssignedModelAndThenServes(t *testing.T) {
	fixture := newRoleFixture(t)
	assigned := fixture.mutate(t, http.MethodPost, "/api/v1/roles", `{"role":"reasoning","recipe_id":"idle-model"}`)
	if assigned.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(assigned.Body)
		t.Fatalf("assign status=%d body=%s", assigned.StatusCode, data)
	}
	assigned.Body.Close()

	response := fixture.infer(t, `{"model":"role/reasoning","messages":[]}`)
	if response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("inference status=%d body=%s", response.StatusCode, data)
	}
	response.Body.Close()

	forwarded, _ := fixture.runtime.lastBody(t)
	if !strings.Contains(forwarded, `"model":"publisher/idle-model-nvfp4"`) {
		t.Fatalf("the request was not served by the assigned model: %s", forwarded)
	}
	model, err := fixture.store.Model(t.Context(), "idle-model")
	if err != nil {
		t.Fatal(err)
	}
	if !model.Active || model.Status != "ready" {
		t.Fatalf("the assigned model was not left serving: %+v", model)
	}
	previous, err := fixture.store.Model(t.Context(), "serving-model")
	if err != nil {
		t.Fatal(err)
	}
	if previous.Active {
		t.Fatalf("two models are marked active: %+v", previous)
	}
}

// Two clients asking for the same role at the same moment must cost one
// switch, not one each, and both must be answered.
func TestConcurrentRequestsForOneRoleShareASingleActivation(t *testing.T) {
	fixture := newRoleFixture(t)
	assigned := fixture.mutate(t, http.MethodPost, "/api/v1/roles", `{"role":"reasoning","recipe_id":"idle-model"}`)
	assigned.Body.Close()

	var wg sync.WaitGroup
	statuses := make([]int, 4)
	for index := range statuses {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			response := fixture.infer(t, `{"model":"role/reasoning","messages":[]}`)
			statuses[index] = response.StatusCode
			response.Body.Close()
		}(index)
	}
	wg.Wait()
	for index, status := range statuses {
		if status != http.StatusOK {
			t.Fatalf("request %d status=%d", index, status)
		}
	}
	jobs, err := fixture.store.ListJobs(t.Context(), 50)
	if err != nil {
		t.Fatal(err)
	}
	starts := 0
	for _, job := range jobs {
		if job.Kind == "start" && job.RecipeID == "idle-model" {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("%d start jobs were created for one role, want 1", starts)
	}
}

// An unassigned role is a configuration question, so the answer says where to
// answer it rather than reading as a runtime fault.
func TestUnassignedRoleNamesTheConsolePage(t *testing.T) {
	fixture := newRoleFixture(t)
	response := fixture.infer(t, `{"model":"role/vision","messages":[]}`)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", response.StatusCode)
	}
	var failure struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&failure); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if failure.Error.Type != "model_not_found" || !strings.Contains(failure.Error.Message, "Roles page") {
		t.Fatalf("unhelpful answer for an unassigned role: %+v", failure.Error)
	}
	if len(fixture.runtime.bodies) != 0 {
		t.Fatal("an unassigned role reached the runtime")
	}
}

// A role may only point at a model this Spark can actually serve, and being
// told so at assignment time is the whole difference between a clear console
// message and a broken client later.
func TestRoleAssignmentRequiresAnInstalledModel(t *testing.T) {
	fixture := newRoleFixture(t)
	refused := fixture.mutate(t, http.MethodPost, "/api/v1/roles", `{"role":"fast","recipe_id":"never-installed"}`)
	if refused.StatusCode != http.StatusConflict {
		t.Fatalf("status=%d, want 409", refused.StatusCode)
	}
	var failure struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(refused.Body).Decode(&failure); err != nil {
		t.Fatal(err)
	}
	refused.Body.Close()
	if !strings.Contains(failure.Error, "not installed") || !strings.Contains(failure.Error, "Models page") {
		t.Fatalf("unhelpful refusal: %q", failure.Error)
	}

	badName := fixture.mutate(t, http.MethodPost, "/api/v1/roles", `{"role":"Fast Model","recipe_id":"serving-model"}`)
	if badName.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad role name status=%d, want 400", badName.StatusCode)
	}
	badName.Body.Close()
}

// Assigning and clearing are mutations: a session alone is not enough, and
// clearing leaves nothing for /v1 to route to.
func TestRoleMutationsAreGatedAndClearable(t *testing.T) {
	fixture := newRoleFixture(t)
	forged := doRequest(t, http.MethodPost, fixture.url+"/api/v1/roles", `{"role":"fast","recipe_id":"serving-model"}`, fixture.cookies, map[string]string{"Origin": fixture.url})
	if forged.StatusCode != http.StatusForbidden {
		t.Fatalf("missing CSRF status=%d, want 403", forged.StatusCode)
	}
	forged.Body.Close()

	assigned := fixture.mutate(t, http.MethodPost, "/api/v1/roles", `{"role":"fast","recipe_id":"serving-model"}`)
	assigned.Body.Close()
	listed := doRequest(t, http.MethodGet, fixture.url+"/api/v1/roles", "", fixture.cookies, nil)
	var roles []store.Role
	if err := json.NewDecoder(listed.Body).Decode(&roles); err != nil {
		t.Fatal(err)
	}
	listed.Body.Close()
	if len(roles) != 1 || roles[0].Name != "fast" || roles[0].RecipeID != "serving-model" {
		t.Fatalf("unexpected role list: %+v", roles)
	}

	cleared := fixture.mutate(t, http.MethodDelete, "/api/v1/roles/fast", "{}")
	if cleared.StatusCode != http.StatusOK {
		t.Fatalf("clear status=%d", cleared.StatusCode)
	}
	cleared.Body.Close()
	again := fixture.mutate(t, http.MethodDelete, "/api/v1/roles/fast", "{}")
	if again.StatusCode != http.StatusNotFound {
		t.Fatalf("clearing an unassigned role status=%d, want 404", again.StatusCode)
	}
	again.Body.Close()

	response := fixture.infer(t, `{"model":"role/fast","messages":[]}`)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("a cleared role still routed: status=%d", response.StatusCode)
	}
	response.Body.Close()
}

// Only the top-level model field decides where a request goes. A "model" key
// inside a message, a tool definition or a nested object is content, not
// addressing, and must never be read as either.
func TestModelFieldSpanReadsOnlyTheTopLevelField(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"first field", `{"model":"role/fast","messages":[]}`, "role/fast"},
		{"after a nested model key", `{"messages":[{"role":"user","model":"decoy"}],"model":"role/vision"}`, "role/vision"},
		{"with whitespace", "{\n  \"model\" : \"role/fast\" ,\n  \"stream\": true\n}", "role/fast"},
		{"escaped value", `{"model":"role/fast"}`, "role/fast"},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			value, start, end, ok := modelFieldSpan([]byte(item.body))
			if !ok || value != item.want {
				t.Fatalf("value=%q ok=%v, want %q", value, ok, item.want)
			}
			var decoded string
			if err := json.Unmarshal([]byte(item.body)[start:end], &decoded); err != nil || decoded != item.want {
				t.Fatalf("span %d:%d is not the value: %q %v", start, end, item.body[start:end], err)
			}
		})
	}
	for _, body := range []string{``, `[]`, `{"messages":[]}`, `{"model":7}`, `{"model":"role/fast"`, `not json`} {
		if _, _, _, ok := modelFieldSpan([]byte(body)); ok && body != `{"model":"role/fast"` {
			t.Errorf("%s was read as carrying a model field", body)
		}
	}
}

// A stuck activation must not hold a client forever without an answer.
func TestRoleActivationTimeoutIsTheAdvertisedTenMinutes(t *testing.T) {
	if roleActivationTimeout != 10*time.Minute {
		t.Fatalf("roleActivationTimeout=%s, but the console tells owners ten minutes", roleActivationTimeout)
	}
}

// The default role is what an app can point at before anything has been set
// up: with no assignment it answers from the model that is serving, exactly
// as a request naming that model would, rather than refusing.
func TestDefaultRoleFollowsTheServingModelUntilAssigned(t *testing.T) {
	fixture := newRoleFixture(t)
	response := fixture.infer(t, `{"model":"role/`+defaultRoleName+`","messages":[]}`)
	if response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("status=%d body=%s", response.StatusCode, data)
	}
	response.Body.Close()
	forwarded, _ := fixture.runtime.lastBody(t)
	if !strings.Contains(forwarded, `"model":"publisher/serving-model-nvfp4"`) {
		t.Fatalf("the default role did not follow the serving model: %s", forwarded)
	}
	// Nothing was started for it: following is not switching.
	jobs, err := fixture.store.ListJobs(t.Context(), 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("following the serving model started a job: %+v", jobs)
	}

	// Assigned, it is a role like any other, including bringing its model up.
	assigned := fixture.mutate(t, http.MethodPost, "/api/v1/roles", `{"role":"`+defaultRoleName+`","recipe_id":"idle-model"}`)
	if assigned.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(assigned.Body)
		t.Fatalf("assign status=%d body=%s", assigned.StatusCode, data)
	}
	assigned.Body.Close()
	response = fixture.infer(t, `{"model":"role/`+defaultRoleName+`","messages":[]}`)
	if response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("assigned default role status=%d body=%s", response.StatusCode, data)
	}
	response.Body.Close()
	forwarded, _ = fixture.runtime.lastBody(t)
	if !strings.Contains(forwarded, `"model":"publisher/idle-model-nvfp4"`) {
		t.Fatalf("the assigned default role did not switch: %s", forwarded)
	}
}

// A request that has been let through to one model must reach that model
// before a switch to another one may begin, or it would be answered by a
// container that is being stopped underneath it.
func TestSwitchWaitsForAnAdmittedRequestToReachItsModel(t *testing.T) {
	fixture := newRoleFixtureWith(t, func(runtime *fakeRuntime) {
		runtime.arrived = make(chan struct{})
		runtime.hold = make(chan struct{})
	})
	for _, body := range []string{
		`{"role":"fast","recipe_id":"serving-model"}`,
		`{"role":"reasoning","recipe_id":"idle-model"}`,
	} {
		assigned := fixture.mutate(t, http.MethodPost, "/api/v1/roles", body)
		if assigned.StatusCode != http.StatusOK {
			data, _ := io.ReadAll(assigned.Body)
			t.Fatalf("assign status=%d body=%s", assigned.StatusCode, data)
		}
		assigned.Body.Close()
	}

	first := make(chan int, 1)
	go func() {
		response := fixture.infer(t, `{"model":"role/fast","messages":[]}`)
		first <- response.StatusCode
		response.Body.Close()
	}()
	select {
	case <-fixture.runtime.arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("the first request never reached the runtime")
	}

	second := make(chan int, 1)
	go func() {
		response := fixture.infer(t, `{"model":"role/reasoning","messages":[]}`)
		second <- response.StatusCode
		response.Body.Close()
	}()
	// While the first request is still inside the runtime, the switch it would
	// be cut off by must not have happened.
	time.Sleep(300 * time.Millisecond)
	model, err := fixture.store.Model(t.Context(), "idle-model")
	if err != nil {
		t.Fatal(err)
	}
	if model.Active {
		t.Fatal("a switch started while a request was still on its way to the model it was let through to")
	}
	close(fixture.runtime.hold)

	if status := <-first; status != http.StatusOK {
		t.Fatalf("first request status=%d", status)
	}
	if status := <-second; status != http.StatusOK {
		t.Fatalf("second request status=%d", status)
	}
	if model, err = fixture.store.Model(t.Context(), "idle-model"); err != nil || !model.Active {
		t.Fatalf("the queued switch never happened: %+v %v", model, err)
	}
	if fixture.runtime.count() != 2 {
		t.Fatalf("the runtime saw %d requests, want 2", fixture.runtime.count())
	}
}

// A switch is a switch whoever asked for it. The console's own Start button
// goes through the same gate as a role, so it cannot stop a model out from
// under a request that has been let through to it and has not arrived yet.
// It must also not deadlock: requests waiting behind the switch are what lets
// the drain finish.
func TestConsoleStartWaitsForAnAdmittedRequestAndDoesNotDeadlock(t *testing.T) {
	fixture := newRoleFixtureWith(t, func(runtime *fakeRuntime) {
		runtime.arrived = make(chan struct{})
		runtime.hold = make(chan struct{})
	})
	assigned := fixture.mutate(t, http.MethodPost, "/api/v1/roles", `{"role":"fast","recipe_id":"serving-model"}`)
	assigned.Body.Close()

	held := make(chan int, 1)
	go func() {
		response := fixture.infer(t, `{"model":"role/fast","messages":[]}`)
		held <- response.StatusCode
		response.Body.Close()
	}()
	select {
	case <-fixture.runtime.arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("the held request never reached the runtime")
	}

	started := fixture.mutate(t, http.MethodPost, "/api/v1/models/idle-model/start", "{}")
	if started.StatusCode != http.StatusAccepted {
		data, _ := io.ReadAll(started.Body)
		t.Fatalf("start status=%d body=%s", started.StatusCode, data)
	}
	var accepted struct {
		Job store.Job `json:"job"`
	}
	if err := json.NewDecoder(started.Body).Decode(&accepted); err != nil {
		t.Fatal(err)
	}
	started.Body.Close()

	// More traffic for the outgoing model arrives while the console's switch
	// waits. It must queue behind the switch rather than keep feeding it work
	// to wait for.
	queued := make(chan int, 1)
	go func() {
		response := fixture.infer(t, `{"model":"role/fast","messages":[]}`)
		queued <- response.StatusCode
		response.Body.Close()
	}()

	time.Sleep(300 * time.Millisecond)
	model, err := fixture.store.Model(t.Context(), "idle-model")
	if err != nil {
		t.Fatal(err)
	}
	if model.Active {
		t.Fatal("the console's start switched models while a request was still on its way to the one serving")
	}
	close(fixture.runtime.hold)

	if status := <-held; status != http.StatusOK {
		t.Fatalf("held request status=%d", status)
	}
	// The console's start finishes, which for a start job means it activated
	// its model, so the switch it was waiting to make did happen.
	waitAPIJob(t, fixture.url, accepted.Job.ID, fixture.cookies, "ready")
	// The request that arrived while the switch was waiting is answered either
	// way: before the switch if it got in first, or after it, by switching its
	// model back. Which of the two happened is a matter of timing and is not
	// asserted; that it is answered at all, rather than starved by the switch
	// or cut off by it, is the point.
	if status := <-queued; status != http.StatusOK {
		t.Fatalf("the request that queued behind the switch was not answered: status=%d", status)
	}
}

// A client that hangs up mid-switch leaves the model coming up. The next
// request must join that job rather than ask for a second start, which would
// have two jobs fighting over one serving slot.
func TestActivationJoinsTheStartJobAlreadyRunning(t *testing.T) {
	fixture := newRoleFixture(t)
	ctx := t.Context()
	existing, created, err := fixture.store.CreateJob(ctx, "start", "idle-model", "someone-elses-click", map[string]any{})
	if err != nil || !created {
		t.Fatalf("CreateJob()=%v %v", created, err)
	}
	if err := fixture.store.UpdateJobState(ctx, existing.ID, "starting", ""); err != nil {
		t.Fatal(err)
	}
	target, ok := recipe.Find(fixture.recipes, "idle-model")
	if !ok {
		t.Fatal("fixture recipe is missing")
	}
	joined, problem := fixture.server.startJobFor(ctx, target)
	if problem != nil {
		t.Fatalf("startJobFor: %+v", problem)
	}
	if joined.ID != existing.ID {
		t.Fatalf("a second start job was created: %s, want %s", joined.ID, existing.ID)
	}
	jobs, err := fixture.store.ListJobs(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	starts := 0
	for _, job := range jobs {
		if job.Kind == "start" {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("%d start jobs exist, want 1", starts)
	}
}

// A model field that cannot be reached is refused in plain language. Sending
// the request on would answer it with whichever model happened to be running,
// which is the silent misrouting this bound exists to prevent.
func TestUnreachableModelFieldIsRefusedRatherThanMisrouted(t *testing.T) {
	filler := strings.Repeat("x", 4096)
	buried := `{"messages":["` + strings.Repeat(filler, 40) + `"],"model":"role/fast"}`
	head, _, model, _, _, unreachable := peekModelFieldLimit(io.NopCloser(strings.NewReader(buried)), 1024)
	if model != "" || !unreachable {
		t.Fatalf("model=%q unreachable=%v, want an unreachable field", model, unreachable)
	}
	if len(head) < 1024 {
		t.Fatalf("only %d bytes were read before giving up", len(head))
	}
	// The same body, with room to reach the field, routes normally.
	_, _, model, _, _, unreachable = peekModelFieldLimit(io.NopCloser(strings.NewReader(buried)), len(buried)+1)
	if model != "role/fast" || unreachable {
		t.Fatalf("model=%q unreachable=%v, want role/fast", model, unreachable)
	}
	// Bodies that are not a JSON object still on its way to a model field are
	// forwarded rather than refused, however large they are. The last of these
	// is the one a first-character test gets wrong: it opens with a brace and
	// is not JSON at all.
	for name, body := range map[string]string{
		"multipart":       "--boundary\r\n" + strings.Repeat("binary", 4096),
		"json array":      "[" + strings.Repeat(`"item",`, 4096) + `"end"]`,
		"complete object": `{"stream":true}` + strings.Repeat(" ", 4096),
		"opaque brace":    "{" + strings.Repeat("\x00\x01binary payload", 4096),
		"malformed":       `{"messages": [tru` + strings.Repeat("e", 4096),
	} {
		_, _, model, _, _, unreachable = peekModelFieldLimit(io.NopCloser(strings.NewReader(body)), 1024)
		if model != "" || unreachable {
			t.Errorf("%s was refused: model=%q unreachable=%v", name, model, unreachable)
		}
	}
}

// The whole-request path for the same rule: a JSON request whose model field
// is out of reach gets one clear answer, and nothing is proxied.
func TestOversizedRequestIsAnsweredNotProxied(t *testing.T) {
	fixture := newRoleFixture(t)
	before := fixture.runtime.count()
	body := `{"messages":["` + strings.Repeat("x", rolePeekLimit) + `"],"model":"role/fast"}`
	response := fixture.infer(t, body)
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d, want 413", response.StatusCode)
	}
	var failure struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&failure); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if failure.Error.Type != "invalid_request_error" || !strings.Contains(failure.Error.Message, "model field") {
		t.Fatalf("unhelpful answer: %+v", failure.Error)
	}
	if fixture.runtime.count() != before {
		t.Fatal("an unroutable request was proxied anyway")
	}
}
