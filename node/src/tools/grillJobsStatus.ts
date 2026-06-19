import type { CallToolResult } from "@modelcontextprotocol/sdk/types.js";
import { errorResult, getToken, successResult } from "../common.js";
import { GrillClient } from "../client/grillClient.js";
import { isTerminalGrillStatus, peekJobStatus } from "../client/statusStream.js";

interface JobStatusResult {
  job_id: string;
  status?: string;
  is_terminal: boolean;
  error?: string;
}

const PEEK_CONCURRENCY = 10;

export async function grillJobsStatus(
  args: Record<string, unknown>,
  _ctx: import("../common.js").ToolContext,
): Promise<CallToolResult> {
  const token = getToken(args.token);
  if (token === "") {
    return errorResult("token is required (provide token or set POMA_API_KEY on the server)");
  }
  const ids = Array.isArray(args.job_ids) ? args.job_ids.map(String) : [];
  if (ids.length === 0) {
    return errorResult("job_ids is required");
  }
  if (ids.length > 50) {
    return errorResult("job_ids exceeds limit of 50");
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
          try {
            const s = await peekJobStatus(client, id);
            const terminal = s.is_terminal || isTerminalGrillStatus(s.status);
            results[i] = {
              job_id: id,
              status: s.status,
              is_terminal: terminal,
              ...(s.error ? { error: s.error } : {}),
            };
          } catch (err) {
            const msg = err instanceof Error ? err.message : String(err);
            results[i] = { job_id: id, is_terminal: false, error: msg };
          }
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
