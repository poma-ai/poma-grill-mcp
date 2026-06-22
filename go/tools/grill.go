package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/poma-ai/poma-cli/pkg/client"
)

// -- Grill Ingest ----------------------------------------------------

// projectIDSchema is the shared schema entry for the optional project_id parameter.
var projectIDSchema = &jsonschema.Schema{
	Type:        "string",
	Description: "Project ID to use when authenticating with an account-level API key. Not needed with a project API key. Falls back to POMA_PROJECT_ID env var.",
}

var grillIngestInputSchema = &jsonschema.Schema{
	Type: "object",
	Properties: map[string]*jsonschema.Schema{
		"file_base64": {
			Type:        "string",
			Description: "Standard base64 of the file bytes. Use for small files. Mutually exclusive with file_path.",
		},
		"file_path": {
			Type:        "string",
			Description: "Absolute or cwd-relative path readable by the MCP server process (local stdio). Preferred for large files. Mutually exclusive with file_base64. Optional env: GRILL_INGEST_ALLOWED_PREFIX, GRILL_INGEST_MAX_BYTES.",
		},
		"filename": {
			Type:        "string",
			Description: "Original basename (e.g. report.pdf). Optional; inferred from file_path or content when possible.",
		},
		"token": {
			Type:        "string",
			Description: "POMA API JWT. Usually not needed — the server inherits the token from the Authorization header in the MCP client config or the POMA_API_KEY env var. Only pass explicitly to override.",
		},
		"project_id": projectIDSchema,
	},
}

var grillIngestOutputSchema = &jsonschema.Schema{
	Type: "object",
	Properties: map[string]*jsonschema.Schema{
		"job_id": {Type: "string"},
		"events": {Type: "array"},
		"error":  {Type: "string"},
	},
}

var grillIngestTool = &mcp.Tool{
	Name:         "grill_ingest",
	Description:  "Ingest a file into POMA Grill (context engine). Trigger when the user says \"grill my ...\". Provide exactly one of file_path (large/local) or file_base64 (small). Returns job_id. Once done, doc_id equals job_id for grill_search.",
	InputSchema:  grillIngestInputSchema,
	OutputSchema: grillIngestOutputSchema,
}

var grillIngestSyncTool = &mcp.Tool{
	Name:         "grill_ingest_sync",
	Description:  "Ingest a file into POMA Grill; waits until terminal state. Trigger when the user says \"grill my ...\". Provide exactly one of file_path (large/local) or file_base64 (small). Returns job_id and status events.",
	InputSchema:  grillIngestInputSchema,
	OutputSchema: grillIngestOutputSchema,
}

type GrillIngestInput struct {
	FileBase64 string `json:"file_base64,omitempty"`
	FilePath   string `json:"file_path,omitempty"`
	Filename   string `json:"filename,omitempty"`
	Token      string `json:"token,omitempty"`
	ProjectID  string `json:"project_id,omitempty"`
}

type GrillIngestOutput struct {
	JobID  string          `json:"job_id,omitempty"`
	Events []jobStatusFull `json:"events,omitempty"`
	Error  string          `json:"error,omitempty"`
}

func GrillIngest(ctx context.Context, req *mcp.CallToolRequest, input GrillIngestInput) (*mcp.CallToolResult, GrillIngestOutput, error) {
	return grillIngestWithWait(ctx, req, input, false)
}

func GrillIngestSync(ctx context.Context, req *mcp.CallToolRequest, input GrillIngestInput) (*mcp.CallToolResult, GrillIngestOutput, error) {
	return grillIngestWithWait(ctx, req, input, true)
}

