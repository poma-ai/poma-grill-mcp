package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

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
		"job_id":              {Type: "string"},
		"events":              {Type: "array"},
		"scope":               scopeSchema,
		"error":               {Type: "string"},
		"retryable":           {Type: "boolean", Description: "True when the error is transient job-capacity backpressure (too_many_jobs): the ingest did not happen; retry the same call after retry_after_seconds and pause new ingests until capacity frees."},
		"retry_after_seconds": {Type: "integer", Description: "Suggested seconds to wait before retrying when retryable is true."},
	},
}

var grillIngestTool = &mcp.Tool{
	Name: "grill_ingest",
	Annotations: &mcp.ToolAnnotations{
		// Additive (adds a doc), not destructive; each call creates a new
		// job/doc, so it is not idempotent.
		Title:           "Ingest Document",
		DestructiveHint: boolPtr(false),
		OpenWorldHint:   boolPtr(false),
	},
	Description:  "Ingest a file into POMA Grill (context engine). Provide exactly one of file_path (large/local) or file_base64 (small). Returns job_id. Once done, doc_id equals job_id for grill_search. The response includes a `scope` object identifying which project the document was ingested into — ALWAYS tell the user the project (scope.project_name / scope.hint). Backpressure: if the response has retryable=true (too_many_jobs — account at concurrent-job capacity), the document was NOT ingested; wait retry_after_seconds and retry the SAME call, and pause new ingests until capacity frees rather than retrying in a tight loop.",
	InputSchema:  grillIngestInputSchema,
	OutputSchema: grillIngestOutputSchema,
}

var grillIngestSyncTool = &mcp.Tool{
	Name: "grill_ingest_sync",
	Annotations: &mcp.ToolAnnotations{
		Title:           "Ingest Document (await completion)",
		DestructiveHint: boolPtr(false),
		OpenWorldHint:   boolPtr(false),
	},
	Description:  "Ingest a file into POMA Grill; waits until terminal state. Provide exactly one of file_path (large/local) or file_base64 (small). Returns job_id and status events. The response includes a `scope` object identifying which project the document was ingested into — ALWAYS tell the user the project (scope.project_name / scope.hint). Backpressure: if the response has retryable=true (too_many_jobs — account at concurrent-job capacity), the document was NOT ingested; wait retry_after_seconds and retry the SAME call, and pause new ingests until capacity frees.",
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
	JobID             string          `json:"job_id,omitempty"`
	Events            []jobStatusFull `json:"events,omitempty"`
	Scope             *GrillScope     `json:"scope,omitempty"`
	Error             string          `json:"error,omitempty"`
	Retryable         bool            `json:"retryable,omitempty"`
	RetryAfterSeconds int             `json:"retry_after_seconds,omitempty"`
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
	if throttle, retryAfter, ok := interpretTooManyJobs(st, body); ok {
		return errResult(), GrillIngestOutput{Error: throttle, Retryable: true, RetryAfterSeconds: retryAfter}, nil
	}
	if st != http.StatusCreated {
		return errResult(), GrillIngestOutput{Error: fmt.Sprintf("grill ingest: HTTP %d: %s", st, string(body))}, nil
	}

	j, err := client.ParseJob(body)
	if err != nil || j.JobID == "" {
		return errResult(), GrillIngestOutput{Error: fmt.Sprintf("grill ingest: could not parse job_id from response: %s", string(body))}, nil
	}
	slog.Info("grill ingest job_id", "job_id", j.JobID)

	_, source := projectIDSource(input.ProjectID)
	scope := resolveScope(c, token, projectID, "", source)

	if !waitTerminal {
		return nil, GrillIngestOutput{JobID: j.JobID, Scope: scope}, nil
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
	return nil, GrillIngestOutput{JobID: j.JobID, Events: events, Scope: scope}, nil
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
	Name: "grill_ingest_resume",
	Annotations: &mcp.ToolAnnotations{
		// Resume only observes an existing job's status stream; it does not
		// create or modify server-side state.
		Title:         "Resume Ingest Tracking",
		ReadOnlyHint:  true,
		OpenWorldHint: boolPtr(false),
	},
	Description:  "Resume tracking an in-progress POMA Grill ingestion job started by an earlier grill_ingest call. Connects to the status SSE stream for the given job_id and waits until a terminal state (done, failed, grilled, deleted), emitting progress notifications.",
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
		"scope":   scopeSchema,
		"error":   {Type: "string"},
	},
}

