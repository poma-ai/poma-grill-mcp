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
	IsTerminal bool   `json:"is_terminal"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
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
func peekJobStatus(ctx context.Context, c *client.Client, jobID string) (*jobStatusFull, error) {
	url := strings.TrimSuffix(apiBaseURL(), "/") + "/jobs/" + client.JobPathSegment(jobID) + "/status"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("job status: HTTP %d: %s", resp.StatusCode, string(body))
	}
	var s jobStatusFull
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, err
	}
	return &s, nil
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
