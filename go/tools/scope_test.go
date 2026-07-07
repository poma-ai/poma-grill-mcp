package tools

import (
	"strings"
	"testing"
	"time"
)

// resetProjectsCache clears global cache state between tests.
func resetProjectsCache() {
	projectsCacheMu.Lock()
	projectsCache = map[string]projectsCacheEntry{}
	projectsCacheMu.Unlock()
	nowFunc = time.Now
}

// A user must never read another user's cached projects.
func TestProjectsCacheIsolation(t *testing.T) {
	resetProjectsCache()
	defer resetProjectsCache()

	projectsCachePut("token-user-A", []grillProject{{Name: "A-only", ProjectID: "a1"}})

	if _, ok := projectsCacheGet("token-user-B"); ok {
		t.Fatal("user B read a cache entry keyed by user A's token — cross-tenant leak")
	}
	got, ok := projectsCacheGet("token-user-A")
	if !ok || len(got) != 1 || got[0].Name != "A-only" {
		t.Fatalf("user A should read own entry, got ok=%v val=%v", ok, got)
	}
}

// The raw token must not be retained as a map key.
func TestProjectsCacheKeyHashed(t *testing.T) {
	resetProjectsCache()
	defer resetProjectsCache()

	token := "poma_acc_supersecret_value"
	projectsCachePut(token, []grillProject{{Name: "x"}})

	projectsCacheMu.Lock()
	defer projectsCacheMu.Unlock()
	for k := range projectsCache {
		if strings.Contains(k, "supersecret") || k == token {
			t.Fatalf("cache key retains raw token: %q", k)
		}
	}
}

// Empty tokens are never cached (would otherwise be a shared bucket).
func TestProjectsCacheEmptyTokenNotCached(t *testing.T) {
	resetProjectsCache()
	defer resetProjectsCache()

	projectsCachePut("", []grillProject{{Name: "x"}})
	if _, ok := projectsCacheGet(""); ok {
		t.Fatal("empty token should never be cached")
	}
}

// Entries expire and are evicted after the TTL.
func TestProjectsCacheExpiry(t *testing.T) {
	resetProjectsCache()
	defer resetProjectsCache()

	base := time.Unix(1_000_000, 0)
	nowFunc = func() time.Time { return base }
	projectsCachePut("tok", []grillProject{{Name: "x"}})

	if _, ok := projectsCacheGet("tok"); !ok {
		t.Fatal("entry should be live immediately after put")
	}

	// Advance past the TTL: get must miss, and a subsequent put must have
	// evicted the stale entry.
	nowFunc = func() time.Time { return base.Add(projectsCacheTTL + time.Second) }
	if _, ok := projectsCacheGet("tok"); ok {
		t.Fatal("entry should be expired after TTL")
	}
	projectsCachePut("other", []grillProject{{Name: "y"}})

	projectsCacheMu.Lock()
	_, staleStillThere := projectsCache[projectsCacheKey("tok")]
	projectsCacheMu.Unlock()
	if staleStillThere {
		t.Fatal("expired entry should have been evicted on put")
	}
}

// realProjectsFixture mirrors an actual GET /projects response (fields the
// scope resolver depends on: id/project_id, account_id, orga_id, product,
// is_default). Names shortened but structurally faithful.
var realProjectsFixture = []grillProject{
	{ID: "4f71d1d3", ProjectID: "4f71d1d3", AccountID: "14d12545", Name: "Default Project", Product: "primecut", IsDefault: true},
	{ID: "0d6d50d7", ProjectID: "0d6d50d7", AccountID: "14d12545", Name: "Default Workspace", Product: "grill", IsDefault: true},
	{ID: "6f7e50e2", ProjectID: "6f7e50e2", AccountID: "0481da95", OrgaID: "4adc2778", Name: "Discovery", Product: "grill", IsDefault: false},
	{ID: "941fc013", ProjectID: "941fc013", AccountID: "14d12545", Name: "pc-proj", Product: "primecut", Protected: true, IsDefault: false},
	{ID: "352dcf98", ProjectID: "352dcf98", AccountID: "14d12545", OrgaID: "4adc2778", Name: "docu", Product: "grill", IsDefault: false},
}

func TestScopeFromProjects(t *testing.T) {
	tests := []struct {
		name          string
		projectID     string
		namespace     string
		source        string
		wantName      string
		wantIsDefault bool
		wantNamespace string
	}{
		{
			// grill_docs_list default case: authoritative account_ namespace.
			name:          "docs list account default namespace",
			namespace:     "account_14d12545",
			source:        "account default (no project_id set)",
			wantName:      "Default Workspace",
			wantIsDefault: true,
			wantNamespace: "account_14d12545",
		},
		{
			// grill_docs_list scoped to a named project: namespace is
			// "proj_<project_id>" on the wire.
			name:          "docs list named project namespace",
			namespace:     "proj_352dcf98",
			source:        "project_id argument",
			wantName:      "docu",
			wantIsDefault: false,
			wantNamespace: "proj_352dcf98",
		},
		{
			// search/ingest with no project_id: resolve the own-account default.
			name:          "no project id resolves account default",
			source:        "account default (no project_id set)",
			wantName:      "Default Workspace",
			wantIsDefault: true,
			wantNamespace: "account_14d12545",
		},
		{
			// search/ingest scoped by explicit project_id (no namespace returned).
			name:          "explicit project id",
			projectID:     "6f7e50e2",
			source:        "project_id argument",
			wantName:      "Discovery",
			wantIsDefault: false,
			wantNamespace: "proj_6f7e50e2",
		},
		{
			// Unknown namespace (e.g. project not visible): degrade gracefully.
			name:          "unknown namespace keeps raw value",
			namespace:     "account_ffffffff",
			source:        "account default (no project_id set)",
			wantName:      "",
			wantIsDefault: false,
			wantNamespace: "account_ffffffff",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scopeFromProjects(realProjectsFixture, tt.projectID, tt.namespace, tt.source)
			if got == nil {
				t.Fatal("scopeFromProjects returned nil")
			}
			if got.ProjectName != tt.wantName {
				t.Errorf("ProjectName = %q, want %q", got.ProjectName, tt.wantName)
			}
			if got.IsDefault != tt.wantIsDefault {
				t.Errorf("IsDefault = %v, want %v", got.IsDefault, tt.wantIsDefault)
			}
			if got.Namespace != tt.wantNamespace {
				t.Errorf("Namespace = %q, want %q", got.Namespace, tt.wantNamespace)
			}
			if got.Hint == "" {
				t.Error("Hint should never be empty")
			}
			if got.Source != tt.source {
				t.Errorf("Source = %q, want %q", got.Source, tt.source)
			}
		})
	}
}

// nil projects (e.g. /projects call failed) must not panic and must preserve
// the raw identifiers so the primary operation is unaffected.
func TestScopeFromProjectsNilDegrades(t *testing.T) {
	got := scopeFromProjects(nil, "", "account_14d12545", "account default (no project_id set)")
	if got == nil {
		t.Fatal("nil return")
	}
	if got.Namespace != "account_14d12545" {
		t.Errorf("Namespace = %q, want raw namespace preserved", got.Namespace)
	}
	if got.ProjectName != "" {
		t.Errorf("ProjectName = %q, want empty on nil projects", got.ProjectName)
	}
	if got.Hint == "" {
		t.Error("Hint should never be empty even without projects")
	}
}
