---
name: grill-search
description: Search documents already ingested into the POMA Grill context engine and get back ready-to-use RAG context. Use when the user asks to search, query, look up, find, or ask questions about their POMA documents, wants to restrict a search to one document, needs to list ingested documents, or needs to know which POMA project they are working in.
license: MPL-2.0
compatibility: Requires the poma-grill MCP server from this plugin and a POMA account (OAuth via console.poma-ai.com, or a POMA_API_KEY).
metadata:
  author: poma-ai
  version: "0.5.0"
---

# Search POMA Grill

`grill_search` runs hybrid retrieval over the documents in the active project and returns
one concatenated context block, sized to a server-side token budget:

```json
{ "context": "<context>This is what is relevant [...] inside your document.</context>" }
```

Drop `context` straight into the prompt you are answering from. There is no `top_k` — the
server decides how much to return based on relevance and budget.

## Arguments

| Argument | Use it for |
|---|---|
| `query` | The natural-language question. Required. |
| `doc_filter` | Restrict to a single document. The value is the `job_id` returned by ingest (`doc_id` and `job_id` are the same string). |
| `exclude_doc_ids` | Up to 100 doc ids to leave out. In an agent loop, pass the docs you have already cited so each round surfaces something new. |
| `return_assets` | Include asset references in the context. |
| `return_page_images` | Include page image references. |
| `project_id` | Target project when authenticating with an account-level key. |
| `token` | Override the server's credential. Usually unnecessary. |

## Scope one search or many

Ask which the user means when it is ambiguous:

- **One document** — pass `doc_filter`. Use this right after an ingest, when the user says
  "the contract I just ingested", or when they name a specific file.
- **Everything in the project** — omit `doc_filter`.

Search only sees documents in the **current project**. An empty result on a document you
know was ingested almost always means the ingest and the search resolved to different
projects. Check with `grill_projects`, then re-run with the right `project_id`.

## Finding the right document

- `grill_docs_list` — the documents in the project, with their ids and labels. Use it to
  turn "the Q1 report" into a `doc_filter` without asking the user for an id.
- `grill_projects` — the projects reachable by the current credential, and which one is
  the default.

## Query phrasing

Retrieval is semantic, so full questions beat keyword soup: "what are the termination
notice periods" pulls better context than "termination". When the user's question is
broad, run one search and read the returned context before deciding whether a second,
narrower query is worth it — each call spends its own token budget.

## Errors

Errors carry a machine-readable `code`. `auth_expired`, `forbidden`, and `invalid_input`
are terminal; `transport_error` and 5xx `upstream_error` are retryable after
`retry_after_seconds`. The full table lives in the sibling `grill-ingest` skill, at
`skills/grill-ingest/references/errors.md`.

## Explaining Grill

`grill_explain` returns a structured description of how ingestion, search, and result
formatting work, plus how to obtain an API key. No arguments, no auth. Use it when the
user asks what POMA Grill is or how to get set up, instead of guessing.
