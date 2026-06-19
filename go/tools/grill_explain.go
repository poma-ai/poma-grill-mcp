package tools

import (
	"context"
	"log/slog"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// -- Grill Explain ------------------------------------------------------

const grillExplanation = `# POMA Grill

POMA Grill is a managed context engine. Ingest any document, then search it with natural language.

## Quick Start

1. Get API key from https://console.poma-ai.com
2. ` + "`grill_ingest_sync`" + ` with your file → get ` + "`job_id`" + `
3. ` + "`grill_search`" + ` with your question (optionally set ` + "`doc_filter`" + ` to the ` + "`job_id`" + `)

## Getting an API Key

1. Go to https://console.poma-ai.com
2. Create a new Grill project
3. Copy the generated API key
4. Set it as POMA_API_KEY environment variable, or pass it as the ` + "`token`" + ` argument to any tool

## Ingesting Data

Grill accepts files in any format — PDF, DOCX, TXT, HTML, CSV, Markdown, Json, images, and more. The server handles parsing, chunking, and embedding automatically.

**How to ingest:**
- Use ` + "`grill_ingest`" + ` with either ` + "`file_base64`" + ` (small files) or ` + "`file_path`" + ` (large/local files) — returns immediately with a ` + "`job_id`" + `
- Use ` + "`grill_ingest_sync`" + ` with the same arguments — waits until processing completes
- Use ` + "`grill_ingest_batch`" + ` for multiple files at once (up to 50, controlled concurrency)

**What happens server-side:**
1. File is uploaded to the Grill API
2. A job is created — you receive a ` + "`job_id`" + `
3. The file is parsed, chunked, and embedded
4. Once status reaches "grilled", the document is searchable
5. The ` + "`job_id`" + ` doubles as ` + "`doc_id`" + ` for search filtering

**Tracking progress:**
- ` + "`grill_ingest_sync`" + ` streams status events until completion
- ` + "`grill_ingest_resume`" + ` reconnects to a running job's status stream
- ` + "`grill_jobs_status`" + ` polls current status for one or more jobs

## Searching

Use ` + "`grill_search`" + ` with a natural-language ` + "`query`" + `. The API performs semantic search across all ingested documents.

**Options:**
- ` + "`doc_filter`" + `: restrict search to a single document (pass the ` + "`job_id`" + ` from ingest as ` + "`doc_id`" + `)
- ` + "`exclude_doc_ids`" + `: skip documents already cited (useful in agent loops)
- ` + "`return_assets`" + `: return the cited documents' figures/tables in the ` + "`assets`" + ` output field (keyed by doc_id; images are base64 data URIs)
- ` + "`return_page_images`" + `: DEPRECATED / not available — no-op today (page screenshots are not returned inline)

## Search Results

` + "`grill_search`" + ` returns a single ` + "`context`" + ` field — a concatenated block of the most relevant text chunks, ready for RAG prompting. The server controls result count based on relevance and a token budget.

Example response:
{"context": "<context>\n<doc n=\"1\" id=\"a2541012-a205-4224-816e-23be45bd4826\" rel=\"0.03\">\nChapter 3: Revenue Model\nThe company generates revenue through three primary channels...\n</doc>\n</context>"}

The context block is designed to be inserted directly into an LLM prompt as grounding material.

## Project Selection

You can authenticate with either a **project API key** or an **account-level API key**.

- **Project API key**: Identifies the project automatically. No ` + "`project_id`" + ` needed.
- **Account API key** or **login token**: Pass ` + "`project_id`" + ` on each tool call, set ` + "`POMA_PROJECT_ID`" + ` env var, or use ` + "`grill_projects`" + ` to find project IDs by name.

Use ` + "`grill_projects`" + ` to list your accessible projects and find project IDs.

> **Note:** Your account key grants access to all unprotected projects in your organizations.`

var grillExplainInputSchema = &jsonschema.Schema{
	Type:       "object",
	Properties: map[string]*jsonschema.Schema{},
}

var grillExplainOutputSchema = &jsonschema.Schema{
	Type: "object",
	Properties: map[string]*jsonschema.Schema{
		"explanation": {Type: "string", Description: "Markdown-formatted explanation of POMA Grill."},
	},
}

var grillExplainTool = &mcp.Tool{
	Name:         "grill_explain",
	Description:  "Returns a structured explanation of how POMA Grill works: ingesting data, searching, result format, and how to get an API key. No arguments or authentication required.",
	InputSchema:  grillExplainInputSchema,
	OutputSchema: grillExplainOutputSchema,
}

type GrillExplainInput struct{}

type GrillExplainOutput struct {
	Explanation string `json:"explanation"`
}

func GrillExplain(ctx context.Context, _ *mcp.CallToolRequest, _ GrillExplainInput) (*mcp.CallToolResult, GrillExplainOutput, error) {
	slog.Info("grill explain")
	return nil, GrillExplainOutput{Explanation: grillExplanation}, nil
}
