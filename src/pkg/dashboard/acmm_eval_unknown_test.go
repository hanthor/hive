package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ghpkg "github.com/kubestellar/hive/pkg/github"
)

// These tests pin the difference between "GitHub said the file is not there"
// and "GitHub did not answer". Before probePattern existed the two collapsed
// into a single false, and a rate-limited or timed-out evaluation filed gap
// issues for files that were present (tunaOS#2286/#2287/#2298/#2299).

// triStateMux answers the contents endpoint the four ways the probe must
// tell apart: a real file, a definite 404, a 500, and a 403 rate limit.
func triStateMux(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/myorg/repo1/contents/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/repos/myorg/repo1/contents/")
		switch path {
		case "go.mod":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"name": "go.mod", "path": "go.mod", "type": "file", "sha": "abc"})
		case "broken.txt":
			http.Error(w, `{"message":"Server Error"}`, http.StatusInternalServerError)
		case "limited.txt":
			w.Header().Set("X-RateLimit-Limit", "5000")
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", "9999999999")
			http.Error(w, `{"message":"API rate limit exceeded for user ID 1."}`, http.StatusForbidden)
		default:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func newTriStateServer(t *testing.T) *Server {
	ts := triStateMux(t)
	s := NewServer(0, acmmEvalTestLogger())
	deps := testDeps(t)
	deps.GHClient = ghpkg.NewClientForTest(ts.URL, "myorg", []string{"repo1"}, acmmEvalTestLogger())
	s.RegisterAPI(deps)
	return s
}

func TestProbePattern_TriState(t *testing.T) {
	s := newTriStateServer(t)
	empty := map[string]map[string]bool{}
	cases := map[string]probeResult{
		"go.mod":      probePresent,
		"missing.txt": probeAbsent,
		"broken.txt":  probeUnknown,
		"limited.txt": probeUnknown,
	}
	for path, want := range cases {
		if got := s.probePattern(t.Context(), "myorg", "repo1", path, empty); got != want {
			t.Errorf("probePattern(%q) = %v, want %v", path, got, want)
		}
	}
	// The boolean view is true only for a definite hit — an unknown probe
	// must not read as present, and it must not be the caller's job to know.
	if s.patternExists(t.Context(), "myorg", "repo1", "broken.txt", empty) {
		t.Error("patternExists must be false for an unknown probe")
	}
}

func TestProbePattern_CachedListingIsAuthoritative(t *testing.T) {
	// With the parent directory already listed, an entry that is not in it
	// is a definite absent — the live API (which would 500 here) is never
	// consulted, so a broken API cannot turn a cached miss into unknown.
	s := newTriStateServer(t)
	cache := map[string]map[string]bool{"": {"go.mod": true}}
	if got := s.probePattern(t.Context(), "myorg", "repo1", "broken.txt", cache); got != probeAbsent {
		t.Fatalf("cached dir miss = %v, want probeAbsent", got)
	}
	if got := s.probePattern(t.Context(), "myorg", "repo1", "go.mod", cache); got != probePresent {
		t.Fatalf("cached dir hit = %v, want probePresent", got)
	}
}

