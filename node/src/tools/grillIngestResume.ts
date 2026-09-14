import type { CallToolResult } from "@modelcontextprotocol/sdk/types.js";
import { codedError, ErrorCode, getToken, makeGrillError, successResult, toolError, type ToolContext } from "../common.js";
import { GrillClient } from "../client/grillClient.js";
import { streamJobStatus, type JobStatusFull } from "../client/statusStream.js";

export async function grillIngestResume(
  args: Record<string, unknown>,
  ctx: ToolContext,
): Promise<CallToolResult> {
  const token = getToken(args.token);
  if (token === "") {
    return codedError(ErrorCode.MissingToken, "token is required (provide token or set POMA_GRILL_API_KEY or POMA_API_KEY on the server)");
  }
  const jobID = typeof args.job_id === "string" ? args.job_id : "";
  if (jobID === "") {
    return codedError(ErrorCode.InvalidInput, "job_id is required");
  }

  const client = new GrillClient(token); // project_id not needed for status API
  const events: JobStatusFull[] = [];
  let seq = 0;
  try {
    await streamJobStatus(
      client,
      jobID,
      (s) => {
        events.push(s);
        if (ctx.notifyProgress) {
          void ctx.notifyProgress(
            { jobId: jobID, status: s.status, ...(s.error ? { error: s.error } : {}) },
            seq,
          );
        }
        seq++;
      },
      ctx.signal,
    );
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err);
    return toolError(makeGrillError(ErrorCode.StreamError, `status stream failed: ${msg}`), { job_id: jobID, events });
  }

  if (events.length > 0) {
    const last = events[events.length - 1]!;
    if (last.status === "failed") {
      const message = last.error ? `job failed: ${last.error}` : "job failed";
      return toolError(makeGrillError(ErrorCode.JobFailed, message), { job_id: jobID, events });
    }
  }

  return successResult({ job_id: jobID, events });
}
