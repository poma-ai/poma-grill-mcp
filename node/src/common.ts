import type { CallToolResult } from "@modelcontextprotocol/sdk/types.js";

export interface JobProgressEvent {
  jobId: string;
  status: string;
  error?: string;
}

export interface ToolContext {
  // notifyProgress is set when the caller passed a progressToken in _meta.
  // Tools should call it with each meaningful event; failures are absorbed.
  notifyProgress?: (event: JobProgressEvent, seq: number) => Promise<void> | void;
  signal?: AbortSignal;
}

export type ToolHandler = (
  args: Record<string, unknown>,
  ctx: ToolContext,
) => Promise<CallToolResult>;

const DEFAULT_API_BASE_URL = "https://api.poma-ai.com";
const DEFAULT_VERSION_PREFIX = "/v3";
const DEFAULT_STATUS_PREFIX = "/status/v1";

const VERSION_SUFFIX_RE = /\/v[0-9]+$/;

/**
 * API token from the environment, plus the variable it came from. Two names
 * are accepted, mirroring the poma-sdk convention:
 *
 * - `POMA_GRILL_API_KEY` — a grill *project* key (prefix `poma_proj_gr_`).
 *   Bound to one project server-side; checked first because it is the more
 *   specific credential.
 * - `POMA_API_KEY` — an *account* key (prefix `poma_acc_`) or a login JWT.
 *   Scope it to a project with `POMA_PROJECT_ID` or the `project_id` argument.
 *
 * Existing configs that put a project key under `POMA_API_KEY` keep working.
 * An empty value counts as unset and falls through to the next name.
 */
export function envToken(): { token: string; name: string } {
  for (const name of ["POMA_GRILL_API_KEY", "POMA_API_KEY"]) {
    const token = process.env[name];
    if (token) return { token, name };
  }
  return { token: "", name: "" };
}

export function getToken(arg: unknown): string {
  if (typeof arg === "string" && arg !== "") return arg;
  return envToken().token;
}

export function getProjectID(arg: unknown): string {
  if (typeof arg === "string" && arg !== "") return arg;
  return process.env.POMA_PROJECT_ID ?? "";
}

/**
 * Resolves the project ID and reports where it came from, for the human-readable
 * scope hint. Mirrors Go's projectIDSource.
 */
export function projectIDSource(arg: unknown): { id: string; source: string } {
  if (typeof arg === "string" && arg !== "") return { id: arg, source: "project_id argument" };
  const env = process.env.POMA_PROJECT_ID;
  if (env && env !== "") return { id: env, source: "POMA_PROJECT_ID env var" };
  return { id: "", source: "account default (no project_id set)" };
}

function trimRight(s: string, ch: string): string {
  let i = s.length;
  while (i > 0 && s[i - 1] === ch) i--;
  return s.slice(0, i);
}

export function apiBaseURL(): string {
  const v = process.env.POMA_API_BASE_URL;
  if (v && v !== "") {
    const trimmed = trimRight(v, "/");
    return VERSION_SUFFIX_RE.test(trimmed) ? trimmed : trimmed + DEFAULT_VERSION_PREFIX;
  }
  return DEFAULT_API_BASE_URL + DEFAULT_VERSION_PREFIX;
}

export function statusAPIBaseURL(): string {
  const v = process.env.POMA_STATUS_API_BASE_URL;
  if (v && v !== "") {
    const trimmed = trimRight(v, "/");
    return VERSION_SUFFIX_RE.test(trimmed) ? trimmed : trimmed + DEFAULT_STATUS_PREFIX;
  }
  const api = process.env.POMA_API_BASE_URL;
  if (api && api !== "") {
    return trimRight(api, "/") + DEFAULT_STATUS_PREFIX;
  }
  return DEFAULT_API_BASE_URL + DEFAULT_STATUS_PREFIX;
}

// Stable machine-readable error codes emitted alongside the human-readable
// error string on every grill tool error. Clients branch on these (and on
// retryable), never on the prose message. Mirrors go/tools/common.go.
export const ErrorCode = {
  MissingToken: "missing_token",
  InvalidInput: "invalid_input",
  AuthExpired: "auth_expired",
  PaymentRequired: "payment_required",
  ProjectProtected: "project_protected",
  Forbidden: "forbidden",
  TooManyJobs: "too_many_jobs",
  UpstreamError: "upstream_error",
  TransportError: "transport_error",
  ParseError: "parse_error",
  JobFailed: "job_failed",
  StreamError: "stream_error",
} as const;

/**
 * Single source of truth for the taxonomy's retry contract: true only for
 * too_many_jobs, transport_error, stream_error, and upstream_error when
 * httpStatus is 5xx. Pass httpStatus 0 for codes that don't depend on it.
 * Mirrors go/tools/common.go isRetryableCode.
 */
