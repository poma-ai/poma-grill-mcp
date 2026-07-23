import type { CallToolResult } from "@modelcontextprotocol/sdk/types.js";
import {
  codedError,
  ErrorCode,
  getProjectID,
  getToken,
  interpretAuthError,
  interpretTooManyJobs,
  makeGrillError,
  projectIDSource,
  successResult,
  toolError,
  type ToolContext,
} from "../common.js";
import { GrillClient, parseJob } from "../client/grillClient.js";
import { resolveIngestPayload } from "../client/ingestPayload.js";
import { resolveScope, scopeFields } from "../scope.js";

export async function grillIngest(
  args: Record<string, unknown>,
  _ctx: ToolContext,
): Promise<CallToolResult> {
  const token = getToken(args.token);
  if (token === "") {
    return codedError(ErrorCode.MissingToken, "token is required (provide token or set POMA_API_KEY on the server)");
  }

  let resolved;
  try {
    resolved = resolveIngestPayload({
      file_base64: typeof args.file_base64 === "string" ? args.file_base64 : undefined,
      file_path: typeof args.file_path === "string" ? args.file_path : undefined,
      filename: typeof args.filename === "string" ? args.filename : undefined,
    });
  } catch (err) {
    return codedError(ErrorCode.InvalidInput, err instanceof Error ? err.message : String(err));
  }

  const projectID = getProjectID(args.project_id);
  const client = new GrillClient(token, projectID);
  const res = await client.ingestRaw(resolved.data, resolved.filename);

  const authErr = interpretAuthError(args.token, res.status, res.body, "grill ingest");
  if (authErr) return codedError(authErr.code, authErr.message);
  const throttle = interpretTooManyJobs(res.status, res.body);
  if (throttle.ok) {
    return toolError(
      makeGrillError(ErrorCode.TooManyJobs, throttle.message, { retryAfterSeconds: throttle.retryAfterSeconds }),
    );
  }
  if (res.status !== 201) {
    const text = new TextDecoder("utf-8").decode(res.body);
    return codedError(ErrorCode.UpstreamError, `grill ingest: HTTP ${res.status}: ${text}`, { httpStatus: res.status });
  }
  const job = parseJob(res.body);
  if (!job) {
    const text = new TextDecoder("utf-8").decode(res.body);
    return codedError(ErrorCode.ParseError, `grill ingest: could not parse job_id from response: ${text}`);
  }

  const { source } = projectIDSource(args.project_id);
  const scope = await resolveScope(client, token, projectID, "", source);
  const scopeOut = scopeFields(scope);
  return successResult({ job_id: job.job_id, ...(scopeOut ? { scope: scopeOut } : {}) });
}
