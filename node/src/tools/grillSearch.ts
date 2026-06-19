import type { CallToolResult } from "@modelcontextprotocol/sdk/types.js";
import { errorResult, getProjectID, getToken, interpretAuthError, successResult } from "../common.js";
import { GrillClient } from "../client/grillClient.js";

interface SearchRequest {
  query: string;
  doc_filter?: string;
  exclude_doc_ids?: string[];
  return_assets?: boolean;
  return_page_images?: boolean;
}

function buildBody(args: Record<string, unknown>, withDocFilter: boolean): SearchRequest {
  const body: SearchRequest = { query: String(args.query ?? "") };
  if (withDocFilter) body.doc_filter = String(args.doc_filter);
  if (Array.isArray(args.exclude_doc_ids) && args.exclude_doc_ids.length > 0) {
    body.exclude_doc_ids = args.exclude_doc_ids.map(String);
  }
  if (args.return_assets === true) body.return_assets = true;
  if (args.return_page_images === true) body.return_page_images = true;
  return body;
}

export async function grillSearch(
  args: Record<string, unknown>,
  _ctx: import("../common.js").ToolContext,
): Promise<CallToolResult> {
  const token = getToken(args.token);
  if (token === "") {
    return errorResult("token is required (provide token or set POMA_API_KEY on the server)");
  }
  const query = typeof args.query === "string" ? args.query : "";
  if (query === "") {
    return errorResult("query is required");
  }

  const projectID = getProjectID(args.project_id);
  const client = new GrillClient(token, projectID);
  const docFilter = typeof args.doc_filter === "string" && args.doc_filter !== "" ? args.doc_filter : "";
  const path = docFilter !== "" ? "/grill/searchInDoc" : "/grill/search";
  const body = buildBody(args, docFilter !== "");

  const res = await client.doJSON("POST", path, body);
  const authErr = interpretAuthError(args.token, res.status, res.body, "grill search");
  if (authErr) return errorResult(authErr);
  if (res.status !== 200) {
    const text = new TextDecoder("utf-8").decode(res.body);
    return errorResult(`grill search: HTTP ${res.status}: ${text}`);
  }

  let parsed: { context?: string; assets?: Record<string, unknown> | null };
  try {
    parsed = JSON.parse(new TextDecoder("utf-8").decode(res.body)) as {
      context?: string;
      assets?: Record<string, unknown> | null;
    };
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err);
    return errorResult(`grill search: parse response: ${msg}`);
  }

  // Surface return_assets figures/tables (keyed by doc_id; images are base64
  // data URIs) when present. Treat null and absent identically (omit), so the
  // structured output matches the Go variant's `omitzero` map byte-for-byte on
  // every upstream shape: grill emits `assets: null` when there are no figures,
  // which api/go's `omitempty` then collapses to absent — either way we drop it.
  const out: { context: string; assets?: Record<string, unknown> } = {
    context: parsed.context ?? "",
  };
  if (parsed.assets !== undefined && parsed.assets !== null) {
    out.assets = parsed.assets;
  }
  // Use the context string (the prompt-ready RAG block) as the text content so
  // the assets payload — which can carry large base64 image data URIs — isn't
  // duplicated into the text block alongside structuredContent.
  return successResult(out, out.context);
}
