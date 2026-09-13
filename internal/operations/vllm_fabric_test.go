package operations

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/punkjazz-labs/basement/internal/recipe"
)

func mustContainerEnvironment(t *testing.T, r recipe.Recipe, placement Placement) []string {
	t.Helper()
	environment, err := containerEnvironment(r, placement)
	if err != nil {
		t.Fatal(err)
	}
	return environment
}

func TestDistributedVLLMAdvertisesEachRanksOwnFabricAddress(t *testing.T) {
	for _, role := range []string{RoleHead, RoleWorker} {
		t.Run(role, func(t *testing.T) {
			address := "192.0.2.10"
			if role == RoleWorker {
				address = "192.0.2.11"
			}
			withFabric(t, FabricLink{NetDev: "fabric0", HCA: "rdma0"}, nil, address, nil)
			r := twoSparkRecipe(t)
			placement := Placement{Role: role, NodeCount: 2, MasterAddress: "192.0.2.10", MasterPort: 29501}
			var body struct{ Env []string }
			client := &DockerClient{client: &http.Client{Transport: withoutNegotiation(func(request *http.Request) (*http.Response, error) {
				if request.Method != http.MethodPost || request.URL.Path != "/containers/create" {
					t.Fatalf("unexpected Docker request: %s %s", request.Method, request.URL.Path)
				}
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				return dockerFixtureResponse(http.StatusCreated, `{"Id":"created"}`), nil
			})}}
			if _, err := client.Create(context.Background(), "model", r.Runtime.Reference(), []string{"/model"}, "/cache", nil, r, placement); err != nil {
				t.Fatal(err)
			}
			var pins int
			for _, entry := range body.Env {
				if strings.HasPrefix(entry, "VLLM_HOST_IP=") {
					pins++
					if entry != "VLLM_HOST_IP="+address {
						t.Fatalf("message queue address = %s, want this rank's address %s", entry, address)
					}
				}
			}
			if pins != 1 {
				t.Fatalf("message queue address pins = %d, want one", pins)
			}
		})
	}
}

func TestDistributedVLLMRefusesCreationWithoutFabricAddress(t *testing.T) {
	withFabric(t, FabricLink{NetDev: "fabric0", HCA: "rdma0"}, nil, "", errors.New("fabric0 has no address"))
	r := twoSparkRecipe(t)
	client := &DockerClient{client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Fatal("address failure must precede every Docker mutation")
		return nil, errors.New("unexpected Docker request")
	})}}
	_, err := client.Create(context.Background(), "model", r.Runtime.Reference(), []string{"/model"}, "/cache", nil, r, Placement{Role: RoleWorker, NodeCount: 2})
	if err == nil || !strings.Contains(err.Error(), "fabric0 has no address") {
		t.Fatalf("creation error = %v", err)
	}
}

func TestVLLMAddressFailureDoesNotReplaceAnExistingContainer(t *testing.T) {
	withFabric(t, FabricLink{NetDev: "fabric0", HCA: "rdma0"}, nil, "", errors.New("fabric0 has no address"))
	r := twoSparkRecipe(t)
	body := containerFixtureJSON("existing", true, map[string]string{labelManaged: "true", labelRecipeID: r.ID, labelRecipeVersion: strconv.Itoa(r.Version)}, nil)
	client := &DockerClient{client: &http.Client{Transport: withoutNegotiation(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet {
			t.Fatalf("address failure triggered %s %s", request.Method, request.URL.Path)
		}
		return dockerFixtureResponse(http.StatusOK, string(body)), nil
	})}}
	host := &HostExecutor{docker: client}
	placement := Placement{Role: RoleWorker, NodeCount: 2}
	if _, err := host.replaceStaleContainer(context.Background(), r, placement); err == nil {
		t.Fatal("unresolved address accepted for an existing container")
	}
	if host.Completed(context.Background(), Execution{Placement: placement}, recipe.Operation{Type: "create_container"}, r, nil) {
		t.Fatal("unresolved address accepted as completed creation")
	}
}

func TestVLLMFabricAddressParticipatesInRestartDrift(t *testing.T) {
	withFabric(t, FabricLink{NetDev: "fabric0", HCA: "rdma0"}, nil, "192.0.2.11", nil)
	r := twoSparkRecipe(t)
	environment := mustContainerEnvironment(t, r, Placement{Role: RoleWorker, NodeCount: 2})
	for _, old := range []string{"", "192.0.2.12"} {
		var previous []string
		for _, entry := range environment {
			if !strings.HasPrefix(entry, "VLLM_HOST_IP=") {
				previous = append(previous, entry)
			} else if old != "" {
				previous = append(previous, "VLLM_HOST_IP="+old)
			}
		}
		drift := staleEnvironment(ContainerState{Environment: previous}, environment)
		if len(drift) != 1 || drift[0].Name != "VLLM_HOST_IP" || drift[0].Expected != "192.0.2.11" {
			t.Fatalf("old address %q: drift = %+v", old, drift)
		}
	}
	if drift := staleEnvironment(ContainerState{Environment: environment}, environment); len(drift) != 0 {
		t.Fatalf("matching address would be rebuilt: %+v", drift)
	}
}

func TestOtherRuntimesDoNotResolveVLLMMessageQueueAddress(t *testing.T) {
	withFabric(t, FabricLink{NetDev: "fabric0", HCA: "rdma0"}, nil, "", errors.New("must not read address"))
	for _, tc := range []struct {
		r         recipe.Recipe
		placement Placement
	}{
		{twoSparkRecipe(t), Placement{}},
		{sglangRecipe(), Placement{Role: RoleWorker, NodeCount: 2}},
	} {
		environment := mustContainerEnvironment(t, tc.r, tc.placement)
		for _, entry := range environment {
			if strings.HasPrefix(entry, "VLLM_HOST_IP=") {
				t.Fatalf("unexpected vLLM pin: %s", entry)
			}
		}
	}
}