var grillSearchTool = &mcp.Tool{
	Name: "grill_search",
	Annotations: &mcp.ToolAnnotations{
		Title:         "Search Context Engine",
		ReadOnlyHint:  true,
		OpenWorldHint: boolPtr(false),
	},
	Description:  "Search the POMA Grill context engine and return a context block for RAG. The doc_filter parameter restricts the search to a single document (its doc_id, which equals the job_id from grill_ingest); exclude_doc_ids omits the given doc_ids from results. Result count is bounded server-side by relevance and a token budget — there is no top_k. The response includes a `scope` object identifying which project was searched — ALWAYS tell the user the project (scope.project_name / scope.hint) when presenting results.",
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
	Scope  *GrillScope    `json:"scope,omitempty"`
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
	_, source := projectIDSource(input.ProjectID)
	scope := resolveScope(c, token, projectID, "", source)
	slog.Info("grill search", "doc_filter", input.DocFilter, "context_bytes", len(result.Context), "assets_docs", len(result.Assets), "project", scope.ProjectName)
	// Set Content explicitly to just the prompt-ready context text so the
	// assets payload (potentially large base64 image data URIs) doesn't get
	// duplicated into the text content block — the SDK still populates
	// StructuredContent from the typed output (see go-sdk mcp/server.go).
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: result.Context}},
	}, GrillSearchOutput{Context: result.Context, Assets: result.Assets, Scope: scope}, nil
}

// -- Project Scope ---------------------------------------------------
//
// Every grill operation runs against exactly one project namespace. Named
// projects use their project_id as the namespace; the account's default
// workspace uses "account_<account_id>". These tools surface which project the
// data belongs to via a `scope` object so the calling LLM can always tell the
// user, e.g. "these documents belong to your Default Workspace".

// GrillScope describes which project an operation's data belongs to.
type GrillScope struct {
	ProjectName string `json:"project_name,omitempty"`
	ProjectID   string `json:"project_id,omitempty"`
	Namespace   string `json:"namespace,omitempty"`
	IsDefault   bool   `json:"is_default,omitempty"`
	Source      string `json:"source,omitempty"`
	Hint        string `json:"hint,omitempty"`
}

// scopeSchema is the shared output schema entry for the scope object.
var scopeSchema = &jsonschema.Schema{
	Type:        "object",
	Description: "Which project this result belongs to. ALWAYS tell the user the project (scope.project_name / scope.hint) when presenting results.",
	Properties: map[string]*jsonschema.Schema{
		"project_name": {Type: "string", Description: "Human-readable project name, e.g. \"Default Workspace\"."},
		"project_id":   {Type: "string"},
		"namespace":    {Type: "string"},
		"is_default":   {Type: "boolean", Description: "True when this is the account's default workspace (no specific project selected)."},
		"source":       {Type: "string", Description: "How the project was determined (project_id argument, POMA_PROJECT_ID env var, or account default)."},
		"hint":         {Type: "string", Description: "Ready-to-relay sentence naming the project for the user."},
	},
}

// projectsCache memoizes the /projects listing per token so scope resolution
// does not add a round-trip to every search/ingest. Entries expire after
// projectsCacheTTL; the default workspace never changes, so even a stale cache
// resolves the common (account-default) case correctly.
//
// MULTI-TENANT SAFETY: this MCP is deployed and serves many users concurrently,
// each with their own token. The cache is partitioned by a hash of the token, so
// an entry is only ever readable by a caller presenting the SAME token — i.e.
// the same account. A user can never read another user's cached projects. The
// token itself is never stored (only its SHA-256), so raw secrets are not
// retained in memory, and expired entries are evicted so the map stays bounded
// to roughly the number of users active within a TTL window.
const (
	projectsCacheTTL        = 5 * time.Minute
	projectsCacheMaxEntries = 10000 // pathological-churn backstop
)

