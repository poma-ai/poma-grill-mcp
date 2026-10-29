package tools

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
)

// requestOrigin is the X-Request-Origin the API records for this server's calls: job
// origin "mcp" (models.JobOriginMcp in poma-services-go) and the CRM attribution origin.
// poma-cli's client hard-codes "cli" on every request, so apiTransport overrides it.
const requestOrigin = "mcp"

// headerClientTraceID is the API's correlation header. The API logs it next to its own
// X-Trace-ID (it never adopts it), so one id ties this server's "api request" log lines
// for a tool call to the API's log lines for those requests.
const headerClientTraceID = "X-Client-Trace-ID"

// apiTransport stamps every API request of one client with the origin and that client's
// trace id. grillClient builds one client per tool call, so the id is per tool call.
type apiTransport struct {
	base    http.RoundTripper
	traceID string
}

func (t *apiTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// A RoundTripper must not modify the caller's request.
	req = req.Clone(req.Context())
	req.Header.Set("X-Request-Origin", requestOrigin)
	req.Header.Set(headerClientTraceID, t.traceID)
	resp, err := t.base.RoundTrip(req)
	attrs := []any{"method", req.Method, "path", req.URL.Path, "client_trace_id", t.traceID}
	if err != nil {
		slog.Warn("api request failed", append(attrs, "err", err)...)
		return resp, err
	}
	slog.Info("api request", append(attrs, "status", resp.StatusCode)...)
	return resp, nil
}

// withAPITransport wraps base (http.DefaultTransport when nil) in an apiTransport with a
// fresh trace id: 32 hex chars, inside the API's accepted ^[A-Za-z0-9._-]{1,32}$.
func withAPITransport(base http.RoundTripper) *apiTransport {
	if base == nil {
		base = http.DefaultTransport
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error (Go 1.24+).
	return &apiTransport{base: base, traceID: hex.EncodeToString(b)}
}
