import { apiBaseURL, statusAPIBaseURL } from "../common.js";
import type { GrillClient } from "./grillClient.js";

export interface JobStatusFull {
  is_terminal: boolean;
  status: string;
  error?: string;
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
export async function peekJobStatus(client: GrillClient, jobID: string): Promise<JobStatusFull> {
  const url = trimRightSlash(apiBaseURL()) + "/jobs/" + jobPathSegment(jobID) + "/status";
  const headers: Record<string, string> = {};
  if (client.authToken !== "") headers.Authorization = `Bearer ${client.authToken}`;

  const res = await fetch(url, { method: "GET", headers });
  const text = await res.text();
  if (!res.ok) {
    throw new Error(`job status: HTTP ${res.status}: ${text}`);
  }
  return JSON.parse(text) as JobStatusFull;
}
