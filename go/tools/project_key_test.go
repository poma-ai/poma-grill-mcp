package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A project API key (poma_proj_…) selects its project by itself. Found during
// the 2026-09-09 production e2e test: with the recommended project-key setup,
// grill_docs_list reported "default grill workspace — no specific project is
// selected" for documents that live in the named project `immoscout`, because
// /projects (403 for project keys) yielded no listing to name it. The source
// must say the key chose the project, and the hint must never claim the
// default workspace for a proj_ namespace.

func TestProjectIDSourceProjectKey(t *testing.T) {
	t.Setenv("POMA_PROJECT_ID", "")
	id, src := projectIDSource("poma_proj_gr_abc", "")
	if id != "" || src != sourceProjectKey {
		t.Fatalf("project key: got (%q,%q), want (\"\",%q)", id, src, sourceProjectKey)
	}
	// Explicit selections still win (the gateway then arbitrates with 409).
	if _, src := projectIDSource("poma_proj_gr_abc", "p1"); src != "project_id argument" {
		t.Fatalf("argument should win over key, got %q", src)
	}
	t.Setenv("POMA_PROJECT_ID", "p2")
	if _, src := projectIDSource("poma_proj_gr_abc", ""); src != "POMA_PROJECT_ID env var" {
		t.Fatalf("env should win over key, got %q", src)
	}
	t.Setenv("POMA_PROJECT_ID", "")
	if _, src := projectIDSource("poma_acc_xyz", ""); src != "account default (no project_id set)" {
		t.Fatalf("account key: got %q", src)
	}
	if isProjectKey("eyJhbGciOi") || isProjectKey("poma_acc_x") || !isProjectKey("poma_proj_pc_x") {
		t.Fatal("isProjectKey prefix classification wrong")
	}
}

func TestScopeFromProjectsProjectKey(t *testing.T) {
	bound := []grillProject{{ID: "242d", ProjectID: "242d", AccountID: "bdaa", Name: "immoscout", Product: "grill"}}

	// docs_list path: namespace known, listing = the single /projects/info entry.
	got := scopeFromProjects(bound, "", "proj_242d", sourceProjectKey)
	if got.ProjectName != "immoscout" || got.IsDefault || got.Source != sourceProjectKey {
		t.Fatalf("namespace+key: %+v", got)
	}
	if !strings.Contains(got.Hint, `"immoscout"`) || !strings.Contains(got.Hint, "project API key") || strings.Contains(got.Hint, "default grill workspace") {
		t.Fatalf("hint = %q", got.Hint)
	}

	// ingest/search path: no namespace, no id — the key alone selects.
	got = scopeFromProjects(bound, "", "", sourceProjectKey)
	if got.ProjectName != "immoscout" || got.Namespace != "proj_242d" {
		t.Fatalf("key only: %+v", got)
	}

	// Listing unavailable: still never "default workspace".
	got = scopeFromProjects(nil, "", "proj_242d", sourceProjectKey)
	if strings.Contains(got.Hint, "default grill workspace") || !strings.Contains(got.Hint, "project API key") {
		t.Fatalf("nil listing hint = %q", got.Hint)
	}
	got = scopeFromProjects(nil, "", "", sourceProjectKey)
	if strings.Contains(got.Hint, "default grill workspace") {
		t.Fatalf("nil listing, no namespace: hint = %q", got.Hint)
	}

	// Account credential with a proj_ namespace but a failed listing: name the
	// namespace, do not claim the default workspace.
	got = scopeFromProjects(nil, "", "proj_242d", "account default (no project_id set)")
	if strings.Contains(got.Hint, "default grill workspace") || !strings.Contains(got.Hint, "proj_242d") {
		t.Fatalf("account+proj namespace hint = %q", got.Hint)
	}
}

func TestInterpretProjectConflict(t *testing.T) {
	body := []byte(`{"code":409,"reason":"project_id_conflict","error":"X-Project-ID does not match the project this API key is bound to"}`)
	msg, ok := interpretProjectConflict(http.StatusConflict, body, "grill ingest")
	if !ok || !strings.Contains(msg, "HTTP 409") || !strings.Contains(msg, "POMA_PROJECT_ID") {
		t.Fatalf("conflict not recognised: ok=%v msg=%q", ok, msg)
	}
	if _, ok := interpretProjectConflict(http.StatusConflict, []byte(`{"reason":"other"}`), "x"); ok {
		t.Fatal("unrelated 409 must not be a project conflict")
	}
	if _, ok := interpretProjectConflict(http.StatusBadRequest, body, "x"); ok {
		t.Fatal("non-409 must not be a project conflict")
	}
}

