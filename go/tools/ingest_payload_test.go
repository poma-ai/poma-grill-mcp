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
	if got.Error != "missing API token (x-api-key, Authorization: Bearer, or POMA_API_KEY)" {
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

func TestMCPRequestBodyBytes(t *testing.T) {
	tests := []struct {
		name      string
		mcpMax    string
		ingestMax string
		want      int64
	}{
		{"defaults", "", "", defaultMCPMaxBodyBytes},
		{"unlimited ingest still bounds the MCP body", "", "0", defaultMCPMaxBodyBytes},
		{"huge ingest still bounds the MCP body", "", "9223372036854775807", defaultMCPMaxBodyBytes},
		{"small ingest cap lowers the MCP body", "", "1048576", 1048576/3*4 + (64 << 10)},
		{"explicit override wins", "1000000", "0", 1000000},
		{"explicit override beats a smaller ingest cap", "33554432", "1048576", 33554432},
		{"explicit 0 opts out", "0", "", -1},
		{"invalid override falls back", "not-a-number", "", defaultMCPMaxBodyBytes},
		{"negative override falls back", "-5", "", defaultMCPMaxBodyBytes},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GRILL_MCP_MAX_BODY_BYTES", tt.mcpMax)
			t.Setenv("GRILL_INGEST_MAX_BYTES", tt.ingestMax)
			if got := MCPRequestBodyBytes(); got != tt.want {
				t.Errorf("MCPRequestBodyBytes() = %d, want %d", got, tt.want)
			}
			// 0 would silently mean the SDK's 4 MiB default, never what we intend.
			if got := MCPRequestBodyBytes(); got == 0 {
				t.Error("MCPRequestBodyBytes() = 0, which the SDK reads as its 4 MiB default")
			}
		})
	}
}

func TestBase64BodyBytesOverflow(t *testing.T) {
	const slack = 64 << 10
	threshold := int64((1<<63 - 1 - slack) / 4 * 3)
	if got := base64BodyBytes(threshold); got <= 0 {
		t.Errorf("base64BodyBytes(%d) = %d, want a positive value at the overflow boundary", threshold, got)
	}
	if got := base64BodyBytes(threshold + 1); got != -1 {
		t.Errorf("base64BodyBytes(%d) = %d, want -1 just past the boundary", threshold+1, got)
	}
	if got := base64BodyBytes(0); got != -1 {
		t.Errorf("base64BodyBytes(0) = %d, want -1", got)
	}
}

func TestMCPMaxLineLength(t *testing.T) {
	const floor = int(defaultMCPMaxBodyBytes)
	tests := []struct {
		name      string
		mcpMax    string
		ingestMax string
		want      int
	}{
		// The default ingest ceiling must not be throttled by the transport:
		// grill_ingest accepts 512 MiB, so a frame carrying it has to fit.
		{"defaults size the frame off the ingest ceiling", "", "", int(defaultIngestMaxBytes/3*4 + (64 << 10))},
		{"unlimited ingest uncaps the frame", "", "0", -1},
		// Reached through base64BodyBytes' overflow return, not through "unlimited".
		// TestMCPRequestBodyBytes expects 16 MiB for this same input: the HTTP body
		// keeps its bound when the scaled value overflows, the stdio frame does not.
		{"a near-overflow ingest cap uncaps the frame", "", "9223372036854775807", -1},
		{"a small ingest cap is floored, not honoured", "", "100000", floor},
		{"an ingest cap just under the floor is floored", "", "12000000", floor},
		{"an ingest cap above the floor raises it", "", "104857600", 104857600/3*4 + (64 << 10)},
		// GRILL_MCP_MAX_BODY_BYTES is the HTTP knob; binding it here would make an
		// ingest-file setting a fatal bound on calls that carry no file.
		{"the HTTP body knob does not reach stdio", "1000", "", int(defaultIngestMaxBytes/3*4 + (64 << 10))},
		{"the HTTP opt-out does not uncap stdio", "0", "", int(defaultIngestMaxBytes/3*4 + (64 << 10))},
		{"invalid ingest override falls back to the default", "", "not-a-number", int(defaultIngestMaxBytes/3*4 + (64 << 10))},
		{"negative ingest override falls back to the default", "", "-5", int(defaultIngestMaxBytes/3*4 + (64 << 10))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GRILL_MCP_MAX_BODY_BYTES", tt.mcpMax)
			t.Setenv("GRILL_INGEST_MAX_BYTES", tt.ingestMax)
			got := MCPMaxLineLength()
			if got != tt.want {
				t.Errorf("MCPMaxLineLength() = %d, want %d", got, tt.want)
			}
			// 0 would silently mean the SDK's 16 MiB default, never what we intend.
			if got == 0 {
				t.Error("MCPMaxLineLength() = 0, which the SDK reads as its 16 MiB default")
			}
			// A positive result below the floor would let an ingest cap bound
			// unrelated calls (grill_search, tools/list).
			if got > 0 && got < floor {
				t.Errorf("MCPMaxLineLength() = %d, below the %d floor", got, floor)
			}
		})
	}
}
