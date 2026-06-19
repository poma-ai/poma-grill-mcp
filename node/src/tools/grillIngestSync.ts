import type { CallToolResult } from "@modelcontextprotocol/sdk/types.js";
import { errorResult, getProjectID, getToken, interpretAuthError, successResult, type ToolContext } from "../common.js";
import { GrillClient, parseJob } from "../client/grillClient.js";
import { resolveIngestPayload } from "../client/ingestPayload.js";
import { streamJobStatus, type JobStatusFull } from "../client/statusStream.js";

export async function grillIngestSync(
  args: Record<string, unknown>,
  ctx: ToolContext,
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
  const ingestRes = await client.ingestRaw(resolved.data, resolved.filename);
  const authErr = interpretAuthError(args.token, ingestRes.status, ingestRes.body, "grill ingest");
  if (authErr) return errorResult(authErr);
  if (ingestRes.status !== 201) {
    const text = new TextDecoder("utf-8").decode(ingestRes.body);
    return errorResult(`grill ingest: HTTP ${ingestRes.status}: ${text}`);
  }
  const job = parseJob(ingestRes.body);
  if (!job) {
    const text = new TextDecoder("utf-8").decode(ingestRes.body);
    return errorResult(`grill ingest: could not parse job_id from response: ${text}`);
  }

  const events: JobStatusFull[] = [];
  let seq = 0;
  try {
    await streamJobStatus(
      client,
      job.job_id,
      (s) => {
        events.push(s);
        if (ctx.notifyProgress) {
          void ctx.notifyProgress(
            { jobId: job.job_id, status: s.status, ...(s.error ? { error: s.error } : {}) },
            seq,
          );
        }
        seq++;
      },
      ctx.signal,
    );
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err);
    return errorResult(`status stream failed: ${msg}`);
  }

  if (events.length > 0) {
    const last = events[events.length - 1]!;
    if (last.status === "failed") {
      const message = last.error ? `job failed: ${last.error}` : "job failed";
      return {
        content: [{ type: "text", text: message }],
        structuredContent: { job_id: job.job_id, events, error: message },
        isError: true,
      };
    }
  }

  return successResult({ job_id: job.job_id, events });
}