func TestGrillIngestProjectConflictIsInvalidInput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"code":409,"reason":"project_id_conflict","error":"X-Project-ID does not match"}`))
	}))
	defer srv.Close()
	t.Setenv("POMA_API_BASE_URL", srv.URL)

	_, out, err := GrillIngest(context.Background(), nil, GrillIngestInput{Token: "poma_proj_gr_k", ProjectID: "other", FileBase64: "aGVsbG8=", Filename: "a.txt"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Code != CodeInvalidInput || out.Retryable {
		t.Fatalf("code=%q retryable=%v, want invalid_input/non-retryable; err=%s", out.Code, out.Retryable, out.Error)
	}
}

// grill_projects under a project key answers with the bound project from
// /projects/info instead of the gateway's 403 on /projects.
func TestGrillProjectsWithProjectKeyUsesProjectInfo(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/projects/info") {
			_, _ = w.Write([]byte(`{"id":"242d","project_id":"242d","account_id":"bdaa","name":"immoscout","product":"grill","protected":false,"is_default":false}`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":403,"reason":"forbidden","error":"project API keys are not accepted on this endpoint"}`))
	}))
	defer srv.Close()
	t.Setenv("POMA_API_BASE_URL", srv.URL)

	_, out, err := GrillProjects(context.Background(), nil, GrillProjectsInput{Token: "poma_proj_gr_k"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Code != "" || !strings.Contains(out.Projects, "immoscout") || !strings.Contains(out.Projects, "project API key") {
		t.Fatalf("out = %+v", out)
	}
	if len(paths) != 1 || !strings.HasSuffix(paths[0], "/projects/info") {
		t.Fatalf("paths = %v, want only /projects/info", paths)
	}

	// A product filter that does not match the bound project yields the empty answer.
	_, out, _ = GrillProjects(context.Background(), nil, GrillProjectsInput{Token: "poma_proj_gr_k", Product: "primecut"})
	if out.Code != "" || !strings.Contains(out.Projects, "No accessible projects") {
		t.Fatalf("product filter: %+v", out)
	}

	// Account keys keep the listing path.
	paths = nil
	_, out, _ = GrillProjects(context.Background(), nil, GrillProjectsInput{Token: "poma_acc_k"})
	if out.Code != CodeForbidden || len(paths) != 1 || strings.HasSuffix(paths[0], "/info") {
		t.Fatalf("account key: code=%q paths=%v", out.Code, paths)
	}
}

// grill_docs_list errors must serialise `documents` as [] not null: the output
// schema declares an array and the SDK validates structured output against it.
func TestGrillDocsListErrorOutputHasArrayDocuments(t *testing.T) {
	t.Setenv("POMA_API_KEY", "")
	t.Setenv("POMA_GRILL_API_KEY", "")
	_, out, err := GrillDocsList(context.Background(), nil, GrillDocsListInput{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Code != CodeMissingToken {
		t.Fatalf("code = %q", out.Code)
	}
	b, _ := json.Marshal(out)
	if !strings.Contains(string(b), `"documents":[]`) {
		t.Fatalf("documents must be an empty array on error, got %s", b)
	}
	resolved, rerr := grillDocsListOutputSchema.Resolve(nil)
	if rerr != nil {
		t.Fatalf("resolve schema: %v", rerr)
	}
	var v map[string]any
	_ = json.Unmarshal(b, &v)
	if verr := resolved.Validate(v); verr != nil {
		t.Fatalf("error output does not validate against the output schema: %v", verr)
	}
	// And prove the pre-fix shape (nil slice → null) is what the schema rejects.
	var nullv map[string]any
	nb, _ := json.Marshal(GrillDocsListOutput{GrillError: errOut(CodeMissingToken, "x")})
	_ = json.Unmarshal(nb, &nullv)
	if verr := resolved.Validate(nullv); verr == nil {
		t.Fatalf("expected null documents to fail schema validation (else this fix is moot): %s", nb)
	}
}
