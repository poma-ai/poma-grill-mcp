# POMA Grill — MCP connector

MCP server for the [POMA Grill](https://www.poma-ai.com) context engine. Ingest
documents and run semantic search over them directly from your desktop MCP host
(Claude Desktop and other MCP-compatible clients).

## Install

1. Install this `.mcpb` bundle in your MCP host (in Claude Desktop:
   **Settings → Extensions → Install Extension…**, then select the file).
2. When prompted, enter your **POMA API Key**. It is stored securely by the host
   and passed to the server as the `POMA_API_KEY` environment variable.
3. Get a key from the POMA console: <https://console.poma-ai.com>.

## Tools

| Tool | What it does |
|------|--------------|
| `grill_ingest` | Ingest a document into the context engine — from a local file, base64 bytes, or a remote `url` the server fetches. Optional `labels`. Returns a `job_id`. |
| `grill_ingest_sync` | Ingest a document and wait until processing completes. |
| `grill_ingest_resume` | Resume tracking a previously started ingest job. |
| `grill_ingest_batch` | Ingest multiple documents in one call. |
| `grill_jobs_status` | Check the status of one or more ingest jobs. |
| `grill_search` | Semantic search over ingested documents (returns a context block for RAG). |
| `grill_docs_list` | List documents ingested for the authenticated project. |
| `grill_projects` | List projects accessible with the configured API key. |
| `grill_explain` | Explain how POMA Grill works (no authentication required). |

A returned `job_id` doubles as the `doc_id` for `grill_search` — pass it as
`doc_filter` to scope a query to a single document.

## Privacy & data

This connector sends the documents and queries you provide to the POMA Grill API
(`https://api.poma-ai.com`) for processing. See the privacy and security policy:
<https://www.poma-ai.com/security>.

## Links

- Source & issues: <https://github.com/poma-ai/poma-grill-mcp>
- POMA console (API keys): <https://console.poma-ai.com>

License: MPL-2.0
