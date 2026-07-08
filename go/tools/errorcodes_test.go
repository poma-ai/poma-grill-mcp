package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The embedded GrillError envelope must promote its fields to the TOP LEVEL of
// each output struct's JSON so the `error` key stays byte-for-byte compatible
// with the pre-envelope shape, and `code`/`retryable`/`retry_after_seconds` are
// purely additive. This locks in the anonymous-embedding contract for every
// output struct.
func TestGrillErrorEmbeddingPromotesFields(t *testing.T) {
	ge := GrillError{Error: "boom", Code: CodeUpstreamError, Retryable: true, RetryAfterSeconds: 7}

	// Each case marshals an output struct carrying the envelope and asserts the
	// promoted keys appear at the top level with the expected values.
	cases := []struct {
		name string
		v    any
	}{
		{"ingest", GrillIngestOutput{JobID: "j1", GrillError: ge}},
		{"search", GrillSearchOutput{Context: "ctx", GrillError: ge}},
		{"docs_list", GrillDocsListOutput{GrillError: ge}},
		{"batch_result", GrillIngestBatchResult{FilePath: "/a", GrillError: ge}},
		{"batch", GrillIngestBatchOutput{GrillError: ge}},
		{"job_status_result", GrillJobStatusResult{JobID: "j1", GrillError: ge}},
		{"jobs_status", GrillJobsStatusOutput{GrillError: ge}},
		{"projects", GrillProjectsOutput{GrillError: ge}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.v)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var m map[string]any
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			// error key must be present at top level and unchanged.
			if got, ok := m["error"].(string); !ok || got != "boom" {
				t.Fatalf("top-level error = %v (present=%v), want %q; json=%s", m["error"], ok, "boom", b)
			}
			if got, ok := m["code"].(string); !ok || got != CodeUpstreamError {
				t.Fatalf("top-level code = %v (present=%v), want %q; json=%s", m["code"], ok, CodeUpstreamError, b)
			}
			if got, ok := m["retryable"].(bool); !ok || !got {
				t.Fatalf("top-level retryable = %v (present=%v), want true; json=%s", m["retryable"], ok, b)
			}
			if got, ok := m["retry_after_seconds"].(float64); !ok || int(got) != 7 {
				t.Fatalf("top-level retry_after_seconds = %v (present=%v), want 7; json=%s", m["retry_after_seconds"], ok, b)
			}
			// There must be no nested "GrillError" object — embedding promotes, not nests.
			if _, nested := m["GrillError"]; nested {
				t.Fatalf("envelope leaked as a nested object; json=%s", b)
			}
		})
	}
}

