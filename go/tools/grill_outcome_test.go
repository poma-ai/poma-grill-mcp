package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Register validates every tool's schemas (jsonschema-go rejects shared
// sub-schema pointers within one tool, for instance). The unit tests call the
// tool functions directly and would never notice a schema that panics at
// startup, so exercise Register itself.
func TestRegisterToolsDoesNotPanic(t *testing.T) {
	Register(mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil))
}

// The gateway's optional `grill` object (poma-services-go#133) is how a
// customer learns that a re-ingest was a dedup hit — the job succeeds while
// grill keeps the existing document. Absent from older gateways, so the parse
// must tolerate both shapes and never invent the object.
func TestJobStatusParseGrillOutcome(t *testing.T) {
	t.Run("without grill object", func(t *testing.T) {
		var s jobStatusFull
		if err := json.Unmarshal([]byte(`{"is_terminal":true,"status":"done"}`), &s); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if s.Grill != nil {
			t.Fatalf("grill = %+v, want nil", s.Grill)
		}
		b, _ := json.Marshal(s)
		if strings.Contains(string(b), `"grill"`) {
			t.Fatalf("grill key must be omitted when absent; json=%s", b)
		}
	})
	t.Run("with grill object", func(t *testing.T) {
		var s jobStatusFull
		body := `{"is_terminal":true,"status":"done","grill":{"deduplicated":true,"doc_id":"doc-orig","replaced_doc_ids":["doc-old-1","doc-old-2"]}}`
		if err := json.Unmarshal([]byte(body), &s); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if s.Grill == nil || !s.Grill.Deduplicated || s.Grill.DocID != "doc-orig" || len(s.Grill.ReplacedDocIDs) != 2 {
			t.Fatalf("grill = %+v, want deduplicated=true doc_id=doc-orig 2 replaced ids", s.Grill)
		}
	})
	t.Run("lastGrillOutcome picks the latest event that carries one", func(t *testing.T) {
		events := []jobStatusFull{
			{Status: "queued"},
			{Status: "chunked", Grill: &jobGrillOutcome{DocID: "early"}},
			{Status: "done", IsTerminal: true, Grill: &jobGrillOutcome{Deduplicated: true, DocID: "final"}},
		}
		if got := lastGrillOutcome(events); got == nil || got.DocID != "final" {
			t.Fatalf("lastGrillOutcome = %+v, want doc_id=final", got)
		}
		if got := lastGrillOutcome([]jobStatusFull{{Status: "done"}}); got != nil {
			t.Fatalf("lastGrillOutcome = %+v, want nil", got)
		}
	})
}

// grill_jobs_status must surface the gateway's grill object per result, and
// omit it entirely when the gateway did not send one. The JSON shape asserted
// here is the contract the Node implementation mirrors (node/test/smoke.ts).
func TestGrillJobsStatusSurfacesGrillOutcome(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/jobs/job-dedup/"):
			_, _ = w.Write([]byte(`{"is_terminal":true,"status":"done","grill":{"deduplicated":true,"doc_id":"doc-orig","replaced_doc_ids":[]}}`))
		case strings.Contains(r.URL.Path, "/jobs/job-replaced/"):
			_, _ = w.Write([]byte(`{"is_terminal":true,"status":"done","grill":{"deduplicated":false,"doc_id":"job-replaced","replaced_doc_ids":["doc-old"]}}`))
		default:
			_, _ = w.Write([]byte(`{"is_terminal":true,"status":"done"}`))
		}
	}))
	defer srv.Close()
	t.Setenv("POMA_API_BASE_URL", srv.URL)

	_, out, err := GrillJobsStatus(context.Background(), nil, GrillJobsStatusInput{
		JobIDs: []string{"job-dedup", "job-replaced", "job-plain"},
		Token:  "test-token",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Error != "" || out.DoneCount != 3 {
		t.Fatalf("out = %+v, want no error and done_count=3", out)
	}

	b, err := json.Marshal(out.Results)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Exact wire shape: deduplicated always present; doc_id present when set;
	// replaced_doc_ids omitted when empty; grill omitted when the gateway did
	// not send it.
	want := `[` +
		`{"job_id":"job-dedup","status":"done","is_terminal":true,"grill":{"deduplicated":true,"doc_id":"doc-orig"}},` +
		`{"job_id":"job-replaced","status":"done","is_terminal":true,"grill":{"deduplicated":false,"doc_id":"job-replaced","replaced_doc_ids":["doc-old"]}},` +
		`{"job_id":"job-plain","status":"done","is_terminal":true}` +
		`]`
	if string(b) != want {
		t.Fatalf("results json mismatch\n got: %s\nwant: %s", b, want)
	}
}

// grill_ingest_resume (same code path as grill_ingest_sync after upload) must
// lift the grill object of the terminal SSE event to the top level of the
// output, next to job_id and events.
func TestGrillIngestResumeSurfacesGrillOutcome(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/status/v1/jobs/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: job_status\ndata: {\"is_terminal\":false,\"status\":\"queued\"}\n\n"))
		_, _ = w.Write([]byte("event: job_status\ndata: {\"is_terminal\":true,\"status\":\"done\",\"grill\":{\"deduplicated\":true,\"doc_id\":\"doc-orig\",\"replaced_doc_ids\":[]}}\n\n"))
	}))
	defer srv.Close()
	t.Setenv("POMA_API_BASE_URL", srv.URL)

	_, out, err := GrillIngestResume(context.Background(), nil, GrillIngestResumeInput{JobID: "job-1", Token: "test-token"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("unexpected tool error: %+v", out.GrillError)
	}
	b, _ := json.Marshal(out)
	want := `{"job_id":"job-1","events":[{"is_terminal":false,"status":"queued"},{"is_terminal":true,"status":"done","grill":{"deduplicated":true,"doc_id":"doc-orig"}}],"grill":{"deduplicated":true,"doc_id":"doc-orig"}}`
	if string(b) != want {
		t.Fatalf("output json mismatch\n got: %s\nwant: %s", b, want)
	}
}
