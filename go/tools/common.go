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

// Stable machine-readable error codes emitted alongside the human-readable
// error string on every grill tool error. Clients branch on these (and on
// Retryable), never on the prose message. See GrillError.
const (
	CodeMissingToken     = "missing_token"
	CodeInvalidInput     = "invalid_input"
	CodeAuthExpired      = "auth_expired"
	CodePaymentRequired  = "payment_required"
	CodeProjectProtected = "project_protected"
	CodeForbidden        = "forbidden"
	CodeTooManyJobs      = "too_many_jobs"
	CodeUpstreamError    = "upstream_error"
	CodeTransportError   = "transport_error"
	CodeParseError       = "parse_error"
	CodeJobFailed        = "job_failed"
	CodeStreamError      = "stream_error"
)

// GrillError is the shared, embeddable error envelope for every grill tool
// output. It is embedded anonymously into each output struct so its fields are
// promoted to the top level of the JSON output: the `error` key is byte-for-byte
// unchanged from before this envelope existed, and `code`/`retryable`/
// `retry_after_seconds` are purely additive (all omitempty). A machine-readable
// Code accompanies every error so clients can branch on it instead of
// string-matching the prose Error. Retryable is true only for transient codes
// (too_many_jobs, transport_error, stream_error, and 5xx upstream_error).
type GrillError struct {
	Error             string `json:"error,omitempty"`
	Code              string `json:"code,omitempty"`
	Retryable         bool   `json:"retryable,omitempty"`
	RetryAfterSeconds int    `json:"retry_after_seconds,omitempty"`
}

// errOut builds a GrillError with a code and a formatted message. Retryable is
// left false; call sites that need it (too_many_jobs, transport_error,
// stream_error, 5xx upstream_error) set it explicitly.
func errOut(code, format string, args ...any) GrillError {
	return GrillError{Error: fmt.Sprintf(format, args...), Code: code}
}