// A success (no-error) output must omit error/code/retryable/retry_after_seconds
// entirely, so the on-the-wire shape of successful responses is unchanged.
func TestGrillErrorOmitEmptyOnSuccess(t *testing.T) {
	b, err := json.Marshal(GrillIngestOutput{JobID: "j1"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	for _, k := range []string{`"error"`, `"code"`, `"retryable"`, `"retry_after_seconds"`} {
		if strings.Contains(s, k) {
			t.Fatalf("success output should omit %s, got %s", k, s)
		}
	}
	if !strings.Contains(s, `"job_id":"j1"`) {
		t.Fatalf("success output missing job_id: %s", s)
	}
}

// retryable must be true ONLY for the designated transient codes. This table is
// the authoritative contract for client back-off behaviour. It exercises the
// real production isRetryableCode (common.go) — every call site that sets
// GrillError.Retryable derives it from that same function, so this test
// actually protects against a future site drifting from the taxonomy instead
// of just checking a copy of the logic.
func TestRetryableOnlyForTransientCodes(t *testing.T) {
	wantRetryable := map[string]bool{
		CodeMissingToken:     false,
		CodeInvalidInput:     false,
		CodeAuthExpired:      false,
		CodePaymentRequired:  false,
		CodeProjectProtected: false,
		CodeForbidden:        false,
		CodeParseError:       false,
		CodeJobFailed:        false,
		CodeTooManyJobs:      true,
		CodeTransportError:   true,
		CodeStreamError:      true,
		// upstream_error is retryable only when HTTP status >= 500 (checked below).
		CodeUpstreamError: false,
	}
	if len(wantRetryable) != 12 {
		t.Fatalf("expected 12 codes in the taxonomy, got %d", len(wantRetryable))
	}
	for code, want := range wantRetryable {
		if code == "" {
			t.Fatalf("empty code in taxonomy table")
		}
		if got := isRetryableCode(code, 0); got != want {
			t.Fatalf("isRetryableCode(%q, 0) = %v, want %v", code, got, want)
		}
	}
	// upstream_error flips to retryable at 5xx.
	if !isRetryableCode(CodeUpstreamError, 503) {
		t.Fatal("upstream_error with 503 must be retryable")
	}
	if isRetryableCode(CodeUpstreamError, 404) {
		t.Fatal("upstream_error with 404 must NOT be retryable")
	}
}

// A permanent 4xx from the job-status endpoint (e.g. job not found) must NOT be
// reported as a retryable transport_error — that's exactly the bug this spec
// exists to prevent (agents looping on a terminal error they can't distinguish
// from transient backpressure). Regression test for peekJobStatus/GrillJobsStatus
// correctly threading the HTTP status through to the classification.
func TestGrillJobsStatusNon200IsUpstreamErrorNotTransport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"job not found"}`))
	}))
	defer srv.Close()
	t.Setenv("POMA_API_BASE_URL", srv.URL)

	_, out, err := GrillJobsStatus(context.Background(), nil, GrillJobsStatusInput{
		Token:  "tok",
		JobIDs: []string{"job-1"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(out.Results))
	}
	r := out.Results[0]
	if r.Code != CodeUpstreamError {
		t.Fatalf("code = %q, want %q (a 404 must not be transport_error)", r.Code, CodeUpstreamError)
	}
	if r.Retryable {
		t.Fatal("a 404 upstream_error must NOT be retryable")
	}
}

// A 5xx from the job-status endpoint IS a retryable upstream_error.
func TestGrillJobsStatus5xxIsRetryableUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"unavailable"}`))
	}))
	defer srv.Close()
	t.Setenv("POMA_API_BASE_URL", srv.URL)

	_, out, err := GrillJobsStatus(context.Background(), nil, GrillJobsStatusInput{
		Token:  "tok",
		JobIDs: []string{"job-1"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r := out.Results[0]
	if r.Code != CodeUpstreamError {
		t.Fatalf("code = %q, want %q", r.Code, CodeUpstreamError)
	}
	if !r.Retryable {
		t.Fatal("a 503 upstream_error must be retryable")
	}
}

// A genuine network/transport failure (server unreachable) must still classify
// as transport_error and remain retryable.
func TestGrillJobsStatusUnreachableIsTransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // closed immediately: connections to it fail.

	t.Setenv("POMA_API_BASE_URL", srv.URL)

	_, out, err := GrillJobsStatus(context.Background(), nil, GrillJobsStatusInput{
		Token:  "tok",
		JobIDs: []string{"job-1"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r := out.Results[0]
	if r.Code != CodeTransportError {
		t.Fatalf("code = %q, want %q", r.Code, CodeTransportError)
	}
	if !r.Retryable {
		t.Fatal("a transport_error must be retryable")
	}
}

// When every file in a batch fails, the top-level aggregate rollup must still
// carry a non-empty code — it must not be the one error site in the tool that
// leaves `code` empty. It should propagate the (guaranteed non-empty) code from
// the per-file results.
func TestGrillIngestBatchAllFailedAggregateCarriesCode(t *testing.T) {
	_, out, err := GrillIngestBatch(context.Background(), nil, GrillIngestBatchInput{
		Token:     "tok",
		FilePaths: []string{"/nonexistent/path/does-not-exist.txt"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Error == "" {
		t.Fatal("expected a top-level error message")
	}
	if out.Code == "" {
		t.Fatal("expected a non-empty top-level code on the all-failed aggregate")
	}
	if out.Code != CodeInvalidInput {
		t.Fatalf("code = %q, want %q (bad file_path is an input error)", out.Code, CodeInvalidInput)
	}
	if len(out.Results) != 1 || out.Results[0].Code != CodeInvalidInput {
		t.Fatalf("per-file result code mismatch: %+v", out.Results)
	}
}