type projectsCacheEntry struct {
	projects []grillProject
	expires  time.Time
}

var (
	projectsCacheMu sync.Mutex
	projectsCache   = map[string]projectsCacheEntry{}
	// nowFunc is overridable in tests to exercise expiry deterministically.
	nowFunc = time.Now
)

// projectsCacheKey derives a non-reversible per-token key. Different tokens
// (different users) always map to different keys; the raw token is never used as
// a map key, so it is not retained in the cache.
func projectsCacheKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func projectsCacheGet(token string) ([]grillProject, bool) {
	if token == "" {
		return nil, false
	}
	key := projectsCacheKey(token)
	projectsCacheMu.Lock()
	defer projectsCacheMu.Unlock()
	e, ok := projectsCache[key]
	if !ok || !nowFunc().Before(e.expires) {
		return nil, false
	}
	return e.projects, true
}

func projectsCachePut(token string, projects []grillProject) {
	if token == "" {
		return
	}
	key := projectsCacheKey(token)
	now := nowFunc()
	projectsCacheMu.Lock()
	defer projectsCacheMu.Unlock()
	// Evict expired entries so the map stays bounded to active users.
	for k, e := range projectsCache {
		if !now.Before(e.expires) {
			delete(projectsCache, k)
		}
	}
	// Hard backstop against pathological churn (many short-lived tokens).
	if len(projectsCache) >= projectsCacheMaxEntries {
		projectsCache = make(map[string]projectsCacheEntry)
	}
	projectsCache[key] = projectsCacheEntry{projects: projects, expires: now.Add(projectsCacheTTL)}
}

// fetchProjectsCached returns the token's grill projects, using a short-lived
// cache. Returns nil on any error — scope resolution degrades gracefully and
// never blocks the primary operation.
func fetchProjectsCached(c *client.Client, token string) []grillProject {
	if projects, ok := projectsCacheGet(token); ok {
		return projects
	}
	body, st, err := grillListProjects(c, "grill")
	if err != nil || st != http.StatusOK {
		return nil
	}
	projects, err := parseProjects(body)
	if err != nil {
		return nil
	}
	projectsCachePut(token, projects)
	return projects
}

// resolveScope maps a request's project context to a friendly scope. Provide the
// authoritative namespace when known (grill_docs_list returns it); otherwise
// pass "" and the resolved project_id. It never returns nil.
func resolveScope(c *client.Client, token, resolvedProjectID, namespace, source string) *GrillScope {
	return scopeFromProjects(fetchProjectsCached(c, token), resolvedProjectID, namespace, source)
}

