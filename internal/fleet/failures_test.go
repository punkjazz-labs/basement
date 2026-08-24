package fleet

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/punkjazz-labs/basement/internal/recipe"
	"github.com/punkjazz-labs/basement/internal/store"
)

// The console shows these sentences to the person who owns the machines. Each
// case here is one thing that can go wrong on another Spark and the one
// sentence that says it plainly. A node id, an address and an HTTP status
// number are all facts about the protocol, not about the fleet, so none of
// them may reach a row.
func TestNodeFailureSaysWhatIsWrongInPlainWords(t *testing.T) {
	dialFailure := nodeUnreachable{err: &url.Error{
		Op: "Post", URL: "https://192.168.99.31:7071/internal/fleet/v1/deployments/adopt",
		Err: errors.New("dial tcp 192.168.99.31:7071: connect: connection refused"),
	}}
	pinFailure := nodeUnreachable{err: &url.Error{
		Op: "Post", URL: "https://192.168.99.31:7071/internal/fleet/v1/deployments/adopt",
		Err: errServerCertificatePin,
	}}
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{name: "an address that does not answer", err: dialFailure, want: "loft is not answering."},
		{name: "a member with no such endpoint", err: nodeStatus{status: http.StatusNotFound},
			want: "loft runs another manager version, so update the fleet first."},
		{name: "a member that refuses the method", err: nodeStatus{status: http.StatusMethodNotAllowed},
			want: "loft runs another manager version, so update the fleet first."},
		{name: "a member that has not implemented it", err: nodeStatus{status: http.StatusNotImplemented},
			want: "loft runs another manager version, so update the fleet first."},
		{name: "a planner that found the node at another release", err: errors.New(nodeVersionSkew),
			want: "loft runs another manager version, so update the fleet first."},
		{name: "a planner that found another build", err: errors.New(nodeBuildSkew),
			want: "loft runs another manager version, so update the fleet first."},
		{name: "a planner that found another catalogue", err: errors.New(nodeCatalogueSkew),
			want: "loft runs another manager version, so update the fleet first."},
		{name: "a member that refused the placement on its release", err: errors.New(nodeReleaseSkew),
			want: "loft runs another manager version, so update the fleet first."},
		{name: "a reason the member gave itself", err: errors.New("the model is not installed on that node"),
			want: "loft could not do this: the model is not installed on that node"},
		{name: "a status with no reason in it", err: nodeStatus{status: http.StatusInternalServerError},
			want: "loft could not do this: fleet manager returned status 500"},
		{name: "a certificate this controller does not trust", err: pinFailure,
			want: "loft could not do this: fleet server certificate does not match its pin"},
	} {
		got := nodeFailure("loft", test.err)
		if got == nil || got.Error() != test.want {
			t.Fatalf("%s: sentence %v, want %q", test.name, got, test.want)
		}
	}
	if nodeFailure("loft", nil) != nil {
		t.Fatal("a failure was invented where there was none")
	}
}

// The address the manager called is the manager's own business. It belongs in
// the log line the client wrote, and never in a sentence a row shows.
func TestNodeFailureKeepsAddressesOutOfTheConsole(t *testing.T) {
	failure := nodeUnreachable{err: &url.Error{
		Op: "Post", URL: "https://192.168.99.31:7071/internal/fleet/v1/deployments/adopt",
		Err: errServerCertificateValidity,
	}}
	sentence := nodeFailure("loft", failure).Error()
	if strings.Contains(sentence, "192.168.99.31") || strings.Contains(sentence, "internal/fleet") {
		t.Fatalf("the console sentence carries the address: %q", sentence)
	}
	// The text the logs read is the client's own, exactly as it was before the
	// mark was added to it.
	if failure.Error() != `Post "https://192.168.99.31:7071/internal/fleet/v1/deployments/adopt": fleet server certificate is outside its validity period` {
		t.Fatalf("the logged text changed: %q", failure.Error())
	}
}

// The name in the sentence is the name the owner gave that Spark. The
// controller reads it from its own fleet table, so a row never has to show a
// node id, and this manager knows its own name without a table at all.
func TestNodeNameReadsTheFleetTable(t *testing.T) {
	ctx := context.Background()
	recipes, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	controller, _ := newPlacementManager(t, "head", "192.168.99.60", recipes)
	member, _ := newPlacementManager(t, "loft", "192.168.99.61", recipes)
	controller.newClient = inMemoryFleetClients(t, controller, map[string]*Manager{member.identity.CertificateFingerprint: member})
	code, err := member.CreateJoinCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Adopt(ctx, AdoptRequest{
		DisplayName: member.displayName, ConsoleURL: member.consoleURL, NodeURL: member.nodeURL, JoinCode: code.Code,
	}); err != nil {
		t.Fatal(err)
	}
	if name := controller.nodeName(ctx, member.identity.NodeID); name != "loft" {
		t.Fatalf("a member is called %q, want loft", name)
	}
	if name := controller.nodeName(ctx, controller.identity.NodeID); name != "head" {
		t.Fatalf("this manager calls itself %q, want head", name)
	}
	// A node this fleet has never held has no name to give, and an id says
	// more than nothing at all.
	if name := controller.nodeName(ctx, "node_missing"); name != "node_missing" {
		t.Fatalf("an unknown node is called %q", name)
	}
}

// The whole path, end to end: a member that says nothing and a member that
// has no such endpoint both reach the console as one sentence naming the
// Spark, with no node id, no address and no status number in it.
func TestAdoptionReportsASilentOrOlderMemberByName(t *testing.T) {
	ctx := context.Background()
	builtin, err := recipe.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	serving := independentRecipes(t, builtin, 1)[0]
	controller, _ := newPlacementManager(t, "head", "192.168.99.70", builtin)
	member, memberStore := newPlacementManager(t, "loft", "192.168.99.71", builtin)
	member.SetIndependentRuntime(&placementRuntime{database: memberStore, allocator: member.Allocator()})
	controller.newClient = inMemoryFleetClients(t, controller, map[string]*Manager{member.identity.CertificateFingerprint: member})
	code, err := member.CreateJoinCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Adopt(ctx, AdoptRequest{
		DisplayName: member.displayName, ConsoleURL: member.consoleURL, NodeURL: member.nodeURL, JoinCode: code.Code,
	}); err != nil {
		t.Fatal(err)
	}
	if err := memberStore.SetInstalled(ctx, store.InstalledModel{
		RecipeID: serving.ID, RecipeVersion: serving.Version, Status: "ready", Active: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := controller.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}

	// The Spark stopped answering between the last heartbeat and this click.
	controller.newClient = func(string) *http.Client {
		return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("connect: connection refused")
		})}
	}
	_, _, err = controller.AdoptIndependentDeployment(ctx, member.identity.NodeID, serving.ID, "adopt-silent")
	if err == nil || err.Error() != "loft is not answering." {
		t.Fatalf("a silent member reads as %v", err)
	}

	// The Spark answers, but it runs a manager that has never heard of
	// adoption.
	controller.newClient = func(string) *http.Client {
		return &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
		})}
	}
	_, _, err = controller.AdoptIndependentDeployment(ctx, member.identity.NodeID, serving.ID, "adopt-old")
	if err == nil || err.Error() != "loft runs another manager version, so update the fleet first." {
		t.Fatalf("an older member reads as %v", err)
	}
	for _, sentence := range []string{member.identity.NodeID, "192.168.99.71", "404"} {
		if strings.Contains(err.Error(), sentence) {
			t.Fatalf("the console sentence carries %q: %q", sentence, err.Error())
		}
	}
}