// isRetryableCode is the single source of truth for the taxonomy's retry
// contract: true only for too_many_jobs, transport_error, stream_error, and
// upstream_error when httpStatus is 5xx. Pass httpStatus 0 for codes that
// don't depend on it. Every call site that sets GrillError.Retryable derives
// it from this function so the contract can't drift out of sync per-site.
func isRetryableCode(code string, httpStatus int) bool {
	switch code {
	case CodeTooManyJobs, CodeTransportError, CodeStreamError:
		return true
	case CodeUpstreamError:
		return httpStatus >= 500
	default:
		return false
	}
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

// projectKeyPrefix is the wire prefix of project-bound API keys
// (poma_proj_gr_… for grill, poma_proj_pc_… for PrimeCut). Account keys are
// poma_acc_…; login JWTs have no prefix. Mirrors the gateway's
// auth/mechanism constants.
const projectKeyPrefix = "poma_proj_"

// sourceProjectKey is the scope.source value when the project is selected by
// the project API key itself rather than by an argument or env var.
const sourceProjectKey = "project API key"

// isProjectKey reports whether token is a project-bound API key. Such a key
// selects exactly one project on the gateway: /projects (listing) refuses it
// with 403, /projects/info returns the bound project, and a divergent
// X-Project-ID is rejected with 409 project_id_conflict.
func isProjectKey(token string) bool {
	return strings.HasPrefix(token, projectKeyPrefix)
}

// projectIDSource resolves the project ID and reports where it came from, for
// the human-readable scope hint. A project API key with no explicit selection
// reports sourceProjectKey and an empty id: the gateway binds the project from
// the key, so the server never needs to know the id up front.
func projectIDSource(token, inputProjectID string) (id, source string) {
	if inputProjectID != "" {
		return inputProjectID, "project_id argument"
	}
	if v := os.Getenv("POMA_PROJECT_ID"); v != "" {
		return v, "POMA_PROJECT_ID env var"
	}
	if isProjectKey(token) {
		return "", sourceProjectKey
	}
	return "", "account default (no project_id set)"
}

// interpretProjectConflict recognises the gateway's 409 project_id_conflict:
// the request carried an X-Project-ID (from the project_id argument or
// POMA_PROJECT_ID) that differs from the project the presented project API key
// is bound to. The key always wins on the gateway, so the request is
// ambiguous and rejected. This is a configuration error on the caller's side,
// classified invalid_input (terminal), never a transient upstream_error.
func interpretProjectConflict(statusCode int, body []byte, operation string) (msg string, ok bool) {
	if statusCode != http.StatusConflict {
		return "", false
	}
	var errResp struct {
		Code   json.RawMessage `json:"code"`
		Reason string          `json:"reason"`
	}
	_ = json.Unmarshal(body, &errResp)
	code := strings.Trim(string(errResp.Code), `"`)
	if errResp.Reason != "project_id_conflict" && code != "project_id_conflict" {
		return "", false
	}
	return fmt.Sprintf(
		"%s: project_id conflict (HTTP 409). The project_id argument or POMA_PROJECT_ID env var names a different project "+
			"than the one the project API key is bound to. A project key selects its project by itself: drop the project_id / "+
			"POMA_PROJECT_ID, or use an account API key (POMA_API_KEY) to address other projects.",
		operation,
	), true
}

// envToken returns the API token from the environment and the name of the
// variable it came from. Two names are accepted, mirroring the poma-sdk
// convention:
//
//   - POMA_GRILL_API_KEY — a grill *project* key (prefix poma_proj_gr_). Bound
//     to one project server-side; checked first because it is the more
//     specific credential.
//   - POMA_API_KEY — an *account* key (prefix poma_acc_) or a login JWT.
//     Scope it to a project with POMA_PROJECT_ID or the project_id argument.
//
// Existing configs that put a project key under POMA_API_KEY keep working.
// An empty value counts as unset and falls through to the next name.
func envToken() (token, name string) {
	for _, n := range []string{"POMA_GRILL_API_KEY", "POMA_API_KEY"} {
		if v := os.Getenv(n); v != "" {
			return v, n
		}
	}
	return "", ""
}

// getToken resolves the API token with this priority:
//  1. Explicit tool argument
//  2. Per-request token injected by HTTP middleware (x-api-key header)
//  3. POMA_GRILL_API_KEY, then POMA_API_KEY environment variable (see envToken)
func getToken(ctx context.Context, inputToken string) string {
	if inputToken != "" {
		return inputToken
	}
	if v, ok := ctx.Value(contextKeyAPIToken{}).(string); ok && v != "" {
		return v
	}
	t, _ := envToken()
	return t
}

// tokenSource describes which credential was used, for error messages.
func tokenSource(ctx context.Context, inputToken string) string {
	if inputToken != "" {
		return "per-call token argument"
	}
	if v, ok := ctx.Value(contextKeyAPIToken{}).(string); ok && v != "" {
		return "x-api-key / Authorization header"
	}
	if _, name := envToken(); name != "" {
		return name + " env var"
	}
	return "unknown"
}

// interpretAuthError returns a user-friendly error string and a stable error
// code for 401/402/403 responses. Returns ("", "") if the status code is not an
// auth/billing error (including capacity/quota 403s, which are handled by
// interpretTooManyJobs and the batch quota path).
func interpretAuthError(ctx context.Context, inputToken string, statusCode int, body []byte, operation string) (msg, code string) {
	if statusCode != http.StatusUnauthorized && statusCode != http.StatusPaymentRequired && statusCode != http.StatusForbidden {
		return "", ""
	}

	src := tokenSource(ctx, inputToken)

	if statusCode == http.StatusPaymentRequired {
		return fmt.Sprintf(
			"%s: credits exceeded (HTTP 402). The account associated with the token provided via %s has no remaining credits. "+
				"Visit https://console.poma-ai.com to check your usage and upgrade your plan.",
			operation, src,
		), CodePaymentRequired
	}

	if statusCode == http.StatusUnauthorized {
		return fmt.Sprintf(
			"%s: authentication failed (HTTP 401). The token provided via %s is invalid, expired, or malformed. "+
				"Generate a valid API key at https://console.poma-ai.com and set it as POMA_GRILL_API_KEY (project key) or POMA_API_KEY (account key), or pass it as the token argument.",
			operation, src,
		), CodeAuthExpired
	}

	// 403 — try to parse the JSON error envelope for a specific message.
	//
	// Two wire shapes: the migrated envelope carries a snake_case `reason`
	// discriminator (and `code` becomes the numeric HTTP status), while the
	// legacy envelope carries the discriminator as the string `code`. Switch on
	// `reason` when present and never fall through to the code-string switch —
	// the numeric code deserializes to "403" and matches no token. Only the
	// pre-migration server (empty `reason`) uses the legacy string `code`.
	var errResp struct {
		Code   json.RawMessage `json:"code"`
		Reason string          `json:"reason"`
	}
	if json.Unmarshal(body, &errResp) == nil {
		code := strings.Trim(string(errResp.Code), `"`)
		projectProtectedMsg := fmt.Sprintf(
			"%s: this project is protected (HTTP 403). The token provided via %s is an account-level key, "+
				"but this project requires a project API key. Generate one at https://console.poma-ai.com in the project settings, "+
				"or set the project to unprotected.",
			operation, src,
		)
		forbiddenMsg := fmt.Sprintf(
			"%s: access denied (HTTP 403). The token provided via %s does not have access to this project — "+
				"you may not own it or aren't a member of the organization. "+
				"Use grill_projects to list projects accessible with your current key.",
			operation, src,
		)
		if errResp.Reason != "" {
			switch errResp.Reason {
			case "too_many_jobs", "quota_exceeded":
				// Not an auth error — this is a capacity/quota limit.
				return "", ""
			case "project_protected":
				return projectProtectedMsg, CodeProjectProtected
			case "forbidden":
				return forbiddenMsg, CodeForbidden
			}
		} else {
			switch code {
			case "too_many_jobs", "quota_exceeded":
				// Not an auth error — this is a capacity/quota limit.
				return "", ""
			case "project_protected":
				return projectProtectedMsg, CodeProjectProtected
			case "forbidden":
				return forbiddenMsg, CodeForbidden
			}
		}
	}

	// Legacy: plain-text "too many jobs" from older API versions.
	bodyStr := strings.TrimSpace(string(body))
	if bodyStr == "too many jobs" || bodyStr == "quota exceeded" {
		return "", ""
	}

	return fmt.Sprintf(
		"%s: forbidden (HTTP 403). The token provided via %s was rejected. Response: %s",
		operation, src, bodyStr,
	), CodeForbidden
}

// interpretTooManyJobs recognises the job-capacity backpressure signal and, when
// present, returns an explicit throttle instruction plus the suggested retry
// delay in seconds. ok is false for any other response.
//
// The signal is HTTP 429 with code "too_many_jobs" (current API), a legacy 403
// carrying the same code, or the legacy plaintext body "too many jobs". The API
// also sets a Retry-After header, but the JSON body's retry_after_seconds
// carries the same hint and is what we can read here without header access.
//
// The message is written FOR the calling agent (e.g. langdock firing a
// pipeline): it states that the ingest did NOT happen, that this is transient
// (retry the same ingest after the delay), and that it must stop sending new
// ingests until capacity frees. This tool result is the only backpressure
// channel an LLM client sees, so it has to be an imperative instruction, not a
// terse error.
func interpretTooManyJobs(statusCode int, body []byte) (msg string, retryAfter int, ok bool) {
	// code is numeric (429) on the current API and a string ("too_many_jobs") on
	// legacy 403 responses — RawMessage accepts either without a decode error.
	var errResp struct {
		Code              json.RawMessage `json:"code"`
		Reason            string          `json:"reason"`
		Error             string          `json:"error"`
		Message           string          `json:"message"`
		RetryAfterSeconds int             `json:"retry_after_seconds"`
	}
	_ = json.Unmarshal(body, &errResp)
	code := strings.Trim(string(errResp.Code), `"`)

	isCapacity := statusCode == http.StatusTooManyRequests ||
		errResp.Reason == "too_many_jobs" ||
		code == "too_many_jobs" ||
		strings.TrimSpace(string(body)) == "too many jobs"
	if !isCapacity {
		return "", 0, false
	}

	retryAfter = errResp.RetryAfterSeconds
	if retryAfter <= 0 {
		retryAfter = 5
	}

	// The migrated envelope renames the human field message→error; read error
	// first so the LLM detail stays populated across the rename, falling back to
	// the legacy message and finally the raw body.
	detail := errResp.Error
	if detail == "" {
		detail = errResp.Message
	}
	if detail == "" {
		detail = strings.TrimSpace(string(body))
	}

	msg = fmt.Sprintf(
		"grill ingest: job queue full — the account is at its concurrent-job capacity (too_many_jobs). "+
			"This is a TRANSIENT backpressure signal, NOT a failure: the document was NOT ingested and no credits were spent. "+
			"Wait ~%ds, then retry this same ingest. Do NOT send more ingests until capacity frees — "+
			"poll grill_jobs_status and resume only once active jobs drop below the account limit. Server: %s",
		retryAfter, detail,
	)
	return msg, retryAfter, true
}
