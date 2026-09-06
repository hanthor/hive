package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/worksource"
)

const acmmEvalTTL = time.Hour

// acmmRefreshDebounce is the floor under `?refresh=1` (#5877): an operator-
// forced re-evaluation still returns the cached result when the cache is
// younger than this, so holding the Re-evaluate button down cannot spend the
// GitHub API budget (a full refresh is up to ~29 GetContents calls per repo).
const acmmRefreshDebounce = time.Minute
const acmmLevelThreshold = 0.70
const acmmEvalTimeout = 30 * time.Second
const acmmPerRepoTimeout = 20 * time.Second
const acmmIssueLabelName = "acmm"
const acmmIssueLabelColor = "0075ca"
const acmmIssueLabelDesc = "ACMM criterion gap identified by hive evaluation"

// ACMMEvaluation is the combined codebase + operational ACMM evaluation result.
type ACMMEvaluation struct {
	CodebaseLevel     int    `json:"codebase_level"`
	CodebaseLevelName string `json:"codebase_level_name"`
	OperationalLevel  int    `json:"operational_level"`
	OperationalName   string `json:"operational_name"`
	OverallLevel      int    `json:"overall_level"`
	CriteriaTotal     int    `json:"criteria_total"`
	CriteriaPassed    int    `json:"criteria_passed"`
	// CriteriaUnknown counts criteria the evaluation could not decide because
	// GitHub did not answer (rate limit, per-repo deadline, 5xx). They are
	// scored as not-matched so an incomplete evaluation can never inflate a
	// level, and they are never eligible for a gap issue.
	CriteriaUnknown int               `json:"criteria_unknown,omitempty"`
	LastEvaluatedAt string            `json:"last_evaluated_at"`
	Levels          []ACMMLevelScore  `json:"levels"`
	CriteriaResults []CriterionResult `json:"criteria_results,omitempty"`
	RepoResults     []RepoEvaluation  `json:"repo_results,omitempty"`
	Error           string            `json:"error,omitempty"`
	// IssueTracker is where "Open Issue" files by default for this hive —
	// "github" or "linear" — after resolving governor.acmm.issue_tracker
	// against the work source. The dashboard's tracker selector defaults
	// to it. WorkSourceType is governor.work_source.type ("" = github) so
	// the UI knows whether a non-GitHub choice exists at all.
	IssueTracker   string `json:"issue_tracker"`
	WorkSourceType string `json:"work_source_type,omitempty"`
}

// RepoEvaluation holds ACMM results for a single repository.
type RepoEvaluation struct {
	Repo            string            `json:"repo"`
	CodebaseLevel   int               `json:"codebase_level"`
	LevelName       string            `json:"level_name"`
	BlockedAtLevel  int               `json:"blocked_at_level"`
	CriteriaTotal   int               `json:"criteria_total"`
	CriteriaPassed  int               `json:"criteria_passed"`
	CriteriaUnknown int               `json:"criteria_unknown,omitempty"`
	Levels          []ACMMLevelScore  `json:"levels"`
	CriteriaResults []CriterionResult `json:"criteria_results"`
}

// ACMMLevelScore summarizes pass/fail for a single ACMM level.
type ACMMLevelScore struct {
	Level     int     `json:"level"`
	Name      string  `json:"name"`
	Score     float64 `json:"score"`
	Threshold float64 `json:"threshold"`
	Passed    bool    `json:"passed"`
	Total     int     `json:"total"`
	Matched   int     `json:"matched"`
	// Unknown is how many of Total could not be evaluated. Total still
	// includes them, so Score is a floor, never an estimate.
	Unknown int `json:"unknown,omitempty"`
}

// CriterionResult records whether an individual criterion was detected.
type CriterionResult struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Level    int      `json:"level"`
	Category string   `json:"category"`
	Patterns []string `json:"patterns"`
	Passed   bool     `json:"passed"`
	// Unknown is set when no pattern was found AND at least one probe got no
	// answer from GitHub. A criterion is only "missing" when every probe was
	// a definite 404; anything else — rate limit, the per-repo deadline
	// expiring mid-evaluation, a 5xx — is not evidence of absence. Before
	// this flag existed those errors read as "file absent", and the
	// dashboard's Open All filed gap issues for files that were there
	// (tunaOS#2286/#2287/#2298/#2299 asked for tests/, tests/e2e/,
	// .github/workflows/ and codecov.yml on a repo that had all four).
	Unknown bool   `json:"unknown,omitempty"`
	Repo    string `json:"repo,omitempty"`
}

