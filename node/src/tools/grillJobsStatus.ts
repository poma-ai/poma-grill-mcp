import type { CallToolResult } from "@modelcontextprotocol/sdk/types.js";
import {
  codedError,
  ErrorCode,
  getToken,
  grillErrorFields,
  makeGrillError,
  successResult,
  type ToolContext,
} from "../common.js";
import { GrillClient } from "../client/grillClient.js";
import { grillOutcomeFields, isTerminalGrillStatus, peekJobStatus, type JobGrillOutcome } from "../client/statusStream.js";

interface JobStatusResult {
  job_id: string;
  status?: string;
  is_terminal: boolean;
  // Gateway dedup/replacement outcome (poma-services-go#133); omitted when
  // the gateway did not send one.
  grill?: JobGrillOutcome;
  error?: string;
  code?: string;
  retryable?: boolean;
  retry_after_seconds?: number;
}

const PEEK_CONCURRENCY = 10;

export async function grillJobsStatus(
  args: Record<string, unknown>,
  _ctx: ToolContext,
): Promise<CallToolResult> {
  const token = getToken(args.token);
  if (token === "") {
    return codedError(ErrorCode.MissingToken, "token is required (provide token or set POMA_GRILL_API_KEY or POMA_API_KEY on the server)");
  }
  const ids = Array.isArray(args.job_ids) ? args.job_ids.map(String) : [];
  if (ids.length === 0) {
    return codedError(ErrorCode.InvalidInput, "job_ids is required");
  }
  if (ids.length > 50) {
    return codedError(ErrorCode.InvalidInput, "job_ids exceeds limit of 50");
  }

  const client = new GrillClient(token); // project_id not needed for status API
  const results: JobStatusResult[] = new Array(ids.length);

  // Fixed-window concurrency: workers pull from a shared cursor.
  let cursor = 0;
  const workers: Promise<void>[] = [];
  const workerCount = Math.min(PEEK_CONCURRENCY, ids.length);
  for (let w = 0; w < workerCount; w++) {
    workers.push(
      (async () => {
        while (true) {
          const i = cursor++;
          if (i >= ids.length) return;
          const id = ids[i]!;
          const peek = await peekJobStatus(client, id);
          if (peek.status === null) {
            // Classify like Go: httpStatus 0 = transport (retryable); a non-2xx
            // status = upstream (retryable only at 5xx); 200-but-parse-fail =
            // parse_error. A permanent 4xx must NOT read as transient transport.
            const msg = peek.error ?? "job status: unknown error";
            let ge;
            if (peek.httpStatus === 0) {
              ge = makeGrillError(ErrorCode.TransportError, msg);
            } else if (peek.httpStatus === 200) {
              ge = makeGrillError(ErrorCode.ParseError, msg);
            } else {
              ge = makeGrillError(ErrorCode.UpstreamError, msg, { httpStatus: peek.httpStatus });
            }
            results[i] = { job_id: id, is_terminal: false, ...grillErrorFields(ge) };
            continue;
          }
          const s = peek.status;
          const terminal = s.is_terminal || isTerminalGrillStatus(s.status);
          const grill = grillOutcomeFields(s.grill);
          const res: JobStatusResult = {
            job_id: id,
            status: s.status,
            is_terminal: terminal,
            ...(grill ? { grill } : {}),
            ...(s.error ? { error: s.error } : {}),
          };
          if (res.status === "failed" || (res.error && res.error !== "")) {
            // Terminal job failure (or an error surfaced on a non-terminal
            // status) — not retryable; fix the source doc.
            res.code = ErrorCode.JobFailed;
          }
          results[i] = res;
        }
      })(),
    );
  }
  await Promise.all(workers);

  let pendingCount = 0;
  let doneCount = 0;
  let failedCount = 0;
  for (const r of results) {
    if ((r.error && r.error !== "") || r.status === "failed") {
      failedCount++;
    } else if (r.is_terminal) {
      doneCount++;
    } else {
      pendingCount++;
    }
  }

  return successResult({
    results,
    pending_count: pendingCount,
    done_count: doneCount,
    failed_count: failedCount,
  });
}
