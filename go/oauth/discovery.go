package oauth

import (
	"encoding/json"
	"log/slog"
	"net/http"
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

// apiBaseURL returns the authorization server's (api's) base URL.
// Uses POMA_API_BASE_URL, stripping any version suffix (e.g. "/v3").
// Falls back to https://api.poma-ai.com for safety.
func apiBaseURL() string {
	v := os.Getenv("POMA_API_BASE_URL")
	if v == "" {
		return "https://api.poma-ai.com"
	}
	v = strings.TrimRight(v, "/")
	// Strip /v<N> suffix if present (e.g. "https://api.poma-ai.com/v3" → "https://api.poma-ai.com").
	if idx := strings.LastIndex(v, "/v"); idx > 0 {
		rest := v[idx+1:] // "v3" or "v3/something"
		isVer := len(rest) > 1
		for _, c := range rest[1:] {
			if c < '0' || c > '9' {
				isVer = false
				break
			}
		}
		if isVer {
			v = v[:idx]
		}
	}
	return v
}

// handleProtectedResourceMeta serves GET /.well-known/oauth-protected-resource.
// Required by RFC 9728 / MCP spec so the SDK can discover the authorization server.
// The MCP is the protected resource; the api is the authorization server.
func handleProtectedResourceMeta(w http.ResponseWriter, r *http.Request) {
	slog.Debug("oauth.protected_resource_meta", "remote_addr", r.RemoteAddr)

	meta := protectedResourceMeta{
		Resource:               publicBaseURL() + "/",
		AuthorizationServers:   []string{apiBaseURL()},
		BearerMethodsSupported: []string{"header"},
		ScopesSupported:        []string{"mcp.tools.read", "mcp.tools.write"},
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	// Allow browser-based OAuth clients to fetch this discovery document cross-origin.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(meta) //nolint:errcheck
}