// ACMMIssueRequest is the payload for creating an ACMM gap issue.
type ACMMIssueRequest struct {
	Repo         string `json:"repo"`
	CriterionID  string `json:"criterion_id"`
	CriterionLvl int    `json:"criterion_level"`
	// Tracker optionally overrides governor.acmm.issue_tracker for this one
	// request: "github" | "work_source". Empty = the configured default;
	// anything else is a 400.
	Tracker string `json:"tracker,omitempty"`
}

func (s *Server) handleACMMEvaluation(w http.ResponseWriter, r *http.Request) {
	opsLevel := s.detectCurrentLevel()
	opsName := acmmLevelNames[opsLevel]
	if opsName == "" {
		opsName = "Unknown"
	}

	// #5877: `?refresh=1` lets the operator bypass the hour-long TTL — the
	// panel drives a fix-and-verify loop, and a fix could not be verified for
	// up to an hour otherwise. It bypasses the TTL, not the cache machinery:
	// the request is served from cache when the entry is younger than the
	// debounce window, so a forced refresh is rate-limited server-side.
	ttl := acmmEvalTTL
	if r.URL.Query().Get("refresh") == "1" {
		ttl = acmmRefreshDebounce
	}

	s.acmmEvalMu.RLock()
	cached := s.acmmEvalCache
	cacheAge := time.Since(s.acmmEvalCachedAt)
	s.acmmEvalMu.RUnlock()

	if cached != nil && cacheAge < ttl {
		result := *cached
		result.OperationalLevel = opsLevel
		result.OperationalName = opsName
		result.OverallLevel = minInt(result.CodebaseLevel, opsLevel)
		s.stampACMMIssueTracker(&result)
		jsonResponse(w, result)
		return
	}

	s.acmmEvalMu.Lock()
	defer s.acmmEvalMu.Unlock()

	if s.acmmEvalCache != nil && time.Since(s.acmmEvalCachedAt) < ttl {
		result := *s.acmmEvalCache
		result.OperationalLevel = opsLevel
		result.OperationalName = opsName
		result.OverallLevel = minInt(result.CodebaseLevel, opsLevel)
		s.stampACMMIssueTracker(&result)
		jsonResponse(w, result)
		return
	}

	eval := s.evaluateAllRepos()
	eval.OperationalLevel = opsLevel
	eval.OperationalName = opsName
	eval.OverallLevel = minInt(eval.CodebaseLevel, opsLevel)

	s.acmmEvalCache = &eval
	s.acmmEvalCachedAt = time.Now()

	s.stampACMMIssueTracker(&eval)
	jsonResponse(w, eval)
}

// stampACMMIssueTracker fills the tracker fields from live config. It runs
// on every response, cached or not, so a config edit shows up in the UI
// without waiting for the hour-long evaluation cache to expire.
func (s *Server) stampACMMIssueTracker(eval *ACMMEvaluation) {
	eval.IssueTracker = acmmIssueDestinationGitHub
	if s.deps == nil || s.deps.Config == nil {
		return
	}
	eval.WorkSourceType = s.deps.Config.Governor.WorkSource.Type
	eval.IssueTracker = s.acmmIssueDestination(s.deps.Config.Governor.ACMM.EffectiveIssueTracker())
}