// scopeFromProjects is the pure mapping from a project listing + request context
// to a friendly scope. Split out from resolveScope so it is testable without a
// network round-trip. projects may be nil (resolution degrades to the raw
// identifiers). It never returns nil.
func scopeFromProjects(projects []grillProject, resolvedProjectID, namespace, source string) *GrillScope {
	scope := &GrillScope{ProjectID: resolvedProjectID, Namespace: namespace, Source: source}

	find := func(pred func(grillProject) bool) *grillProject {
		for i := range projects {
			if pred(projects[i]) {
				return &projects[i]
			}
		}
		return nil
	}

	var p *grillProject
	switch {
	case namespace != "":
		// Grill docs namespaces are "account_<account_id>" for the default
		// workspace and "proj_<project_id>" for named projects. Tolerate a bare
		// id too, in case the wire format changes.
		if acct, ok := strings.CutPrefix(namespace, "account_"); ok {
			p = find(func(x grillProject) bool {
				return x.Product == "grill" && x.IsDefault && x.AccountID == acct
			})
		} else {
			id := strings.TrimPrefix(namespace, "proj_")
			p = find(func(x grillProject) bool {
				return x.ProjectID == id || x.ID == id
			})
		}
	case resolvedProjectID != "":
		p = find(func(x grillProject) bool {
			return x.ProjectID == resolvedProjectID || x.ID == resolvedProjectID
		})
	default:
		// Account default: the key owner's default grill workspace (own account,
		// not an org's) — identified by is_default with no orga.
		p = find(func(x grillProject) bool {
			return x.Product == "grill" && x.IsDefault && x.OrgaID == ""
		})
	}

	if p != nil {
		scope.ProjectName = p.Name
		scope.ProjectID = p.ProjectID
		scope.IsDefault = p.IsDefault
		if scope.Namespace == "" {
			if p.IsDefault {
				scope.Namespace = "account_" + p.AccountID
			} else {
				scope.Namespace = "proj_" + p.ProjectID
			}
		}
	}

	switch {
	case scope.ProjectName != "" && scope.IsDefault:
		scope.Hint = fmt.Sprintf("This belongs to your default grill workspace %q — no specific project is selected. Pass project_id or set POMA_PROJECT_ID to target another project.", scope.ProjectName)
	case scope.ProjectName != "":
		scope.Hint = fmt.Sprintf("Scoped to project %q.", scope.ProjectName)
	case resolvedProjectID != "":
		scope.Hint = fmt.Sprintf("Scoped to project_id %s (name unavailable).", resolvedProjectID)
	default:
		scope.Hint = "This belongs to your default grill workspace — no specific project is selected."
	}
	return scope
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
		"note": {
			Type:        "string",
			Description: "Present only when the returned documents are fewer than total_documents (internal pagination cap reached, a follow-up page failed, or some documents were temporarily unavailable). Explains the gap.",
		},
		"scope": scopeSchema,
		"error": {Type: "string"},
	},
}

var grillDocsListTool = &mcp.Tool{
	Name: "grill_docs_list",
	Annotations: &mcp.ToolAnnotations{
		Title:         "List Documents",
		ReadOnlyHint:  true,
		OpenWorldHint: boolPtr(false),
	},
	Description:  "List documents currently ingested into POMA Grill for the authenticated project namespace. The tool follows server-side pagination internally and returns the complete merged list in one response; `total_documents` is the authoritative full count. If fewer documents than total_documents are returned (safety cap or temporarily unavailable documents), a `note` field explains the gap — surface it to the user. Returns metadata only (doc_id, filename, ingested_at, chunk/page counts, etc.); document content is retrieved via grill_search. A returned doc_id serves as the doc_filter on grill_search to scope a query to a specific document. The response includes a `scope` object identifying which project these documents belong to — ALWAYS tell the user the project (scope.project_name / scope.hint) when presenting the list.",
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
	Note           string         `json:"note,omitempty"`
	Scope          *GrillScope    `json:"scope,omitempty"`
	Error          string         `json:"error,omitempty"`
}

// grillDocsMaxPages caps transparent auto-paging: the tool follows next_cursor
// for at most this many pages per call, then returns what it has accumulated
// with a truncation note.
const grillDocsMaxPages = 10

// grillDocsPage is one wire page of GET /grill/docs. On the currently deployed
// API has_more/next_cursor/degraded are absent and unmarshal to their zero
// values, which collapses the auto-paging loop to exactly one request —
// today's single-request behavior.
type grillDocsPage struct {
	Documents      []GrillDocInfo `json:"documents"`
	Namespace      string         `json:"namespace"`
	TotalDocuments int            `json:"total_documents"`
	HasMore        bool           `json:"has_more"`
	NextCursor     string         `json:"next_cursor"`
	Degraded       bool           `json:"degraded"`
}

// grillDocsListNote builds the LLM-facing annotation for a merged docs
// listing. Empty when the listing is complete: truncated marks that the server
// reported more documents than were accumulated (page cap or a failed
// follow-up page), degraded that at least one page was served with documents
// temporarily unavailable, and pagingErr carries the error of a follow-up page
// that failed after the first page succeeded.
func grillDocsListNote(shown, total int, truncated, degraded bool, pagingErr string) string {
	var notes []string
	if truncated || shown < total {
		if total < shown {
			total = shown // defensive: never claim less than what is returned
		}
		notes = append(notes, fmt.Sprintf("Showing %d of %d documents.", shown, total))
	}
	if pagingErr != "" {
		notes = append(notes, fmt.Sprintf("Fetching additional pages failed (%s); the list may be incomplete.", pagingErr))
	}
	if degraded {
		notes = append(notes, "Some documents were temporarily unavailable when this list was generated; retry later for a complete listing.")
	}
	return strings.Join(notes, " ")
}

