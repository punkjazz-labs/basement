package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// hfFixture is one test's canned answers for the two Hugging Face endpoints,
// keyed by repository (model info) and "repository@revision" (revision
// info). A missing key answers 404; an explicit status entry overrides
// whatever body would otherwise be sent, so a test can simulate an outage
// for exactly one repository while the rest still answer normally.
type hfFixture struct {
	modelInfo      map[string]hfModelInfo
	modelStatus    map[string]int
	revisionInfo   map[string]hfRevisionInfo
	revisionStatus map[string]int
}

func newHFTestServer(t *testing.T, fx *hfFixture) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/models/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		repository := r.PathValue("owner") + "/" + r.PathValue("repo")
		if status, ok := fx.modelStatus[repository]; ok {
			w.WriteHeader(status)
			return
		}
		info, ok := fx.modelInfo[repository]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeTestJSON(t, w, info)
	})
	mux.HandleFunc("GET /api/models/{owner}/{repo}/revision/{sha}", func(w http.ResponseWriter, r *http.Request) {
		repository := r.PathValue("owner") + "/" + r.PathValue("repo")
		key := repository + "@" + r.PathValue("sha")
		if status, ok := fx.revisionStatus[key]; ok {
			w.WriteHeader(status)
			return
		}
		info, ok := fx.revisionInfo[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeTestJSON(t, w, info)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server.URL
}

// githubFixture mirrors hfFixture for the two GitHub endpoints this tool
// reads, keyed by "owner/repo" and "owner/repo@branch".
type githubFixture struct {
	repoInfo     map[string]githubRepoInfo
	repoStatus   map[string]int
	branchInfo   map[string]githubBranchInfo
	branchStatus map[string]int
}

func newGitHubTestServer(t *testing.T, fx *githubFixture) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("owner") + "/" + r.PathValue("repo")
		if status, ok := fx.repoStatus[key]; ok {
			w.WriteHeader(status)
			return
		}
		info, ok := fx.repoInfo[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeTestJSON(t, w, info)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/branches/{branch}", func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("owner") + "/" + r.PathValue("repo") + "@" + r.PathValue("branch")
		if status, ok := fx.branchStatus[key]; ok {
			w.WriteHeader(status)
			return
		}
		info, ok := fx.branchInfo[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeTestJSON(t, w, info)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server.URL
}

func writeTestJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatalf("encode test response: %v", err)
	}
}
