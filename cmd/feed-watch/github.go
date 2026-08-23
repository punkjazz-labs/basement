package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// githubClient reads two GitHub REST endpoints: a repository's default
// branch, and that branch's HEAD commit. Together they answer "has the
// launcher repository a recipe's source pins moved on".
type githubClient struct {
	base   string
	client *http.Client
}

func newGitHubClient(base string) *githubClient {
	return &githubClient{
		base:   strings.TrimRight(base, "/"),
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

type githubRepoInfo struct {
	DefaultBranch string `json:"default_branch"`
}

type githubBranchInfo struct {
	Commit githubCommit `json:"commit"`
}

type githubCommit struct {
	SHA string `json:"sha"`
}

func (c *githubClient) RepoInfo(owner, repo string) (githubRepoInfo, error) {
	var info githubRepoInfo
	url := c.base + "/repos/" + owner + "/" + repo
	if err := getJSON(c.client, url, &info); err != nil {
		return githubRepoInfo{}, fmt.Errorf("github repo info for %s/%s: %w", owner, repo, err)
	}
	if info.DefaultBranch == "" {
		return githubRepoInfo{}, fmt.Errorf("github repo info for %s/%s: response carries no default_branch", owner, repo)
	}
	return info, nil
}

func (c *githubClient) BranchInfo(owner, repo, branch string) (githubBranchInfo, error) {
	var info githubBranchInfo
	url := c.base + "/repos/" + owner + "/" + repo + "/branches/" + branch
	if err := getJSON(c.client, url, &info); err != nil {
		return githubBranchInfo{}, fmt.Errorf("github branch info for %s/%s@%s: %w", owner, repo, branch, err)
	}
	if info.Commit.SHA == "" {
		return githubBranchInfo{}, fmt.Errorf("github branch info for %s/%s@%s: response carries no commit sha", owner, repo, branch)
	}
	return info, nil
}

// isGitHubSource reports whether a recipe's source.url names a github.com
// repository. A recipe's source may also be a Hugging Face URL (the schema
// allows both), and that case is not this tool's business: only a GitHub
// source has a branch HEAD to compare against.
func isGitHubSource(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	return err == nil && parsed.Scheme == "https" && parsed.Host == "github.com"
}

// githubOwnerRepo splits a github.com source URL into the owner and
// repository name the API endpoints take as path segments.
func githubOwnerRepo(rawURL string) (owner, repo string, ok bool) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", "", false
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], strings.TrimSuffix(parts[1], ".git"), true
}
