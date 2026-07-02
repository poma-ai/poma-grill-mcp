package tools

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// interpretTooManyJobs is the only backpressure channel the calling agent sees,
// so these cases lock in: the current 429+JSON signal, the legacy 403/plaintext
// signals, the retry_after_seconds parse (and 5s default), and that unrelated
// responses are left alone.
func TestInterpretTooManyJobs(t *testing.T) {
	tests := []struct {
		name          string
		statusCode    int
		body          string
		wantOK        bool
		wantRetry     int
		wantMsgSubstr string
	}{
		{
			name:          "current 429 with numeric code, reason and retry hint",
			statusCode:    http.StatusTooManyRequests,
			body:          `{"code":429,"reason":"too_many_jobs","message":"Customer accounts can have 1000 active jobs at a time.","retry_after_seconds":7}`,
			wantOK:        true,
			wantRetry:     7,
			wantMsgSubstr: "1000 active jobs",
		},
		{
			name:       "429 without retry hint defaults to 5s",
			statusCode: http.StatusTooManyRequests,
			body:       `{"code":429,"reason":"too_many_jobs","message":"at capacity"}`,
			wantOK:     true,
			wantRetry:  5,
		},
		{
			name:          "post-migration 429 with error field (message→error rename)",
			statusCode:    http.StatusTooManyRequests,
			body:          `{"code":429,"reason":"too_many_jobs","error":"Queue full at 500 jobs.","retry_after_seconds":10}`,
			wantOK:        true,
			wantRetry:     10,
			wantMsgSubstr: "Queue full at 500 jobs.",
		},
		{
			name:       "legacy 403 with string too_many_jobs code",
			statusCode: http.StatusForbidden,
			body:       `{"code":"too_many_jobs"}`,
			wantOK:     true,
			wantRetry:  5,
		},
		{
			name:       "legacy plaintext body",
			statusCode: http.StatusForbidden,
			body:       "too many jobs",
			wantOK:     true,
			wantRetry:  5,
		},
		{
			name:       "quota_exceeded is not capacity backpressure",
			statusCode: http.StatusForbidden,
			body:       `{"code":"quota_exceeded"}`,
			wantOK:     false,
		},
		{
			name:       "created is not an error",
			statusCode: http.StatusCreated,
			body:       `{"job_id":"abc"}`,
			wantOK:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, retryAfter, ok := interpretTooManyJobs(tt.statusCode, []byte(tt.body))
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if retryAfter != tt.wantRetry {
				t.Fatalf("retryAfter = %d, want %d", retryAfter, tt.wantRetry)
			}
			// The message must instruct the agent to back off, not just report an error.
			if !strings.Contains(msg, "retry") || !strings.Contains(msg, "Do NOT send more ingests") {
				t.Fatalf("message missing throttle instruction: %q", msg)
			}
			if tt.wantMsgSubstr != "" && !strings.Contains(msg, tt.wantMsgSubstr) {
				t.Fatalf("message missing server detail %q: %q", tt.wantMsgSubstr, msg)
			}
		})
	}
}

// interpretAuthError must key on the migrated envelope's snake_case `reason`
// (where `code` is the numeric HTTP status) while still honouring the legacy
// envelope that carries the discriminator as a string `code`. These cases lock
// in both shapes and confirm capacity/quota reasons stay out of the auth channel.
func TestInterpretAuthError(t *testing.T) {
	tests := []struct {
		name          string
		statusCode    int
		body          string
		wantEmpty     bool
		wantMsgSubstr string
	}{
		{
			name:          "new-shape 403 reason project_protected",
			statusCode:    http.StatusForbidden,
			body:          `{"code":403,"reason":"project_protected","error":"project is protected"}`,
			wantMsgSubstr: "this project is protected",
		},
		{
			name:          "new-shape 403 reason forbidden",
			statusCode:    http.StatusForbidden,
			body:          `{"code":403,"reason":"forbidden","error":"no access"}`,
			wantMsgSubstr: "access denied",
		},
		{
			name:       "new-shape 403 reason too_many_jobs is capacity, not auth",
			statusCode: http.StatusForbidden,
			body:       `{"code":403,"reason":"too_many_jobs"}`,
			wantEmpty:  true,
		},
		{
			name:       "new-shape 403 reason quota_exceeded is not auth",
			statusCode: http.StatusForbidden,
			body:       `{"code":403,"reason":"quota_exceeded"}`,
			wantEmpty:  true,
		},
		{
			name:          "legacy 403 string code project_protected",
			statusCode:    http.StatusForbidden,
			body:          `{"code":"project_protected"}`,
			wantMsgSubstr: "this project is protected",
		},
		{
			name:          "legacy 403 string code forbidden",
			statusCode:    http.StatusForbidden,
			body:          `{"code":"forbidden"}`,
			wantMsgSubstr: "access denied",
		},
		{
			name:       "legacy 403 string code quota_exceeded is not auth",
			statusCode: http.StatusForbidden,
			body:       `{"code":"quota_exceeded"}`,
			wantEmpty:  true,
		},
		{
			name:          "unknown 403 falls back to generic forbidden",
			statusCode:    http.StatusForbidden,
			body:          `{"code":403,"reason":"some_other_reason","error":"nope"}`,
			wantMsgSubstr: "forbidden (HTTP 403)",
		},
		{
			name:          "401 status branch preserved",
			statusCode:    http.StatusUnauthorized,
			body:          "",
			wantMsgSubstr: "authentication failed (HTTP 401)",
		},
		{
			name:          "402 status branch preserved",
			statusCode:    http.StatusPaymentRequired,
			body:          "",
			wantMsgSubstr: "credits exceeded (HTTP 402)",
		},
		{
			name:       "non-auth status returns empty",
			statusCode: http.StatusCreated,
			body:       `{"job_id":"abc"}`,
			wantEmpty:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := interpretAuthError(context.Background(), "tok", tt.statusCode, []byte(tt.body), "grill ingest")
			if tt.wantEmpty {
				if msg != "" {
					t.Fatalf("expected empty msg, got %q", msg)
				}
				return
			}
			if msg == "" {
				t.Fatalf("expected non-empty msg for %q", tt.body)
			}
			if tt.wantMsgSubstr != "" && !strings.Contains(msg, tt.wantMsgSubstr) {
				t.Fatalf("message missing %q: %q", tt.wantMsgSubstr, msg)
			}
		})
	}
}