func GrillDocsList(ctx context.Context, _ *mcp.CallToolRequest, input GrillDocsListInput) (*mcp.CallToolResult, GrillDocsListOutput, error) {
	token := getToken(ctx, input.Token)
	if token == "" {
		return errResult(), GrillDocsListOutput{Error: "token is required (provide token or set POMA_API_KEY on the server)"}, nil
	}

	projectID := getProjectID(input.ProjectID)
	c := grillClient(token)

	// fetchPage returns the parsed page, an error message (empty on success),
	// and whether that error is an auth/billing failure — which is fatal, not a
	// transient paging hiccup, and must abort the whole call.
	fetchPage := func(cursor string) (grillDocsPage, string, bool) {
		var p grillDocsPage
		body, st, err := grillListDocs(c, projectID, cursor)
		if err != nil {
			return p, err.Error(), false
		}
		if authErr := interpretAuthError(ctx, input.Token, st, body, "grill docs list"); authErr != "" {
			return p, authErr, true
		}
		if st != http.StatusOK {
			return p, fmt.Sprintf("grill docs list: HTTP %d: %s", st, string(body)), false
		}
		if err := json.Unmarshal(body, &p); err != nil {
			return p, fmt.Sprintf("grill docs list: parse response: %v", err), false
		}
		return p, "", false
	}

	var (
		docs      = []GrillDocInfo{}
		namespace string
		total     int
		degraded  bool
		truncated bool   // server reported more documents than were accumulated
		pagingErr string // a follow-up page failed after the first page succeeded
		cursor    string
	)
	for page := 0; page < grillDocsMaxPages; page++ {
		p, errMsg, isAuthErr := fetchPage(cursor)
		if errMsg != "" {
			// An auth/billing failure is not transient: the credential is bad,
			// expired, or forbidden and every further page would fail the same
			// way. Surface the actionable message as a hard error on any page,
			// rather than burying it in a note.
			if page == 0 || isAuthErr {
				return errResult(), GrillDocsListOutput{Error: errMsg}, nil
			}
			// Keep the pages already fetched; surface the gap in the note.
			truncated = true
			pagingErr = errMsg
			break
		}
		docs = append(docs, p.Documents...)
		if p.Namespace != "" {
			namespace = p.Namespace
		}
		if p.TotalDocuments > 0 {
			total = p.TotalDocuments
		}
		degraded = degraded || p.Degraded
		truncated = p.HasMore
		// Old API (fields absent), last page, or a cursor the loop cannot make
		// progress with: stop.
		if !p.HasMore || p.NextCursor == "" || p.NextCursor == cursor {
			break
		}
		cursor = p.NextCursor
	}
	if total == 0 {
		total = len(docs) // pre-pagination API always sends total_documents == len(documents); keep len as the authoritative count
	}

	out := GrillDocsListOutput{
		Documents:      docs,
		Namespace:      namespace,
		TotalDocuments: total,
		Note:           grillDocsListNote(len(docs), total, truncated, degraded, pagingErr),
	}
	_, source := projectIDSource(input.ProjectID)
	out.Scope = resolveScope(c, token, projectID, out.Namespace, source)
	slog.Info("grill docs list", "count", out.TotalDocuments, "returned", len(out.Documents), "namespace", out.Namespace, "project", out.Scope.ProjectName)
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
		"scope":                scopeSchema,
		"error":                {Type: "string"},
	},
}