// handleACMMCreateIssue files an issue for a failed ACMM criterion — on
// GitHub (the default) or, when governor.acmm.issue_tracker is work_source
// and the backlog is Linear, on the Linear team mapped to the repo. The
// request's `tracker` field overrides the config for one click.
func (s *Server) handleACMMCreateIssue(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	var req ACMMIssueRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Repo == "" || req.CriterionID == "" {
		http.Error(w, "repo and criterion_id are required", http.StatusBadRequest)
		return
	}

	var criterion *ACMMCriterion
	for i := range universalCriteria {
		if universalCriteria[i].ID == req.CriterionID {
			criterion = &universalCriteria[i]
			break
		}
	}
	if criterion == nil {
		http.Error(w, "unknown criterion", http.StatusBadRequest)
		return
	}

	if s.deps == nil || s.deps.Config == nil {
		http.Error(w, "config not loaded", http.StatusInternalServerError)
		return
	}
	// Per-click override beats the configured default; an unknown value is
	// the caller's mistake, not a reason to fall back to GitHub.
	tracker, err := s.deps.Config.Governor.ACMM.ResolveACMMIssueTracker(req.Tracker)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	owner := s.deps.Config.Project.Org
	if owner == "" {
		http.Error(w, "org not configured", http.StatusInternalServerError)
		return
	}

	// A gap issue is only meaningful for a criterion the last evaluation
	// found MISSING. Filing for one that passes is noise; filing for one the
	// evaluation could not decide is how a rate-limited run turned into
	// bogus "Add CI/CD pipeline" issues on a repo with 40 workflows.
	if verdict, ok := s.cachedCriterionVerdict(req.Repo, req.CriterionID); ok {
		switch {
		case verdict.Passed:
			http.Error(w, fmt.Sprintf("criterion %s currently passes in %s; re-evaluate before filing", req.CriterionID, req.Repo), http.StatusConflict)
			return
		case verdict.Unknown:
			http.Error(w, fmt.Sprintf("criterion %s could not be evaluated in %s (GitHub did not answer: rate limit, timeout or error); re-evaluate before filing", req.CriterionID, req.Repo), http.StatusConflict)
			return
		}
	}

	title, body := acmmIssueContent(criterion)

	// Invocation-attribution trail: this issue is created by the hive on an
	// operator's dashboard action — stamp the (config-gated) visible trailer
	// and, after creation, record the (unconditional) audit entry: the same
	// two layers the PR-request watcher applies (see pkg/github/attribution.go).
	meta := github.InvocationMeta{Agent: github.AttributionAgentDashboard}
	if s.deps.Config.Governor.AttributionTrailerEnabled() {
		body = github.AppendTrailer(body, meta)
	}

	ctx, cancel := context.WithTimeout(context.Background(), acmmEvalTimeout)
	defer cancel()

	if s.acmmIssueDestination(tracker) == acmmIssueDestinationLinear {
		s.createACMMLinearIssue(ctx, w, r, req.Repo, title, body, meta)
		return
	}

	if s.deps.GHClient == nil {
		http.Error(w, "GitHub client not available", http.StatusInternalServerError)
		return
	}
	ghClient := s.deps.GHClient.GoGitHub()
	if ghClient == nil {
		http.Error(w, "GitHub client not initialized", http.StatusInternalServerError)
		return
	}

	// The dashboard remembers issues it opened only for the current page
	// load, so a reload — or a second operator — re-files the same gap. The
	// repo is the durable record: reuse an open acmm issue for this
	// criterion when one exists.
	if existing := s.findOpenACMMIssue(ctx, ghClient, owner, req.Repo, criterion.ID); existing != nil {
		jsonResponse(w, map[string]interface{}{
			"tracker":      acmmIssueDestinationGitHub,
			"issue_number": existing.GetNumber(),
			"issue_url":    existing.GetHTMLURL(),
			"existing":     true,
		})
		return
	}

	_, _, labelErr := ghClient.Issues.CreateLabel(ctx, owner, req.Repo, &gh.Label{
		Name:        gh.Ptr(acmmIssueLabelName),
		Description: gh.Ptr(acmmIssueLabelDesc),
		Color:       gh.Ptr(acmmIssueLabelColor),
	})
	labelExists := labelErr == nil || strings.Contains(labelErr.Error(), "already_exists")

	issueReq := &gh.IssueRequest{
		Title: gh.Ptr(title),
		Body:  gh.Ptr(body),
	}
	if labelExists {
		issueReq.Labels = &[]string{acmmIssueLabelName, "ai-fix-requested"}
	}

	issue, _, err := ghClient.Issues.Create(ctx, owner, req.Repo, issueReq)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to create issue: %v", err), http.StatusInternalServerError)
		return
	}
	// Unconditional audit entry — written even with the trailer toggled off.
	s.auditFromRequest(r, github.AuditActionHiveIssueCreated,
		meta.AuditDetail(
			"repo", owner+"/"+req.Repo,
			"number", strconv.Itoa(issue.GetNumber()),
			"url", issue.GetHTMLURL(),
			"tracker", acmmIssueDestinationGitHub,
			"flow", "acmm-eval"), "")

	jsonResponse(w, map[string]interface{}{
		"tracker":      acmmIssueDestinationGitHub,
		"issue_number": issue.GetNumber(),
		"issue_url":    issue.GetHTMLURL(),
	})
}