func grillIngestWithWait(ctx context.Context, req *mcp.CallToolRequest, input GrillIngestInput, waitTerminal bool) (*mcp.CallToolResult, GrillIngestOutput, error) {
	token := getToken(ctx, input.Token)
	if token == "" {
		return errResult(), GrillIngestOutput{Error: "token is required (provide token or set POMA_API_KEY on the server)"}, nil
	}
	data, filename, err := resolveGrillIngestPayload(input)
	if err != nil {
		return errResult(), GrillIngestOutput{Error: err.Error()}, nil
	}

	projectID := getProjectID(input.ProjectID)
	slog.Info("grill ingest", "filename", filename, "bytes", len(data))
	c := grillClient(token)

	body, st, err := grillIngestData(c, data, filename, projectID)
	if err != nil {
		return errResult(), GrillIngestOutput{Error: err.Error()}, nil
	}
	if authErr := interpretAuthError(ctx, input.Token, st, body, "grill ingest"); authErr != "" {
		return errResult(), GrillIngestOutput{Error: authErr}, nil
	}
	if st != http.StatusCreated {
		return errResult(), GrillIngestOutput{Error: fmt.Sprintf("grill ingest: HTTP %d: %s", st, string(body))}, nil
	}

	j, err := client.ParseJob(body)
	if err != nil || j.JobID == "" {
		return errResult(), GrillIngestOutput{Error: fmt.Sprintf("grill ingest: could not parse job_id from response: %s", string(body))}, nil
	}
	slog.Info("grill ingest job_id", "job_id", j.JobID)

	if !waitTerminal {
		return nil, GrillIngestOutput{JobID: j.JobID}, nil
	}

	var events []jobStatusFull
	eventNum := 0
	streamErr := streamJobStatus(ctx, c, j.JobID, statusAPIBaseURL(), func(s *jobStatusFull) {
		events = append(events, *s)
		notifyJobProgress(ctx, req, j.JobID, eventNum, s)
		eventNum++
	})
	if streamErr != nil {
		slog.Error("grill ingest status stream failed", "job_id", j.JobID, "err", streamErr)
		return errResult(), GrillIngestOutput{JobID: j.JobID, Events: events, Error: fmt.Sprintf("status stream failed: %v", streamErr)}, nil
	}
	if len(events) > 0 {
		last := events[len(events)-1]
		if last.Status == "failed" {
			msg := "job failed"
			if last.Error != "" {
				msg = "job failed: " + last.Error
			}
			slog.Error("grill ingest job failed", "job_id", j.JobID)
			return errResult(), GrillIngestOutput{JobID: j.JobID, Events: events, Error: msg}, nil
		}
	}
	slog.Info("grill ingest done", "job_id", j.JobID)
	return nil, GrillIngestOutput{JobID: j.JobID, Events: events}, nil
}

// -- Grill Ingest Resume ---------------------------------------------

var grillIngestResumeInputSchema = &jsonschema.Schema{
	Type: "object",
	Properties: map[string]*jsonschema.Schema{
		"job_id": {
			Type:        "string",
			Description: "Job ID returned by a previous grill_ingest call.",
		},
		"token": {
			Type:        "string",
			Description: "POMA API JWT. Usually not needed — the server inherits the token from the Authorization header in the MCP client config or the POMA_API_KEY env var. Only pass explicitly to override.",
		},
	},
	Required: []string{"job_id"},
}

var grillIngestResumeTool = &mcp.Tool{
	Name:         "grill_ingest_resume",
	Description:  "Resume tracking an in-progress POMA Grill ingestion job. Connects to the status SSE stream for the given job_id and waits until a terminal state (done, failed, grilled, deleted), emitting progress notifications. Use when grill_ingest was called earlier and you need to wait for completion.",
	InputSchema:  grillIngestResumeInputSchema,
	OutputSchema: grillIngestOutputSchema,
}

type GrillIngestResumeInput struct {
	JobID string `json:"job_id"`
	Token string `json:"token,omitempty"`
}

