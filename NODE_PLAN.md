# Node implementation — current state and remaining work

Working file for the `node/` implementation. PR cadence and process notes have been removed; this is the state of the code, the decisions behind it, and what's still missing.

## Goal

A Node/TypeScript MCP server with the **same tool surface** as the Go server (`grill_ingest`, `grill_ingest_sync`, `grill_ingest_resume`, `grill_ingest_batch`, `grill_jobs_status`, `grill_search`), publishable via npm and runnable in stdio + HTTP modes. Local-first; release/CI integration deferred.

## Decisions

1. **Package name:** `@poma-ai/poma-grill-mcp` on npm.
2. **Schema source-of-truth — cheap path:** Node consumes `schemas/tools.json`; Go keeps its hand-written schemas for now. Go-side codegen is deferred — see *Open follow-ups*.
3. **Top-level dirs:** `go/` and `node/`.
4. **OpenAPI integration deferred:** the public OpenAPI spec at `https://api.poma-ai.com/api/v3/docs/api/v3/openapi.yaml` does **not** document the `/grill/*` endpoints the MCP tools depend on (only `/primeCut/*`, `/jobs/*`, accounts/orgs/projects), nor the `/status/v1/jobs/{id}` SSE stream. Codegen would only cover `GET /jobs/{id}/status` of the endpoints we actually use; not worth the toolchain. Reconsider if/when a Grill spec is published.

## Repo layout (current)

```
poma-grill-mcp/
├── go/                          ← Go implementation (unchanged behaviour, moved into subdir)
│   ├── main.go, go.mod/go.sum, tools/, Dockerfile, .goreleaser.yaml, .dockerignore
├── node/                        ← Node implementation (this plan)
│   ├── package.json             (name: @poma-ai/poma-grill-mcp, ESM, Node 20+)
│   ├── tsconfig.json            (strict, NodeNext, ES2022)
│   ├── src/
│   │   ├── index.ts             ← entry; -input/-http flag parsing
│   │   ├── server.ts            ← MCP Server wiring, dispatcher, progress plumbing
│   │   ├── schemas.ts           ← loads ../schemas/tools.json
│   │   ├── common.ts            ← getToken, apiBaseURL/statusAPIBaseURL, ToolContext, result helpers
│   │   ├── client/
│   │   │   ├── grillClient.ts   ← fetch wrapper, raw-octet ingest, JSON POST
│   │   │   ├── ingestPayload.ts ← file_path/file_base64 resolution, prefix/size guards, magic-byte sniff
│   │   │   └── statusStream.ts  ← SSE consumer, peekJobStatus, isTerminalGrillStatus
│   │   └── tools/               ← six tool handlers (one file each)
│   ├── test/
│   │   └── smoke.ts             ← spawn-and-drive harness (in progress)
│   └── README.md
└── schemas/
    └── tools.json               ← single source of truth for the six tools
```

## What's done

- Repo restructured: Go code under `go/`, CI (`release.yml`, `docker-publish.yml`) updated, `CLAUDE.md` and root `README.md` updated.
- `schemas/tools.json` written and **byte-verified** against the Go binary's `tools/list` response across all six tools (descriptions, input/output schemas all match).
- Node project scaffolded with strict TypeScript; builds clean with `npm run typecheck` and `npm run build`.
- Stdio mode and HTTP mode both work end-to-end at the protocol layer (handshake, `tools/list`, `tools/call`, `notifications/progress`, `/health`).
- Six tool handlers implemented with the same wire shapes and validation behaviour as Go:
  - **`grill_search`** — JSON POST, routes to `/grill/search` or `/grill/searchInDoc` based on `doc_filter`.
  - **`grill_jobs_status`** — fan-out peek with concurrency cap of 10, returns per-job status + counts.
  - **`grill_ingest`** — raw octet-stream POST `/grill/ingest` with `Content-Disposition: attachment; filename="…"`.
  - **`grill_ingest_sync` / `grill_ingest_resume`** — consume the status SSE stream, push each event through `ToolContext.notifyProgress` when the caller passed `_meta.progressToken`, return on terminal state.
  - **`grill_ingest_batch`** — concurrency-limited fan-out (default 5, cap 10, max 50 paths), distinguishes HTTP 403 quota_exceed from other failures.
