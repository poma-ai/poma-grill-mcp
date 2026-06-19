# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`poma-grill-mcp` is a [Model Context Protocol](https://modelcontextprotocol.io) server wrapping the POMA Grill context engine API. It lets IDEs (Claude Desktop, Cursor, etc.) ingest documents and perform semantic search via MCP tools. Active tools include `grill_ingest` (+ variants), `grill_search`, `grill_jobs_status`, and `grill_docs_list`.

## Repo layout

- `go/` — current Go implementation (this is the active code path)
- `node/` — planned Node/TypeScript implementation (in progress on `feat/node-variant`)
- `schemas/` — shared tool JSON Schemas (planned; will be the single source of truth)
- `PLAN.md` — Node-variant implementation plan

All Go commands and references below assume `cd go` first.

## Commands

```bash
# Build
cd go && go build -o bin/poma-grill-mcp .

# Run in stdio mode (default for IDE integration)
POMA_API_KEY=<key> ./go/bin/poma-grill-mcp -input -

# Run in HTTP mode
POMA_API_KEY=<key> ./go/bin/poma-grill-mcp -http :8080

# Integration test (requires POMA_API_KEY)
POMA_API_KEY=<key> bash go/test.sh

# Run with verbose MCP logging
MCP_VERBOSE=1 POMA_API_KEY=<key> bash go/test.sh
```

There are no unit tests — `test.sh` is the test harness (Python-based MCP client that spawns the binary).

## Architecture

The server has two modes, selected at startup:

- **Stdio mode** (`-input <file>` or `-input -`): Reads MCP JSON-RPC from file/stdin. If the first message is a `tools/list` request, `go/tools/stdio.go` short-circuits and responds immediately without full initialization.
- **HTTP mode** (`-http <addr>`): Serves MCP at `/`, health check at `/health`.

**Flow for `grill_ingest`** (`go/tools/grill.go`):
1. Accepts base64-encoded file + filename
2. POSTs to POMA Grill API (`https://api.poma-ai.com/v3/grill/`)
3. Reads SSE status stream (`https://api.poma-ai.com/status/v1`) until terminal state
4. Sends each status event as an MCP progress notification
5. Returns `job_id` and status events; `job_id` doubles as `doc_id` for search

**Flow for `grill_search`** (`go/tools/grill.go`):
1. Accepts a natural-language query and optional `doc_filter` (= `job_id` from ingest)
2. Routes to `/grill/searchInDoc` when `doc_filter` is set, `/grill/search` otherwise
3. Returns concatenated context text for RAG prompting

**Tool registration** is in `go/tools/tools.go`. `grill_docs_delete` is not yet wired up pending upstream `poma-cli` client support.

**Grill HTTP client** is in `go/tools/grill_client.go` — placeholder wrappers around `(*client.Client).Do` / `DoJSON` until `poma-cli` adds native Grill methods.

**Authentication**: Per-call `token` argument takes precedence over `POMA_API_KEY` env var (resolved in `go/tools/common.go:getToken`).

**API base URLs** can be overridden via `POMA_API_BASE_URL` and `POMA_STATUS_API_BASE_URL` env vars.

## Key flags

| Flag | Description |
|------|-------------|
| `-input <path\|->` | Stdio mode; `-` reads from stdin |
| `-http <addr>` | HTTP mode (e.g. `:8080`) |

## Release

Push to the `release` branch to trigger the CI pipeline (`.github/workflows/release.yml`). It auto-determines a semver tag, builds multi-platform binaries, publishes to GitHub Releases, updates the Homebrew tap (`poma-ai/homebrew-poma-mcp`), and signs Docker images with cosign.
## Domain

The canonical company domain is **`poma-ai.com`** (e.g. `api.poma-ai.com`, `storage.poma-ai.com`, emails `@poma-ai.com`; GitHub org `poma-ai`). **Never write `poma.ai`** — it is not our domain and has shipped broken links (Slack notifier, MCP examples). Always `poma-ai.com`; fix any `poma.ai` on sight.

The POMA web console is **`console.poma-ai.com`**. **Never write `app.poma-ai.com`** — that URL does not exist. All user-facing links (API key generation, plan upgrades, project settings) point to `https://console.poma-ai.com`; fix any `app.poma-ai.com` on sight.
