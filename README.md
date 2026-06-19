# POMA Grill MCP

Ingest documents into the [POMA Grill](https://poma-ai.com) context engine and run semantic search — from any MCP client (Claude Code, Claude Desktop, Cursor, or your own agent).

**Hosted at [`https://mcp.poma-ai.com/grill`](https://mcp.poma-ai.com/grill)** with OAuth 2.0 — no install, no API key in config. See [Option B](#option-b--hosted-http-endpoint-no-binary-required-oauth) below. Or run the binary yourself ([Option A](#option-a--run-the-binary-locally-stdio)).

## Implementations

This repo ships two implementations of the same MCP tool surface. Pick whichever fits your install path — both expose the same six tools (`grill_ingest`, `grill_ingest_sync`, `grill_ingest_resume`, `grill_ingest_batch`, `grill_jobs_status`, `grill_search`) and the same `-input`/`-http` flags, against the same backend.

| | Source | README | Distribution | Status |
|---|---|---|---|---|
| **Go** | [`go/`](./go) | [`go/README.md`](./go/README.md) | Homebrew tap, `go install`, GitHub Releases, Docker image | Stable, production |
| **Node/TypeScript** | [`node/`](./node) | [`node/README.md`](./node/README.md) | Build from source today; npm publish planned (`@poma-ai/poma-grill-mcp`) | Implemented; see [`NODE_PLAN.md`](./NODE_PLAN.md) |

See the README in each folder for implementation-specific details (build, test, architecture).

The Go binary is the default everywhere the instructions below don't specify otherwise.

## 1. Get an API key

Sign up at [console.poma-ai.com](https://console.poma-ai.com) and create a grill project. Copy the API key.

## 2. Install

- **Go** — see [Install](./go/README.md#2-install) in `go/README.md`
- **Node** — see [Install](./node/README.md#install) in `node/README.md`

## 3. Add to your agent

Paste this into your MCP config — replace the path and key.

**Claude Code** — `~/.claude/claude.json` (or `Settings → MCP servers`)

**Claude Desktop** — `~/Library/Application Support/Claude/claude_desktop_config.json` (restart after saving)

**Cursor** — `~/.cursor/mcp.json` (or Settings → MCP)

### Option A — run the binary locally (stdio)

**Go binary**
```json
{
  "mcpServers": {
    "poma": {
      "command": "/full/path/to/poma-grill-mcp",
      "args": ["-input", "-"],
      "env": {
        "POMA_API_KEY": "your-api-key"
      }
    }
  }
}
```

**Node binary** (build-from-source path; once `node/dist/index.js` exists):
```json
{
  "mcpServers": {
    "poma": {
      "command": "node",
      "args": ["/full/path/to/poma-grill-mcp/node/dist/index.js", "-input", "-"],
      "env": {
        "POMA_API_KEY": "your-api-key"
      }
    }
  }
}
```

### Option B — hosted HTTP endpoint (no binary required, OAuth)

POMA runs the server at **`https://mcp.poma-ai.com/grill`**. Point your MCP client at it directly — no local install, no API key in your config.

Auth is **OAuth 2.0**: the first time your client connects, it gets a `401` with a `WWW-Authenticate` challenge, then walks you through a browser-based login at [console.poma-ai.com](https://console.poma-ai.com). MCP SDKs (Claude Code, Claude Desktop, Cursor, etc.) handle this end-to-end — Dynamic Client Registration, authorize, token exchange, refresh — automatically.

**Claude Code / Claude Desktop**
```json
{
  "mcpServers": {
    "poma": {
      "type": "http",
      "url": "https://mcp.poma-ai.com/grill"
    }
  }
}
```

**Cursor** (`~/.cursor/mcp.json`)
```json
{
  "mcpServers": {
    "poma": {
      "url": "https://mcp.poma-ai.com/grill"
    }
  }
}
```

> Prefer an API key instead of OAuth? Add `"headers": { "x-api-key": "your-api-key" }` to the config above. The hosted endpoint accepts both.

That's it. You can now ask the agent to ingest a document and search it:

> "Ingest `/path/to/report.pdf` with POMA Grill using **file_path** (not base64), then search for 'coverage limits'"

For **large files**, have the agent pass `file_path` to `grill_ingest` / `grill_ingest_sync` so bytes are read by the server instead of embedded in JSON. Base64 in tool args hits JSON/message size limits and inflates memory.

---

## Typical workflow

1. **`grill_ingest_sync`** (or `grill_ingest`) — upload a document; use **`file_path`** for large files. Note the returned `job_id` (same as `doc_id` when done).
2. **`grill_search`** — query the context engine; pass `job_id` as `doc_filter` to restrict to one doc

---

## Using from Claude Code

Once the MCP server is configured, just talk to Claude naturally. For **large** documents, ask the agent to call ingest with **`file_path`** so the MCP server reads the file from disk. Base64 is fine for small files only.

### Ingest a document

```
Ingest ~/docs/contract.pdf into POMA Grill
```

```
Use POMA Grill to ingest this file: /Users/me/reports/q1-2026.pdf
```

The agent should call `grill_ingest` or `grill_ingest_sync` with **`file_path`** set to your document path (server-readable path). It will return a `job_id`; keep that ID to search that document later with `doc_filter`.

### Search across all ingested documents

```
Search my POMA Grill for information about termination clauses
```

```
Use POMA Grill to find everything about payment schedules
```

### Search within a specific document

Pass the `job_id` you got from ingest — or just reference the document by name and Claude will use the ID it already knows:

```
Search the contract I just ingested for indemnification
```

```
In the Q1 report (doc_id: abc123...), search for gross margin
```

### Ingest then search in one shot

```
Ingest ~/docs/spec.pdf into POMA Grill, then search it for authentication requirements
```

---

## Tools

| Tool | What it does |
|------|-------------|
| `grill_ingest` | Starts ingest; returns `job_id` after upload (does not wait for indexing to finish). |
| `grill_ingest_sync` | Ingest and wait until the job reaches a terminal state; returns `job_id` and status `events`. |
| `grill_ingest_resume` | Reconnect to an in-progress job's status stream and wait until terminal. Useful when a previous `grill_ingest` returned a `job_id` and you need to wait without re-uploading. |
| `grill_ingest_batch` | Upload up to 50 files with controlled concurrency (default 5, max 10). Returns `job_ids` immediately after uploads complete; use `grill_jobs_status` to monitor. |
| `grill_jobs_status` | Get current status snapshots for up to 50 jobs in one call. No streaming. |
| `grill_search` | Hybrid search returning concatenated context text for RAG. Set `doc_filter` (= `job_id`) to restrict to one document. |

### `grill_ingest` / `grill_ingest_sync` arguments

Provide **exactly one** of `file_path` or `file_base64`.

| Argument | Type | Required | Description |
|----------|------|----------|-------------|
| `file_path` | string | one-of | Path readable by the **MCP server process** (absolute or relative to server cwd). Best for large files; avoids giant JSON payloads. |
| `file_base64` | string | one-of | Standard base64 of the file bytes; fine for small files. |
| `filename` | string | no | Original basename (e.g. `report.pdf`). With `file_path`, defaults to the path basename; otherwise inferred from bytes when possible. |
| `token` | string | no | API key — omit if `POMA_API_KEY` is set on the server process |

**`file_path` notes**

- Works when the server runs on the **same machine** as the file (typical local stdio config). Hosted HTTP MCP (`mcp.poma-ai.com`) cannot read your laptop paths unless that file exists on the host.
- **Security:** optional **`GRILL_INGEST_ALLOWED_PREFIX`**: if set, `file_path` must resolve (after symlink evaluation) under that directory. Non-regular files are rejected.
- **`GRILL_INGEST_MAX_BYTES`**: max payload size in bytes. Unset defaults to 512 MiB. Set to **`0`** for no limit (use with care).

**Very large files without MCP**

You can POST the raw file directly to the POMA API (same shape as the server: `POST` … `/grill/ingest` with octet-stream body and `Content-Disposition: attachment; filename="…"`), obtain `job_id`, then use `grill_search` with `doc_filter` set to that id. This bypasses MCP message limits entirely.

### `grill_search` arguments

| Argument | Type | Required | Description |
|----------|------|----------|-------------|
| `query` | string | yes | Natural-language search query |
| `doc_filter` | string | no | `doc_id` to restrict search to one document |
| `exclude_doc_ids` | array of string | no | Doc ids to exclude from results (max 100). Useful in agent loops to avoid re-citing docs already shown. |
| `return_assets` | boolean | no | Include asset references in context |
| `return_page_images` | boolean | no | Include page image references in context |
| `token` | string | no | API key |

Result count is bounded server-side by relevance and a token budget — there is no `top_k`.

### `grill_ingest_batch` arguments

| Argument | Type | Required | Description |
|----------|------|----------|-------------|
| `file_paths` | array of string | yes | Up to 50 paths readable by the MCP server process. |
| `concurrency` | integer | no | Upload concurrency (default 5, max 10). Use `1` on free-tier accounts. |
| `token` | string | no | API key |

Returns `{results, submitted_count, failed_count, quota_exceeded_count}`. `quota_exceeded` entries (HTTP 403 from the queue) are retryable once running jobs finish.

### `grill_jobs_status` arguments

| Argument | Type | Required | Description |
|----------|------|----------|-------------|
| `job_ids` | array of string | yes | Up to 50 job IDs to query. |
| `token` | string | no | API key |

Returns `{results, pending_count, done_count, failed_count}`. Each result has `{job_id, status, is_terminal, error?}`.

---

## What the output looks like

`grill_ingest_sync` waits until processing reaches a terminal state, then returns (example shape):

```json
{
  "job_id": "100c65a03a304aa343a1518aa79e8300-20260414T083549Z",
  "events": [
    { "status": "queued" },
    { "status": "chunking" },
    { "status": "chunked" },
    { "status": "grilled" },
    { "doc_id": "xxxxxx-xxxxx-xxxxx-xxxxx"}
  ]
}
```

`grill_search` returns:

```json
{
  "context": "<context>This is what is relevant [...] inside your document.</context>"
}
```

Use `context` directly in your RAG prompt.

On error, `isError` is `true` in the MCP response and `error` describes what went wrong:

```json
{ "error": "job failed: unsupported file type" }
```

---

## HTTP mode

Both implementations support a long-lived HTTP server for custom integrations:

```bash
POMA_API_KEY=your-key poma-grill-mcp -http :8080            # Go
POMA_API_KEY=your-key node node/dist/index.js -http :8080   # Node
```

MCP endpoint: `POST http://localhost:8080/`  
Health check: `GET http://localhost:8080/health`

### Large uploads: `POST /ingest-upload` (Go binary only)

The Go binary exposes a direct upload endpoint that bypasses MCP message-size limits. Same auth as MCP: `x-api-key` or `Authorization: Bearer`, or rely on `POMA_API_KEY` on the server.

- **Raw body:** send file bytes as the request body (`application/octet-stream` or any non-multipart type). Pass basename via query `?filename=report.pdf` or header `X-Filename`.
- **Multipart:** `Content-Type: multipart/form-data` with a part named **`file`**.

Response `201` with JSON `{"job_id":"…"}`. Size limits follow **`GRILL_INGEST_MAX_BYTES`** (see above).

```bash
curl -sS -X POST "http://localhost:8080/ingest-upload?filename=report.pdf" \
  -H "x-api-key: $POMA_API_KEY" \
  -H "Content-Type: application/octet-stream" \
  --data-binary @./report.pdf
```

### Docker (Go only)

```bash
docker run -e POMA_API_KEY=your-key -p 8080:8080 ghcr.io/poma-ai/poma-grill-mcp -http :8080
```

Stdio (default entrypoint): `docker run -i -e POMA_API_KEY=your-key ghcr.io/poma-ai/poma-grill-mcp`

A Node Docker image is not currently published.

---

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-input <path\|->` | — | Stdio mode: MCP on stdin (`-`) or file path |
| `-http <addr>` | — | HTTP mode, e.g. `:8080`. Mutually exclusive with `-input`. |

Both implementations accept the same flags.

---

## License

MPL-2.0