// Where an ACMM gap issue actually lands, after resolving
// config.ACMMIssueTrackerWorkSource against the hive's work source. These
// are the values of the `tracker` field in the /api/acmm/issue response and
// of ACMMEvaluation.IssueTracker.
const (
	acmmIssueDestinationGitHub = "github"
	acmmIssueDestinationLinear = "linear"
)

// acmmIssueDestination maps a resolved tracker choice to a destination.
// work_source resolves to Linear only when the backlog is Linear; every
// other work source (GitHub Issues, GitHub Projects, Jira — which has no
// write path — or unset) files on GitHub exactly as before.
func (s *Server) acmmIssueDestination(tracker string) string {
	if tracker == config.ACMMIssueTrackerWorkSource && s.deps != nil && s.deps.Config != nil &&
		s.deps.Config.Governor.WorkSource.Type == "linear" {
		return acmmIssueDestinationLinear
	}
	return acmmIssueDestinationGitHub
}

// acmmLinearSource builds a Linear adapter from the hive's work_source
// config for the issueCreate write. It is constructed per request — the
// config can be edited live from the dashboard and the adapter is a thin
// HTTP wrapper, so there is nothing worth caching.
func (s *Server) acmmLinearSource() (*worksource.LinearSource, error) {
	lc := s.deps.Config.Governor.WorkSource.Linear
	if strings.TrimSpace(lc.APIKey) == "" {
		return nil, fmt.Errorf("work_source.linear.api_key not configured")
	}
	if len(lc.Teams) == 0 {
		return nil, fmt.Errorf("work_source.linear.teams is empty")
	}
	teams := make([]worksource.LinearTeamConfig, len(lc.Teams))
	for i, t := range lc.Teams {
		teams[i] = worksource.LinearTeamConfig{Key: t.Key, Repo: t.Repo}
	}
	return worksource.NewLinearSource(worksource.LinearConfig{
		APIKey:  lc.APIKey,
		Teams:   teams,
		BaseURL: s.acmmLinearBaseURL,
		Logger:  s.logger,
	}, nil), nil
}

// createACMMLinearIssue files the gap issue on the Linear team mapped to
// repo (see LinearSource.TeamForRepo) and writes the same audit entry the
// GitHub path does, with tracker=linear so the trail says where it went.
func (s *Server) createACMMLinearIssue(ctx context.Context, w http.ResponseWriter, r *http.Request, repo, title, body string, meta github.InvocationMeta) {
	src, err := s.acmmLinearSource()
	if err != nil {
		http.Error(w, fmt.Sprintf("Linear issue tracker not available: %v", err), http.StatusInternalServerError)
		return
	}
	team, ok := src.TeamForRepo(repo)
	if !ok {
		http.Error(w, "Linear issue tracker not available: no teams configured", http.StatusInternalServerError)
		return
	}
	issue, err := src.CreateIssue(ctx, team.Key, title, body)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to create issue: %v", err), http.StatusInternalServerError)
		return
	}
	s.auditFromRequest(r, github.AuditActionHiveIssueCreated,
		meta.AuditDetail(
			"repo", s.deps.Config.Project.Org+"/"+repo,
			"number", strconv.Itoa(issue.Number),
			"identifier", issue.Identifier,
			"url", issue.URL,
			"tracker", acmmIssueDestinationLinear,
			"team", team.Key,
			"flow", "acmm-eval"), "")

	jsonResponse(w, map[string]interface{}{
		"tracker":      acmmIssueDestinationLinear,
		"issue_number": issue.Number,
		"identifier":   issue.Identifier,
		"issue_url":    issue.URL,
		"team":         team.Key,
	})
}