func GrillIngestResume(ctx context.Context, req *mcp.CallToolRequest, input GrillIngestResumeInput) (*mcp.CallToolResult, GrillIngestOutput, error) {
	token := getToken(ctx, input.Token)
	if token == "" {
		return errResult(), GrillIngestOutput{Error: "token is required (provide token or set POMA_API_KEY on the server)"}, nil
	}
	if input.JobID == "" {
		return errResult(), GrillIngestOutput{Error: "job_id is required"}, nil
	}

	c := grillClient(token)

	var events []jobStatusFull
	eventNum := 0
	streamErr := streamJobStatus(ctx, c, input.JobID, statusAPIBaseURL(), func(s *jobStatusFull) {
		events = append(events, *s)
		notifyJobProgress(ctx, req, input.JobID, eventNum, s)
		eventNum++
	})
	if streamErr != nil {
		slog.Error("grill ingest resume status stream failed", "job_id", input.JobID, "err", streamErr)
		return errResult(), GrillIngestOutput{JobID: input.JobID, Events: events, Error: fmt.Sprintf("status stream failed: %v", streamErr)}, nil
	}
	if len(events) > 0 {
		last := events[len(events)-1]
		if last.Status == "failed" {
			msg := "job failed"
			if last.Error != "" {
				msg = "job failed: " + last.Error
			}
			slog.Error("grill ingest resume job failed", "job_id", input.JobID)
			return errResult(), GrillIngestOutput{JobID: input.JobID, Events: events, Error: msg}, nil
		}
	}
	slog.Info("grill ingest resume done", "job_id", input.JobID)
	return nil, GrillIngestOutput{JobID: input.JobID, Events: events}, nil
}

// -- Grill Search ----------------------------------------------------

var grillSearchInputSchema = &jsonschema.Schema{
	Type: "object",
	Properties: map[string]*jsonschema.Schema{
		"query": {
			Type:        "string",
			Description: "Natural-language search query.",
		},
		"doc_filter": {
			Type:        "string",
			Description: "Optional doc_id to restrict search to a single document. When set, routes to the searchInDoc endpoint.",
		},
		"exclude_doc_ids": {
			Type:        "array",
			Items:       &jsonschema.Schema{Type: "string"},
			Description: "Doc ids to exclude from results. Useful in agent loops to avoid re-citing docs already shown. Max 100.",
		},
		"return_assets": {
			Type:        "boolean",
			Description: "Return the cited documents' figures/tables in the `assets` output field (keyed by doc_id; images are base64 data URIs).",
		},
		"return_page_images": {
			Type:        "boolean",
			Description: "DEPRECATED / not available — no-op today. Full-page screenshots are not returned inline.",
		},
		"token": {
			Type:        "string",
			Description: "POMA API JWT. Usually not needed — the server inherits the token from the Authorization header in the MCP client config or the POMA_API_KEY env var. Only pass explicitly to override.",
		},
		"project_id": projectIDSchema,
	},
	Required: []string{"query"},
}

var grillSearchOutputSchema = &jsonschema.Schema{
	Type: "object",
	Properties: map[string]*jsonschema.Schema{
		"context": {Type: "string", Description: "Concatenated chunk text for RAG prompting."},
		"assets":  {Type: "object", Description: "Per-doc figures/tables when return_assets=true, keyed by doc_id. Images are base64 data URIs. Omitted when none."},
		"error":   {Type: "string"},
	},
}

var grillSearchTool = &mcp.Tool{
	Name:         "grill_search",
	Description:  "Search the POMA Grill context engine and return a context block for RAG. Set doc_filter to restrict search to a specific document (use the job_id returned by grill_ingest as the doc_id). Use exclude_doc_ids to skip docs already cited in this conversation. Result count is bounded server-side by relevance and a token budget — there is no top_k.",
	InputSchema:  grillSearchInputSchema,
	OutputSchema: grillSearchOutputSchema,
}

type GrillSearchInput struct {
	Query            string   `json:"query"`
	DocFilter        string   `json:"doc_filter,omitempty"`
	ExcludeDocIDs    []string `json:"exclude_doc_ids,omitempty"`
	ReturnAssets     bool     `json:"return_assets,omitempty"`
	ReturnPageImages bool     `json:"return_page_images,omitempty"`
	Token            string   `json:"token,omitempty"`
	ProjectID        string   `json:"project_id,omitempty"`
}

