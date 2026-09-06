package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// governor.acmm.repo_roots: the criteria are spelled root-relative, so a repo
// whose module lives under src/ (this one) matched none of go.mod, test/,
// .golangci.yml, … and self-evaluated at L0 while carrying every artifact.
// With a configured root each pattern is also probed at <root>/<pattern>.

// subRootMux is a repo with nothing at the root except a src/ directory;
// src/go.mod exists, src/broken.txt answers 500, everything else is a 404.
func subRootMux(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/myorg/repo1/contents/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/repos/myorg/repo1/contents/")
		switch path {
		case "src/go.mod":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"name": "go.mod", "path": "src/go.mod", "type": "file", "sha": "abc"})
		case "src/broken.txt":
			http.Error(w, `{"message":"Server Error"}`, http.StatusInternalServerError)
		default:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func newSubRootServer(t *testing.T) *Server {
	ts := subRootMux(t)
	s := NewServer(0, acmmEvalTestLogger())
	deps := testDeps(t)
	deps.GHClient = ghpkg.NewClientForTest(ts.URL, "myorg", []string{"repo1"}, acmmEvalTestLogger())
	s.RegisterAPI(deps)
	return s
}

func TestACMMPatternVariants(t *testing.T) {
	tests := []struct {
		root, pattern string
		want          []string
	}{
		{"", "go.mod", []string{"go.mod"}},
		{"", "test/", []string{"test/"}},
		{"src", "go.mod", []string{"go.mod", "src/go.mod"}},
		{"src", "test/", []string{"test/", "src/test/"}},
		{"services/api", ".github/workflows/", []string{".github/workflows/", "services/api/.github/workflows/"}},
	}
	for _, tc := range tests {
		got := acmmPatternVariants(tc.root, tc.pattern)
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("acmmPatternVariants(%q, %q) = %v, want %v", tc.root, tc.pattern, got, tc.want)
		}
	}
	if got := acmmUnderRoot("src", ""); got != "src" {
		t.Errorf("acmmUnderRoot(src, \"\") = %q, want src", got)
	}
}

func TestCheckCriterionUnder_ProbesTheConfiguredRootToo(t *testing.T) {
	s := newSubRootServer(t)
	empty := map[string]map[string]bool{}
	crit := func(patterns ...string) ACMMCriterion {
		return ACMMCriterion{ID: "acmm:t", Patterns: patterns}
	}

	// Without a root the sub-root file is invisible: a definite gap, since
	// every root probe is a real 404.
	if passed, unknown := s.checkCriterionUnder(t.Context(), "myorg", "repo1", "", crit("go.mod"), empty); passed || unknown {
		t.Fatalf("no root: got (passed=%v, unknown=%v), want (false, false)", passed, unknown)
	}
	// checkCriterion is the root-only view and must agree.
	if passed, unknown := s.checkCriterion(t.Context(), "myorg", "repo1", crit("go.mod"), empty); passed || unknown {
		t.Fatalf("checkCriterion: got (passed=%v, unknown=%v), want (false, false)", passed, unknown)
	}

	tests := []struct {
		name        string
		patterns    []string
		wantPassed  bool
		wantUnknown bool
	}{
		{"present under the root passes", []string{"go.mod"}, true, false},
		{"absent at both roots is a real gap", []string{"a.txt", "b.txt"}, false, false},
		{"an unanswered sub-root probe with no hit is unknown", []string{"broken.txt"}, false, true},
		{"a hit after an unanswered probe is still a pass", []string{"broken.txt", "go.mod"}, true, false},
	}
	for _, tc := range tests {
		passed, unknown := s.checkCriterionUnder(t.Context(), "myorg", "repo1", "src", crit(tc.patterns...), empty)
		if passed != tc.wantPassed || unknown != tc.wantUnknown {
			t.Errorf("%s: got (passed=%v, unknown=%v), want (%v, %v)", tc.name, passed, unknown, tc.wantPassed, tc.wantUnknown)
		}
	}
}

func TestCheckCriterionUnder_UsesThePrefetchedSubRootListing(t *testing.T) {
	// A listing cached for the sub-root parent is authoritative there just as
	// it is at the root: the live API (which would 500 for src/broken.txt) is
	// never consulted, and directory markers resolve under the root.
	s := newSubRootServer(t)
	cache := map[string]map[string]bool{
		"":    {"src/": true},
		"src": {"go.mod": true, "test/": true},
	}
	crit := func(patterns ...string) ACMMCriterion {
		return ACMMCriterion{ID: "acmm:t", Patterns: patterns}
	}
	if passed, unknown := s.checkCriterionUnder(t.Context(), "myorg", "repo1", "src", crit("broken.txt"), cache); passed || unknown {
		t.Fatalf("cached miss under root: got (passed=%v, unknown=%v), want (false, false)", passed, unknown)
	}
	if passed, unknown := s.checkCriterionUnder(t.Context(), "myorg", "repo1", "src", crit("test/"), cache); !passed || unknown {
		t.Fatalf("cached directory under root: got (passed=%v, unknown=%v), want (true, false)", passed, unknown)
	}
}

func TestPrefetchDirectoriesUnder_ListsTheSubRootParentsToo(t *testing.T) {
	// The prefetch answers the listing endpoint for both the root parents and
	// their sub-root twins, so the probes are served from the cache at the
	// same rate at either root.
	var requested []string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/myorg/repo1/contents/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/repos/myorg/repo1/contents/")
		requested = append(requested, path)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]interface{}{{"name": "go.mod", "path": path + "/go.mod", "type": "file"}})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	s := NewServer(0, acmmEvalTestLogger())
	deps := testDeps(t)
	deps.GHClient = ghpkg.NewClientForTest(ts.URL, "myorg", []string{"repo1"}, acmmEvalTestLogger())
	s.RegisterAPI(deps)

	cache := s.prefetchDirectoriesUnder(t.Context(), "myorg", "repo1", "src")
	seen := strings.Join(requested, "\n")
	for _, want := range []string{"src", "src/.github", "src/docs/security"} {
		if !strings.Contains(seen+"\n", want+"\n") {
			t.Errorf("prefetch never listed %q; requested:\n%s", want, seen)
		}
	}
	if _, ok := cache["src/docs"]; !ok {
		t.Errorf("cache has no entry for src/docs; keys: %v", cacheKeys(cache))
	}
	if _, ok := cache["docs"]; !ok {
		t.Errorf("root docs listing must still be prefetched; keys: %v", cacheKeys(cache))
	}
}

func cacheKeys(m map[string]map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