// acmmIssueContent renders the title and Markdown body of a gap issue for
// criterion. It is tracker-agnostic: GitHub and Linear receive byte-identical
// text (before the attribution trailer), so an operator switching trackers
// sees the same issue either way.
func acmmIssueContent(criterion *ACMMCriterion) (title, body string) {
	levelName := acmmLevelNames[criterion.Level]
	if levelName == "" {
		levelName = "Unknown"
	}

	title = fmt.Sprintf("[ACMM L%d] Add %s", criterion.Level, criterion.Name)

	var patternsStr strings.Builder
	for _, p := range criterion.Patterns {
		patternsStr.WriteString(fmt.Sprintf("- `%s`\n", p))
	}

	body = fmt.Sprintf("## ACMM Gap: %s\n\n"+
		"**Level:** L%d %s\n"+
		"**Category:** %s\n"+
		"%s\n\n"+
		"### What's needed\n\n"+
		"This repository is missing one of the following files or directories:\n\n"+
		"%s\n"+
		"Adding any one of these will satisfy this criterion and help the repository "+
		"advance to ACMM Level %d (%s).\n\n"+
		"### Why it matters\n\n"+
		"%s\n\n"+
		"### How to fix\n\n"+
		"Create one of the files listed above. The ACMM evaluation checks for file existence — "+
		"the content can follow your project's conventions.\n\n"+
		"---\n"+
		"*Opened by Hive ACMM Evaluation*",
		criterion.Name,
		criterion.Level, levelName,
		criterion.Category,
		acmmCriterionMarker(criterion.ID),
		patternsStr.String(),
		criterion.Level, levelName,
		acmmCriterionWhyItMatters(criterion.Level, criterion.Category),
	)
	return title, body
}

func acmmCriterionWhyItMatters(level int, category string) string {
	switch category {
	case "prerequisite":
		return "Prerequisites are foundational — without test suites, CI/CD, and templates, AI agents lack the guardrails needed for safe autonomous work."
	case "feedback-loop":
		return "Feedback loops let agents learn from outcomes. Metrics, review rubrics, and automated review application create the signal that drives improvement."
	case "learning":
		return "Learning artifacts (memory, corrections, session summaries) let agents carry context across sessions instead of starting cold each time."
	case "governance":
		return "Governance ensures agents operate within boundaries — safety models, structural gates, and risk assessment prevent autonomous drift."
	case "observability":
		return "Observability makes agent behavior visible — dashboards, metrics, and quality reports let humans audit what agents are doing."
	case "self-tuning":
		return "Self-tuning means the system adjusts its own quality thresholds based on observed outcomes, reducing the need for manual calibration."
	case "autonomy":
		return "Autonomy criteria enable agents to operate independently — generating issues, orchestrating multi-agent workflows, and managing merge queues."
	case "traceability":
		return "Traceability links agent actions to outcomes, making it possible to audit decisions and understand why changes were made."
	default:
		return fmt.Sprintf("This criterion is part of ACMM Level %d, which requires a minimum set of capabilities before advancing to the next level.", level)
	}
}

