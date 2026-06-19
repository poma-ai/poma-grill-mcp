# Project plan

This repo carries two MCP server implementations of the same tool surface:

- **Go** under `go/` — production binary; release pipeline lives here.
- **Node/TypeScript** under `node/` — local-first, npm distribution planned.

Tool definitions are shared via `schemas/tools.json`, which the Node implementation loads at startup. The Go implementation currently maintains hand-written copies of the same schemas in `go/tools/grill.go`; codegen to remove that duplication is an open follow-up.

## Active workstreams

- **Node implementation** — see [`NODE_PLAN.md`](./NODE_PLAN.md).

## Cross-cutting follow-ups (not tied to a specific implementation)

- Go-side schema codegen from `schemas/tools.json` to remove the hand-maintained duplication of tool definitions.
- Top-level docs split (`README-go.md`, `README-node.md`) once the root README grows unwieldy.
- Reconsider OpenAPI codegen for the Node HTTP client if/when a Grill-specific spec is published. The current public spec (`https://api.poma-ai.com/api/v3/docs/api/v3/openapi.yaml`) does not document the `/grill/*` endpoints the tools depend on.
