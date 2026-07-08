import type { CallToolResult } from "@modelcontextprotocol/sdk/types.js";
import { errorResult, getProjectID, getToken, interpretAuthError, successResult, type ToolContext } from "../common.js";
import { GrillClient } from "../client/grillClient.js";

// grillDocsMaxPages caps transparent auto-paging: the tool follows next_cursor
// for at most this many pages per call, then returns what it has accumulated
// with a truncation note. Mirrors the Go implementation.
const grillDocsMaxPages = 10;

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
    return errorResult("token is required (provide token or set POMA_API_KEY on the server)");
  }

  const projectID = getProjectID(args.project_id);
  const client = new GrillClient(token, projectID);

  // Fetches and parses one page; returns an error message instead on failure.
  const fetchPage = async (cursor: string): Promise<GrillDocsPage | { errMsg: string }> => {
    const path = cursor === "" ? "/grill/docs" : `/grill/docs?cursor=${encodeURIComponent(cursor)}`;
    let res;
    try {
      res = await client.doGet(path);
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err);
      return { errMsg: `grill docs list: ${msg}` };
    }

    const authErr = interpretAuthError(args.token, res.status, res.body, "grill docs list");
    if (authErr) return { errMsg: authErr };
    const text = new TextDecoder("utf-8").decode(res.body);
    if (res.status !== 200) {
      return { errMsg: `grill docs list: HTTP ${res.status}: ${text}` };
    }

    try {
      return parseDocsPage(JSON.parse(text) as Record<string, unknown>);
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err);
      return { errMsg: `grill docs list: parse response: ${msg}` };
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
    if ("errMsg" in result) {
      if (page === 0) return errorResult(result.errMsg);
      // Keep the pages already fetched; surface the gap in the note.
      truncated = true;
      pagingErr = result.errMsg;
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
  return successResult({
    documents,
    ...(namespace !== "" ? { namespace } : {}),
    total_documents: total,
    ...(note !== "" ? { note } : {}),
  });
}
