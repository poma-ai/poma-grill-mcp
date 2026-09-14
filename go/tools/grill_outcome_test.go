package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// A malformed or unexpectedly-typed `grill` must never invalidate the status
// event it rides on. Before `grill` was a typed field it was an unknown key and
// was ignored, so a strict decode would be a regression with two teeth: the SSE
// loop in readJobStatusStream drops events it cannot unmarshal, so a rejected
// terminal event means the wait never returns; and peekJobStatus surfaces the
// error, so grill_jobs_status reports parse_error for a job that succeeded.
//
// The wanted values also pin Go to Node's grillOutcomeFields coercion, so the
// two implementations still agree byte-for-byte on malformed input.
func TestJobStatusTolerantGrillDecode(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string // marshalled `grill` value, "" meaning the key must be absent
	}{
		{"grill is a string", `{"is_terminal":true,"status":"done","grill":"x"}`, ""},
		{"grill is an array", `{"is_terminal":true,"status":"done","grill":["x"]}`, ""},
		{"grill is a number", `{"is_terminal":true,"status":"done","grill":7}`, ""},
		{"grill is null", `{"is_terminal":true,"status":"done","grill":null}`, ""},
		{"grill is empty object", `{"is_terminal":true,"status":"done","grill":{}}`, `{"deduplicated":false}`},
		{"deduplicated wrong type", `{"is_terminal":true,"status":"done","grill":{"deduplicated":"true","doc_id":"d"}}`, `{"deduplicated":false,"doc_id":"d"}`},
		{"doc_id wrong type", `{"is_terminal":true,"status":"done","grill":{"deduplicated":true,"doc_id":123}}`, `{"deduplicated":true}`},
		{"doc_id empty string", `{"is_terminal":true,"status":"done","grill":{"deduplicated":true,"doc_id":""}}`, `{"deduplicated":true}`},
		{"replaced_doc_ids wrong type", `{"is_terminal":true,"status":"done","grill":{"deduplicated":false,"replaced_doc_ids":"a"}}`, `{"deduplicated":false}`},
		{"replaced_doc_ids empty", `{"is_terminal":true,"status":"done","grill":{"deduplicated":false,"replaced_doc_ids":[]}}`, `{"deduplicated":false}`},
		// String(null) is "null" in JS; neither side may hand an agent that as a doc id.
		{"replaced_doc_ids drops non-strings", `{"is_terminal":true,"status":"done","grill":{"deduplicated":false,"replaced_doc_ids":["a",null,7,"","b"]}}`, `{"deduplicated":false,"replaced_doc_ids":["a","b"]}`},
		{"unknown extra key", `{"is_terminal":true,"status":"done","grill":{"deduplicated":true,"future":1}}`, `{"deduplicated":true}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var s jobStatusFull
			if err := json.Unmarshal([]byte(tc.body), &s); err != nil {
				t.Fatalf("unmarshal must not fail on a malformed grill: %v", err)
			}
			// The event itself must survive intact — this is what the SSE loop
			// and peekJobStatus actually act on.
			if !s.IsTerminal || s.Status != "done" {
				t.Fatalf("status event lost: is_terminal=%v status=%q", s.IsTerminal, s.Status)
			}
			got := ""
			if s.Grill != nil {
				b, err := json.Marshal(s.Grill)
				if err != nil {
					t.Fatalf("marshal grill: %v", err)
				}
				got = string(b)
			}
			if got != tc.want {
				t.Fatalf("grill = %s, want %s", orNone(got), orNone(tc.want))
			}
			// And the key must be genuinely absent, not null, when there is no outcome.
			b, _ := json.Marshal(s)
			if tc.want == "" && strings.Contains(string(b), `"grill"`) {
				t.Fatalf("grill key must be omitted; json=%s", b)
			}
		})
	}
}

func orNone(s string) string {
	if s == "" {
		return "<absent>"
	}
	return s
}

// The SSE path is where a rejected event does real damage: readJobStatusStream
// drops what it cannot unmarshal, so a terminal event carrying a malformed
// `grill` would never terminate the wait — the tool hangs until the gateway
// closes the stream and then returns isError:false with a truncated events
// array and no explanation. Assert the terminal event survives and the bad
// grill is simply absent.
func TestGrillIngestResumeTolerantOfMalformedGrill(t *testing.T) {
	held := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/status/v1/jobs/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: job_status\ndata: {\"is_terminal\":false,\"status\":\"queued\"}\n\n"))
		_, _ = w.Write([]byte("event: job_status\ndata: {\"is_terminal\":true,\"status\":\"done\",\"grill\":{\"deduplicated\":\"yes\"}}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Hold the stream open, as a real gateway does. This is what makes a
		// dropped terminal event a hang rather than a truncated result: the
		// reader blocks here until the context deadline instead of returning.
		<-held
	}))
	defer srv.Close()
	// Registered after defer srv.Close() so LIFO releases the handler first —
	// srv.Close() waits for in-flight requests and would otherwise deadlock.
	defer close(held)
	t.Setenv("POMA_API_BASE_URL", srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	_, out, err := GrillIngestResume(ctx, nil, GrillIngestResumeInput{JobID: "job-1", Token: "test-token"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The stream is still open, so returning at all means the terminal event was
	// honoured. Anything near the deadline means it was dropped and we waited.
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("took %s: terminal event was dropped and the read blocked", elapsed)
	}
	if out.Error != "" {
		t.Fatalf("unexpected tool error: %+v", out.GrillError)
	}
	b, _ := json.Marshal(out)
	// `{"deduplicated":"yes"}` is still a JSON object, so it normalizes to
	// deduplicated:false and is lifted like any other outcome — same as Node's
	// grillOutcomeFields. What matters is that the terminal event survived and
	// ended the wait.
	want := `{"job_id":"job-1","events":[{"is_terminal":false,"status":"queued"},{"is_terminal":true,"status":"done","grill":{"deduplicated":false}}],"grill":{"deduplicated":false}}`
	if string(b) != want {
		t.Fatalf("output json mismatch\n got: %s\nwant: %s", b, want)
	}
}

// peekJobStatus feeds grill_jobs_status. A malformed `grill` must not turn a
// finished job into code:"parse_error" with is_terminal:false.
func TestGrillJobsStatusTolerantOfMalformedGrill(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"is_terminal":true,"status":"done","grill":"not-an-object"}`))
	}))
	defer srv.Close()
	t.Setenv("POMA_API_BASE_URL", srv.URL)

	_, out, err := GrillJobsStatus(context.Background(), nil, GrillJobsStatusInput{
		JobIDs: []string{"job-1"},
		Token:  "test-token",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Error != "" || out.DoneCount != 1 || out.FailedCount != 0 {
		t.Fatalf("out = %+v, want no error and done_count=1", out)
	}
	b, _ := json.Marshal(out.Results)
	want := `[{"job_id":"job-1","status":"done","is_terminal":true}]`
	if string(b) != want {
		t.Fatalf("results json mismatch\n got: %s\nwant: %s", b, want)
	}
}