var grillIngestBatchTool = &mcp.Tool{
	Name: "grill_ingest_batch",
	Annotations: &mcp.ToolAnnotations{
		Title:           "Ingest Documents (batch)",
		DestructiveHint: boolPtr(false),
		OpenWorldHint:   boolPtr(false),
	},
	Description:  "Ingest multiple files into POMA Grill with controlled upload concurrency (default 5, max 10). Accepts up to 50 file paths. Returns job_ids immediately after uploads complete — does not wait for server-side processing; progress is reported by grill_jobs_status. The response includes a `scope` object identifying which project the documents were ingested into — ALWAYS tell the user the project (scope.project_name / scope.hint). Free-tier accounts should set concurrency to 1. When the account is at its concurrent-job capacity the API returns HTTP 429 too_many_jobs; those files come back with quota_exceed=true (counted in quota_exceeded_count) — they were NOT ingested. Retry only the quota_exceed files once running jobs finish (poll grill_jobs_status); lower concurrency if it recurs.",
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
	Scope              *GrillScope              `json:"scope,omitempty"`
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
			if throttle, _, ok := interpretTooManyJobs(st, body); ok {
				// Job-capacity backpressure (HTTP 429 too_many_jobs). Transient —
				// bucket as quota_exceed so the caller retries once slots free.
				results[i] = GrillIngestBatchResult{FilePath: fp, Error: throttle, QuotaExceed: true}
				return
			}
			if st == http.StatusForbidden {
				// interpretAuthError returned "" — legacy quota/capacity 403 (older API), not auth.
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
	_, source := projectIDSource(input.ProjectID)
	out.Scope = resolveScope(c, token, projectID, "", source)
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
	Name: "grill_jobs_status",
	Annotations: &mcp.ToolAnnotations{
		Title:         "Job Status",
		ReadOnlyHint:  true,
		OpenWorldHint: boolPtr(false),
	},
	Description:  "Get current status for one or more POMA Grill jobs (up to 50). Returns a JSON snapshot per job — no streaming — reporting progress for jobs created by grill_ingest or grill_ingest_batch. pending_count/done_count/failed_count give a quick summary.",
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
	Name: "grill_projects",
	Annotations: &mcp.ToolAnnotations{
		Title:         "List Projects",
		ReadOnlyHint:  true,
		OpenWorldHint: boolPtr(false),
	},
	Description:  "List your accessible projects. Returns project IDs, names, product types, and protection status, mapping a project name to the project_id used by other Grill tools.",
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
	ProjectID string `json:"project_id"`
	AccountID string `json:"account_id"`
	Name      string `json:"name"`
	Product   string `json:"product"`
	Protected bool   `json:"protected"`
	OrgaID    string `json:"orga_id,omitempty"`
	IsDefault bool   `json:"is_default"`
}

// parseProjects decodes the /projects response, accepting either a bare array or
// a { "projects": [...] } wrapper.
func parseProjects(body []byte) ([]grillProject, error) {
	var projects []grillProject
	if err := json.Unmarshal(body, &projects); err != nil {
		var wrapped struct {
			Projects []grillProject `json:"projects"`
		}
		if err2 := json.Unmarshal(body, &wrapped); err2 != nil {
			return nil, err
		}
		projects = wrapped.Projects
	}
	return projects, nil
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

	projects, err := parseProjects(body)
	if err != nil {
		return errResult(), GrillProjectsOutput{Error: fmt.Sprintf("grill projects: parse response: %v", err)}, nil
	}

	if len(projects) == 0 {
		return nil, GrillProjectsOutput{Projects: "No accessible projects found."}, nil
	}

	var sb strings.Builder
	sb.WriteString("Projects:\n")
	for _, p := range projects {
		line := fmt.Sprintf("- %s (project_id: %s, product: %s, protected: %v, default: %v", p.Name, p.ID, p.Product, p.Protected, p.IsDefault)
		if p.OrgaID != "" {
			line += fmt.Sprintf(", org: %s", p.OrgaID)
		}
		line += ")"
		sb.WriteString(line + "\n")
	}

	slog.Info("grill projects", "count", len(projects))
	return nil, GrillProjectsOutput{Projects: sb.String()}, nil
}
