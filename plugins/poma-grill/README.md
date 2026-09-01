# POMA Grill — agent plugin

An [Agent Plugin](https://agent-plugins.org) (spec 1.0.0) that gives any supporting agent
the POMA Grill context engine: ingest documents, then search them semantically.

## Contents

```
poma-grill/
├── plugin.json                              # Agent Plugins manifest
├── mcp.json                                 # Agent Plugins MCP declaration
├── .claude-plugin/plugin.json               # Claude Code manifest
├── .mcp.json                                # Claude Code MCP declaration
├── MANUAL.md                                # install guide: Claude Code, Cursor, Codex
└── skills/
    ├── grill-ingest/
    │   ├── SKILL.md                         # ingest, batch, resume, job status
    │   └── references/errors.md             # error codes, retry rules, env limits
    └── grill-search/
        └── SKILL.md                         # search, doc scoping, projects
```

`plugin.json` and `mcp.json` validate against the 1.0.0 schemas
([plugin](https://agent-plugins.org/schemas/1.0.0/plugin.schema.json),
[mcp](https://agent-plugins.org/schemas/1.0.0/mcp.schema.json)). The skills follow the
[Agent Skills](https://agentskills.io/specification) spec.

The `.claude-plugin/` and `.mcp.json` files are Claude Code's own format, which is not the
Agent Plugins standard — Claude Code reads `.mcp.json`, never `mcp.json`. Both MCP
declarations point at the same endpoint; keep them in sync. Install steps for each client
are in [MANUAL.md](MANUAL.md) — the short version is
`npx plugins add https://github.com/poma-ai/poma-grill-mcp`.

## MCP server

`mcp.json` points at the hosted endpoint, so there is nothing to install:

```json
{
  "poma-grill": {
    "type": "streamable-http",
    "url": "https://mcp.poma-ai.com/grill"
  }
}
```

Auth is OAuth 2.0. The first connection returns `401` with a `WWW-Authenticate` challenge
and the client walks you through a browser login at
[console.poma-ai.com](https://console.poma-ai.com). MCP SDKs handle registration, token
exchange, and refresh automatically.

Prefer an API key over OAuth? Add headers:

```json
{
  "poma-grill": {
    "type": "streamable-http",
    "url": "https://mcp.poma-ai.com/grill",
    "headers": { "x-api-key": "your-api-key" }
  }
}
```

### Running the binary instead

The hosted endpoint cannot read paths on your laptop, so `file_path` ingestion needs a
local server. Swap the entry for the stdio form:

```json
{
  "poma-grill": {
    "type": "stdio",
    "command": "poma-grill-mcp",
    "args": ["-input", "-"],
    "env": { "POMA_API_KEY": "your-api-key" }
  }
}
```

Install the binary via the Homebrew tap, `go install`, or a GitHub release — see the
[repo README](https://github.com/poma-ai/poma-grill-mcp#readme). With the hosted
endpoint, ingest remote documents by `url` or small ones by `file_base64` instead.

## Tools the plugin exposes

| Tool | What it does |
|---|---|
| `grill_explain` | How Grill works, and how to get an API key. No auth. |
| `grill_ingest` | Start an ingest, return `job_id` immediately. |
| `grill_ingest_sync` | Ingest and wait for a terminal state. |
| `grill_ingest_resume` | Reattach to a running job's status stream. |
| `grill_ingest_batch` | Up to 50 files, concurrency 1–10. |
| `grill_jobs_status` | Status snapshots for up to 50 jobs. |
| `grill_search` | Hybrid search returning concatenated RAG context. |
| `grill_docs_list` | Documents in the project, with ids and labels. |
| `grill_projects` | Projects reachable by the credential, and the default. |

## Versioning

`plugin.json` tracks the MCP server version it documents (currently `0.5.0`). When the
tool surface in [`schemas/tools.json`](../../schemas/tools.json) changes, update the
skills and bump `version` here.

## License

MPL-2.0
