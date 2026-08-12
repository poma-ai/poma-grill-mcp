package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// serializeLabels must produce a deterministic, sorted "key:value,…" header and
// skip empty keys — matching the Node serializeLabels byte-for-byte.
func TestSerializeLabelsSorted(t *testing.T) {
	got := serializeLabels(map[string]string{"b": "2", "a": "1", "  ": "skip"})
	if got != "a:1,b:2" {
		t.Fatalf("serializeLabels = %q, want %q", got, "a:1,b:2")
	}
	if serializeLabels(nil) != "" {
		t.Fatal("serializeLabels(nil) must be empty")
	}
}

// grill_ingest with a url must POST /grill/ingest carrying X-Remote-URL (and the
// serialized X-Labels), no file body, and return the parsed job_id.
func TestGrillIngestURLSendsRemoteURLAndLabels(t *testing.T) {
	var gotRemoteURL, gotLabels string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v3/grill/ingest":
			gotRemoteURL = r.Header.Get("X-Remote-URL")
			gotLabels = r.Header.Get("X-Labels")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"job_id":"job-url-1"}`))
		case "/v3/projects":
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
	if gotLabels != "a:1,b:2" {
		t.Fatalf("X-Labels = %q, want %q", gotLabels, "a:1,b:2")
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
