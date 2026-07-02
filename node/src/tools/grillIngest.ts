import type { CallToolResult } from "@modelcontextprotocol/sdk/types.js";
import { errorResult, getProjectID, getToken, interpretAuthError, interpretTooManyJobs, successResult } from "../common.js";
import { GrillClient, parseJob } from "../client/grillClient.js";
import { resolveIngestPayload } from "../client/ingestPayload.js";

export async function grillIngest(
  args: Record<string, unknown>,
  _ctx: import("../common.js").ToolContext,
): Promise<CallToolResult> {
  const token = getToken(args.token);
  if (token === "") {
    return errorResult("token is required (provide token or set POMA_API_KEY on the server)");
  }

  let resolved;
  try {
    resolved = resolveIngestPayload({
      file_base64: typeof args.file_base64 === "string" ? args.file_base64 : undefined,
      file_path: typeof args.file_path === "string" ? args.file_path : undefined,
      filename: typeof args.filename === "string" ? args.filename : undefined,
    });
  } catch (err) {
    return errorResult(err instanceof Error ? err.message : String(err));
  }

  const projectID = getProjectID(args.project_id);
  const client = new GrillClient(token, projectID);
  const res = await client.ingestRaw(resolved.data, resolved.filename);
  const authErr = interpretAuthError(args.token, res.status, res.body, "grill ingest");
  if (authErr) return errorResult(authErr);
  const throttle = interpretTooManyJobs(res.status, res.body);
  if (throttle.ok) {
    return {
      content: [{ type: "text", text: throttle.message }],
      structuredContent: {
        error: throttle.message,
        retryable: true,
        retry_after_seconds: throttle.retryAfterSeconds,
      },
      isError: true,
    };
  }
  if (res.status !== 201) {
    const text = new TextDecoder("utf-8").decode(res.body);
    return errorResult(`grill ingest: HTTP ${res.status}: ${text}`);
  }
  const job = parseJob(res.body);
  if (!job) {
    const text = new TextDecoder("utf-8").decode(res.body);
    return errorResult(`grill ingest: could not parse job_id from response: ${text}`);
  }
  return successResult({ job_id: job.job_id });
}
