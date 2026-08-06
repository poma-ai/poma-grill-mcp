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
import { parseLabelsArg, resolveIngestPayload, serializeLabels } from "../client/ingestPayload.js";
import { resolveScope, scopeFields } from "../scope.js";

export async function grillIngest(
  args: Record<string, unknown>,
  _ctx: ToolContext,
): Promise<CallToolResult> {
  const token = getToken(args.token);
  if (token === "") {
    return codedError(ErrorCode.MissingToken, "token is required (provide token or set POMA_API_KEY on the server)");
  }

  const url = typeof args.url === "string" ? args.url.trim() : "";
  const labels = serializeLabels(parseLabelsArg(args.labels));
  const projectID = getProjectID(args.project_id);
  const client = new GrillClient(token, projectID);

  let res;
  if (url !== "") {
    // URL ingest: the server fetches the remote URL. Mutually exclusive with the
    // file inputs.
    if (typeof args.file_path === "string" && args.file_path !== "") {
      return codedError(ErrorCode.InvalidInput, "provide only one of url, file_path, or file_base64");
    }
    if (typeof args.file_base64 === "string" && args.file_base64 !== "") {
      return codedError(ErrorCode.InvalidInput, "provide only one of url, file_path, or file_base64");
    }
    res = await client.ingestRemoteURL(url, labels);
  } else {
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
    res = await client.ingestRaw(resolved.data, resolved.filename, labels);
  }

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