export function isRetryableCode(code: string, httpStatus = 0): boolean {
  switch (code) {
    case ErrorCode.TooManyJobs:
    case ErrorCode.TransportError:
    case ErrorCode.StreamError:
      return true;
    case ErrorCode.UpstreamError:
      return httpStatus >= 500;
    default:
      return false;
  }
}

// The shared error envelope promoted to the top level of every grill tool
// output. `retryable`/`retry_after_seconds` are additive and omitted (matching
// Go's omitempty) unless set.
export interface GrillError {
  error: string;
  code: string;
  retryable?: boolean;
  retry_after_seconds?: number;
}

/**
 * Builds a GrillError, deriving `retryable` from the taxonomy (isRetryableCode)
 * so no call site can drift from the retry contract. Pass httpStatus for
 * upstream_error (retryable only at 5xx) and retryAfterSeconds for too_many_jobs.
 */
export function makeGrillError(
  code: string,
  message: string,
  opts: { httpStatus?: number; retryAfterSeconds?: number } = {},
): GrillError {
  const ge: GrillError = { error: message, code };
  if (isRetryableCode(code, opts.httpStatus ?? 0)) ge.retryable = true;
  if (opts.retryAfterSeconds && opts.retryAfterSeconds > 0) {
    ge.retry_after_seconds = opts.retryAfterSeconds;
  }
  return ge;
}

/**
 * Serializes a GrillError to plain fields with omitempty semantics (drop
 * retryable when false, retry_after_seconds when 0) for embedding into a
 * structuredContent object or a per-item result entry.
 */
export function grillErrorFields(ge: GrillError): Record<string, unknown> {
  const o: Record<string, unknown> = { error: ge.error, code: ge.code };
  if (ge.retryable) o.retryable = true;
  if (ge.retry_after_seconds && ge.retry_after_seconds > 0) {
    o.retry_after_seconds = ge.retry_after_seconds;
  }
  return o;
}

/**
 * Builds an error CallToolResult carrying the structured envelope. `extra`
 * merges additional top-level fields (e.g. job_id, events) into
 * structuredContent alongside the promoted error fields.
 */
export function toolError(
  ge: GrillError,
  extra: Record<string, unknown> = {},
): CallToolResult {
  return {
    content: [{ type: "text", text: ge.error }],
    structuredContent: { ...extra, ...grillErrorFields(ge) },
    isError: true,
  };
}

/**
 * Convenience for the common "code + message" error result. Equivalent to
 * toolError(makeGrillError(code, message, opts), extra).
 */
export function codedError(
  code: string,
  message: string,
  opts: { httpStatus?: number; retryAfterSeconds?: number; extra?: Record<string, unknown> } = {},
): CallToolResult {
  return toolError(makeGrillError(code, message, opts), opts.extra ?? {});
}

/** Describes which credential was used, for error messages. */
export function tokenSource(tokenArg: unknown): string {
  if (typeof tokenArg === "string" && tokenArg !== "") return "per-call token argument";
  const { name } = envToken();
  if (name) return `${name} env var`;
  return "unknown";
}

/**
 * Returns a user-friendly error string plus a stable error code for 401/402/403
 * API responses. Returns undefined if the status code is not an auth/billing
 * error (including capacity/quota 403s, handled by interpretTooManyJobs).
 * Mirrors go/tools/common.go interpretAuthError.
 */