- Validation messages match Go for missing required args (`query`, `job_ids`, `file_paths`, `job_id`, exclusive `file_base64`/`file_path`).

## Smoke harness — offline-only

`node/test/smoke.ts` (wired as `npm run smoke`):
- Spawns the built binary as a child process and drives it via newline-delimited JSON-RPC over stdio.
- Asserts: `initialize` handshake, `tools/list` returns the six expected tool names, and each tool's argument-validation path returns `isError: true` with the expected message fragment.
- No POMA API calls. Runs anywhere without secrets.

Real-API verification stays manual — wire the binary into an MCP client (Claude Code, Cursor, etc.) and exercise it. The wire format is matched against the Go implementation; integration testing against the live API is intentionally out of scope for the harness.

## Open follow-ups (not part of current Node work)

- **Node release pipeline** — npm publish on `release` branch (next step; schema bundling is now in place — see below).
- **Go schema codegen** from `schemas/tools.json` so both impls share one source of truth (currently both maintained by hand; sync risk noted).
- **Top-level docs split** — `README-go.md`, `README-node.md` split if/when the top-level README grows unwieldy.
- **OpenAPI integration** if a Grill spec is published.

## Done: schema bundling for npm distribution

`scripts/copy-schemas.mjs` (run after `tsc` in `npm run build`) copies `schemas/tools.json` into `node/dist/schemas/tools.json`. The loader at `src/schemas.ts` looks there first and falls back to the in-repo path (`../../schemas/tools.json`) for `tsx` dev. `npm pack` ships the bundled copy; `prepublishOnly` runs the build automatically before publish.

Verified by `npm pack` + `npm install <tarball>` into a scratch project: the installed binary at `node_modules/.bin/poma-grill-mcp` runs, finds the bundled schema, and returns all six tools to `tools/list`.

## Critical files (already in place locally)

| File | Role |
|---|---|
| `schemas/tools.json` | Single source of truth for the six MCP tool definitions. |
| `node/package.json`, `node/tsconfig.json` | Node 20+ ESM, strict TypeScript. |
| `node/src/index.ts` | Flag parsing (`-input`, `-http`); selects `StdioServerTransport` or `StreamableHTTPServerTransport`. |
| `node/src/server.ts` | MCP `Server` wiring; loads schemas; dispatcher; turns `_meta.progressToken` into a `ToolContext.notifyProgress` callback. |
| `node/src/common.ts` | `apiBaseURL` / `statusAPIBaseURL` with `/v3` and `/status/v1` suffix handling matching `go/tools/common.go`. |
| `node/src/client/grillClient.ts` | `fetch` wrapper, `Bearer` auth, raw-octet ingest with `Content-Disposition`, `parseJob`. |
| `node/src/client/ingestPayload.ts` | `file_path`/`file_base64` resolution, `GRILL_INGEST_ALLOWED_PREFIX` symlink check, `GRILL_INGEST_MAX_BYTES` enforcement, magic-byte filename inference (PDF/PNG/JPG/GIF/ZIP). |
| `node/src/client/statusStream.ts` | SSE consumer (`event: job_status`/`data:` line parser), non-streaming `peekJobStatus`, `isTerminalGrillStatus` (`grilled` / `done` / `failed` / `deleted`). |
| `node/src/tools/*.ts` | Six handlers (~30–100 lines each). |
| `node/README.md` | Build/run instructions. |

## Verification

- `cd node && npm install && npm run typecheck` clean.
- `cd node && npm run build` produces `dist/`.
- Stdio mode: `tools/list` matches Go binary byte-for-byte across name, description, inputSchema, outputSchema (Python-based JSON deep-equal).
- Stdio mode: validation paths return matching error messages for missing required args.
- HTTP mode: `GET /health` → 200, `POST /v1` handles MCP `initialize` over SSE.
- `cd node && npm run smoke` exits 0 (offline assertions).
