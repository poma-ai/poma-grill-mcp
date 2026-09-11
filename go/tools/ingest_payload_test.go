package tools

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

// On the hosted (HTTP) server file_path would read the pod's filesystem
// (found 2026-09-09: mcp.poma-ai.com advertises the path-based ingest tools
// and runs with no GRILL_INGEST_ALLOWED_PREFIX). Refuse it there unless the
// operator opts a directory in; stdio mode is unchanged.
func TestReadFileForIngestRefusedInHTTPMode(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GRILL_INGEST_ALLOWED_PREFIX", "")

	SetHTTPMode(true)
	t.Cleanup(func() { SetHTTPMode(false) })
	if _, err := readFileForIngest(p); err == nil || !strings.Contains(err.Error(), "hosted HTTP server") {
		t.Fatalf("http mode without prefix must refuse file_path, got err=%v", err)
	}
	// The refusal surfaces as invalid_input through the payload resolver.
	if _, _, err := resolveGrillIngestPayload(GrillIngestInput{FilePath: p}); err == nil {
		t.Fatal("resolveGrillIngestPayload must propagate the refusal")
	}

	// Operator opt-in: a prefix re-enables reads, still confined to it.
	t.Setenv("GRILL_INGEST_ALLOWED_PREFIX", dir)
	if data, err := readFileForIngest(p); err != nil || string(data) != "hello" {
		t.Fatalf("http mode with prefix should read the file, got %q err=%v", data, err)
	}

	SetHTTPMode(false)
	t.Setenv("GRILL_INGEST_ALLOWED_PREFIX", "")
	if data, err := readFileForIngest(p); err != nil || string(data) != "hello" {
		t.Fatalf("stdio mode must be unchanged, got %q err=%v", data, err)
	}
}
