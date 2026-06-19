// Package oauth implements the OAuth 2.0 protected-resource side of poma-grill-mcp.
//
// The authorization server now lives in poma-services-go. This package is
// responsible for:
//  1. Advertising the protected-resource metadata (RFC 9728) so the MCP SDK can
//     discover which authorization server to use.
//  2. Validating incoming Bearer tokens (HMAC-SHA256 JWTs issued by the api) via
//     RequireBearer middleware before handing off to the MCP handler.
package oauth

import (
	"log"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

// jwtSecret is loaded from POMA_API_JWT_SECRET at Register() time.
var jwtSecret []byte

// mcpResourceURI is loaded from POMA_MCP_RESOURCE at Register() time.
// It must match the "resource" field in the protected-resource metadata.
var mcpResourceURI string

// loadJWTConfig reads POMA_API_JWT_SECRET and POMA_MCP_RESOURCE.
// It is called from Register() so that fail-fast happens at startup (when the
// HTTP server is being wired up), not at package init time (which would break
// tests that import the package without intending to run a server).
// If both vars are unset the function is a no-op (legacy api-key-only mode).
func loadJWTConfig() {
	secret := os.Getenv("POMA_API_JWT_SECRET")
	resource := os.Getenv("POMA_MCP_RESOURCE")

	if secret == "" && resource == "" {
		return // Legacy mode — JWT verification disabled.
	}
	if secret == "" {
		log.Fatal("oauth: POMA_API_JWT_SECRET is required when POMA_MCP_RESOURCE is set")
	}
	if resource == "" {
		log.Fatal("oauth: POMA_MCP_RESOURCE is required when POMA_API_JWT_SECRET is set")
	}
	jwtSecret = []byte(secret)
	mcpResourceURI = strings.TrimRight(resource, "/") + "/"
	slog.Info("oauth: JWT verification enabled", "resource", mcpResourceURI)
}

// Register mounts the RFC 9728 protected-resource well-known route on mux.
// addr is the listen address (e.g. ":8080") used as a fallback for URL derivation.
// This is also where JWT env-var validation happens — missing required vars cause
// log.Fatal so the server refuses to start rather than silently accepting all tokens.
func Register(mux *http.ServeMux, addr string) {
	listenAddr = addr
	loadJWTConfig()

	mux.HandleFunc("GET /.well-known/oauth-protected-resource", handleProtectedResourceMeta)

	slog.Info("oauth: protected-resource metadata registered",
		"well_known", "/.well-known/oauth-protected-resource",
		"authorization_server", apiBaseURL(),
	)
}

// oauthClaims is the JWT payload expected from the api's GenerateOAuthAccessToken.
type oauthClaims struct {
	jwt.RegisteredClaims
	AccountID string `json:"account_id"`
	Project   string `json:"project"`
	Scope     string `json:"scope"`
	ClientID  string `json:"client_id"`
	TokenType string `json:"typ"`
}

// RequireBearer returns middleware that validates an HMAC-SHA256 JWT Bearer token.
//
// Accepted tokens must satisfy:
//   - Valid HMAC-SHA256 signature (key: POMA_API_JWT_SECRET)
//   - exp > now
//   - typ == "oauth-access"
//   - aud contains POMA_MCP_RESOURCE
//
// Tokens that fail JWT parsing (not structurally JWT) are treated as legacy api
// keys and passed to next so that the downstream apiKeyMiddleware can handle them.
// Tokens that parse as JWT but fail signature or claims validation are rejected 401.
//
// x-api-key header always bypasses JWT verification (legacy api-key path).
func RequireBearer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// x-api-key: legacy bypass — pass straight through.
		if r.Header.Get("x-api-key") != "" {
			next.ServeHTTP(w, r)
			return
		}

		authHeader := r.Header.Get("Authorization")
		if len(authHeader) > 7 && strings.EqualFold(authHeader[:7], "bearer ") {
			token := authHeader[7:]

			// Try to parse as JWT. A non-JWT token (no dots, wrong structure) returns
			// a parse error that is NOT a validation error — let it fall through to
			// apiKeyMiddleware as a legacy api key.
			parsed, err := jwt.ParseWithClaims(token, &oauthClaims{}, func(t *jwt.Token) (any, error) {
				if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
					return nil, jwt.ErrSignatureInvalid
				}
				return jwtSecret, nil
			})

			if err != nil {
				// Distinguish parse errors (malformed token) from validation errors
				// (bad signature, expired, etc.).
				if isJWTStructure(token) {
					// Structurally a JWT but failed validation — reject it.
					slog.Warn("oauth: bearer token rejected", "err", err, "remote", r.RemoteAddr)
					sendChallenge(w, r)
					return
				}
				// Not a JWT — treat as legacy api key. Pass to next.
				next.ServeHTTP(w, r)
				return
			}

			claims, ok := parsed.Claims.(*oauthClaims)
			if !ok || !parsed.Valid {
				sendChallenge(w, r)
				return
			}

			// Enforce typ == "oauth-access" to prevent login JWTs being accepted.
			if claims.TokenType != "oauth-access" {
				slog.Warn("oauth: bearer token has wrong typ", "typ", claims.TokenType, "remote", r.RemoteAddr)
				sendChallenge(w, r)
				return
			}

			// Enforce aud contains our resource URI.
			if !audContains(claims.Audience, mcpResourceURI) {
				slog.Warn("oauth: bearer token aud mismatch",
					"aud", claims.Audience,
					"expected", mcpResourceURI,
					"remote", r.RemoteAddr,
				)
				sendChallenge(w, r)
				return
			}

			// Valid OAuth bearer — pass through.
			next.ServeHTTP(w, r)
			return
		}

		// No credentials at all — emit 401 challenge.
		sendChallenge(w, r)
	})
}

// sendChallenge writes a 401 with the WWW-Authenticate header pointing to the
// protected-resource metadata URL, triggering the MCP SDK's OAuth flow.
func sendChallenge(w http.ResponseWriter, _ *http.Request) {
	base := publicBaseURL()
	w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+base+`/.well-known/oauth-protected-resource"`)
	w.WriteHeader(http.StatusUnauthorized)
}

// isJWTStructure reports whether s looks like a JWT (three dot-separated parts).
func isJWTStructure(s string) bool {
	first := strings.Index(s, ".")
	if first < 0 {
		return false
	}
	second := strings.Index(s[first+1:], ".")
	return second >= 0
}

// audContains reports whether aud contains target.
func audContains(aud jwt.ClaimStrings, target string) bool {
	for _, a := range aud {
		if a == target {
			return true
		}
	}
	return false
}
