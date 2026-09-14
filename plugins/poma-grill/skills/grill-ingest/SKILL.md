---
name: grill-ingest
description: Ingest documents (PDF, DOCX, PPTX, XLSX, HTML, Markdown, plain text, images) into the POMA Grill context engine so they become semantically searchable. Use when the user says "grill my <file>", or asks to ingest, index, upload, or add a document to POMA Grill, or wants a PDF/report/contract made searchable. Also covers batch ingestion, job status polling, and resuming an in-progress ingest.
license: MPL-2.0
compatibility: Requires the poma-grill MCP server from this plugin and a POMA account (OAuth via console.poma-ai.com, or a POMA_API_KEY).
metadata:
  author: poma-ai
  version: "0.5.0"
---

# Ingest into POMA Grill

Grill turns a document into semantically chunked, searchable context. Ingest returns a
`job_id`; once the job reaches a terminal state that same id is the `doc_id` you pass to
`grill_search` as `doc_filter` — except when the result carries a `grill` object, where
`grill.doc_id` is the document to filter on. See [Re-ingesting a file](#re-ingesting-a-file).

## Pick the right tool

| Situation | Tool |
|---|---|
| One file, you want the result before continuing | `grill_ingest_sync` |
| One file, fire-and-forget (poll later) | `grill_ingest` |
| 2–50 files | `grill_ingest_batch` |
| You already have a `job_id` and just need to wait | `grill_ingest_resume` |
| Checking up to 50 jobs at once | `grill_jobs_status` |

Default to `grill_ingest_sync` for a single file. It waits and returns the status events,
so you can tell the user the document is actually searchable rather than merely queued.

## Choosing the input

Provide **exactly one** of `file_path`, `file_base64`, or `url`.

- **`file_path`** — the default for anything local. The MCP server reads the bytes off
  disk. Always prefer this for large files: base64 in tool arguments blows past JSON
  message-size limits and inflates memory on both ends.
  Only works when the server runs on the same machine as the file, which is the case for
  the local stdio config but **not** for the hosted endpoint at `mcp.poma-ai.com`.
- **`file_base64`** — small files only, or when you hold the bytes and not a path.
- **`url`** — a remote document the **POMA Grill backend** fetches itself. The MCP server
  does not download it, so this works fine against the hosted endpoint.

Optional on all three: `filename` (basename shown in the UI; inferred from `file_path` or
content when omitted) and `labels`, a flat `{key: value}` map sent as the `X-Labels`
header. Avoid `:` and `,` in label keys and values — they are the wire delimiters.

## Always report the project

Ingest responses carry a `scope` object naming the project the document landed in. Tell
the user which project that was (`scope.project_name`, or `scope.hint`). Documents are
only searchable within their project, so a silent ingest into the wrong project is the
most common reason a later search comes back empty.

With an **account-level** API key, pass `project_id` (or set `POMA_PROJECT_ID`) to choose
the target. With a **project** key the scope is fixed and `project_id` is unnecessary.
Use `grill_projects` to list available projects and see which one is the default.

## Backpressure

A response with `code: "too_many_jobs"` and `retryable: true` means the account is at its
concurrent-job ceiling and **the document was not ingested**. Wait
`retry_after_seconds`, then retry the identical call. Do not retry in a tight loop, and
pause any further ingests until capacity frees up. In `grill_ingest_batch` these surface
per file as `quota_exceeded` entries in `quota_exceeded_count`; re-submit just those once
running jobs finish.

For batch, `concurrency` defaults to 5 and caps at 10. Use `1` on free-tier accounts.

## Errors

Every error carries a machine-readable `code`. Branch on the code, never on the prose
message. Retry only when `retryable` is true, and only after `retry_after_seconds`.
Full table: [references/errors.md](references/errors.md).

## Re-ingesting a file

Re-ingesting is safe and the job succeeds either way, but what Grill did with it depends on
whether anything changed. When the gateway reports the outcome, `grill_ingest_sync`,
`grill_ingest_resume` and each `grill_jobs_status` result carry a `grill` object:
`{"deduplicated": bool, "doc_id": string, "replaced_doc_ids": [string]}`.

Whenever that object is present, **`grill.doc_id` is the document to pass as `doc_filter`** —
not `job_id`. The two differ on a dedup hit, and filtering on the `job_id` there matches no
indexed document and silently returns nothing.

- `deduplicated: true` — the same bytes under the same conversion build were already indexed.
  Nothing new was stored (conversion credits are still consumed) and `doc_id` names the
  document that was already there.
- `deduplicated: false` with `replaced_doc_ids` — a newer conversion build replaced the
  listed older documents; `doc_id` is the replacement.
- `deduplicated: false` with no `replaced_doc_ids` — a plain ingest; `doc_id` equals `job_id`.

The object is absent on older gateways, or before the job reaches the grill stage.

## Worked example

User: "grill my ~/docs/contract.pdf and find the indemnification clause"

1. `grill_ingest_sync` with `file_path: "/Users/me/docs/contract.pdf"`.
2. Read `job_id`, `grill` and `scope` from the result; tell the user the project name.
3. `grill_search` with `query: "indemnification clause"`, and `doc_filter` set to
   `grill.doc_id` when the result carried a `grill` object, otherwise to `job_id`.

## Very large files, no MCP

Documents big enough to strain the MCP transport can go straight to the API: `POST` the
raw bytes to `/grill/ingest` with `Content-Type: application/octet-stream` and
`Content-Disposition: attachment; filename="…"`. You get back the same `job_id`, usable as
`doc_filter` in `grill_search` under the same caveat as above. The Go binary also exposes `POST /ingest-upload` in HTTP
mode for the same purpose.
