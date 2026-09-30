package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// attributesStub serves GET /v3/grill/attributes with the given status/body and
// records the request it saw.
type attributesSeen struct {
	method, path, auth, project string
	requests                    int
}

func startAttributesStub(t *testing.T, status int, body string) (*attributesSeen, func()) {
	t.Helper()
	seen := &attributesSeen{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v3/projects", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id":"p1","project_id":"p1","name":"Contracts","product":"grill"}]`))
	})
	mux.HandleFunc("/v3/grill/attributes", func(w http.ResponseWriter, r *http.Request) {
		seen.requests++
		seen.method = r.Method
		seen.path = r.URL.Path
		seen.auth = r.Header.Get("Authorization")
		seen.project = r.Header.Get("X-Project-ID")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	ts := httptest.NewServer(mux)
	t.Setenv("POMA_API_BASE_URL", ts.URL)
	return seen, ts.Close
}

const attributesBody = `{"attributes":[{"name":"region","type":"string"},{"name":"notes","type":"encrypted_text"}],"max_names":64,"note":"Reuse an existing attribute name and type where one fits; a name, once declared, is permanent and counts against max_names."}`

func TestGrillAttributesRequestAndRendering(t *testing.T) {
	resetProjectsCache()
	defer resetProjectsCache()
	t.Setenv("POMA_PROJECT_ID", "")
	seen, stop := startAttributesStub(t, 200, attributesBody)
	defer stop()

	res, out, err := GrillAttributes(context.Background(), nil, GrillAttributesInput{Token: "tok-attrs-1", ProjectID: "p1"})
	if err != nil {
		t.Fatalf("protocol error: %v", err)
	}
	if res != nil && res.IsError {
		t.Fatalf("unexpected tool error: %+v", out.GrillError)
	}
	if seen.requests != 1 || seen.method != http.MethodGet || seen.path != "/v3/grill/attributes" {
		t.Errorf("request = %d x %s %s, want 1 x GET /v3/grill/attributes", seen.requests, seen.method, seen.path)
	}
	if seen.auth != "Bearer tok-attrs-1" {
		t.Errorf("Authorization = %q, want Bearer token", seen.auth)
	}
	if seen.project != "p1" {
		t.Errorf("X-Project-ID = %q, want p1", seen.project)
	}
	want := []GrillAttribute{{"region", "string"}, {"notes", "encrypted_text"}}
	if len(out.Attributes) != len(want) {
		t.Fatalf("attributes = %+v, want %+v", out.Attributes, want)
	}
	for i := range want {
		if out.Attributes[i] != want[i] {
			t.Errorf("attributes[%d] = %+v, want %+v", i, out.Attributes[i], want[i])
		}
	}
	if out.MaxNames != 64 {
		t.Errorf("max_names = %d, want 64", out.MaxNames)
	}
	if !strings.Contains(out.Note, "permanent") {
		t.Errorf("note = %q, want the server note", out.Note)
	}
	if out.Scope == nil || out.Scope.ProjectName != "Contracts" {
		t.Errorf("scope = %+v, want project Contracts", out.Scope)
	}

	b, _ := json.Marshal(out)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for _, k := range []string{"attributes", "max_names", "note", "scope"} {
		if _, ok := m[k]; !ok {
			t.Errorf("output JSON missing %q: %s", k, b)
		}
	}
	if _, ok := m["error"]; ok {
		t.Errorf("success output carries error: %s", b)
	}
}

// Without a project selection no X-Project-ID header is sent (account default
// or project key), and an empty schema renders as [] with the default note.
func TestGrillAttributesNoProjectEmptySchema(t *testing.T) {
	resetProjectsCache()
	defer resetProjectsCache()
	t.Setenv("POMA_PROJECT_ID", "")
	seen, stop := startAttributesStub(t, 200, `{"attributes":[],"max_names":64}`)
	defer stop()

	_, out, _ := GrillAttributes(context.Background(), nil, GrillAttributesInput{Token: "tok-attrs-2"})
	if seen.project != "" {
		t.Errorf("X-Project-ID = %q, want none", seen.project)
	}
	if out.Attributes == nil || len(out.Attributes) != 0 {
		t.Errorf("attributes = %#v, want empty non-nil slice", out.Attributes)
	}
	if out.Note != grillAttributesDefaultNote {
		t.Errorf("note = %q, want default", out.Note)
	}
}

func TestGrillAttributesUnavailable503(t *testing.T) {
	resetProjectsCache()
	defer resetProjectsCache()
	_, stop := startAttributesStub(t, 503, `{"error":"attribute schema unavailable"}`)
	defer stop()

	res, out, err := GrillAttributes(context.Background(), nil, GrillAttributesInput{Token: "tok-attrs-3"})
	if err != nil {
		t.Fatalf("protocol error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatal("expected IsError result")
	}
	if out.Code != CodeUpstreamError || !out.Retryable {
		t.Errorf("code/retryable = %q/%v, want upstream_error/true", out.Code, out.Retryable)
	}
	if !strings.Contains(out.Error, "HTTP 503") || !strings.Contains(out.Error, "do not declare new attribute names") {
		t.Errorf("error = %q", out.Error)
	}
	b, _ := json.Marshal(out)
	if !strings.Contains(string(b), `"attributes":[]`) {
		t.Errorf("error output must carry attributes: [] (schema declares an array): %s", b)
	}
}

func TestGrillAttributesMissingToken(t *testing.T) {
	t.Setenv("POMA_API_KEY", "")
	res, out, _ := GrillAttributes(context.Background(), nil, GrillAttributesInput{})
	if res == nil || !res.IsError || out.Code != CodeMissingToken {
		t.Fatalf("want missing_token error, got %+v", out.GrillError)
	}
}