export function interpretAuthError(
  tokenArg: unknown,
  statusCode: number,
  body: Uint8Array,
  operation: string,
): { message: string; code: string } | undefined {
  if (statusCode !== 401 && statusCode !== 402 && statusCode !== 403) return undefined;

  const src = tokenSource(tokenArg);

  if (statusCode === 402) {
    return {
      message:
        `${operation}: credits exceeded (HTTP 402). The account associated with the token provided via ${src} has no remaining credits. ` +
        `Visit https://console.poma-ai.com to check your usage and upgrade your plan.`,
      code: ErrorCode.PaymentRequired,
    };
  }

  if (statusCode === 401) {
    return {
      message:
        `${operation}: authentication failed (HTTP 401). The token provided via ${src} is invalid, expired, or malformed. ` +
        `Generate a valid API key at https://console.poma-ai.com and set it as POMA_GRILL_API_KEY (project key) or POMA_API_KEY (account key), or pass it as the token argument.`,
      code: ErrorCode.AuthExpired,
    };
  }

  // 403 — try to parse the JSON error envelope for a specific message.
  //
  // Two wire shapes: the migrated envelope carries a snake_case `reason`
  // discriminator (and `code` becomes the numeric HTTP status), while the
  // legacy envelope carries the discriminator as the string `code`. Switch on
  // `reason` when present; otherwise switch on `code` ONLY if it is a string —
  // never `switch (errResp.code)` unguarded, since post-migration `code` is the
  // numeric status and would fall through for every 403.
  const text = new TextDecoder("utf-8").decode(body).trim();
  const projectProtectedMsg =
    `${operation}: this project is protected (HTTP 403). The token provided via ${src} is an account-level key, ` +
    `but this project requires a project API key. Generate one at https://console.poma-ai.com in the project settings, ` +
    `or set the project to unprotected.`;
  const forbiddenMsg =
    `${operation}: access denied (HTTP 403). The token provided via ${src} does not have access to this project — ` +
    `you may not own it or aren't a member of the organization. ` +
    `Use grill_projects to list projects accessible with your current key.`;
  try {
    const errResp = JSON.parse(text) as {
      code?: string | number;
      reason?: string;
      error?: string;
      message?: string;
    };
    if (errResp.reason) {
      switch (errResp.reason) {
        case "too_many_jobs":
        case "quota_exceeded":
          // Not an auth error — this is a capacity/quota limit.
          return undefined;
        case "project_protected":
          return { message: projectProtectedMsg, code: ErrorCode.ProjectProtected };
        case "forbidden":
          return { message: forbiddenMsg, code: ErrorCode.Forbidden };
      }
    } else if (typeof errResp.code === "string") {
      switch (errResp.code) {
        case "too_many_jobs":
        case "quota_exceeded":
          // Not an auth error — this is a capacity/quota limit.
          return undefined;
        case "project_protected":
          return { message: projectProtectedMsg, code: ErrorCode.ProjectProtected };
        case "forbidden":
          return { message: forbiddenMsg, code: ErrorCode.Forbidden };
      }
    }
  } catch {
    // fall through to generic
  }

  // Legacy: plain-text responses from older API versions.
  if (text === "too many jobs" || text === "quota exceeded") {
    return undefined;
  }

  return {
    message: `${operation}: forbidden (HTTP 403). The token provided via ${src} was rejected. Response: ${text}`,
    code: ErrorCode.Forbidden,
  };
}

/**
 * Recognises the job-capacity backpressure signal and returns an explicit
 * throttle instruction plus the suggested retry delay in seconds. `ok` is false
 * for any other response.
 *
 * The signal is HTTP 429 with code "too_many_jobs" (current API), a legacy 403
 * carrying the same code, or the legacy plaintext body "too many jobs". The API
 * also sets a Retry-After header; the JSON body's retry_after_seconds carries
 * the same hint.
 *
 * The message is written FOR the calling agent (e.g. langdock firing a
 * pipeline): the ingest did NOT happen, it is transient (retry the same ingest
 * after the delay), and it must stop sending new ingests until capacity frees.
 * This tool result is the only backpressure channel an LLM client sees.
 */
export function interpretTooManyJobs(
  statusCode: number,
  body: Uint8Array,
): { ok: boolean; message: string; retryAfterSeconds: number } {
  const text = new TextDecoder("utf-8").decode(body).trim();
  // code is numeric (429) on the current API and a string ("too_many_jobs") on
  // legacy 403 responses; reason carries the discriminator on the current API.
  let code: string | number | undefined;
  let reason: string | undefined;
  let serverMessage: string | undefined;
  let retryAfterSeconds = 0;
  try {
    const parsed = JSON.parse(text) as {
      code?: string | number;
      reason?: string;
      error?: string;
      message?: string;
      retry_after_seconds?: number;
    };
    code = parsed.code;
    reason = parsed.reason;
    // The migrated envelope renames the human field message→error; prefer error
    // so the LLM detail stays populated across the rename.
    serverMessage = parsed.error ?? parsed.message;
    if (typeof parsed.retry_after_seconds === "number") {
      retryAfterSeconds = parsed.retry_after_seconds;
    }
  } catch {
    // non-JSON body (e.g. legacy plaintext) — handled below
  }

  const isCapacity =
    statusCode === 429 || reason === "too_many_jobs" || code === "too_many_jobs" || text === "too many jobs";
  if (!isCapacity) {
    return { ok: false, message: "", retryAfterSeconds: 0 };
  }

  if (retryAfterSeconds <= 0) retryAfterSeconds = 5;
  const detail = serverMessage && serverMessage.length > 0 ? serverMessage : text;

  const message =
    `grill ingest: job queue full — the account is at its concurrent-job capacity (too_many_jobs). ` +
    `This is a TRANSIENT backpressure signal, NOT a failure: the document was NOT ingested and no credits were spent. ` +
    `Wait ~${retryAfterSeconds}s, then retry this same ingest. Do NOT send more ingests until capacity frees — ` +
    `poll grill_jobs_status and resume only once active jobs drop below the account limit. Server: ${detail}`;

  return { ok: true, message, retryAfterSeconds };
}

// Optional `text` override: callers whose structuredContent contains large
// payloads (e.g. base64 image data URIs) can supply a slim text view rather
// than restating the whole structure as JSON in the text content block.
export function successResult(
  structuredContent: Record<string, unknown>,
  text?: string,
): CallToolResult {
  return {
    content: [{ type: "text", text: text ?? JSON.stringify(structuredContent) }],
    structuredContent,
  };
}