type GrillSearchOutput struct {
	Context string `json:"context,omitempty"`
	// omitzero (not omitempty) keeps this byte-identical to the Node variant
	// for every upstream shape: a nil map (null or absent from the API) is
	// omitted; a non-nil empty map (`assets: {}`) is emitted as `"assets":{}`,
	// matching Node's `assets !== undefined && assets !== null` guard. In
	// practice grill sends `assets: null` for no-figures and api/go's
	// omitempty collapses it to absent, so both variants simply drop it.
	Assets map[string]any `json:"assets,omitzero"`
	Error  string         `json:"error,omitempty"`
}

func GrillSearch(ctx context.Context, _ *mcp.CallToolRequest, input GrillSearchInput) (*mcp.CallToolResult, GrillSearchOutput, error) {
	token := getToken(ctx, input.Token)
	if token == "" {
		return errResult(), GrillSearchOutput{Error: "token is required (provide token or set POMA_API_KEY on the server)"}, nil
	}
	if input.Query == "" {
		return errResult(), GrillSearchOutput{Error: "query is required"}, nil
	}

	projectID := getProjectID(input.ProjectID)
	c := grillClient(token)
	var respBody []byte
	var st int
	var err error

	// Route to searchInDoc when doc_filter is set, plain search otherwise.
	if input.DocFilter != "" {
		respBody, st, err = grillSearchInDoc(c, grillSearchInDocRequest{
			Query:            input.Query,
			DocFilter:        input.DocFilter,
			ExcludeDocIDs:    input.ExcludeDocIDs,
			ReturnAssets:     input.ReturnAssets,
			ReturnPageImages: input.ReturnPageImages,
		}, projectID)
	} else {
		respBody, st, err = grillSearch(c, grillSearchRequest{
			Query:            input.Query,
			ExcludeDocIDs:    input.ExcludeDocIDs,
			ReturnAssets:     input.ReturnAssets,
			ReturnPageImages: input.ReturnPageImages,
		}, projectID)
	}
	if err != nil {
		return errResult(), GrillSearchOutput{Error: err.Error()}, nil
	}
	if authErr := interpretAuthError(ctx, input.Token, st, respBody, "grill search"); authErr != "" {
		return errResult(), GrillSearchOutput{Error: authErr}, nil
	}
	if st != http.StatusOK {
		return errResult(), GrillSearchOutput{Error: fmt.Sprintf("grill search: HTTP %d: %s", st, string(respBody))}, nil
	}

	var result struct {
		Context string         `json:"context"`
		Assets  map[string]any `json:"assets"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return errResult(), GrillSearchOutput{Error: fmt.Sprintf("grill search: parse response: %v", err)}, nil
	}
	slog.Info("grill search", "doc_filter", input.DocFilter, "context_bytes", len(result.Context), "assets_docs", len(result.Assets))
	// Set Content explicitly to just the prompt-ready context text so the
	// assets payload (potentially large base64 image data URIs) doesn't get
	// duplicated into the text content block — the SDK still populates
	// StructuredContent from the typed output (see go-sdk mcp/server.go).
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: result.Context}},
	}, GrillSearchOutput{Context: result.Context, Assets: result.Assets}, nil
}

// -- Grill Docs List -------------------------------------------------

var grillDocsListInputSchema = &jsonschema.Schema{
	Type: "object",
	Properties: map[string]*jsonschema.Schema{
		"token": {
			Type:        "string",
			Description: "POMA API JWT. Usually not needed — the server inherits the token from the Authorization header in the MCP client config or the POMA_API_KEY env var. Only pass explicitly to override.",
		},
		"project_id": projectIDSchema,
	},
}

var grillDocsListOutputSchema = &jsonschema.Schema{
	Type: "object",
	Properties: map[string]*jsonschema.Schema{
		"documents":       {Type: "array"},
		"namespace":       {Type: "string"},
		"total_documents": {Type: "integer"},
		"error":           {Type: "string"},
	},
}

var grillDocsListTool = &mcp.Tool{
	Name:         "grill_docs_list",
	Description:  "List documents currently ingested into POMA Grill for the authenticated project namespace. Returns metadata only (doc_id, filename, ingested_at, chunk/page counts, etc.) — use grill_search to retrieve content. Pass a returned doc_id as doc_filter on grill_search to scope a query to a specific document.",
	InputSchema:  grillDocsListInputSchema,
	OutputSchema: grillDocsListOutputSchema,
}

type GrillDocsListInput struct {
	Token     string `json:"token,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
}

type GrillDocInfo struct {
	DocID         string `json:"doc_id"`
	Title         string `json:"title,omitempty"`
	Language      string `json:"language,omitempty"`
	Filename      string `json:"filename,omitempty"`
	Pages         int    `json:"pages,omitempty"`
	ChunksetCount int    `json:"chunkset_count,omitempty"`
	ChunkCount    int    `json:"chunk_count,omitempty"`
	ImageCount    int    `json:"image_count,omitempty"`
	TableCount    int    `json:"table_count,omitempty"`
	IngestedAt    string `json:"ingested_at,omitempty"`
	SourceJobID   string `json:"source_job_id,omitempty"`
}

type GrillDocsListOutput struct {
	Documents      []GrillDocInfo `json:"documents"`
	Namespace      string         `json:"namespace,omitempty"`
	TotalDocuments int            `json:"total_documents"`
	Error          string         `json:"error,omitempty"`
}

func GrillDocsList(ctx context.Context, _ *mcp.CallToolRequest, input GrillDocsListInput) (*mcp.CallToolResult, GrillDocsListOutput, error) {
	token := getToken(ctx, input.Token)
	if token == "" {
		return errResult(), GrillDocsListOutput{Error: "token is required (provide token or set POMA_API_KEY on the server)"}, nil
	}

	projectID := getProjectID(input.ProjectID)
	c := grillClient(token)
	body, st, err := grillListDocs(c, projectID)
	if err != nil {
		return errResult(), GrillDocsListOutput{Error: err.Error()}, nil
	}
	if authErr := interpretAuthError(ctx, input.Token, st, body, "grill docs list"); authErr != "" {
		return errResult(), GrillDocsListOutput{Error: authErr}, nil
	}
	if st != http.StatusOK {
		return errResult(), GrillDocsListOutput{Error: fmt.Sprintf("grill docs list: HTTP %d: %s", st, string(body))}, nil
	}

	var out GrillDocsListOutput
	if err := json.Unmarshal(body, &out); err != nil {
		return errResult(), GrillDocsListOutput{Error: fmt.Sprintf("grill docs list: parse response: %v", err)}, nil
	}
	if out.Documents == nil {
		out.Documents = []GrillDocInfo{}
	}
	slog.Info("grill docs list", "count", out.TotalDocuments, "namespace", out.Namespace)
	return nil, out, nil
}

// -- Grill Ingest Batch ----------------------------------------------

var grillIngestBatchInputSchema = &jsonschema.Schema{
	Type: "object",
	Properties: map[string]*jsonschema.Schema{
		"file_paths": {
			Type:        "array",
			Description: "Absolute or cwd-relative paths readable by the MCP server process. Max 50 files.",
			Items:       &jsonschema.Schema{Type: "string"},
		},
		"token": {
			Type:        "string",
			Description: "POMA API JWT. Usually not needed — the server inherits the token from the Authorization header in the MCP client config or the POMA_API_KEY env var. Only pass explicitly to override.",
		},
		"concurrency": {
			Type:        "integer",
			Description: "Upload concurrency (default 5, max 10). Use 1 for free-tier accounts.",
		},
		"project_id": projectIDSchema,
	},
	Required: []string{"file_paths"},
}

var grillIngestBatchOutputSchema = &jsonschema.Schema{
	Type: "object",
	Properties: map[string]*jsonschema.Schema{
		"results":              {Type: "array"},
		"submitted_count":      {Type: "integer"},
		"failed_count":         {Type: "integer"},
		"quota_exceeded_count": {Type: "integer"},
		"error":                {Type: "string"},
	},
}

var grillIngestBatchTool = &mcp.Tool{
	Name:         "grill_ingest_batch",
	Description:  "Ingest multiple files into POMA Grill with controlled upload concurrency (default 5, max 10). Accepts up to 50 file paths. Returns job_ids immediately after uploads complete — does not wait for server-side processing. Use grill_jobs_status to monitor progress. On free-tier accounts use concurrency:1; the API returns 403 when the job queue is full — quota_exceed results can be retried once running jobs finish.",
	InputSchema:  grillIngestBatchInputSchema,
	OutputSchema: grillIngestBatchOutputSchema,
}

type GrillIngestBatchInput struct {
	FilePaths   []string `json:"file_paths"`
	Token       string   `json:"token,omitempty"`
	Concurrency int      `json:"concurrency,omitempty"`
	ProjectID   string   `json:"project_id,omitempty"`
}

type GrillIngestBatchResult struct {
	FilePath    string `json:"file_path"`
	JobID       string `json:"job_id,omitempty"`
	Error       string `json:"error,omitempty"`
	QuotaExceed bool   `json:"quota_exceed,omitempty"`
}

type GrillIngestBatchOutput struct {
	Results            []GrillIngestBatchResult `json:"results"`
	SubmittedCount     int                      `json:"submitted_count"`
	FailedCount        int                      `json:"failed_count"`
	QuotaExceededCount int                      `json:"quota_exceeded_count"`
	Error              string                   `json:"error,omitempty"`
}

func GrillIngestBatch(ctx context.Context, _ *mcp.CallToolRequest, input GrillIngestBatchInput) (*mcp.CallToolResult, GrillIngestBatchOutput, error) {
	token := getToken(ctx, input.Token)
	if token == "" {
		return errResult(), GrillIngestBatchOutput{Error: "token is required (provide token or set POMA_API_KEY on the server)"}, nil
	}
	if len(input.FilePaths) == 0 {
		return errResult(), GrillIngestBatchOutput{Error: "file_paths is required"}, nil
	}
	if len(input.FilePaths) > 50 {
		return errResult(), GrillIngestBatchOutput{Error: "file_paths exceeds limit of 50"}, nil
	}

	concurrency := input.Concurrency
	if concurrency <= 0 {
		concurrency = 5
	}
	if concurrency > 10 {
		concurrency = 10
	}

	projectID := getProjectID(input.ProjectID)
	results := make([]GrillIngestBatchResult, len(input.FilePaths))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	c := grillClient(token)

	for i, fp := range input.FilePaths {
		wg.Add(1)
		go func(i int, fp string) {
			sem <- struct{}{}
			defer wg.Done()
			defer func() { <-sem }()

			data, filename, err := resolveGrillIngestPayload(GrillIngestInput{FilePath: fp})
			if err != nil {
				results[i] = GrillIngestBatchResult{FilePath: fp, Error: err.Error()}
				return
			}

			body, st, err := grillIngestData(c, data, filename, projectID)
			if err != nil {
				results[i] = GrillIngestBatchResult{FilePath: fp, Error: err.Error()}
				return
			}
			if authErr := interpretAuthError(ctx, input.Token, st, body, "grill ingest"); authErr != "" {
				results[i] = GrillIngestBatchResult{FilePath: fp, Error: authErr}
				return
			}
			if st == http.StatusForbidden {
				// interpretAuthError returned "" — this is a quota/capacity error, not auth.
				results[i] = GrillIngestBatchResult{FilePath: fp, Error: fmt.Sprintf("quota exceeded: %s", string(body)), QuotaExceed: true}
				return
			}
			if st != http.StatusCreated {
				results[i] = GrillIngestBatchResult{FilePath: fp, Error: fmt.Sprintf("HTTP %d: %s", st, string(body))}
				return
			}

			j, err := client.ParseJob(body)
			if err != nil || j.JobID == "" {
				results[i] = GrillIngestBatchResult{FilePath: fp, Error: "could not parse job_id: " + string(body)}
				return
			}

			slog.Info("grill batch ingest", "file_path", fp, "job_id", j.JobID)
			results[i] = GrillIngestBatchResult{FilePath: fp, JobID: j.JobID}
		}(i, fp)
	}
	wg.Wait()

	var submittedCount, quotaExceededCount, failedCount int
	for _, r := range results {
		switch {
		case r.JobID != "":
			submittedCount++
		case r.QuotaExceed:
			quotaExceededCount++
		default:
			failedCount++
		}
	}

	out := GrillIngestBatchOutput{
		Results:            results,
		SubmittedCount:     submittedCount,
		FailedCount:        failedCount,
		QuotaExceededCount: quotaExceededCount,
	}
	if submittedCount == 0 && quotaExceededCount == 0 {
		out.Error = fmt.Sprintf("all %d file(s) failed to submit", len(results))
		return errResult(), out, nil
	}
	return nil, out, nil
}

// -- Grill Jobs Status -----------------------------------------------

var grillJobsStatusInputSchema = &jsonschema.Schema{
	Type: "object",
	Properties: map[string]*jsonschema.Schema{
		"job_ids": {
			Type:        "array",
			Description: "Job IDs to query. Max 50.",
			Items:       &jsonschema.Schema{Type: "string"},
		},
		"token": {
			Type:        "string",
			Description: "POMA API JWT. Usually not needed — the server inherits the token from the Authorization header in the MCP client config or the POMA_API_KEY env var. Only pass explicitly to override.",
		},
	},
	Required: []string{"job_ids"},
}

var grillJobsStatusOutputSchema = &jsonschema.Schema{
	Type: "object",
	Properties: map[string]*jsonschema.Schema{
		"results":       {Type: "array"},
		"pending_count": {Type: "integer"},
		"done_count":    {Type: "integer"},
		"failed_count":  {Type: "integer"},
		"error":         {Type: "string"},
	},
}

var grillJobsStatusTool = &mcp.Tool{
	Name:         "grill_jobs_status",
	Description:  "Get current status for one or more POMA Grill jobs (up to 50). Returns a JSON snapshot per job — no streaming. Use periodically after grill_ingest or grill_ingest_batch to check progress. pending_count/done_count/failed_count give a quick summary.",
	InputSchema:  grillJobsStatusInputSchema,
	OutputSchema: grillJobsStatusOutputSchema,
}

type GrillJobsStatusInput struct {
	JobIDs []string `json:"job_ids"`
	Token  string   `json:"token,omitempty"`
}

type GrillJobStatusResult struct {
	JobID      string `json:"job_id"`
	Status     string `json:"status,omitempty"`
	IsTerminal bool   `json:"is_terminal"`
	Error      string `json:"error,omitempty"`
}

type GrillJobsStatusOutput struct {
	Results      []GrillJobStatusResult `json:"results"`
	PendingCount int                    `json:"pending_count"`
	DoneCount    int                    `json:"done_count"`
	FailedCount  int                    `json:"failed_count"`
	Error        string                 `json:"error,omitempty"`
}

func GrillJobsStatus(ctx context.Context, _ *mcp.CallToolRequest, input GrillJobsStatusInput) (*mcp.CallToolResult, GrillJobsStatusOutput, error) {
	token := getToken(ctx, input.Token)
	if token == "" {
		return errResult(), GrillJobsStatusOutput{Error: "token is required (provide token or set POMA_API_KEY on the server)"}, nil
	}
	if len(input.JobIDs) == 0 {
		return errResult(), GrillJobsStatusOutput{Error: "job_ids is required"}, nil
	}
	if len(input.JobIDs) > 50 {
		return errResult(), GrillJobsStatusOutput{Error: "job_ids exceeds limit of 50"}, nil
	}

	results := make([]GrillJobStatusResult, len(input.JobIDs))
	sem := make(chan struct{}, 10)
	var wg sync.WaitGroup

	c := grillClient(token)
	for i, id := range input.JobIDs {
		wg.Add(1)
		go func(i int, id string) {
			sem <- struct{}{}
			defer wg.Done()
			defer func() { <-sem }()

			s, err := peekJobStatus(ctx, c, id)
			if err != nil {
				results[i] = GrillJobStatusResult{JobID: id, Error: err.Error()}
				return
			}
			terminal := s.IsTerminal || isTerminalGrillStatus(s.Status)
			results[i] = GrillJobStatusResult{JobID: id, Status: s.Status, IsTerminal: terminal, Error: s.Error}
		}(i, id)
	}
	wg.Wait()

	var pendingCount, doneCount, failedCount int
	for _, r := range results {
		switch {
		case r.Error != "" || r.Status == "failed":
			failedCount++
		case r.IsTerminal:
			doneCount++
		default:
			pendingCount++
		}
	}

	return nil, GrillJobsStatusOutput{
		Results:      results,
		PendingCount: pendingCount,
		DoneCount:    doneCount,
		FailedCount:  failedCount,
	}, nil
}

// -- Grill Projects --------------------------------------------------

var grillProjectsInputSchema = &jsonschema.Schema{
	Type: "object",
	Properties: map[string]*jsonschema.Schema{
		"token": {
			Type:        "string",
			Description: "POMA API JWT. Usually not needed — the server inherits the token from the Authorization header in the MCP client config or the POMA_API_KEY env var. Only pass explicitly to override.",
		},
		"product": {
			Type:        "string",
			Description: "Filter by product type. If omitted, returns all projects.",
			Enum:        []any{"grill", "primecut"},
		},
	},
}

var grillProjectsOutputSchema = &jsonschema.Schema{
	Type: "object",
	Properties: map[string]*jsonschema.Schema{
		"projects": {Type: "string", Description: "Human-readable list of accessible projects with IDs, names, products, and protection status."},
		"error":    {Type: "string"},
	},
}

var grillProjectsTool = &mcp.Tool{
	Name:         "grill_projects",
	Description:  "List your accessible projects. Returns project IDs, names, product types, and protection status. Use this to find the project_id for a project by name before calling other Grill tools.",
	InputSchema:  grillProjectsInputSchema,
	OutputSchema: grillProjectsOutputSchema,
}

type GrillProjectsInput struct {
	Token   string `json:"token,omitempty"`
	Product string `json:"product,omitempty"`
}

type GrillProjectsOutput struct {
	Projects string `json:"projects,omitempty"`
	Error    string `json:"error,omitempty"`
}

type grillProject struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Product   string `json:"product"`
	Protected bool   `json:"protected"`
	OrgID     string `json:"org_id,omitempty"`
}

