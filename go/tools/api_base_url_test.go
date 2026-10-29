package tools

import "testing"

// apiBaseURL always targets <origin>/index4ai/v1 and statusAPIBaseURL <origin>/status/v1, including for grill-era values that
// still carry /v3: /v3/ingest does not exist, only /v3/grill/ingest does.
func TestAPIBaseURL(t *testing.T) {
	for _, tt := range []struct{ env, want, status string }{
		{"", "https://api.index4.ai/index4ai/v1", "https://api.index4.ai/status/v1"},
		{"https://api-dev.index4.ai", "https://api-dev.index4.ai/index4ai/v1", "https://api-dev.index4.ai/status/v1"},
		{"https://api-dev.index4.ai/", "https://api-dev.index4.ai/index4ai/v1", "https://api-dev.index4.ai/status/v1"},
		{"https://api.poma-ai.com/v3", "https://api.poma-ai.com/index4ai/v1", "https://api.poma-ai.com/status/v1"},
		{"https://api.index4.ai/index4ai/v1", "https://api.index4.ai/index4ai/v1", "https://api.index4.ai/status/v1"},
		{" https://api-dev.index4.ai/ ", "https://api-dev.index4.ai/index4ai/v1", "https://api-dev.index4.ai/status/v1"},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080/index4ai/v1", "http://127.0.0.1:8080/status/v1"},
	} {
		t.Run(tt.env, func(t *testing.T) {
			t.Setenv("POMA_API_BASE_URL", tt.env)
			t.Setenv("POMA_STATUS_API_BASE_URL", "")
			if got := apiBaseURL(); got != tt.want {
				t.Fatalf("apiBaseURL() = %q, want %q", got, tt.want)
			}
			if got := statusAPIBaseURL(); got != tt.status {
				t.Fatalf("statusAPIBaseURL() = %q, want %q", got, tt.status)
			}
		})
	}
}
