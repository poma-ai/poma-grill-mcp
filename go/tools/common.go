package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/poma-ai/poma-cli/pkg/client"
)

const (
	defaultAPIBaseURL    = "https://api.poma-ai.com"
	defaultVersionPrefix = "/v3"
	defaultStatusPrefix  = "/status/v1"
)

// apiBaseURLVersionSuffixRE matches a trailing API version path segment: /v then digits (e.g. /v2, /v10).
var apiBaseURLVersionSuffixRE = regexp.MustCompile(`/v[0-9]+$`)

func apiBaseURL() string {
	// If POMA_API_BASE_URL is set, use it.
	if v := os.Getenv("POMA_API_BASE_URL"); v != "" {
		if apiBaseURLVersionSuffixRE.MatchString(v) {
			// If it already has a version suffix, use it.
			return strings.TrimRight(v, "/")
		}
		return strings.TrimRight(v, "/") + defaultVersionPrefix
	}
	return defaultAPIBaseURL + defaultVersionPrefix
}

func statusAPIBaseURL() string {
	// If POMA_STATUS_API_BASE_URL is set, use it.
	if v := os.Getenv("POMA_STATUS_API_BASE_URL"); v != "" {
		if apiBaseURLVersionSuffixRE.MatchString(v) {
			// If it already has a version suffix, use it.
			return strings.TrimRight(v, "/")
		}
		return strings.TrimRight(v, "/") + defaultStatusPrefix
	}
	// If POMA_API_BASE_URL is set, use it and append /status/v1.
	if v := os.Getenv("POMA_API_BASE_URL"); v != "" {
		return strings.TrimRight(v, "/") + defaultStatusPrefix
	}
	// Use default
	return defaultAPIBaseURL + defaultStatusPrefix
}

// errResult returns a CallToolResult with IsError set. When the caller also
// returns a non-nil output value, the SDK marshals that value into
// StructuredContent and (since Content is nil here) auto-populates Content
// with the marshaled JSON as a TextContent block. IsError: true is preserved
// throughout, signaling a tool-level error to spec-compliant MCP clients.
func errResult() *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true}
}

func guessExtensionFromContent(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	ct := http.DetectContentType(data)
	exts, err := mime.ExtensionsByType(ct)
	if err != nil || len(exts) == 0 {
		return ""
	}
	return exts[0]
}

// contextKeyAPIToken is the context key for a per-request API token injected by HTTP middleware.
type contextKeyAPIToken struct{}

// WithAPIToken returns a context carrying the given API token.
func WithAPIToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, contextKeyAPIToken{}, token)
}

func grillClient(token string) *client.Client {
	return client.New(apiBaseURL(), token)
}

// getProjectID resolves the project ID with priority: arg > POMA_PROJECT_ID env var.
func getProjectID(inputProjectID string) string {
	if inputProjectID != "" {
		return inputProjectID
	}
	return os.Getenv("POMA_PROJECT_ID")
}

// getToken resolves the API token with this priority:
//  1. Explicit tool argument
//  2. Per-request token injected by HTTP middleware (x-api-key header)
//  3. POMA_API_KEY environment variable
func getToken(ctx context.Context, inputToken string) string {
	if inputToken != "" {
		return inputToken
	}
	if v, ok := ctx.Value(contextKeyAPIToken{}).(string); ok && v != "" {
		return v
	}
	return os.Getenv("POMA_API_KEY")
}

// tokenSource describes which credential was used, for error messages.
func tokenSource(ctx context.Context, inputToken string) string {
	if inputToken != "" {
		return "per-call token argument"
	}
	if v, ok := ctx.Value(contextKeyAPIToken{}).(string); ok && v != "" {
		return "x-api-key / Authorization header"
	}
	if os.Getenv("POMA_API_KEY") != "" {
		return "POMA_API_KEY env var"
	}
	return "unknown"
}

// interpretAuthError returns a user-friendly error string for 401/402/403 responses.
// Returns "" if the status code is not an auth/billing error.
func interpretAuthError(ctx context.Context, inputToken string, statusCode int, body []byte, operation string) string {
	if statusCode != http.StatusUnauthorized && statusCode != http.StatusPaymentRequired && statusCode != http.StatusForbidden {
		return ""
	}

	src := tokenSource(ctx, inputToken)

	if statusCode == http.StatusPaymentRequired {
		return fmt.Sprintf(
			"%s: credits exceeded (HTTP 402). The account associated with the token provided via %s has no remaining credits. "+
				"Visit https://console.poma-ai.com to check your usage and upgrade your plan.",
			operation, src,
		)
	}

	if statusCode == http.StatusUnauthorized {
		return fmt.Sprintf(
			"%s: authentication failed (HTTP 401). The token provided via %s is invalid, expired, or malformed. "+
				"Generate a valid API key at https://console.poma-ai.com and set it as POMA_API_KEY or pass it as the token argument.",
			operation, src,
		)
	}

	// 403 — try to parse JSON error code for a specific message.
	var errResp struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(body, &errResp) == nil {
		switch errResp.Code {
		case "too_many_jobs", "quota_exceeded":
			// Not an auth error — this is a capacity/quota limit.
			return ""
		case "project_protected":
			return fmt.Sprintf(
				"%s: this project is protected (HTTP 403). The token provided via %s is an account-level key, "+
					"but this project requires a project API key. Generate one at https://console.poma-ai.com in the project settings, "+
					"or set the project to unprotected.",
				operation, src,
			)
		case "forbidden":
			return fmt.Sprintf(
				"%s: access denied (HTTP 403). The token provided via %s does not have access to this project — "+
					"you may not own it or aren't a member of the organization. "+
					"Use grill_projects to list projects accessible with your current key.",
				operation, src,
			)
		}
	}

	// Legacy: plain-text "too many jobs" from older API versions.
	bodyStr := strings.TrimSpace(string(body))
	if bodyStr == "too many jobs" || bodyStr == "quota exceeded" {
		return ""
	}

	return fmt.Sprintf(
		"%s: forbidden (HTTP 403). The token provided via %s was rejected. Response: %s",
		operation, src, bodyStr,
	)
}
