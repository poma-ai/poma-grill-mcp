package tools

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Missing token still yields the same prose message and HTTP status as before,
// plus the additive `code` field for HTTP-mode parity with the MCP tools.
func TestHandleIngestUploadMissingToken(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/ingest-upload", bytes.NewReader([]byte("hello world")))
	rec := httptest.NewRecorder()

	HandleIngestUpload(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("http status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	var got GrillError
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal response: %v; body=%s", err, rec.Body.String())
	}
	if got.Error != "missing API token (x-api-key, Authorization: Bearer, POMA_GRILL_API_KEY, or POMA_API_KEY)" {
		t.Fatalf("error message changed: %q", got.Error)
	}
	if got.Code != CodeMissingToken {
		t.Fatalf("code = %q, want %q", got.Code, CodeMissingToken)
	}
}

// Regression test: before this fix, HandleIngestUpload classified EVERY non-201
// response from the upstream Grill API as upstream_error, even 401/402/403/429 —
// the exact class of terminal-vs-transient confusion this whole feature exists
// to eliminate. A 401 must come back as auth_expired (terminal, not retryable),
// not upstream_error.
func TestHandleIngestUploadClassifiesAuthErrorNotUpstream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	t.Setenv("POMA_API_BASE_URL", srv.URL)

	req := httptest.NewRequest(http.MethodPost, "/ingest-upload", bytes.NewReader([]byte("hello world")))
	req = req.WithContext(WithAPIToken(req.Context(), "tok"))
	rec := httptest.NewRecorder()

	HandleIngestUpload(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("http status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	var got GrillError
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal response: %v; body=%s", err, rec.Body.String())
	}
	if got.Code != CodeAuthExpired {
		t.Fatalf("code = %q, want %q (must not be upstream_error); body=%s", got.Code, CodeAuthExpired, rec.Body.String())
	}
	if got.Retryable {
		t.Fatal("auth_expired must not be retryable")
	}
}

// A 429 too_many_jobs from the upstream API must classify as too_many_jobs
// (retryable), not a generic upstream_error.
func TestHandleIngestUploadClassifiesTooManyJobs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"code":"too_many_jobs","retry_after_seconds":5}`))
	}))
	defer srv.Close()
	t.Setenv("POMA_API_BASE_URL", srv.URL)

	req := httptest.NewRequest(http.MethodPost, "/ingest-upload", bytes.NewReader([]byte("hello world")))
	req = req.WithContext(WithAPIToken(req.Context(), "tok"))
	rec := httptest.NewRecorder()

	HandleIngestUpload(rec, req)

	var got GrillError
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal response: %v; body=%s", err, rec.Body.String())
	}
	if got.Code != CodeTooManyJobs {
		t.Fatalf("code = %q, want %q; body=%s", got.Code, CodeTooManyJobs, rec.Body.String())
	}
	if !got.Retryable {
		t.Fatal("too_many_jobs must be retryable")
	}
}

// A genuine non-auth, non-4xx-throttle upstream failure (5xx) still classifies
// as a retryable upstream_error; a 4xx (other than auth/throttle) is a
// non-retryable upstream_error.
func TestHandleIngestUploadUpstream5xxIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"unavailable"}`))
	}))
	defer srv.Close()
	t.Setenv("POMA_API_BASE_URL", srv.URL)

	req := httptest.NewRequest(http.MethodPost, "/ingest-upload", bytes.NewReader([]byte("hello world")))
	req = req.WithContext(WithAPIToken(req.Context(), "tok"))
	rec := httptest.NewRecorder()

	HandleIngestUpload(rec, req)

	var got GrillError
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal response: %v; body=%s", err, rec.Body.String())
	}
	if got.Code != CodeUpstreamError {
		t.Fatalf("code = %q, want %q; body=%s", got.Code, CodeUpstreamError, rec.Body.String())
	}
	if !got.Retryable {
		t.Fatal("5xx upstream_error must be retryable")
	}
}