func TestProbePattern_ExpiredContextIsUnknown(t *testing.T) {
	// The per-repo deadline (acmmPerRepoTimeout) expiring mid-evaluation is
	// the everyday way probes go unanswered. It must never read as absent.
	s := newTriStateServer(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if got := s.probePattern(ctx, "myorg", "repo1", "go.mod", map[string]map[string]bool{}); got != probeUnknown {
		t.Fatalf("cancelled context = %v, want probeUnknown (go.mod exists, but nobody asked GitHub)", got)
	}
}

func TestCheckCriterion_UnknownOnlyWhenNothingFoundAndAProbeFailed(t *testing.T) {
	s := newTriStateServer(t)
	empty := map[string]map[string]bool{}
	tests := []struct {
		name        string
		patterns    []string
		wantPassed  bool
		wantUnknown bool
	}{
		{"every pattern a definite 404 is a real gap", []string{"a.txt", "b.txt"}, false, false},
		{"nothing found and one probe failed is unknown", []string{"a.txt", "broken.txt"}, false, true},
		{"a hit after a failed probe is still a pass", []string{"broken.txt", "go.mod"}, true, false},
		{"rate limited is unknown, not absent", []string{"limited.txt"}, false, true},
	}
	for _, tc := range tests {
		passed, unknown := s.checkCriterion(t.Context(), "myorg", "repo1", ACMMCriterion{ID: "acmm:t", Patterns: tc.patterns}, empty)
		if passed != tc.wantPassed || unknown != tc.wantUnknown {
			t.Errorf("%s: got (passed=%v, unknown=%v), want (%v, %v)", tc.name, passed, unknown, tc.wantPassed, tc.wantUnknown)
		}
	}
}

func TestScoreResults_UnknownStaysInDenominator(t *testing.T) {
	s := NewServer(0, acmmEvalTestLogger())
	// Two known passes and six unanswered probes at one level. Dropping the
	// unknowns from the denominator would score this 100% and pass the
	// level; keeping them scores 25% and does not. Incompleteness may only
	// ever lower a level, never raise it.
	results := []CriterionResult{
		{ID: "p1", Level: 3, Passed: true},
		{ID: "p2", Level: 3, Passed: true},
	}
	for i := 0; i < 6; i++ {
		results = append(results, CriterionResult{ID: fmt.Sprintf("u%d", i), Level: 3, Unknown: true})
	}
	eval := s.scoreResults(results)
	if len(eval.Levels) != 1 {
		t.Fatalf("levels = %d, want 1", len(eval.Levels))
	}
	lvl := eval.Levels[0]
	if lvl.Total != 8 || lvl.Matched != 2 || lvl.Unknown != 6 {
		t.Fatalf("level total/matched/unknown = %d/%d/%d, want 8/2/6", lvl.Total, lvl.Matched, lvl.Unknown)
	}
	if lvl.Passed {
		t.Fatal("a level that is 75% unanswered must not pass")
	}
	if eval.CriteriaTotal != 8 || eval.CriteriaPassed != 2 || eval.CriteriaUnknown != 6 {
		t.Fatalf("eval total/passed/unknown = %d/%d/%d, want 8/2/6", eval.CriteriaTotal, eval.CriteriaPassed, eval.CriteriaUnknown)
	}
}

// ---------- gap-issue filing: reuse, fall-through, refusal ----------

// issueMux serves the labels and issues endpoints for myorg/repo1 and
// records whether an issue was actually created, which is what the
// dedupe tests assert on.
type issueMux struct {
	ts      *httptest.Server
	created atomic.Int32
}

func newIssueMux(t *testing.T, listStatus int, listBody []map[string]interface{}) *issueMux {
	m := &issueMux{}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/myorg/repo1/labels", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"name": acmmIssueLabelName})
	})
	mux.HandleFunc("/repos/myorg/repo1/issues", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if listStatus != http.StatusOK {
				http.Error(w, `{"message":"boom"}`, listStatus)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(listBody)
		case http.MethodPost:
			m.created.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"number": 42, "html_url": "https://github.com/myorg/repo1/issues/42"})
		default:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
	})
	m.ts = httptest.NewServer(mux)
	t.Cleanup(m.ts.Close)
	return m
}

func newIssueServer(t *testing.T, m *issueMux) *Server {
	s := NewServer(0, acmmEvalTestLogger())
	deps := testDeps(t)
	deps.GHClient = ghpkg.NewClientForTest(m.ts.URL, "myorg", []string{"repo1"}, acmmEvalTestLogger())
	s.RegisterAPI(deps)
	return s
}

func criterionByID(t *testing.T, id string) *ACMMCriterion {
	for i := range universalCriteria {
		if universalCriteria[i].ID == id {
			return &universalCriteria[i]
		}
	}
	t.Fatalf("no criterion %q", id)
	return nil
}

