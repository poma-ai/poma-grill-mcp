package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/poma-ai/poma-cli/pkg/client"
)

// jobStatusFull is the full SSE event payload from the status server, including fields the client library does not expose.
type jobStatusFull struct {
	IsTerminal bool             `json:"is_terminal"`
	Status     string           `json:"status"`
	Error      string           `json:"error,omitempty"`
	Grill      *jobGrillOutcome `json:"grill,omitempty"`
}

// jobGrillOutcome is the optional `grill` object the gateway attaches to a job
// status (poma-services-go#133). Nil when the gateway did not send it — older
// gateways, non-grill jobs, or a job that has not reached the grill stage yet.
//
// Deduplicated: the same input bytes under the same conversion build were
// already indexed; nothing new was stored and DocID names the existing
// document (it may differ from the job_id). ReplacedDocIDs: documents grill
// evicted in favour of this job after a conversion-build change.
type jobGrillOutcome struct {
	Deduplicated   bool     `json:"deduplicated"`
	DocID          string   `json:"doc_id,omitempty"`
	ReplacedDocIDs []string `json:"replaced_doc_ids,omitempty"`
}

// UnmarshalJSON decodes a status event, tolerating any `grill` value the
// gateway sends. `grill` is owned by the gateway and is not needed to act on a
// job, so a shape change there must never invalidate the status it rides on: a
// rejected terminal event makes the SSE wait in readJobStatusStream hang, and
// makes peekJobStatus report parse_error for a job that actually succeeded.
// Before `grill` was typed here it was an unknown field and was ignored, so
// strict decoding would be a regression. Field-wise leniency also keeps Go
// byte-identical to Node's grillOutcomeFields, which coerces the same way.
func (s *jobStatusFull) UnmarshalJSON(b []byte) error {
	// alias drops this method, so the embedded decode does not recurse. The
	// outer Grill is shallower than alias's, so encoding/json binds `grill` to
	// the raw bytes and leaves alias.Grill nil.
	type alias jobStatusFull
	var raw struct {
		alias
		Grill json.RawMessage `json:"grill"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*s = jobStatusFull(raw.alias)
	s.Grill = parseGrillOutcome(raw.Grill)
	return nil
}

// parseGrillOutcome decodes the gateway's `grill` value field by field,
// discarding anything of the wrong type rather than failing the whole object.
// Only a JSON object yields an outcome: null, a string, a number and an array
// all return nil, matching Node's grillOutcomeFields.
func parseGrillOutcome(raw json.RawMessage) *jobGrillOutcome {
	if len(raw) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		// Not a JSON object, or the literal null.
		return nil
	}
	out := &jobGrillOutcome{}
	var dedup bool
	if err := json.Unmarshal(fields["deduplicated"], &dedup); err == nil {
		out.Deduplicated = dedup
	}
	var docID string
	if err := json.Unmarshal(fields["doc_id"], &docID); err == nil {
		out.DocID = docID
	}
	var replaced []json.RawMessage
	if err := json.Unmarshal(fields["replaced_doc_ids"], &replaced); err == nil {
		for _, item := range replaced {
			var id string
			if err := json.Unmarshal(item, &id); err == nil && id != "" {
				out.ReplacedDocIDs = append(out.ReplacedDocIDs, id)
			}
		}
	}
	return out
}

// lastGrillOutcome returns the grill object of the most recent status event
// that carried one, or nil. The gateway attaches it to the terminal status, so
// this is the outcome to surface at the top level of a wait-style tool output.
func lastGrillOutcome(events []jobStatusFull) *jobGrillOutcome {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Grill != nil {
			return events[i].Grill
		}
	}
	return nil
}

// jobProgressWire is the JSON payload in MCP progress notifications.
type jobProgressWire struct {
	JobID  string `json:"job_id"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

func notifyJobProgress(ctx context.Context, req *mcp.CallToolRequest, jobID string, seq int, s *jobStatusFull) {
	if req == nil || req.Session == nil || s == nil {
		return
	}
	msg, err := json.Marshal(jobProgressWire{JobID: jobID, Status: s.Status, Error: s.Error})
	if err != nil {
		return
	}
	if err := req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
		Message:  string(msg),
		Progress: float64(seq),
		Total:    0,
	}); err != nil {
		slog.Warn("job progress notify failed", "job_id", jobID, "err", err)
	}
}

// streamJobStatus opens the status SSE stream and calls onEvent for each job_status event.
// It captures the full event JSON (including download_url) which the client library discards.
func streamJobStatus(ctx context.Context, c *client.Client, jobID, statusBaseURL string, onEvent func(*jobStatusFull)) error {
	url := strings.TrimSuffix(statusBaseURL, "/") + "/jobs/" + client.JobPathSegment(jobID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("status stream: HTTP %d: %s", resp.StatusCode, string(body))
	}
	scanner := bufio.NewScanner(resp.Body)
	var eventType, data string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if eventType == "job_status" && data != "" {
				var s jobStatusFull
				if err := json.Unmarshal([]byte(data), &s); err != nil {
					slog.Warn("job status: ignoring malformed SSE event", "job_id", jobID, "err", err)
				} else {
					onEvent(&s)
					if s.IsTerminal || isTerminalGrillStatus(s.Status) {
						return nil
					}
					// Check for context cancellation after dispatching each event.
					// Body reads will also surface the error on the next Scan(), but
					// this avoids an unnecessary extra read when ctx is already done.
					if err := ctx.Err(); err != nil {
						return err
					}
				}
			}
			eventType, data = "", ""
			continue
		}
		if trimmed, ok := strings.CutPrefix(line, "event:"); ok {
			eventType = strings.TrimSpace(trimmed)
		} else if trimmed, ok := strings.CutPrefix(line, "data:"); ok {
			data = strings.TrimSpace(trimmed)
		}
	}
	return scanner.Err()
}

// peekJobStatus fetches a single status snapshot for the given job (non-streaming).
// The returned int is the upstream HTTP status code, or 0 when err originates
// before/without an HTTP response (request build failure or network/client
// error) — callers need this to classify the error correctly: 0 is a transport
// error, a non-2xx status is an upstream error, and 200 with a non-nil err is a
// parse error. Collapsing all three into one error kind would misclassify a
// permanent 4xx (e.g. job not found) as a transient transport error.
func peekJobStatus(ctx context.Context, c *client.Client, jobID string) (*jobStatusFull, int, error) {
	url := strings.TrimSuffix(apiBaseURL(), "/") + "/jobs/" + client.JobPathSegment(jobID) + "/status"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, fmt.Errorf("job status: HTTP %d: %s", resp.StatusCode, string(body))
	}
	var s jobStatusFull
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, resp.StatusCode, err
	}
	return &s, resp.StatusCode, nil
}

// isTerminalGrillStatus returns true for status values that indicate a job has
// reached a terminal state. We check these explicitly because the Grill status
// API does not always set is_terminal:true on terminal events (e.g. "grilled").
// This mirrors the check in poma-cli's readSSEJobStatus for the PrimeCut pipeline.
func isTerminalGrillStatus(status string) bool {
	switch status {
	case "grilled", "done", "failed", "deleted":
		return true
	}
	return false
}