// evaluateAllRepos scans all configured repos and produces aggregate + per-repo results.
func (s *Server) evaluateAllRepos() ACMMEvaluation {
	if s.deps == nil || s.deps.Config == nil {
		return ACMMEvaluation{
			Error:           "config not loaded",
			LastEvaluatedAt: time.Now().UTC().Format(time.RFC3339),
		}
	}

	owner := s.deps.Config.Project.Org
	if owner == "" {
		return ACMMEvaluation{
			Error:           "org not configured",
			LastEvaluatedAt: time.Now().UTC().Format(time.RFC3339),
		}
	}

	repos := s.deps.Config.Project.Repos
	if len(repos) == 0 && s.deps.Config.Project.PrimaryRepo != "" {
		repos = []string{s.deps.Config.Project.PrimaryRepo}
	}
	if len(repos) == 0 {
		return ACMMEvaluation{
			Error:           "no repos configured",
			LastEvaluatedAt: time.Now().UTC().Format(time.RFC3339),
		}
	}

	if s.deps.GHClient == nil {
		return ACMMEvaluation{
			Error:           "GitHub client not available",
			LastEvaluatedAt: time.Now().UTC().Format(time.RFC3339),
		}
	}
	ghClient := s.deps.GHClient.GoGitHub()
	if ghClient == nil {
		return ACMMEvaluation{
			Error:           "GitHub client not initialized",
			LastEvaluatedAt: time.Now().UTC().Format(time.RFC3339),
		}
	}

	var repoEvals []RepoEvaluation
	// Aggregate: a criterion passes if it passes in ANY repo, and is unknown
	// if it passed nowhere but could not be evaluated somewhere.
	aggPassed := make(map[string]bool)
	aggUnknown := make(map[string]bool)

	for _, repo := range repos {
		ctx, cancel := context.WithTimeout(context.Background(), acmmPerRepoTimeout)
		dirCache := s.prefetchDirectories(ctx, owner, repo)

		var results []CriterionResult
		for _, c := range universalCriteria {
			passed, unknown := s.checkCriterion(ctx, owner, repo, c, dirCache)
			results = append(results, CriterionResult{
				ID:       c.ID,
				Name:     c.Name,
				Level:    c.Level,
				Category: c.Category,
				Patterns: c.Patterns,
				Passed:   passed,
				Unknown:  unknown,
				Repo:     repo,
			})
			if passed {
				aggPassed[c.ID] = true
			}
			if unknown {
				aggUnknown[c.ID] = true
			}
		}
		cancel()

		scored := s.scoreResults(results)
		repoEvals = append(repoEvals, RepoEvaluation{
			Repo:            repo,
			CodebaseLevel:   scored.CodebaseLevel,
			LevelName:       scored.CodebaseLevelName,
			BlockedAtLevel:  acmmBlockedAtLevel(scored.Levels),
			CriteriaTotal:   scored.CriteriaTotal,
			CriteriaPassed:  scored.CriteriaPassed,
			CriteriaUnknown: scored.CriteriaUnknown,
			Levels:          scored.Levels,
			CriteriaResults: scored.CriteriaResults,
		})
	}

	// Build aggregate criterion results (pass if ANY repo has it).
	var aggResults []CriterionResult
	for _, c := range universalCriteria {
		aggResults = append(aggResults, CriterionResult{
			ID:       c.ID,
			Name:     c.Name,
			Level:    c.Level,
			Category: c.Category,
			Patterns: c.Patterns,
			Passed:   aggPassed[c.ID],
			Unknown:  !aggPassed[c.ID] && aggUnknown[c.ID],
		})
	}

	eval := s.scoreResults(aggResults)
	eval.RepoResults = repoEvals
	return eval
}

// prefetchDirectories fetches directory listings for parent dirs that
// many criteria share, reducing individual API calls.
func (s *Server) prefetchDirectories(ctx context.Context, owner, repo string) map[string]map[string]bool {
	cache := make(map[string]map[string]bool)

	dirs := []string{
		"",                  // root
		".github",           // templates, configs
		".github/workflows", // CI workflows
		".github/ISSUE_TEMPLATE",
		".github/prompts",
		".github/agents",
		".claude",
		"docs",
		"docs/security",
	}

	// GoGitHub() is nil-receiver safe and returns nil, so the deref below would
	// panic on a hive running without GitHub credentials. Sibling handlers
	// guard the same way.
	ghClient := s.deps.GHClient.GoGitHub()
	if ghClient == nil {
		return cache
	}
	for _, dir := range dirs {
		_, dirContents, _, err := ghClient.Repositories.GetContents(ctx, owner, repo, dir, nil)
		if err != nil {
			continue
		}
		entries := make(map[string]bool)
		for _, entry := range dirContents {
			name := entry.GetName()
			entryType := entry.GetType()
			entries[name] = true
			if entryType == "dir" {
				entries[name+"/"] = true
			}
		}
		cache[dir] = entries
	}

	return cache
}

