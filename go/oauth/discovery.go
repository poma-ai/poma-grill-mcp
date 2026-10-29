package oauth

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// protectedResourceMeta is the RFC 9728 Protected Resource Metadata response.
type protectedResourceMeta struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	BearerMethodsSupported []string `json:"bearer_methods_supported"`
	ScopesSupported        []string `json:"scopes_supported"`
}

// listenAddr is stored at Register() time so publicBaseURL can fall back to it.
var listenAddr string

// publicBaseURL returns the MCP server's own public base URL (no trailing slash).
// Priority:
//  1. POMA_MCP_PUBLIC_URL env var (always preferred — set this in production)
//  2. http://localhost:<listenAddr port>
//
// X-Forwarded-Proto / X-Forwarded-Host headers are intentionally NOT trusted: an
// attacker who can inject those headers could steer an MCP SDK to a malicious
// authorization server via the WWW-Authenticate challenge. Operators behind a
// reverse proxy must set POMA_MCP_PUBLIC_URL explicitly.
func publicBaseURL() string {
	if v := os.Getenv("POMA_MCP_PUBLIC_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	// Fallback: derive from listen address.
	port := listenAddr
	if idx := strings.LastIndex(port, ":"); idx >= 0 {
		port = port[idx:] // ":8080"
	}
	return "http://localhost" + port
}

// authServerURL is the OAuth authorization server advertised to MCP clients.
//
// The api answers OAuth discovery on the index4ai hosts too, but the document names the
// poma-ai.com host of the same environment as issuer (api.index4.ai -> api.poma-ai.com,
// api-dev.index4.ai -> api-dev.poma-ai.com), and RFC 8414 clients reject metadata whose
// issuer differs from the URL they fetched. So the MCP must advertise the poma-ai.com host
// even though its API calls go to api(-dev).index4.ai.
//
// Priority: POMA_AUTH_SERVER_URL; else the origin of POMA_API_BASE_URL with a .index4.ai
// host mapped to the matching .poma-ai.com host; else https://api.poma-ai.com. Deriving
// from the API host keeps a dev deployment on the dev issuer: a fixed prod default would
// send dev users to the prod login and mint tokens the dev pod's JWT secret rejects.
// Mirrors index4ai-mcp oauth/discovery.go.
func authServerURL() string {
	if v := strings.TrimRight(strings.TrimSpace(os.Getenv("POMA_AUTH_SERVER_URL")), "/"); v != "" {
		return v
	}
	u, err := url.Parse(strings.TrimSpace(os.Getenv("POMA_API_BASE_URL")))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "https://api.poma-ai.com"
	}
	host := u.Host
	if h, ok := strings.CutSuffix(host, ".index4.ai"); ok {
		host = h + ".poma-ai.com"
	}
	return u.Scheme + "://" + host
}

// handleProtectedResourceMeta serves GET /.well-known/oauth-protected-resource.
// Required by RFC 9728 / MCP spec so the SDK can discover the authorization server.
// The MCP is the protected resource; the api is the authorization server.
func handleProtectedResourceMeta(w http.ResponseWriter, r *http.Request) {
	slog.Debug("oauth.protected_resource_meta", "remote_addr", r.RemoteAddr)

	meta := protectedResourceMeta{
		Resource:               publicBaseURL(),
		AuthorizationServers:   []string{authServerURL()},
		BearerMethodsSupported: []string{"header"},
		ScopesSupported:        []string{"mcp.tools.read", "mcp.tools.write"},
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	// Allow browser-based OAuth clients to fetch this discovery document cross-origin.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(meta) //nolint:errcheck
}
