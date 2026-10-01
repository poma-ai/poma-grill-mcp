package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// labelItemsFromMap must produce deterministic, key-sorted "key:value" items and
// skip empty keys — matching the Node labelItemsFromMap.
func TestLabelItemsFromMapSorted(t *testing.T) {
	got := labelItemsFromMap(map[string]string{"b": "2", "a": "1", "  ": "skip"})
	if strings.Join(got, ",") != "a:1,b:2" || len(got) != 2 {
		t.Fatalf("labelItemsFromMap = %q, want [a:1 b:2]", got)
	}
	if len(labelItemsFromMap(nil)) != 0 {
		t.Fatal("labelItemsFromMap(nil) must be empty")
	}
}

// grill_ingest with a url must POST /ingest carrying X-Remote-URL (and the
// labels as attributes.labels, no X-Labels), no file body, and return the
// parsed job_id.
func TestGrillIngestURLSendsRemoteURLAndLabels(t *testing.T) {
	var gotRemoteURL, gotAttrs string
	var gotLabels []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index4ai/v1/ingest":
			gotRemoteURL = r.Header.Get("X-Remote-URL")
			gotAttrs = r.Header.Get("X-Attributes")
			gotLabels = r.Header.Values("X-Labels")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"job_id":"job-url-1"}`))
		case "/index4ai/v1/projects":
			// scope resolution — degrade gracefully with an empty list.
			_, _ = w.Write([]byte(`[]`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	t.Setenv("POMA_API_BASE_URL", srv.URL)

	_, out, err := GrillIngest(context.Background(), nil, GrillIngestInput{
		Token:  "tok",
		URL:    "https://example.com/doc.pdf",
		Labels: map[string]string{"b": "2", "a": "1"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("unexpected tool error: %s (code=%s)", out.Error, out.Code)
	}
	if out.JobID != "job-url-1" {
		t.Fatalf("job_id = %q, want %q", out.JobID, "job-url-1")
	}
	if gotRemoteURL != "https://example.com/doc.pdf" {
		t.Fatalf("X-Remote-URL = %q, want the input url", gotRemoteURL)
	}
	if gotAttrs != `{"labels":["a:1","b:2"]}` {
		t.Fatalf("X-Attributes = %q, want %q", gotAttrs, `{"labels":["a:1","b:2"]}`)
	}
	if len(gotLabels) != 0 {
		t.Fatalf("X-Labels = %q, the legacy header must not be sent", gotLabels)
	}
}

// url is mutually exclusive with the file inputs.
func TestGrillIngestURLMutualExclusion(t *testing.T) {
	_, out, err := GrillIngest(context.Background(), nil, GrillIngestInput{
		Token:    "tok",
		URL:      "https://example.com/doc.pdf",
		FilePath: "/tmp/x.pdf",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Code != CodeInvalidInput {
		t.Fatalf("code = %q, want %q", out.Code, CodeInvalidInput)
	}
}
