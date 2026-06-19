import type { CallToolResult } from "@modelcontextprotocol/sdk/types.js";
import { errorResult, getProjectID, getToken, interpretAuthError, successResult, type ToolContext } from "../common.js";
import { GrillClient } from "../client/grillClient.js";

// grillDocsList lists documents ingested for the authenticated namespace.
// Mirrors the Go GrillDocsList handler: GET /grill/docs, pass the response
// (documents / namespace / total_documents) straight through.
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
  let res;
  try {
    res = await client.doGet("/grill/docs");
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err);
    return errorResult(`grill docs list: ${msg}`);
  }

  const authErr = interpretAuthError(args.token, res.status, res.body, "grill docs list");
  if (authErr) return errorResult(authErr);
  const text = new TextDecoder("utf-8").decode(res.body);
  if (res.status !== 200) {
    return errorResult(`grill docs list: HTTP ${res.status}: ${text}`);
  }

  let parsed: Record<string, unknown>;
  try {
    parsed = JSON.parse(text) as Record<string, unknown>;
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err);
    return errorResult(`grill docs list: parse response: ${msg}`);
  }

  return successResult({
    documents: Array.isArray(parsed.documents) ? parsed.documents : [],
    ...(typeof parsed.namespace === "string" ? { namespace: parsed.namespace } : {}),
    total_documents:
      typeof parsed.total_documents === "number"
        ? parsed.total_documents
        : Array.isArray(parsed.documents)
          ? parsed.documents.length
          : 0,
  });
}