// probeResult is the tri-state answer to "does this path exist in the repo".
type probeResult int

const (
	// probeAbsent is a definite answer: GitHub said 404, or the parent
	// directory was listed and the entry is not in it.
	probeAbsent probeResult = iota
	// probePresent is a definite answer: the path exists.
	probePresent
	// probeUnknown means GitHub did not answer the question — rate limit,
	// abuse limit, the per-repo deadline expiring mid-evaluation, a 5xx, or
	// no client at all. It is NOT evidence that the path is missing.
	probeUnknown
)

// checkCriterion reports whether any of the criterion's patterns exist in
// the repo. unknown is true when nothing was found AND at least one probe
// got no answer, so the caller can tell "missing" from "could not tell".
func (s *Server) checkCriterion(ctx context.Context, owner, repo string, c ACMMCriterion, dirCache map[string]map[string]bool) (passed, unknown bool) {
	for _, pattern := range c.Patterns {
		switch s.probePattern(ctx, owner, repo, pattern, dirCache) {
		case probePresent:
			return true, false
		case probeUnknown:
			unknown = true
		}
	}
	return false, unknown
}

// patternExists is the boolean view of probePattern: true only for a
// definite hit. Callers that need to distinguish absent from unknown use
// probePattern directly.
func (s *Server) patternExists(ctx context.Context, owner, repo, path string, dirCache map[string]map[string]bool) bool {
	return s.probePattern(ctx, owner, repo, path, dirCache) == probePresent
}

// probePattern checks if a file or directory path exists, using the
// pre-fetched directory cache when possible. A cached listing is
// authoritative for its directory; only the live fallback can be unknown.
func (s *Server) probePattern(ctx context.Context, owner, repo, path string, dirCache map[string]map[string]bool) probeResult {
	isDir := strings.HasSuffix(path, "/")
	cleanPath := strings.TrimSuffix(path, "/")

	parent := ""
	base := cleanPath
	if idx := strings.LastIndex(cleanPath, "/"); idx >= 0 {
		parent = cleanPath[:idx]
		base = cleanPath[idx+1:]
	}

	if entries, ok := dirCache[parent]; ok {
		if isDir {
			if entries[base+"/"] || entries[base] {
				return probePresent
			}
			return probeAbsent
		}
		if entries[base] {
			return probePresent
		}
		return probeAbsent
	}

	ghClient := s.deps.GHClient.GoGitHub()
	if ghClient == nil {
		return probeUnknown
	}
	_, _, _, err := ghClient.Repositories.GetContents(ctx, owner, repo, cleanPath, nil)
	switch {
	case err == nil:
		return probePresent
	case isGitHubNotFound(err):
		return probeAbsent
	default:
		return probeUnknown
	}
}

// isGitHubNotFound reports whether err is GitHub answering 404 for the path —
// the only error that means "absent". Rate-limit and abuse-limit errors are
// distinct types in go-github and never match; neither do context deadlines.
func isGitHubNotFound(err error) bool {
	var er *gh.ErrorResponse
	return errors.As(err, &er) && er.Response != nil && er.Response.StatusCode == http.StatusNotFound
}

