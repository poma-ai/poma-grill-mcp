import type { CallToolResult } from "@modelcontextprotocol/sdk/types.js";
import {
  codedError,
  ErrorCode,
  getProjectID,
  getToken,
  type GrillError,
  interpretAuthError,
  makeGrillError,
  projectIDSource,
  successResult,
  toolError,
  type ToolContext,
} from "../common.js";
import { GrillClient } from "../client/grillClient.js";
import { resolveScope, scopeFields } from "../scope.js";

// grillDocsMaxPages caps transparent auto-paging: the tool follows next_cursor
// for at most this many pages per call, then returns what it has accumulated
// with a truncation note. Mirrors the Go implementation.
const grillDocsMaxPages = 10;

// Sent as an explicit limit on every docs request. Matches the server default,
// so page shapes are unchanged — but its PRESENCE keeps this paging-aware
// client out of grill's legacy mode, whose list_docs_legacy_truncated_total
// metric must only count old, limit-unaware callers. Mirrors the Go client.
const grillDocsPageLimit = 100;

// One wire page of GET /grill/docs. On the currently deployed API
// has_more/next_cursor/degraded are absent and default to their zero values,
// which collapses the auto-paging loop to exactly one request — today's
// single-request behavior.
interface GrillDocsPage {
  documents: unknown[];
  namespace: string;
  totalDocuments: number;
  hasMore: boolean;
  nextCursor: string;
  degraded: boolean;
}

function parseDocsPage(parsed: Record<string, unknown>): GrillDocsPage {
  return {
    documents: Array.isArray(parsed.documents) ? parsed.documents : [],
    namespace: typeof parsed.namespace === "string" ? parsed.namespace : "",
    totalDocuments: typeof parsed.total_documents === "number" ? parsed.total_documents : 0,
    hasMore: parsed.has_more === true,
    nextCursor: typeof parsed.next_cursor === "string" ? parsed.next_cursor : "",
    degraded: parsed.degraded === true,
  };
}

// grillDocsListNote builds the LLM-facing annotation for a merged docs
// listing. Empty when the listing is complete. Mirrors the Go
// grillDocsListNote helper.
export function grillDocsListNote(
  shown: number,
  total: number,
  truncated: boolean,
  degraded: boolean,
  pagingErr: string,
): string {
  const notes: string[] = [];
  if (truncated || shown < total) {
    const claimed = total < shown ? shown : total; // defensive: never claim less than what is returned
    notes.push(`Showing ${shown} of ${claimed} documents.`);
  }
  if (pagingErr !== "") {
    notes.push(`Fetching additional pages failed (${pagingErr}); the list may be incomplete.`);
  }
  if (degraded) {
    notes.push("Some documents were temporarily unavailable when this list was generated; retry later for a complete listing.");
  }
  return notes.join(" ");
}

// grillDocsList lists documents ingested for the authenticated namespace.
// Mirrors the Go GrillDocsList handler: GET /grill/docs, transparently
// following server-side pagination (has_more/next_cursor) and returning the
// merged document list. If the response carries no pagination fields (old
// API), this degrades to exactly one request — today's behavior.
export async function grillDocsList(
  args: Record<string, unknown>,
  _ctx: ToolContext,
): Promise<CallToolResult> {
  const token = getToken(args.token);
  if (token === "") {
    return codedError(ErrorCode.MissingToken, "token is required (provide token or set POMA_GRILL_API_KEY or POMA_API_KEY on the server)");
  }

  const projectID = getProjectID(args.project_id);
  const client = new GrillClient(token, projectID);

  // Fetches and parses one page. On failure returns a structured error plus
  // whether it is an auth/billing failure — which is fatal, not a transient
  // paging hiccup, and must abort the whole call even mid-loop.
  const fetchPage = async (cursor: string): Promise<GrillDocsPage | { err: GrillError; auth: boolean }> => {
    const base = `/grill/docs?limit=${grillDocsPageLimit}`;
    const path = cursor === "" ? base : `${base}&cursor=${encodeURIComponent(cursor)}`;
    let res;
    try {
      res = await client.doGet(path);
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err);
      return { err: makeGrillError(ErrorCode.TransportError, `grill docs list: ${msg}`), auth: false };
    }

    const authErr = interpretAuthError(args.token, res.status, res.body, "grill docs list");
    if (authErr) return { err: makeGrillError(authErr.code, authErr.message), auth: true };
    const text = new TextDecoder("utf-8").decode(res.body);
    if (res.status !== 200) {
      return {
        err: makeGrillError(ErrorCode.UpstreamError, `grill docs list: HTTP ${res.status}: ${text}`, {
          httpStatus: res.status,
        }),
        auth: false,
      };
    }

    try {
      return parseDocsPage(JSON.parse(text) as Record<string, unknown>);
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err);
      return { err: makeGrillError(ErrorCode.ParseError, `grill docs list: parse response: ${msg}`), auth: false };
    }
  };

  const documents: unknown[] = [];
  let namespace = "";
  let total = 0;
  let degraded = false;
  let truncated = false; // server reported more documents than were accumulated
  let pagingErr = ""; // a follow-up page failed after the first page succeeded
  let cursor = "";

  for (let page = 0; page < grillDocsMaxPages; page++) {
    const result = await fetchPage(cursor);
    if ("err" in result) {
      // An auth/billing failure is not transient: the credential is bad,
      // expired, or forbidden and every further page would fail the same way.
      // Surface the actionable structured error as a hard error on any page,
      // rather than burying it in a note.
      if (page === 0 || result.auth) return toolError(result.err);
      // Keep the pages already fetched; surface the gap in the note.
      truncated = true;
      pagingErr = result.err.error;
      break;
    }
    documents.push(...result.documents);
    if (result.namespace !== "") namespace = result.namespace;
    if (result.totalDocuments > 0) total = result.totalDocuments;
    degraded = degraded || result.degraded;
    truncated = result.hasMore;
    // Old API (fields absent), last page, or a cursor the loop cannot make
    // progress with: stop.
    if (!result.hasMore || result.nextCursor === "" || result.nextCursor === cursor) break;
    cursor = result.nextCursor;
  }
  if (total === 0) total = documents.length; // pre-pagination API always sends total_documents == len(documents)

  const note = grillDocsListNote(documents.length, total, truncated, degraded, pagingErr);
  const { source } = projectIDSource(args.project_id);
  const scope = await resolveScope(client, token, projectID, namespace, source);
  const scopeOut = scopeFields(scope);
  return successResult({
    documents,
    ...(namespace !== "" ? { namespace } : {}),
    total_documents: total,
    ...(note !== "" ? { note } : {}),
    ...(scopeOut ? { scope: scopeOut } : {}),
  });
}
