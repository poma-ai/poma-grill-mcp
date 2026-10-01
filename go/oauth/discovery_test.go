package oauth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The api answers OAuth discovery on api(-dev).index4.ai with issuer api(-dev).poma-ai.com,
// so the advertised authorization server must be that issuer, never the index4ai host, and
// a dev deployment must never fall back to the prod issuer.
func TestProtectedResourceMetaAuthServer(t *testing.T) {
	tests := []struct{ name, base, env, want string }{
		{"unset everything is the prod issuer", "", "", "https://api.poma-ai.com"},
		{"prod index4ai host maps to its issuer", "https://api.index4.ai/index4ai/v1", "", "https://api.poma-ai.com"},
		{"dev index4ai host maps to the dev issuer", "https://api-dev.index4.ai", "", "https://api-dev.poma-ai.com"},
		{"a poma-ai.com host is the issuer itself", "https://api-dev.poma-ai.com", "", "https://api-dev.poma-ai.com"},
		{"explicit override wins, trailing slash trimmed", "https://api.index4.ai", "https://api-dev.poma-ai.com/", "https://api-dev.poma-ai.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("POMA_API_BASE_URL", tt.base)
			t.Setenv("POMA_AUTH_SERVER_URL", tt.env)
			t.Setenv("POMA_MCP_PUBLIC_URL", "https://mcp.poma-ai.com")
			rec := httptest.NewRecorder()
			handleProtectedResourceMeta(rec, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource", nil))
			var meta protectedResourceMeta
			if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil {
				t.Fatal(err)
			}
			if len(meta.AuthorizationServers) != 1 || meta.AuthorizationServers[0] != tt.want {
				t.Fatalf("authorization_servers = %v, want [%s]", meta.AuthorizationServers, tt.want)
			}
			if meta.Resource != "https://mcp.poma-ai.com" {
				t.Fatalf("resource = %q", meta.Resource)
			}
		})
	}
}
