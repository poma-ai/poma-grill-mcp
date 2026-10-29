import type { CallToolResult } from "@modelcontextprotocol/sdk/types.js";
import {
  codedError,
  ErrorCode,
  getProjectID,
  getToken,
  interpretAuthError,
  interpretProjectConflict,
  projectIDSource,
  successResult,
  type ToolContext,
} from "../common.js";
import { GrillClient } from "../client/grillClient.js";
import { resolveScope, scopeFields } from "../scope.js";

// Used when the gateway omits its own note. Mirrors Go grillAttributesDefaultNote.
export const grillAttributesDefaultNote =
  "Reuse an existing attribute name and type where one fits; a name, once declared, is permanent and counts against max_names.";

interface GrillAttribute {
  name: string;
  type: string;
}

function parseAttributes(raw: unknown): GrillAttribute[] {
  if (!Array.isArray(raw)) return [];
  const out: GrillAttribute[] = [];
  for (const a of raw) {
    if (a && typeof a === "object") {
      const r = a as Record<string, unknown>;
      out.push({
        name: typeof r.name === "string" ? r.name : "",
        type: typeof r.type === "string" ? r.type : "",
      });
    }
  }
  return out;
}

// grillAttributes lists the typed attributes the project has declared.
// Mirrors the Go GrillAttributes handler: GET /attributes with the same
// auth and X-Project-ID handling as grill_docs_list.
export async function grillAttributes(
  args: Record<string, unknown>,
  _ctx: ToolContext,
): Promise<CallToolResult> {
  const token = getToken(args.token);
  if (token === "") {
    // attributes: [] — the output schema declares an array.
    return codedError(ErrorCode.MissingToken, "token is required (provide token or set POMA_API_KEY on the server)", {
      extra: { attributes: [] },
    });
  }

  const projectID = getProjectID(args.project_id);
  const client = new GrillClient(token, projectID);
  let res;
  try {
    res = await client.doGet("/attributes");
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err);
    return codedError(ErrorCode.TransportError, `grill attributes: ${msg}`, { extra: { attributes: [] } });
  }

  const authErr = interpretAuthError(args.token, res.status, res.body, "grill attributes");
  if (authErr) return codedError(authErr.code, authErr.message, { extra: { attributes: [] } });
  const conflict = interpretProjectConflict(res.status, res.body, "grill attributes");
  if (conflict) return codedError(ErrorCode.InvalidInput, conflict.message, { extra: { attributes: [] } });
  const text = new TextDecoder("utf-8").decode(res.body);
  if (res.status !== 200) {
    let msg = `grill attributes: HTTP ${res.status}: ${text}`;
    if (res.status === 503) {
      msg +=
        " (the project's attribute schema could not be read right now; retry later, and do not declare new attribute names until it can be read)";
    }
    return codedError(ErrorCode.UpstreamError, msg, { httpStatus: res.status, extra: { attributes: [] } });
  }

  let parsed: Record<string, unknown>;
  try {
    parsed = JSON.parse(text) as Record<string, unknown>;
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err);
    return codedError(ErrorCode.ParseError, `grill attributes: parse response: ${msg}`, { extra: { attributes: [] } });
  }

  const attributes = parseAttributes(parsed.attributes);
  const maxNames = typeof parsed.max_names === "number" ? parsed.max_names : 0;
  const note = typeof parsed.note === "string" && parsed.note !== "" ? parsed.note : grillAttributesDefaultNote;

  const { source } = projectIDSource(token, args.project_id);
  const scope = await resolveScope(client, token, projectID, "", source);
  const scopeOut = scopeFields(scope);
  return successResult({
    attributes,
    ...(maxNames > 0 ? { max_names: maxNames } : {}),
    note,
    ...(scopeOut ? { scope: scopeOut } : {}),
  });
}
