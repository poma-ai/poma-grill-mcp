import type { CallToolResult } from "@modelcontextprotocol/sdk/types.js";
import { errorResult, getProjectID, getToken, interpretAuthError, successResult, type ToolContext } from "../common.js";
import { GrillClient, parseJob } from "../client/grillClient.js";
import { resolveIngestPayload } from "../client/ingestPayload.js";

interface BatchResult {
  file_path: string;
  job_id?: string;
  error?: string;
  quota_exceed?: boolean;
}

export async function grillIngestBatch(
  args: Record<string, unknown>,
  _ctx: ToolContext,
): Promise<CallToolResult> {
  const token = getToken(args.token);
  if (token === "") {
    return errorResult("token is required (provide token or set POMA_API_KEY on the server)");
  }
  const filePaths = Array.isArray(args.file_paths) ? args.file_paths.map(String) : [];
  if (filePaths.length === 0) {
    return errorResult("file_paths is required");
  }
  if (filePaths.length > 50) {
    return errorResult("file_paths exceeds limit of 50");
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
          try {
            const resolved = resolveIngestPayload({ file_path: fp });
            const res = await client.ingestRaw(resolved.data, resolved.filename);
            {
              const authErrMsg = interpretAuthError(args.token, res.status, res.body, "grill ingest");
              if (authErrMsg) {
                results[i] = { file_path: fp, error: authErrMsg };
                return;
              }
              if (res.status === 403) {
                // interpretAuthError returned undefined — this is a quota/capacity error, not auth.
                const bodyText = new TextDecoder("utf-8").decode(res.body);
                results[i] = { file_path: fp, error: `quota exceeded: ${bodyText}`, quota_exceed: true };
                return;
              }
            }
            if (res.status !== 201) {
              const text = new TextDecoder("utf-8").decode(res.body);
              results[i] = { file_path: fp, error: `HTTP ${res.status}: ${text}` };
              return;
            }
            const job = parseJob(res.body);
            if (!job) {
              const text = new TextDecoder("utf-8").decode(res.body);
              results[i] = { file_path: fp, error: `could not parse job_id: ${text}` };
              return;
            }
            results[i] = { file_path: fp, job_id: job.job_id };
          } catch (err) {
            const msg = err instanceof Error ? err.message : String(err);
            results[i] = { file_path: fp, error: msg };
          }
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
    return {
      content: [{ type: "text", text: `all ${results.length} file(s) failed to submit` }],
      structuredContent: {
        results,
        submitted_count: submitted,
        failed_count: failed,
        quota_exceeded_count: quota,
        error: `all ${results.length} file(s) failed to submit`,
      },
      isError: true,
    };
  }

  return successResult({
    results,
    submitted_count: submitted,
    failed_count: failed,
    quota_exceeded_count: quota,
  });
}