func GrillProjects(ctx context.Context, _ *mcp.CallToolRequest, input GrillProjectsInput) (*mcp.CallToolResult, GrillProjectsOutput, error) {
	token := getToken(ctx, input.Token)
	if token == "" {
		return errResult(), GrillProjectsOutput{Error: "token is required (provide token or set POMA_API_KEY on the server)"}, nil
	}

	c := grillClient(token)
	body, st, err := grillListProjects(c, input.Product)
	if err != nil {
		return errResult(), GrillProjectsOutput{Error: err.Error()}, nil
	}
	if authErr := interpretAuthError(ctx, input.Token, st, body, "grill projects"); authErr != "" {
		return errResult(), GrillProjectsOutput{Error: authErr}, nil
	}
	if st != http.StatusOK {
		return errResult(), GrillProjectsOutput{Error: fmt.Sprintf("grill projects: HTTP %d: %s", st, string(body))}, nil
	}

	var projects []grillProject
	if err := json.Unmarshal(body, &projects); err != nil {
		// Try wrapped response { "projects": [...] }
		var wrapped struct {
			Projects []grillProject `json:"projects"`
		}
		if err2 := json.Unmarshal(body, &wrapped); err2 != nil {
			return errResult(), GrillProjectsOutput{Error: fmt.Sprintf("grill projects: parse response: %v", err)}, nil
		}
		projects = wrapped.Projects
	}

	if len(projects) == 0 {
		return nil, GrillProjectsOutput{Projects: "No accessible projects found."}, nil
	}

	var sb strings.Builder
	sb.WriteString("Projects:\n")
	for _, p := range projects {
		line := fmt.Sprintf("- %s (project_id: %s, product: %s, protected: %v", p.Name, p.ID, p.Product, p.Protected)
		if p.OrgID != "" {
			line += fmt.Sprintf(", org: %s", p.OrgID)
		}
		line += ")"
		sb.WriteString(line + "\n")
	}

	slog.Info("grill projects", "count", len(projects))
	return nil, GrillProjectsOutput{Projects: sb.String()}, nil
}