func TestCreateIssue_ReusesOpenIssueForSameCriterion(t *testing.T) {
	_, body := acmmIssueContent(criterionByID(t, "acmm:claude-md"))
	_, otherBody := acmmIssueContent(criterionByID(t, "acmm:agents-md"))
	m := newIssueMux(t, http.StatusOK, []map[string]interface{}{
		// A PR carrying the marker shares the issues endpoint and must be skipped.
		{"number": 8, "html_url": "https://github.com/myorg/repo1/pull/8", "body": body, "pull_request": map[string]interface{}{"url": "x"}},
		// An acmm issue for a DIFFERENT criterion must not match.
		{"number": 9, "html_url": "https://github.com/myorg/repo1/issues/9", "body": otherBody},
		// The one we want.
		{"number": 7, "html_url": "https://github.com/myorg/repo1/issues/7", "body": body},
	})
	s := newIssueServer(t, m)

	rec := doPost(s, "/api/acmm/issue", map[string]interface{}{"repo": "repo1", "criterion_id": "acmm:claude-md"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := decodeJSON(t, rec)
	if got["existing"] != true {
		t.Fatalf("expected existing=true, got %v", got)
	}
	if n, _ := got["issue_number"].(float64); int(n) != 7 {
		t.Fatalf("issue_number = %v, want 7 (the open issue, not the PR or the other criterion)", got["issue_number"])
	}
	if m.created.Load() != 0 {
		t.Fatalf("an issue was created (%d) although one was already open", m.created.Load())
	}
}

func TestCreateIssue_ListFailureFallsThroughToCreate(t *testing.T) {
	// Dedupe is best-effort: if GitHub cannot list, the operator's request
	// still results in an issue, exactly as before dedupe existed.
	m := newIssueMux(t, http.StatusInternalServerError, nil)
	s := newIssueServer(t, m)

	rec := doPost(s, "/api/acmm/issue", map[string]interface{}{"repo": "repo1", "criterion_id": "acmm:claude-md"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := decodeJSON(t, rec)
	if got["existing"] == true {
		t.Fatal("nothing could have been found; existing must not be reported")
	}
	if n, _ := got["issue_number"].(float64); int(n) != 42 || m.created.Load() != 1 {
		t.Fatalf("expected exactly one created issue #42, got number=%v created=%d", got["issue_number"], m.created.Load())
	}
}

func TestCreateIssue_RefusesWhenCachedVerdictIsPassingOrUnknown(t *testing.T) {
	m := newIssueMux(t, http.StatusOK, []map[string]interface{}{})
	s := newIssueServer(t, m)
	s.acmmEvalMu.Lock()
	s.acmmEvalCache = &ACMMEvaluation{RepoResults: []RepoEvaluation{{
		Repo: "repo1",
		CriteriaResults: []CriterionResult{
			{ID: "acmm:claude-md", Passed: true},
			{ID: "acmm:agents-md", Unknown: true},
			{ID: "acmm:cursor-rules"},
		},
	}}}
	s.acmmEvalCachedAt = time.Now()
	s.acmmEvalMu.Unlock()

	for _, tc := range []struct {
		id   string
		want int
		why  string
	}{
		{"acmm:claude-md", http.StatusConflict, "passing criterion is not a gap"},
		{"acmm:agents-md", http.StatusConflict, "unanswered criterion is not evidence of a gap"},
		{"acmm:cursor-rules", http.StatusOK, "a definite miss files as before"},
	} {
		rec := doPost(s, "/api/acmm/issue", map[string]interface{}{"repo": "repo1", "criterion_id": tc.id})
		if rec.Code != tc.want {
			t.Errorf("%s: status = %d, want %d (%s); body=%s", tc.id, rec.Code, tc.want, tc.why, rec.Body.String())
		}
	}
	if m.created.Load() != 1 {
		t.Fatalf("created = %d, want exactly 1 (only the definite miss)", m.created.Load())
	}

	// A repo the cache knows nothing about is not refused: no verdict, no
	// grounds. (repo2 is not served by the mux, so creation fails at
	// GitHub — the point is that it is not a 409.)
	rec := doPost(s, "/api/acmm/issue", map[string]interface{}{"repo": "repo2", "criterion_id": "acmm:claude-md"})
	if rec.Code == http.StatusConflict {
		t.Fatal("a repo absent from the cache must not be refused on cached grounds")
	}
}

func TestACMMIssueContent_CarriesTheDedupeMarker(t *testing.T) {
	// findOpenACMMIssue matches on acmmCriterionMarker; if the body format
	// ever drifts from it, dedupe silently stops working. Pin the coupling.
	c := criterionByID(t, "acmm:claude-md")
	_, body := acmmIssueContent(c)
	if !strings.Contains(body, acmmCriterionMarker(c.ID)) {
		t.Fatalf("issue body no longer carries the marker %q:\n%s", acmmCriterionMarker(c.ID), body)
	}
}