// scoreResults calculates per-level scores and the overall codebase level.
func (s *Server) scoreResults(results []CriterionResult) ACMMEvaluation {
	type levelBucket struct {
		total   int
		matched int
		unknown int
	}
	buckets := make(map[int]*levelBucket)

	totalPassed := 0
	totalUnknown := 0
	for _, r := range results {
		b, ok := buckets[r.Level]
		if !ok {
			b = &levelBucket{}
			buckets[r.Level] = b
		}
		// An unknown criterion stays in total and out of matched: the level
		// score is a floor. Excluding it from the denominator would let two
		// known passes and six unanswered probes read as 100%.
		b.total++
		if r.Passed {
			b.matched++
			totalPassed++
		} else if r.Unknown {
			b.unknown++
			totalUnknown++
		}
	}

	levels := make([]int, 0, len(buckets))
	for lvl := range buckets {
		levels = append(levels, lvl)
	}
	sort.Ints(levels)

	var levelScores []ACMMLevelScore
	for _, lvl := range levels {
		b := buckets[lvl]
		score := float64(0)
		if b.total > 0 {
			score = float64(b.matched) / float64(b.total)
		}
		name := acmmLevelNames[lvl]
		if name == "" {
			name = "Unknown"
		}
		levelScores = append(levelScores, ACMMLevelScore{
			Level:     lvl,
			Name:      name,
			Score:     score,
			Threshold: acmmLevelThreshold,
			Passed:    score >= acmmLevelThreshold,
			Total:     b.total,
			Matched:   b.matched,
			Unknown:   b.unknown,
		})
	}

	codebaseLevel := 0
	for _, ls := range levelScores {
		if ls.Passed {
			codebaseLevel = ls.Level
		} else {
			break
		}
	}

	// When L0 passes, the achieved level is L1 (prerequisites met).
	// The criteria levels jump 0→2 with no L1 criteria defined,
	// so passing L0 means you've reached L1.
	if codebaseLevel == 0 {
		for _, ls := range levelScores {
			if ls.Level == 0 && ls.Passed {
				codebaseLevel = 1
				break
			}
		}
	}

	codeLevelName := acmmLevelNames[codebaseLevel]
	if codeLevelName == "" {
		codeLevelName = "None"
	}

	return ACMMEvaluation{
		CodebaseLevel:     codebaseLevel,
		CodebaseLevelName: codeLevelName,
		CriteriaTotal:     len(results),
		CriteriaPassed:    totalPassed,
		CriteriaUnknown:   totalUnknown,
		LastEvaluatedAt:   time.Now().UTC().Format(time.RFC3339),
		Levels:            levelScores,
		CriteriaResults:   results,
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func acmmBlockedAtLevel(levels []ACMMLevelScore) int {
	for _, level := range levels {
		if !level.Passed {
			return level.Level
		}
	}
	return -1
}

// cachedCriterionVerdict returns the most recent per-repo result for one
// criterion, if an evaluation has been cached. ok is false when nothing has
// been evaluated yet or the repo/criterion is not in the cache, in which
// case the caller has no grounds to refuse and proceeds as before.
func (s *Server) cachedCriterionVerdict(repo, criterionID string) (CriterionResult, bool) {
	s.acmmEvalMu.RLock()
	cached := s.acmmEvalCache
	s.acmmEvalMu.RUnlock()
	if cached == nil {
		return CriterionResult{}, false
	}
	for _, re := range cached.RepoResults {
		if re.Repo != repo {
			continue
		}
		for _, c := range re.CriteriaResults {
			if c.ID == criterionID {
				return c, true
			}
		}
	}
	return CriterionResult{}, false
}

// acmmCriterionMarker is the line acmmIssueContent writes into every gap
// issue body; findOpenACMMIssue matches on it, so the two must agree.
func acmmCriterionMarker(criterionID string) string {
	return "**Criterion ID:** `" + criterionID + "`"
}

// findOpenACMMIssue returns an open issue in owner/repo carrying the acmm
// label whose body names criterionID, or nil. A failure to list is not a
// reason to refuse the request — it falls through to creation, which is the
// pre-existing behavior — but it is logged so a silent dedupe miss can be
// seen. Pull requests share the issues endpoint and are skipped.
func (s *Server) findOpenACMMIssue(ctx context.Context, ghClient *gh.Client, owner, repo, criterionID string) *gh.Issue {
	marker := acmmCriterionMarker(criterionID)
	opts := &gh.IssueListByRepoOptions{
		State:       "open",
		Labels:      []string{acmmIssueLabelName},
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	for page := 0; page < 5; page++ {
		issues, resp, err := ghClient.Issues.ListByRepo(ctx, owner, repo, opts)
		if err != nil {
			if s.logger != nil {
				s.logger.Warn("acmm: could not list open gap issues; proceeding to create", "repo", owner+"/"+repo, "criterion", criterionID, "err", err)
			}
			return nil
		}
		for _, issue := range issues {
			if issue.IsPullRequest() {
				continue
			}
			if strings.Contains(issue.GetBody(), marker) {
				return issue
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.ListOptions.Page = resp.NextPage
	}
	return nil
}
