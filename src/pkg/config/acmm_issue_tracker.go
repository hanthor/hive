package config

import (
	"fmt"
	"strings"
)

// ACMM issue-tracker choices for governor.acmm.issue_tracker and the
// per-request `tracker` override on POST /api/acmm/issue.
const (
	// ACMMIssueTrackerGitHub files ACMM gap issues on the repo's GitHub
	// Issues — the historical and default behavior.
	ACMMIssueTrackerGitHub = "github"
	// ACMMIssueTrackerWorkSource files ACMM gap issues where the hive's
	// backlog lives (governor.work_source). For a Linear work source that is
	// the Linear team mapped to the criterion's repo; for GitHub (or an unset
	// work source) it is identical to ACMMIssueTrackerGitHub.
	ACMMIssueTrackerWorkSource = "work_source"
)

// ValidACMMIssueTrackers is the accepted set for governor.acmm.issue_tracker.
// "" is accepted and means "use the default" (GitHub).
var ValidACMMIssueTrackers = map[string]bool{
	"":                         true,
	ACMMIssueTrackerGitHub:     true,
	ACMMIssueTrackerWorkSource: true,
}

// ValidateACMMIssueTracker reports whether v is an accepted issue_tracker value.
func ValidateACMMIssueTracker(v string) bool { return ValidACMMIssueTrackers[v] }

// ACMMConfig tunes the dashboard's ACMM evaluation surface.
type ACMMConfig struct {
	// IssueTracker selects where the dashboard's "Open Issue" / "Open All"
	// buttons on a failed ACMM criterion file the gap issue.
	//
	// Valid values: "" (= github) | github | work_source.
	// See ACMMIssueTrackerWorkSource for what work_source resolves to. A
	// request may override this per click via the `tracker` field of
	// POST /api/acmm/issue.
	IssueTracker string `yaml:"issue_tracker,omitempty" json:"issue_tracker,omitempty"`

	// RepoRoots maps a repository (as named in project.repos) to a
	// subdirectory that the ACMM evaluation probes IN ADDITION to the
	// repository root. The criteria are spelled as root-relative paths
	// (go.mod, test/, .golangci.yml, …); a repo that keeps its module under
	// src/ has all of them and matches none. With
	//
	//   governor:
	//     acmm:
	//       repo_roots:
	//         hive: src
	//
	// each pattern is tried at both <pattern> and src/<pattern>, and the
	// criterion passes if either exists. Paths are relative, without a
	// leading slash or "..". Repositories not listed are probed at the root
	// only, exactly as before.
	RepoRoots map[string]string `yaml:"repo_roots,omitempty" json:"repo_roots,omitempty"`
}

// RepoRoot returns the extra probe root configured for repo, cleaned of
// surrounding slashes and whitespace, or "" when the repo has none.
func (c ACMMConfig) RepoRoot(repo string) string {
	if c.RepoRoots == nil {
		return ""
	}
	return strings.Trim(strings.TrimSpace(c.RepoRoots[repo]), "/")
}

// ValidateACMMRepoRoots rejects roots the probe could not use safely: empty
// after cleaning, absolute, escaping upward, or carrying a backslash. It is
// called from Config.Validate so a typo fails at load time, not as a silent
// evaluation that never finds anything under the wrong directory.
func ValidateACMMRepoRoots(roots map[string]string) error {
	for repo, root := range roots {
		cleaned := strings.Trim(strings.TrimSpace(root), "/")
		switch {
		case strings.TrimSpace(repo) == "":
			return fmt.Errorf("acmm.repo_roots: empty repository name")
		case cleaned == "":
			return fmt.Errorf("acmm.repo_roots[%q]: root %q is empty", repo, root)
		case strings.Contains(cleaned, "\\"):
			return fmt.Errorf("acmm.repo_roots[%q]: root %q must use forward slashes", repo, root)
		}
		for _, seg := range strings.Split(cleaned, "/") {
			if seg == ".." || seg == "." || seg == "" {
				return fmt.Errorf("acmm.repo_roots[%q]: root %q must be a plain relative path (no \"..\", \".\" or empty segments)", repo, root)
			}
		}
	}
	return nil
}

// EffectiveIssueTracker resolves the configured tracker with its default:
// "" → github. It never returns an invalid value; Validate rejects those at
// load time and unknown strings degrade to the default here so a stale
// in-memory config can never route an issue somewhere unexpected.
func (c ACMMConfig) EffectiveIssueTracker() string {
	if strings.TrimSpace(c.IssueTracker) == ACMMIssueTrackerWorkSource {
		return ACMMIssueTrackerWorkSource
	}
	return ACMMIssueTrackerGitHub
}

// ResolveACMMIssueTracker picks the tracker for one issue-creation request:
// the request override when non-empty, else the configured default. An
// unknown override is an error (the API turns it into a 400) rather than a
// silent fallback — an operator who typed "linear" must learn that the
// accepted spelling is "work_source", not get a GitHub issue.
func (c ACMMConfig) ResolveACMMIssueTracker(override string) (string, error) {
	override = strings.TrimSpace(override)
	if override == "" {
		return c.EffectiveIssueTracker(), nil
	}
	if !ValidateACMMIssueTracker(override) {
		return "", fmt.Errorf("invalid tracker %q (must be %s or %s)", override, ACMMIssueTrackerGitHub, ACMMIssueTrackerWorkSource)
	}
	return override, nil
}
