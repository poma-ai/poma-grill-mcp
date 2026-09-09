import { apiBaseURL, statusAPIBaseURL } from "../common.js";
import type { GrillClient } from "./grillClient.js";

export interface JobStatusFull {
  is_terminal: boolean;
  status: string;
  error?: string;
  grill?: JobGrillOutcome;
}

// JobGrillOutcome is the optional `grill` object the gateway attaches to a job
// status (poma-services-go#133). Absent on older gateways, on non-grill jobs,
// or before the job reaches the grill stage.
//
// deduplicated: the same input bytes under the same conversion build were
// already indexed; nothing new was stored and doc_id names the existing
// document (it may differ from the job_id). replaced_doc_ids: documents grill
// evicted in favour of this job after a conversion-build change.
export interface JobGrillOutcome {
  deduplicated: boolean;
  doc_id?: string;
  replaced_doc_ids?: string[];
}

// grillOutcomeFields normalizes a gateway grill object to the exact wire shape
// the Go implementation emits (deduplicated always present, doc_id when set,
// replaced_doc_ids only when non-empty), or undefined when the gateway sent
// none. Mirrors Go's jobGrillOutcome JSON tags.
export function grillOutcomeFields(g: unknown): JobGrillOutcome | undefined {
  if (g === null || typeof g !== "object") return undefined;
  const raw = g as Record<string, unknown>;
  const out: JobGrillOutcome = { deduplicated: raw.deduplicated === true };
  if (typeof raw.doc_id === "string" && raw.doc_id !== "") out.doc_id = raw.doc_id;
  if (Array.isArray(raw.replaced_doc_ids) && raw.replaced_doc_ids.length > 0) {
    out.replaced_doc_ids = raw.replaced_doc_ids.map(String);
  }
  return out;
}

// normalizeGrill rewrites a parsed status event's grill object in place to the
// normalized wire shape, or drops the key when the gateway sent nothing usable,
// so the `events` array matches the Go implementation's serialization.
function normalizeGrill(s: JobStatusFull): void {
  if (!("grill" in s)) return;
  const g = grillOutcomeFields(s.grill);
  if (g) s.grill = g;
  else delete s.grill;
}

// lastGrillOutcome returns the normalized grill object of the most recent
// status event that carried one, or undefined. The gateway attaches it to the
// terminal status, so this is the outcome a wait-style tool surfaces top-level.
export function lastGrillOutcome(events: JobStatusFull[]): JobGrillOutcome | undefined {
  for (let i = events.length - 1; i >= 0; i--) {
    const g = grillOutcomeFields(events[i]?.grill);
    if (g) return g;
  }
  return undefined;
}

// Mirrors Go's isTerminalGrillStatus. The status API doesn't always set
// is_terminal:true on terminal events (e.g. "grilled"), so we check
// explicit values as well.
export function isTerminalGrillStatus(status: string): boolean {
  return status === "grilled" || status === "done" || status === "failed" || status === "deleted";
}

function jobPathSegment(jobID: string): string {
  // Mirrors poma-cli's JobPathSegment: percent-encode the id but leave the
  // hyphen separator we know the server uses. encodeURIComponent is the
  // closest stdlib equivalent and covers the safe characters we need.
  return encodeURIComponent(jobID);
}

function trimRightSlash(s: string): string {
  return s.endsWith("/") ? s.slice(0, -1) : s;
}

// streamJobStatus opens the status SSE stream and invokes onEvent for each
// `event: job_status` event until the server reports a terminal state, the
// response stream ends, or `signal` aborts.
export async function streamJobStatus(
  client: GrillClient,
  jobID: string,
  onEvent: (event: JobStatusFull) => void,
  signal?: AbortSignal,
): Promise<void> {
  const url = trimRightSlash(statusAPIBaseURL()) + "/jobs/" + jobPathSegment(jobID);
  const headers: Record<string, string> = { Accept: "text/event-stream" };
  if (client.authToken !== "") headers.Authorization = `Bearer ${client.authToken}`;

  const res = await fetch(url, { method: "GET", headers, signal });
  if (!res.ok) {
    const body = await res.text();
    throw new Error(`status stream: HTTP ${res.status}: ${body}`);
  }
  if (!res.body) {
    throw new Error("status stream: empty response body");
  }

  const reader = res.body.getReader();
  const decoder = new TextDecoder("utf-8");
  let buffer = "";
  let eventType = "";
  let data = "";

  const dispatch = (): boolean => {
    if (eventType !== "job_status" || data === "") {
      eventType = "";
      data = "";
      return false;
    }
    let parsed: JobStatusFull | null = null;
    try {
      parsed = JSON.parse(data) as JobStatusFull;
    } catch {
      eventType = "";
      data = "";
      return false;
    }
    normalizeGrill(parsed);
    onEvent(parsed);
    eventType = "";
    data = "";
    return parsed.is_terminal || isTerminalGrillStatus(parsed.status);
  };

  // Read the SSE stream line-by-line. Events are separated by blank lines.
  while (true) {
    if (signal?.aborted) {
      await reader.cancel().catch(() => {});
      throw signal.reason instanceof Error ? signal.reason : new Error("aborted");
    }
    const { value, done } = await reader.read();
    if (done) {
      // Flush a trailing event without a closing blank line, if any.
      if (buffer.length > 0) {
        for (const line of buffer.split("\n")) parseLine(line);
        if (dispatch()) return;
      }
      return;
    }
    buffer += decoder.decode(value, { stream: true });
    // Split on \n; keep the trailing partial line in buffer.
    const lines = buffer.split("\n");
    buffer = lines.pop() ?? "";
    for (const raw of lines) {
      const line = raw.replace(/\r$/, "");
      if (line === "") {
        if (dispatch()) return;
        continue;
      }
      parseLine(line);
    }
  }

  function parseLine(line: string): void {
    if (line.startsWith("event:")) {
      eventType = line.slice("event:".length).trim();
    } else if (line.startsWith("data:")) {
      data = line.slice("data:".length).trim();
    }
  }
}

// peekJobStatus fetches a single non-streaming status snapshot for the given job.
//
// The returned httpStatus is the upstream HTTP status code, or 0 when the error
// originates before/without an HTTP response (network/client error) — callers
// need this to classify the error correctly: 0 is a transport error, a non-2xx
// status is an upstream error (retryable only at 5xx), and 200 with an error is
// a parse error. Collapsing all three would misclassify a permanent 4xx (e.g.
// job not found) as a transient transport error. Mirrors Go's peekJobStatus.
export interface PeekResult {
  status: JobStatusFull | null;
  httpStatus: number;
  error?: string;
}

export async function peekJobStatus(client: GrillClient, jobID: string): Promise<PeekResult> {
  const url = trimRightSlash(apiBaseURL()) + "/jobs/" + jobPathSegment(jobID) + "/status";
  const headers: Record<string, string> = {};
  if (client.authToken !== "") headers.Authorization = `Bearer ${client.authToken}`;

  let res: Response;
  try {
    res = await fetch(url, { method: "GET", headers });
  } catch (err) {
    // Network/client error reaching the status endpoint — no HTTP response.
    return { status: null, httpStatus: 0, error: err instanceof Error ? err.message : String(err) };
  }
  const text = await res.text();
  if (!res.ok) {
    return { status: null, httpStatus: res.status, error: `job status: HTTP ${res.status}: ${text}` };
  }
  try {
    const status = JSON.parse(text) as JobStatusFull;
    normalizeGrill(status);
    return { status, httpStatus: res.status };
  } catch (err) {
    return { status: null, httpStatus: res.status, error: err instanceof Error ? err.message : String(err) };
  }
}
