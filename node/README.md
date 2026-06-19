# @poma-ai/poma-grill-mcp

[Model Context Protocol](https://modelcontextprotocol.io) server for the [POMA Grill](https://poma-ai.com) context engine. Ingest documents and run semantic search from any MCP client — Claude Code, Claude Desktop, Cursor, or your own agent.

Node/TypeScript implementation; a Go binary with the same tool surface lives at [`go/`](https://github.com/poma-ai/poma-grill-mcp/tree/main/go) in the same repo.

## Install

Requires Node 20+. Get a POMA API key first at [console.poma-ai.com](https://console.poma-ai.com).

### `npx` (no install)

The smoothest path — MCP clients can invoke `npx` directly:
```bash
npx -y @poma-ai/poma-grill-mcp -input -
```

### Global install

```bash
npm install -g @poma-ai/poma-grill-mcp
poma-grill-mcp -input -
```

### From source

```bash
git clone https://github.com/poma-ai/poma-grill-mcp
cd poma-grill-mcp/node
npm install
npm run build
node dist/index.js -input -
```

## Install via MCPB bundle (`.mcpb`)

For desktop hosts that support [MCP Bundles](https://github.com/modelcontextprotocol/mcpb)
(Claude Desktop), you can install with one click instead of editing JSON config.

1. Build the bundle: `bash node/mcpb/build.sh` → produces `node/mcpb/poma-grill-mcp.mcpb`
   (or download it from a release).
2. Open the `.mcpb` in your host (drag into Claude Desktop's **Settings → Extensions**).
3. The host shows a config form — paste your **POMA API Key** into the (masked) field and enable.
   The bundle targets the production API (`https://api.poma-ai.com`); to point at a different
   environment, run the server directly with `POMA_API_BASE_URL` set (see Environment variables).

The host stores the API key in your OS keychain and passes it to the server as `POMA_API_KEY`
at launch — no terminal or env vars required. Bundle definition lives in `node/mcpb/manifest.json`.

## Add to your MCP client

Drop the server into your client's MCP config. The `env.POMA_API_KEY` is read by the binary at startup.

### Claude Code — `~/.claude/claude.json` (or `Settings → MCP servers`)

```json
{
  "mcpServers": {
    "poma": {
      "command": "npx",
      "args": ["-y", "@poma-ai/poma-grill-mcp", "-input", "-"],
      "env": {
        "POMA_API_KEY": "your-api-key"
      }
    }
  }
}
```

### Claude Desktop — `~/Library/Application Support/Claude/claude_desktop_config.json`

Same shape; restart the app after saving.

```json
{
  "mcpServers": {
    "poma": {
      "command": "npx",
      "args": ["-y", "@poma-ai/poma-grill-mcp", "-input", "-"],
      "env": {
        "POMA_API_KEY": "your-api-key"
      }
    }
  }
}
```

### Cursor — `~/.cursor/mcp.json` (or `Settings → MCP`)

```json
{
  "mcpServers": {
    "poma": {
      "command": "npx",
      "args": ["-y", "@poma-ai/poma-grill-mcp", "-input", "-"],
      "env": {
        "POMA_API_KEY": "your-api-key"
      }
    }
  }
}
```

Once configured, ask the agent something like:

> "Ingest `~/docs/report.pdf` with POMA Grill, then search it for 'coverage limits'"

For large files, pass `file_path` rather than `file_base64` so the server reads bytes from disk instead of embedding them in JSON.

## Modes

| Mode | Use it for | Flag |
|------|------------|------|
| stdio | MCP client integration (the default) | `-input -` (or `-input /path/to/jsonrpc.txt` for replay) |
| HTTP | Long-lived server for custom integrations | `-http :8080` — MCP at `POST /`, health at `GET /health` |

## Tools

Six tools, matching the Go implementation:

| Tool | What it does |
|------|--------------|
| `grill_ingest` | Upload a file; returns `job_id` immediately (does not wait for indexing). |
| `grill_ingest_sync` | Upload and wait until terminal status; returns `job_id` + `events`. |
| `grill_ingest_resume` | Reconnect to an in-progress job's status stream and wait until terminal. |
| `grill_ingest_batch` | Upload up to 50 files with concurrency control (default 5, max 10). |
| `grill_jobs_status` | Snapshot status for up to 50 jobs in one call. |
| `grill_search` | Hybrid search returning concatenated context for RAG. `doc_filter` (= `job_id`) restricts to one doc. |

See the top-level [README](https://github.com/poma-ai/poma-grill-mcp#tools) for full argument tables.

## Environment variables

All read at runtime — nothing is baked in at build/publish time.

| Variable | Default | Purpose |
|----------|---------|---------|
| `POMA_API_KEY` | (unset) | API token. Fallback when a tool call doesn't pass an explicit `token`. Without it, every tool returns `"token is required …"`. |
| `POMA_API_BASE_URL` | `https://api.poma-ai.com` | Override the API host. Auto-appends `/v3` unless the URL already ends in `/vN`. |
| `POMA_STATUS_API_BASE_URL` | `https://api.poma-ai.com/status/v1` (or `${POMA_API_BASE_URL}/status/v1`) | Override the SSE status-stream host. Same versioning rule. |
| `GRILL_INGEST_ALLOWED_PREFIX` | (unset = no restriction) | Security guard for `file_path`. When set, ingest rejects any path that doesn't resolve (after symlink evaluation) under this directory. |
| `GRILL_INGEST_MAX_BYTES` | `536870912` (512 MiB) | Max upload size in bytes. `0` = unlimited. Applied to both `file_path` and `file_base64`. |

MCP client `env` blocks **replace** the shell environment for the spawned process. If you `export POMA_API_KEY=...` in your shell, the client won't see it unless its config explicitly forwards it — set it in the client's `env` block instead.

## For contributors

```bash
npm run dev        # tsx, no build step
npm run typecheck  # src + test
npm run build      # tsc + bundle schemas into dist/
npm run smoke      # build + offline smoke harness (8 assertions, no API calls)
```

The smoke harness spawns the built binary and asserts the protocol handshake, tool registration, and per-tool argument validation. Real-API verification is manual: wire the binary into an MCP client and exercise it.

### Tool definitions

Loaded at runtime from [`schemas/tools.json`](https://github.com/poma-ai/poma-grill-mcp/blob/main/schemas/tools.json) — single source of truth shared with the Go implementation. Adding or modifying a tool means editing that file and (until codegen lands) mirroring the change into `go/tools/grill.go`. The build copies `schemas/tools.json` into `dist/schemas/` so the published package is self-contained.

### Layout

```
src/
├── index.ts                  ← entry, flag parsing, stdio/http selection
├── server.ts                 ← MCP Server wiring, dispatcher, progress plumbing
├── schemas.ts                ← loads tools.json (bundled or in-repo)
├── common.ts                 ← token, base-URL helpers, ToolContext, result helpers
├── client/
│   ├── grillClient.ts        ← fetch wrapper, raw-octet ingest, JSON POST
│   ├── ingestPayload.ts      ← file_path/file_base64 resolution + safety guards
│   └── statusStream.ts       ← SSE consumer + non-streaming peekJobStatus
└── tools/
    ├── grillIngest.ts
    ├── grillIngestSync.ts
    ├── grillIngestResume.ts
    ├── grillIngestBatch.ts
    ├── grillJobsStatus.ts
    └── grillSearch.ts

test/
└── smoke.ts                  ← spawn-and-drive offline smoke harness

scripts/
└── copy-schemas.mjs          ← post-tsc step that bundles tools.json into dist/
```

## License

MPL-2.0
