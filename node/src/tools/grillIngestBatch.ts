import type { CallToolResult } from "@modelcontextprotocol/sdk/types.js";
import {
  codedError,
  ErrorCode,
  getProjectID,
  getToken,
  grillErrorFields,
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

interface BatchResult {
  file_path: string;
  job_id?: string;
  error?: string;
  code?: string;
  retryable?: boolean;
  retry_after_seconds?: number;
  quota_exceed?: boolean;
}

export async function grillIngestBatch(
  args: Record<string, unknown>,
  _ctx: ToolContext,
): Promise<CallToolResult> {
  const token = getToken(args.token);
  if (token === "") {
    return codedError(ErrorCode.MissingToken, "token is required (provide token or set POMA_API_KEY on the server)");
  }
  const filePaths = Array.isArray(args.file_paths) ? args.file_paths.map(String) : [];
  if (filePaths.length === 0) {
    return codedError(ErrorCode.InvalidInput, "file_paths is required");
  }
  if (filePaths.length > 50) {
    return codedError(ErrorCode.InvalidInput, "file_paths exceeds limit of 50");
  }

  let concurrency = typeof args.concurrency === "number" ? Math.trunc(args.concurrency) : 0;
  if (concurrency <= 0) concurrency = 5;
  if (concurrency > 10) concurrency = 10;

  const projectID = getProjectID(args.project_id);
  const client = new GrillClient(token, projectID);
  const results: BatchResult[] = new Array(filePaths.length);

  let cursor = 0;
  const workerCount = Math.min(concurrency, filePaths.length);
  const workers: Promise<void>[] = [];
  for (let w = 0; w < workerCount; w++) {
    workers.push(
      (async () => {
        while (true) {
          const i = cursor++;
          if (i >= filePaths.length) return;
          const fp = filePaths[i]!;

          let resolved;
          try {
            resolved = resolveIngestPayload({ file_path: fp });
          } catch (err) {
            // Payload build / arg validation failure — bad input, not transport.
            const ge = makeGrillError(ErrorCode.InvalidInput, err instanceof Error ? err.message : String(err));
            results[i] = { file_path: fp, ...grillErrorFields(ge) };
            continue;
          }

          let res;
          try {
            res = await client.ingestRaw(resolved.data, resolved.filename);
          } catch (err) {
            // Network/client error reaching the Grill API — transient, retryable.
            const ge = makeGrillError(ErrorCode.TransportError, err instanceof Error ? err.message : String(err));
            results[i] = { file_path: fp, ...grillErrorFields(ge) };
            continue;
          }

          const authErr = interpretAuthError(args.token, res.status, res.body, "grill ingest");
          if (authErr) {
            const ge = makeGrillError(authErr.code, authErr.message);
            results[i] = { file_path: fp, ...grillErrorFields(ge) };
            continue;
          }
          const throttle = interpretTooManyJobs(res.status, res.body);
          if (throttle.ok) {
            // Job-capacity backpressure (HTTP 429 too_many_jobs). Transient —
            // bucket as quota_exceed so the caller retries once slots free.
            const ge = makeGrillError(ErrorCode.TooManyJobs, throttle.message, {
              retryAfterSeconds: throttle.retryAfterSeconds,
            });
            results[i] = { file_path: fp, ...grillErrorFields(ge), quota_exceed: true };
            continue;
          }
          if (res.status === 403) {
            // interpretAuthError returned undefined — legacy quota/capacity 403 (older API), not auth.
            const bodyText = new TextDecoder("utf-8").decode(res.body);
            const ge = makeGrillError(ErrorCode.TooManyJobs, `quota exceeded: ${bodyText}`);
            results[i] = { file_path: fp, ...grillErrorFields(ge), quota_exceed: true };
            continue;
          }
          if (res.status !== 201) {
            const text = new TextDecoder("utf-8").decode(res.body);
            const ge = makeGrillError(ErrorCode.UpstreamError, `HTTP ${res.status}: ${text}`, {
              httpStatus: res.status,
            });
            results[i] = { file_path: fp, ...grillErrorFields(ge) };
            continue;
          }
          const job = parseJob(res.body);
          if (!job) {
            const text = new TextDecoder("utf-8").decode(res.body);
            const ge = makeGrillError(ErrorCode.ParseError, `could not parse job_id: ${text}`);
            results[i] = { file_path: fp, ...grillErrorFields(ge) };
            continue;
          }
          results[i] = { file_path: fp, job_id: job.job_id };
        }
      })(),
    );
  }
  await Promise.all(workers);

  let submitted = 0;
  let quota = 0;
  let failed = 0;
  for (const r of results) {
    if (r.job_id !== undefined && r.job_id !== "") submitted++;
    else if (r.quota_exceed === true) quota++;
    else failed++;
  }

  if (submitted === 0 && quota === 0) {
    // Aggregate rollup: every file failed. Each results[i].code is guaranteed
    // non-empty here (every result is a failure), so propagate the first one as
    // the representative top-level code/retryable rather than leaving it empty.
    const message = `all ${results.length} file(s) failed to submit`;
    const first = results[0]!;
    return toolError(
      {
        error: message,
        code: first.code ?? ErrorCode.UpstreamError,
        ...(first.retryable ? { retryable: true } : {}),
        ...(first.retry_after_seconds ? { retry_after_seconds: first.retry_after_seconds } : {}),
      },
      { results, submitted_count: submitted, failed_count: failed, quota_exceeded_count: quota },
    );
  }

  const { source } = projectIDSource(args.project_id);
  const scope = await resolveScope(client, token, projectID, "", source);
  const scopeOut = scopeFields(scope);
  return successResult({
    results,
    submitted_count: submitted,
    failed_count: failed,
    quota_exceeded_count: quota,
    ...(scopeOut ? { scope: scopeOut } : {}),
  });
}
