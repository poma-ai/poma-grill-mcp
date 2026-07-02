package tools

import (
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
			name:          "current 429 with json retry hint",
			statusCode:    http.StatusTooManyRequests,
			body:          `{"code":"too_many_jobs","message":"Customer accounts can have 1000 active jobs at a time.","retry_after_seconds":7}`,
			wantOK:        true,
			wantRetry:     7,
			wantMsgSubstr: "1000 active jobs",
		},
		{
			name:       "429 without retry hint defaults to 5s",
			statusCode: http.StatusTooManyRequests,
			body:       `{"code":"too_many_jobs","message":"at capacity"}`,
			wantOK:     true,
			wantRetry:  5,
		},
		{
			name:       "legacy 403 with too_many_jobs code",
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
