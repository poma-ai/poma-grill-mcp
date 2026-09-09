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
import { lastGrillOutcome, streamJobStatus, type JobStatusFull } from "../client/statusStream.js";
import { resolveScope, scopeFields } from "../scope.js";

export async function grillIngestSync(
  args: Record<string, unknown>,
  ctx: ToolContext,
): Promise<CallToolResult> {
  const token = getToken(args.token);
  if (token === "") {
    return codedError(ErrorCode.MissingToken, "token is required (provide token or set POMA_GRILL_API_KEY or POMA_API_KEY on the server)");
  }

  const url = typeof args.url === "string" ? args.url.trim() : "";
  const labels = serializeLabels(parseLabelsArg(args.labels));
  const projectID = getProjectID(args.project_id);
  const client = new GrillClient(token, projectID);

  let ingestRes;
  if (url !== "") {
    // URL ingest: the server fetches the remote URL. Mutually exclusive with the
    // file inputs.
    if (typeof args.file_path === "string" && args.file_path !== "") {
      return codedError(ErrorCode.InvalidInput, "provide only one of url, file_path, or file_base64");
    }
    if (typeof args.file_base64 === "string" && args.file_base64 !== "") {
      return codedError(ErrorCode.InvalidInput, "provide only one of url, file_path, or file_base64");
    }
    ingestRes = await client.ingestRemoteURL(url, labels);
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
    ingestRes = await client.ingestRaw(resolved.data, resolved.filename, labels);
  }

  const authErr = interpretAuthError(args.token, ingestRes.status, ingestRes.body, "grill ingest");
  if (authErr) return codedError(authErr.code, authErr.message);
  const throttle = interpretTooManyJobs(ingestRes.status, ingestRes.body);
  if (throttle.ok) {
    return toolError(
      makeGrillError(ErrorCode.TooManyJobs, throttle.message, { retryAfterSeconds: throttle.retryAfterSeconds }),
    );
  }
  if (ingestRes.status !== 201) {
    const text = new TextDecoder("utf-8").decode(ingestRes.body);
    return codedError(ErrorCode.UpstreamError, `grill ingest: HTTP ${ingestRes.status}: ${text}`, {
      httpStatus: ingestRes.status,
    });
  }
  const job = parseJob(ingestRes.body);
  if (!job) {
    const text = new TextDecoder("utf-8").decode(ingestRes.body);
    return codedError(ErrorCode.ParseError, `grill ingest: could not parse job_id from response: ${text}`);
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
    return toolError(makeGrillError(ErrorCode.StreamError, `status stream failed: ${msg}`), {
      job_id: job.job_id,
      events,
    });
  }

  if (events.length > 0) {
    const last = events[events.length - 1]!;
    if (last.status === "failed") {
      const message = last.error ? `job failed: ${last.error}` : "job failed";
      return toolError(makeGrillError(ErrorCode.JobFailed, message), { job_id: job.job_id, events });
    }
  }

  const { source } = projectIDSource(args.project_id);
  const scope = await resolveScope(client, token, projectID, "", source);
  const scopeOut = scopeFields(scope);
  const grill = lastGrillOutcome(events);
  return successResult({ job_id: job.job_id, events, ...(grill ? { grill } : {}), ...(scopeOut ? { scope: scopeOut } : {}) });
}
