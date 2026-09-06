package config

import (
	"strings"
	"testing"
)

// governor.acmm.repo_roots: a repository that keeps its module under a
// subdirectory (this repo's src/) is probed there too, so the root-spelled
// ACMM criteria (go.mod, test/, .golangci.yml, …) see it. The root is
// cleaned once, at read time, and a root the probe could not use safely is
// rejected at load time instead of silently matching nothing.

func TestACMMConfig_RepoRoot(t *testing.T) {
	c := ACMMConfig{RepoRoots: map[string]string{
		"hive":    "src",
		"slashed": "/lib/app/",
		"padded":  "  pkg  ",
		"empty":   "",
	}}
	tests := map[string]string{
		"hive":    "src",
		"slashed": "lib/app",
		"padded":  "pkg",
		"empty":   "",
		"unknown": "",
	}
	for repo, want := range tests {
		if got := c.RepoRoot(repo); got != want {
			t.Errorf("RepoRoot(%q) = %q, want %q", repo, got, want)
		}
	}
	if got := (ACMMConfig{}).RepoRoot("hive"); got != "" {
		t.Errorf("nil map: RepoRoot = %q, want empty", got)
	}
}

func TestValidateACMMRepoRoots(t *testing.T) {
	tests := []struct {
		name    string
		roots   map[string]string
		wantErr string // substring; "" means valid
	}{
		{"nil map is fine", nil, ""},
		{"plain subdirectory", map[string]string{"hive": "src"}, ""},
		{"nested subdirectory", map[string]string{"hive": "services/api"}, ""},
		{"leading and trailing slashes are cleaned, not rejected", map[string]string{"hive": "/src/"}, ""},
		{"empty repo name", map[string]string{"": "src"}, "empty repository name"},
		{"blank repo name", map[string]string{"  ": "src"}, "empty repository name"},
		{"empty root", map[string]string{"hive": ""}, "is empty"},
		{"root that is only slashes", map[string]string{"hive": "///"}, "is empty"},
		{"parent traversal", map[string]string{"hive": "../other"}, "plain relative path"},
		{"traversal in the middle", map[string]string{"hive": "src/../etc"}, "plain relative path"},
		{"dot segment", map[string]string{"hive": "./src"}, "plain relative path"},
		{"empty segment", map[string]string{"hive": "src//pkg"}, "plain relative path"},
		{"backslash", map[string]string{"hive": `src\pkg`}, "forward slashes"},
	}
	for _, tc := range tests {
		err := ValidateACMMRepoRoots(tc.roots)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected error %v", tc.name, err)
		case tc.wantErr != "" && err == nil:
			t.Errorf("%s: want error containing %q, got nil", tc.name, tc.wantErr)
		case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
			t.Errorf("%s: error %v does not mention %q", tc.name, err, tc.wantErr)
		}
		if err != nil && !strings.Contains(err.Error(), "acmm.repo_roots") {
			t.Errorf("%s: error %v does not name the acmm.repo_roots key", tc.name, err)
		}
	}
}

func TestValidate_RejectsBadACMMRepoRoots(t *testing.T) {
	c := &Config{
		Project: ProjectConfig{Org: "my-org"},
		GitHub:  GitHubConfig{Token: "t"},
		Agents: map[string]AgentConfig{
			"scanner": {Backend: "claude"},
		},
	}
	c.Governor.ACMM.RepoRoots = map[string]string{"hive": "../escape"}
	err := c.validate()
	if err == nil || !strings.Contains(err.Error(), "governor: acmm.repo_roots") || !strings.Contains(err.Error(), "../escape") {
		t.Fatalf("validate() = %v, want governor: acmm.repo_roots error naming ../escape", err)
	}
	c.Governor.ACMM.RepoRoots = map[string]string{"hive": "src"}
	if err := c.validate(); err != nil {
		t.Fatalf("a plain subdirectory should validate: %v", err)
	}
}
