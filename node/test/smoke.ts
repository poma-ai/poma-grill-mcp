// Spawn-and-drive smoke harness for the Node MCP server.
//
// Offline-only: spawns the built binary, drives it via JSON-RPC over stdio,
// asserts the protocol handshake, tool registration, and per-tool argument
// validation. No POMA API calls; runs anywhere without secrets.
//
// Real-API verification is intentionally manual — wire the binary into an
// MCP client and use it.
//
// Usage:
//   npm run smoke

import { spawn, type ChildProcessWithoutNullStreams } from "node:child_process";
import { existsSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { createServer, type Server } from "node:http";
import type { AddressInfo } from "node:net";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { createInterface, type Interface } from "node:readline";

const HERE = dirname(fileURLToPath(import.meta.url));
const NODE_ROOT = resolve(HERE, "..");
const BINARY = resolve(NODE_ROOT, "dist", "index.js");

// No smoke test may reach a live POMA API. Every child server starts from an
// environment with ALL POMA_* variables removed (POMA_API_KEY, POMA_API_BASE_URL,
// POMA_STATUS_API_BASE_URL, POMA_PROJECT_ID, POMA_CONSOLE_URL — everything the
// server reads for a token, a host or a scope), and its API host defaults to a
// dead stub started in main(). A test that wants an API passes its own
// POMA_API_BASE_URL; one that forgets lands on the dead stub, and the run fails
// listing the stray requests, instead of reaching https://api.index4.ai with
// whatever key the developer has exported.
let deadAPIURL = "";
const deadAPIHits: string[] = [];

function childEnv(overrides: Record<string, string>): Record<string, string> {
  const base: Record<string, string> = {};
  for (const [k, v] of Object.entries(process.env)) {
    if (v !== undefined && !k.startsWith("POMA_")) base[k] = v;
  }
  if (deadAPIURL === "") throw new Error("dead API stub not started");
  return { ...base, POMA_API_BASE_URL: deadAPIURL, ...overrides };
}

const EXPECTED_TOOLS = [
  "grill_attributes",
  "grill_docs_list",
  "grill_explain",
  "grill_ingest",
  "grill_ingest_sync",
  "grill_ingest_resume",
  "grill_ingest_batch",
  "grill_jobs_status",
  "grill_projects",
  "grill_search",
] as const;

interface JSONRPCResponse {
  jsonrpc: "2.0";
  id: number;
  result?: unknown;
  error?: { code: number; message: string };
}

class MCPClient {
  private proc: ChildProcessWithoutNullStreams;
  private rl: Interface;
  private nextId = 1;
  private pending = new Map<number, (resp: JSONRPCResponse) => void>();
  private stderr = "";

  constructor(env: Record<string, string>) {
    this.proc = spawn(process.execPath, [BINARY, "-input", "-"], {
      env: childEnv(env),
      stdio: ["pipe", "pipe", "pipe"],
    });
    this.proc.stderr.setEncoding("utf8");
    this.proc.stderr.on("data", (chunk: string) => {
      this.stderr += chunk;
    });
    this.rl = createInterface({ input: this.proc.stdout });
    this.rl.on("line", (line) => {
      const trimmed = line.trim();
      if (trimmed === "") return;
      let msg: JSONRPCResponse;
      try {
        msg = JSON.parse(trimmed) as JSONRPCResponse;
      } catch {
        return;
      }
      if (typeof msg.id === "number") {
        const cb = this.pending.get(msg.id);
        if (cb) {
          this.pending.delete(msg.id);
          cb(msg);
        }
      }
    });
  }

  async request(method: string, params?: unknown): Promise<JSONRPCResponse> {
    const id = this.nextId++;
    const payload = JSON.stringify({ jsonrpc: "2.0", id, method, ...(params !== undefined ? { params } : {}) });
    return new Promise((resolveResponse, rejectResponse) => {
      const timer = setTimeout(() => {
        this.pending.delete(id);
        rejectResponse(new Error(`timeout waiting for response to ${method} (id=${id}); stderr so far: ${this.stderr}`));
      }, 10_000);
      this.pending.set(id, (resp) => {
        clearTimeout(timer);
        resolveResponse(resp);
      });
      this.proc.stdin.write(payload + "\n");
    });
  }

  // requestRaw sends params as a literal JSON string, for inputs JSON.stringify
  // cannot produce (an overflowing number such as 1e400).
  async requestRaw(method: string, paramsJSON: string): Promise<JSONRPCResponse> {
    const id = this.nextId++;
    const payload = `{"jsonrpc":"2.0","id":${id},"method":${JSON.stringify(method)},"params":${paramsJSON}}`;
    return new Promise((resolveResponse, rejectResponse) => {
      const timer = setTimeout(() => {
        this.pending.delete(id);
        rejectResponse(new Error(`timeout waiting for response to ${method} (id=${id})`));
      }, 10_000);
      this.pending.set(id, (resp) => {
        clearTimeout(timer);
        resolveResponse(resp);
      });
      this.proc.stdin.write(payload + "\n");
    });
  }

  notify(method: string, params?: unknown): void {
    const payload = JSON.stringify({ jsonrpc: "2.0", method, ...(params !== undefined ? { params } : {}) });
    this.proc.stdin.write(payload + "\n");
  }

  async close(): Promise<void> {
    this.proc.stdin.end();
    await new Promise<void>((res) => this.proc.once("close", () => res()));
  }
}

interface Result {
  name: string;
  ok: boolean;
  detail?: string;
}

const results: Result[] = [];
function record(name: string, ok: boolean, detail?: string): void {
  results.push({ name, ok, ...(detail ? { detail } : {}) });
  const icon = ok ? "PASS" : "FAIL";
  process.stdout.write(`  [${icon}] ${name}${detail ? ` — ${detail}` : ""}\n`);
}

function assertToolError(resp: JSONRPCResponse, expectedFragment: string, label: string): void {
  const result = resp.result as
    | { isError?: boolean; structuredContent?: { error?: string } }
    | undefined;
  if (!result || result.isError !== true) {
    record(label, false, `expected isError:true, got ${JSON.stringify(result)}`);
    return;
  }
  const error = result.structuredContent?.error ?? "";
  if (!error.includes(expectedFragment)) {
    record(label, false, `expected error containing "${expectedFragment}", got "${error}"`);
    return;
  }
  record(label, true);
}

async function offlineTests(client: MCPClient): Promise<void> {
  process.stdout.write("offline tests:\n");

  const initResp = await client.request("initialize", {
    protocolVersion: "2024-11-05",
    capabilities: {},
    clientInfo: { name: "smoke", version: "0" },
  });
  record(
    "initialize handshake",
    initResp.result !== undefined && (initResp.result as { serverInfo?: { name?: string } }).serverInfo?.name === "poma-grill-mcp",
    JSON.stringify((initResp.result as { serverInfo?: unknown })?.serverInfo),
  );
  client.notify("notifications/initialized");

  const listResp = await client.request("tools/list");
  const listResult = listResp.result as { tools?: { name: string }[] } | undefined;
  const names = listResult?.tools?.map((t) => t.name).sort() ?? [];
  const expectedSorted = [...EXPECTED_TOOLS].sort();
  const namesMatch =
    names.length === expectedSorted.length &&
    names.every((n, i) => n === expectedSorted[i]);
  record(
    `tools/list returns ${expectedSorted.length} expected tools`,
    namesMatch,
    namesMatch ? `${names.length} tools` : `got ${JSON.stringify(names)}`,
  );

  const validationCases: { name: string; args: Record<string, unknown>; expect: string }[] = [
    { name: "grill_search", args: {}, expect: "query is required" },
    { name: "grill_jobs_status", args: { job_ids: [] }, expect: "job_ids is required" },
    { name: "grill_ingest_batch", args: { file_paths: [] }, expect: "file_paths is required" },
    { name: "grill_ingest_resume", args: {}, expect: "job_id is required" },
    { name: "grill_ingest", args: {}, expect: "one of file_base64 or file_path is required" },
    { name: "grill_ingest_sync", args: {}, expect: "one of file_base64 or file_path is required" },
  ];
  for (const c of validationCases) {
    const resp = await client.request("tools/call", { name: c.name, arguments: c.args });
    assertToolError(resp, c.expect, `validation: ${c.name}`);
  }

  // grill_explain takes no args and needs no auth — assert it returns a
  // non-empty explanation string.
  const explainResp = await client.request("tools/call", { name: "grill_explain", arguments: {} });
  const explainResult = explainResp.result as
    | { isError?: boolean; structuredContent?: { explanation?: string } }
    | undefined;
  const explanation = explainResult?.structuredContent?.explanation ?? "";
  const explainOk = explainResult !== undefined && explainResult.isError !== true && explanation.length > 0;
  record(
    "grill_explain returns non-empty explanation",
    explainOk,
    explainOk ? `${explanation.length} chars` : `isError=${explainResult?.isError}, len=${explanation.length}`,
  );
}

// -- grill_docs_list auto-paging tests (offline, against an in-process stub API) --
//
// The stub selects a scenario from the Bearer token, so a single server + a
// single spawned MCP process cover every pagination shape. Cursor plumbing:
// the tool must send GET /index4ai/v1/docs?cursor=<next_cursor> for follow-ups.

function docsPageBody(
  docIDs: string[],
  total: number,
  opts: { hasMore?: boolean; nextCursor?: string | null; degraded?: boolean; legacy?: boolean } = {},
): string {
  const page: Record<string, unknown> = {
    documents: docIDs.map((id) => ({ doc_id: id })),
    namespace: "account_14d12545",
    total_documents: total,
  };
  if (!opts.legacy) {
    page.has_more = opts.hasMore === true;
    page.next_cursor = opts.nextCursor ?? null;
    page.degraded = opts.degraded === true;
  }
  return JSON.stringify(page);
}

function startStubAPI(): Promise<{ url: string; docsRequests: Map<string, number>; close: () => Promise<void> }> {
  const docsRequests = new Map<string, number>();
  const server: Server = createServer((req, res) => {
    const u = new URL(req.url ?? "/", "http://localhost");
    res.setHeader("content-type", "application/json");
    if (u.pathname !== "/index4ai/v1/docs") {
      res.statusCode = 404;
      res.end("{}");
      return;
    }
    const scenario = (req.headers.authorization ?? "").replace("Bearer ", "");
    const call = (docsRequests.get(scenario) ?? 0) + 1;
    docsRequests.set(scenario, call);
    const cursor = u.searchParams.get("cursor") ?? "";

    switch (scenario) {
      case "legacy":
        if (cursor !== "") {
          res.statusCode = 500;
          res.end(`{"error":"legacy API must never receive a cursor, got ${cursor}"}`);
          return;
        }
        res.end(docsPageBody(["d1", "d2"], 2, { legacy: true }));
        return;
      case "paged":
        if (cursor === "") res.end(docsPageBody(["d1", "d2"], 5, { hasMore: true, nextCursor: "c2" }));
        else if (cursor === "c2") res.end(docsPageBody(["d3", "d4"], 5, { hasMore: true, nextCursor: "c3" }));
        else res.end(docsPageBody(["d5"], 5));
        return;
      case "cap":
        res.end(docsPageBody([`d${call}`], 100, { hasMore: true, nextCursor: `c${call + 1}` }));
        return;
      case "degraded":
        res.end(docsPageBody(["d1", "d2"], 3, { degraded: true }));
        return;
      case "nocursor":
        // has_more is set but next_cursor is missing: the loop cannot advance,
        // so it stops after one request and notes the gap.
        res.end(docsPageBody(["d1"], 50, { hasMore: true, nextCursor: null }));
        return;
      case "midfail":
        if (cursor === "") res.end(docsPageBody(["d1", "d2"], 6, { hasMore: true, nextCursor: "c2" }));
        else {
          res.statusCode = 500;
          res.end('{"error":"boom"}');
        }
        return;
      case "firstfail":
        res.statusCode = 500;
        res.end('{"error":"boom"}');
        return;
      case "midauth":
        // First page succeeds, then a 401: an auth failure is not transient, so
        // the tool aborts with a hard error rather than a partial + note.
        if (cursor === "") res.end(docsPageBody(["d1", "d2"], 6, { hasMore: true, nextCursor: "c2" }));
        else {
          res.statusCode = 401;
          res.end('{"error":"unauthorized"}');
        }
        return;
      default:
        res.statusCode = 500;
        res.end(`{"error":"unknown scenario ${scenario}"}`);
    }
  });
  return new Promise((resolveServer) => {
    server.listen(0, "127.0.0.1", () => {
      const { port } = server.address() as AddressInfo;
      resolveServer({
        url: `http://127.0.0.1:${port}`,
        docsRequests,
        close: () => new Promise((res) => server.close(() => res())),
      });
    });
  });
}

interface DocsListContent {
  documents?: unknown[];
  total_documents?: number;
  note?: string;
  error?: string;
}

async function docsListPagingTests(client: MCPClient, docsRequests: Map<string, number>): Promise<void> {
  process.stdout.write("grill_docs_list auto-paging tests:\n");

  const initResp = await client.request("initialize", {
    protocolVersion: "2024-11-05",
    capabilities: {},
    clientInfo: { name: "smoke-paging", version: "0" },
  });
  record("paging client handshake", initResp.result !== undefined);
  client.notify("notifications/initialized");

  const cases: {
    scenario: string;
    wantRequests: number;
    wantDocs?: number;
    wantTotal?: number;
    wantNoteSub?: string[];
    wantNoNote?: boolean;
    wantError?: boolean;
    wantErrorSub?: string;
  }[] = [
    { scenario: "legacy", wantRequests: 1, wantDocs: 2, wantTotal: 2, wantNoNote: true },
    { scenario: "paged", wantRequests: 3, wantDocs: 5, wantTotal: 5, wantNoNote: true },
    { scenario: "cap", wantRequests: 10, wantDocs: 10, wantTotal: 100, wantNoteSub: ["Showing 10 of 100 documents."] },
    {
      scenario: "degraded",
      wantRequests: 1,
      wantDocs: 2,
      wantTotal: 3,
      wantNoteSub: ["Showing 2 of 3 documents.", "temporarily unavailable"],
    },
    {
      scenario: "nocursor",
      wantRequests: 1,
      wantDocs: 1,
      wantTotal: 50,
      wantNoteSub: ["Showing 1 of 50 documents."],
    },
    {
      scenario: "midfail",
      wantRequests: 2,
      wantDocs: 2,
      wantTotal: 6,
      wantNoteSub: ["Showing 2 of 6 documents.", "Fetching additional pages failed"],
    },
    { scenario: "firstfail", wantRequests: 1, wantError: true, wantErrorSub: "grill docs list: HTTP 500" },
    {
      // First page succeeds, second returns 401: aborts with a hard error, not a partial + note.
      scenario: "midauth",
      wantRequests: 2,
      wantError: true,
      wantErrorSub: "authentication failed (HTTP 401)",
    },
  ];

  for (const c of cases) {
    const resp = await client.request("tools/call", { name: "grill_docs_list", arguments: { token: c.scenario } });
    const result = resp.result as { isError?: boolean; structuredContent?: DocsListContent } | undefined;
    const content = result?.structuredContent ?? {};
    const docs = Array.isArray(content.documents) ? content.documents : [];
    const requests = docsRequests.get(c.scenario) ?? 0;
    const note = content.note ?? "";

    const problems: string[] = [];
    if (c.wantError) {
      if (result?.isError !== true) problems.push(`expected isError, got ${JSON.stringify(content)}`);
      const errText = content.error ?? "";
      if (c.wantErrorSub && !errText.includes(c.wantErrorSub)) {
        problems.push(`error "${errText}" missing "${c.wantErrorSub}"`);
      }
    } else {
      if (result?.isError === true) problems.push(`unexpected isError: ${JSON.stringify(content)}`);
      if (docs.length !== c.wantDocs) problems.push(`documents=${docs.length}, want ${c.wantDocs}`);
      if (content.total_documents !== c.wantTotal) problems.push(`total_documents=${content.total_documents}, want ${c.wantTotal}`);
      if (c.wantNoNote && note !== "") problems.push(`note should be absent, got "${note}"`);
      for (const sub of c.wantNoteSub ?? []) {
        if (!note.includes(sub)) problems.push(`note "${note}" missing "${sub}"`);
      }
    }
    if (requests !== c.wantRequests) problems.push(`requests=${requests}, want ${c.wantRequests}`);
    record(`docs list: ${c.scenario}`, problems.length === 0, problems.join("; "));
  }
}

// -- structured error-code + scope tests (offline, against an in-process stub) --
//
// Mirrors go/tools/errorcodes_test.go: a 4xx from the status endpoint must be a
// non-retryable upstream_error (not a transient transport_error), a 5xx must be
// a retryable upstream_error, and success must omit the error envelope. Also
// asserts the ported project-scope object appears on search/ingest.

interface IngestCapture {
  remoteURL?: string;
  labels?: string;
  // X-Attributes / X-Attribute-Schema of every /index4ai/v1/ingest request, in order.
  attrHeaders: { attributes?: string; schema?: string }[];
  // Last /index4ai/v1/attributes request: method, Authorization, X-Project-ID.
  attributes?: { method?: string; auth?: string; projectID?: string };
}

function startErrorStubAPI(): Promise<{ url: string; ingest: IngestCapture; close: () => Promise<void> }> {
  const defaultProject = {
    id: "p1",
    project_id: "p1",
    account_id: "acc1",
    name: "Default Workspace",
    product: "grill",
    protected: false,
    orga_id: "",
    is_default: true,
  };
  // Records the headers the last /index4ai/v1/ingest request carried, for assertions.
  const ingest: IngestCapture = { attrHeaders: [] };
  const server: Server = createServer((req, res) => {
    const u = new URL(req.url ?? "/", "http://localhost");
    res.setHeader("content-type", "application/json");
    const scenario = (req.headers.authorization ?? "").replace("Bearer ", "");

    // Projects listing — used by scope resolution and grill_projects. The
    // gateway refuses project keys here (403) and answers /projects/info instead.
    if (u.pathname === "/index4ai/v1/projects") {
      if (scenario.startsWith("poma_proj_")) {
        res.statusCode = 403;
        res.end('{"code":403,"reason":"forbidden","error":"project API keys are not accepted on this endpoint"}');
        return;
      }
      res.end(JSON.stringify([defaultProject]));
      return;
    }
    if (u.pathname === "/index4ai/v1/projects/info") {
      if (!scenario.startsWith("poma_proj_")) {
        res.statusCode = 401;
        res.end('{"error":"A project API key is required"}');
        return;
      }
      res.end(JSON.stringify({ ...defaultProject, id: "242d", project_id: "242d", name: "immoscout", is_default: false }));
      return;
    }
    // Ingest — capture X-Remote-URL / X-Labels (must stay absent) and return a job_id.
    if (u.pathname === "/index4ai/v1/ingest") {
      ingest.remoteURL = (req.headers["x-remote-url"] as string | undefined) ?? undefined;
      ingest.labels = (req.headers["x-labels"] as string | undefined) ?? undefined;
      ingest.attrHeaders.push({
        attributes: req.headers["x-attributes"] as string | undefined,
        schema: req.headers["x-attribute-schema"] as string | undefined,
      });
      if (scenario === "poma_proj_conflict") {
        res.statusCode = 409;
        res.end('{"code":409,"reason":"project_id_conflict","error":"X-Project-ID does not match the project this API key is bound to"}');
        return;
      }
      res.statusCode = 201;
      res.end('{"job_id":"job-url-1"}');
      return;
    }
    // Job status snapshot — scenario selected by token.
    if (/^\/index4ai\/v1\/jobs\/.+\/status$/.test(u.pathname)) {
      if (scenario === "js404") {
        res.statusCode = 404;
        res.end('{"error":"job not found"}');
      } else if (scenario === "js503") {
        res.statusCode = 503;
        res.end('{"error":"unavailable"}');
      } else if (scenario === "jsdedup") {
        // Gateway with poma-services-go#133: dedup hit, existing doc kept.
        // replaced_doc_ids is sent empty and must be normalized away.
        res.end('{"is_terminal":true,"status":"done","grill":{"deduplicated":true,"doc_id":"doc-orig","replaced_doc_ids":[]}}');
      } else if (scenario === "jsreplaced") {
        // Conversion-build change: this job replaced an older document.
        res.end('{"is_terminal":true,"status":"done","grill":{"deduplicated":false,"doc_id":"job-1","replaced_doc_ids":["doc-old"]}}');
      } else if (scenario === "jsbadgrill") {
        // A `grill` of the wrong JSON type must not invalidate the status it
        // rides on: the job is done and must be reported as done.
        res.end('{"is_terminal":true,"status":"done","grill":"not-an-object"}');
      } else if (scenario === "jsbadfields") {
        // Wrong types inside grill, plus a null in replaced_doc_ids. String(null)
        // would be the four-character id "null"; both impls must drop it.
        res.end('{"is_terminal":true,"status":"done","grill":{"deduplicated":"yes","doc_id":123,"replaced_doc_ids":["a",null,"","b"]}}');
      } else {
        res.end('{"is_terminal":true,"status":"done"}');
      }
      return;
    }
    // Status SSE stream (grill_ingest_sync / grill_ingest_resume) — the
    // terminal event carries the grill object for the jsdedup scenario.
    if (/^\/status\/v1\/jobs\/.+$/.test(u.pathname)) {
      res.setHeader("content-type", "text/event-stream");
      res.write('event: job_status\ndata: {"is_terminal":false,"status":"queued"}\n\n');
      if (scenario === "jsdedup") {
        res.write('event: job_status\ndata: {"is_terminal":true,"status":"done","grill":{"deduplicated":true,"doc_id":"doc-orig","replaced_doc_ids":[]}}\n\n');
      } else if (scenario === "jsbadgrill") {
        res.write('event: job_status\ndata: {"is_terminal":true,"status":"done","grill":"not-an-object"}\n\n');
      } else {
        res.write('event: job_status\ndata: {"is_terminal":true,"status":"done"}\n\n');
      }
      res.end();
      return;
    }
    // Attributes — capture method/auth/project header; "attrs503" = unreadable schema.
    if (u.pathname === "/index4ai/v1/attributes") {
      ingest.attributes = {
        method: req.method,
        auth: req.headers.authorization,
        projectID: (req.headers["x-project-id"] as string | undefined) ?? undefined,
      };
      if (scenario === "attrs503") {
        res.statusCode = 503;
        res.end('{"error":"attribute schema unavailable"}');
        return;
      }
      res.end(
        JSON.stringify({
          attributes: [
            { name: "region", type: "string" },
            { name: "notes", type: "encrypted_text" },
          ],
          max_names: 64,
          note: "Reuse an existing attribute name and type where one fits; a name, once declared, is permanent and counts against max_names.",
        }),
      );
      return;
    }
    // Search.
    if (u.pathname === "/index4ai/v1/search" || u.pathname === "/index4ai/v1/searchInDoc") {
      res.end('{"context":"some context","assets":null}');
      return;
    }
    res.statusCode = 404;
    res.end("{}");
  });
  return new Promise((resolveServer) => {
    server.listen(0, "127.0.0.1", () => {
      const { port } = server.address() as AddressInfo;
      resolveServer({
        url: `http://127.0.0.1:${port}`,
        ingest,
        close: () => new Promise((res) => server.close(() => res())),
      });
    });
  });
}

interface EnvelopeContent {
  error?: string;
  code?: string;
  retryable?: boolean;
  retry_after_seconds?: number;
  scope?: { project_name?: string; hint?: string; is_default?: boolean; source?: string };
  projects?: string;
  documents?: unknown[];
  attributes?: { name?: string; type?: string }[];
  max_names?: number;
  note?: string;
  results?: { code?: string; retryable?: boolean; error?: string; grill?: unknown }[];
  submitted_count?: number;
  job_id?: string;
  events?: unknown[];
  grill?: unknown;
}

async function callTool(client: MCPClient, name: string, args: Record<string, unknown>): Promise<{ isError: boolean; content: EnvelopeContent }> {
  const resp = await client.request("tools/call", { name, arguments: args });
  const result = resp.result as { isError?: boolean; structuredContent?: EnvelopeContent } | undefined;
  return { isError: result?.isError === true, content: result?.structuredContent ?? {} };
}

async function errorCodeTests(
  stubClient: MCPClient,
  deadClient: MCPClient,
  noTokenClient: MCPClient,
  ingest: IngestCapture,
): Promise<void> {
  process.stdout.write("structured error-code + scope tests:\n");

  await stubClient.request("initialize", { protocolVersion: "2024-11-05", capabilities: {}, clientInfo: { name: "smoke-err", version: "0" } });
  stubClient.notify("notifications/initialized");
  await deadClient.request("initialize", { protocolVersion: "2024-11-05", capabilities: {}, clientInfo: { name: "smoke-dead", version: "0" } });
  deadClient.notify("notifications/initialized");
  await noTokenClient.request("initialize", { protocolVersion: "2024-11-05", capabilities: {}, clientInfo: { name: "smoke-notok", version: "0" } });
  noTokenClient.notify("notifications/initialized");

  // 1. jobs_status 404 → per-result upstream_error, NOT retryable.
  {
    const { content } = await callTool(stubClient, "grill_jobs_status", { token: "js404", job_ids: ["job-1"] });
    const r = content.results?.[0];
    const ok = r?.code === "upstream_error" && r?.retryable !== true;
    record("jobs_status 404 → upstream_error non-retryable", ok, ok ? undefined : JSON.stringify(r));
  }
  // 2. jobs_status 503 → per-result upstream_error, retryable.
  {
    const { content } = await callTool(stubClient, "grill_jobs_status", { token: "js503", job_ids: ["job-1"] });
    const r = content.results?.[0];
    const ok = r?.code === "upstream_error" && r?.retryable === true;
    record("jobs_status 503 → upstream_error retryable", ok, ok ? undefined : JSON.stringify(r));
  }
  // 3. jobs_status 200 success → no error envelope on the result or top level.
  {
    const { isError, content } = await callTool(stubClient, "grill_jobs_status", { token: "js200", job_ids: ["job-1"] });
    const r = content.results?.[0];
    const ok = !isError && content.error === undefined && r?.code === undefined && r?.error === undefined;
    record("jobs_status success omits error envelope", ok, ok ? undefined : JSON.stringify(content));
  }
  // 4. Transport error (server unreachable) → transport_error, retryable.
  {
    const { isError, content } = await callTool(deadClient, "grill_projects", { token: "tok" });
    const ok = isError && content.code === "transport_error" && content.retryable === true;
    record("projects unreachable → transport_error retryable", ok, ok ? undefined : JSON.stringify(content));
  }
  // 5. missing_token → code=missing_token (no POMA_API_KEY, no token arg).
  {
    const { isError, content } = await callTool(noTokenClient, "grill_search", { query: "hi" });
    const ok = isError && content.code === "missing_token";
    record("missing token → missing_token", ok, ok ? undefined : JSON.stringify(content));
  }
  // 6. batch all-failed aggregate carries a non-empty code (bad path → invalid_input).
  {
    const { isError, content } = await callTool(stubClient, "grill_ingest_batch", { token: "js200", file_paths: ["/no/such/file/here.txt"] });
    const ok = isError && content.code === "invalid_input" && content.results?.[0]?.code === "invalid_input";
    record("batch all-failed aggregate carries code", ok, ok ? undefined : JSON.stringify(content));
  }
  // 7. search success carries the project scope object.
  {
    const { isError, content } = await callTool(stubClient, "grill_search", { token: "scope1", query: "hi" });
    const ok = !isError && content.scope?.project_name === "Default Workspace" && (content.scope?.hint ?? "").length > 0;
    record("search success includes project scope", ok, ok ? undefined : JSON.stringify(content.scope));
  }
  // 8. URL ingest → sends X-Remote-URL, returns job_id (+ scope).
  {
    ingest.remoteURL = undefined;
    const { isError, content } = await callTool(stubClient, "grill_ingest", { token: "scope1", url: "https://example.com/doc.pdf" });
    const ok =
      !isError &&
      content.job_id === "job-url-1" &&
      ingest.remoteURL === "https://example.com/doc.pdf" &&
      content.scope?.project_name === "Default Workspace";
    record("url ingest sends X-Remote-URL + returns job_id", ok, ok ? undefined : `job_id=${content.job_id} remoteURL=${ingest.remoteURL}`);
  }
  // 9. Legacy labels become a sorted attributes.labels array; no X-Labels (D157).
  {
    ingest.labels = undefined;
    ingest.attrHeaders.length = 0;
    const { isError } = await callTool(stubClient, "grill_ingest", {
      token: "scope1",
      url: "https://example.com/doc.pdf",
      labels: { b: "2", a: "1", " ": "skip" },
    });
    const h = ingest.attrHeaders[0];
    const ok = !isError && ingest.labels === undefined && h?.attributes === '{"labels":["a:1","b:2"]}' && h?.schema === undefined;
    record("labels → sorted attributes.labels, no X-Labels", ok, ok ? undefined : `X-Labels=${ingest.labels} ${JSON.stringify(h)}`);
  }
  // 9b. Legacy labels merge after an explicit attributes.labels, de-duplicated
  //     (grill_ingest_sync shares the path).
  {
    ingest.labels = undefined;
    ingest.attrHeaders.length = 0;
    const { isError, content } = await callTool(stubClient, "grill_ingest", {
      token: "scope1",
      url: "https://example.com/doc.pdf",
      labels: { team: "eng", a: "1" },
      attributes: { labels: ["z:9", "team:eng"], year: 2024 },
    });
    const h = ingest.attrHeaders[0];
    const ok = !isError && ingest.labels === undefined && h?.attributes === '{"labels":["z:9","team:eng","a:1"],"year":2024}';
    record("labels merge into attributes.labels (explicit first, deduped)", ok, ok ? undefined : `X-Labels=${ingest.labels} ${JSON.stringify(h)} ${JSON.stringify(content)}`);
  }
  // 9c. attributes.labels that is not an array of strings cannot take the
  //     legacy labels: invalid_input, never a silent drop; an already-invalid
  //     value keeps its usual error; nothing is sent.
  {
    const cases: [unknown, string][] = [
      ["team:eng", "must be an array of strings"],
      [[1, 2], "must be an array of strings"],
      [null, "null is not accepted"],
    ];
    for (const [labelsAttr, want] of cases) {
      ingest.attrHeaders.length = 0;
      const { isError, content } = await callTool(stubClient, "grill_ingest", {
        token: "scope1",
        url: "https://example.com/doc.pdf",
        labels: { team: "eng" },
        attributes: { labels: labelsAttr },
      });
      const ok = isError && content.code === "invalid_input" && (content.error ?? "").includes(want) && ingest.attrHeaders.length === 0;
      record(`labels + attributes.labels=${JSON.stringify(labelsAttr)} → invalid_input`, ok, ok ? undefined : JSON.stringify(content));
    }
  }
  // 10. url + file_path → invalid_input (mutual exclusivity).
  {
    const { isError, content } = await callTool(stubClient, "grill_ingest", {
      token: "scope1",
      url: "https://example.com/doc.pdf",
      file_path: "/tmp/x.pdf",
    });
    const ok = isError && content.code === "invalid_input";
    record("url + file_path → invalid_input", ok, ok ? undefined : JSON.stringify(content));
  }
  // 10. Project key: grill_projects answers from /projects/info, not the 403 listing.
  {
    const { isError, content } = await callTool(stubClient, "grill_projects", { token: "poma_proj_gr_smoke" });
    const ok = !isError && (content.projects ?? "").includes("immoscout") && (content.projects ?? "").includes("project API key");
    record("project key → grill_projects returns the bound project", ok, ok ? undefined : JSON.stringify(content));
  }
  // 11. Project key: scope names the bound project and says the key selected it.
  {
    const { isError, content } = await callTool(stubClient, "grill_search", { token: "poma_proj_gr_smoke", query: "hi" });
    const ok =
      !isError &&
      content.scope?.source === "project API key" &&
      content.scope?.project_name === "immoscout" &&
      (content.scope?.hint ?? "").includes("project API key") &&
      !(content.scope?.hint ?? "").includes("default grill workspace");
    record("project key → scope source/hint name the bound project", ok, ok ? undefined : JSON.stringify(content.scope));
  }
  // 12. 409 project_id_conflict → terminal invalid_input, not upstream_error.
  {
    const { isError, content } = await callTool(stubClient, "grill_ingest", {
      token: "poma_proj_conflict",
      project_id: "other",
      url: "https://example.com/doc.pdf",
    });
    const ok = isError && content.code === "invalid_input" && content.retryable !== true && (content.error ?? "").includes("HTTP 409");
    record("409 project_id_conflict → invalid_input", ok, ok ? undefined : JSON.stringify(content));
  }
  // 13. docs_list error output carries documents: [] (output schema declares an array).
  {
    const { isError, content } = await callTool(noTokenClient, "grill_docs_list", {});
    const ok = isError && content.code === "missing_token" && Array.isArray(content.documents) && content.documents.length === 0;
    record("docs_list error output has documents: []", ok, ok ? undefined : JSON.stringify(content));
  }
  // 13b. grill_attributes: GET /attributes with Bearer + X-Project-ID,
  //      renders names/types, max_names, note and scope.
  {
    ingest.attributes = undefined;
    const { isError, content } = await callTool(stubClient, "grill_attributes", { token: "scope1", project_id: "p1" });
    // Cast: TS narrowed the field to undefined after the reset above, but the stub mutates it.
    const a = ingest.attributes as IngestCapture["attributes"];
    const ok =
      !isError &&
      a?.method === "GET" &&
      a?.auth === "Bearer scope1" &&
      a?.projectID === "p1" &&
      JSON.stringify(content.attributes) ===
        JSON.stringify([
          { name: "region", type: "string" },
          { name: "notes", type: "encrypted_text" },
        ]) &&
      content.max_names === 64 &&
      (content.note ?? "").includes("permanent") &&
      content.scope?.project_name === "Default Workspace" &&
      content.error === undefined;
    record("grill_attributes request + rendering", ok, ok ? undefined : `${JSON.stringify(a)} ${JSON.stringify(content)}`);
  }
  // 13c. grill_attributes 503 (unreadable schema) → retryable upstream_error, attributes: [].
  {
    const { isError, content } = await callTool(stubClient, "grill_attributes", { token: "attrs503" });
    const ok =
      isError &&
      content.code === "upstream_error" &&
      content.retryable === true &&
      (content.error ?? "").includes("HTTP 503") &&
      Array.isArray(content.attributes) &&
      content.attributes.length === 0 &&
      (ingest.attributes as IngestCapture["attributes"])?.projectID === undefined;
    record("grill_attributes 503 → retryable upstream_error", ok, ok ? undefined : JSON.stringify(content));
  }
  // 13d. Typed attributes on ingest → X-Attributes / X-Attribute-Schema, keys
  //      sorted, compact, non-ASCII escaped; legacy labels ride along as
  //      attributes.labels, never X-Labels.
  {
    ingest.attrHeaders.length = 0;
    ingest.labels = undefined;
    const { isError, content } = await callTool(stubClient, "grill_ingest", {
      token: "scope1",
      url: "https://example.com/doc.pdf",
      labels: { team: "eng" },
      attributes: { year: 2024, region: "emea", city: "Z\u00fcrich", tags: ["a", "b"], notes: "private", ["__proto__"]: "p" },
      attribute_schema: { notes: { type: "encrypted_text" } },
    });
    const h = ingest.attrHeaders[0];
    // __proto__ is a legal name (it matches the regex) and must be sent, not
    // swallowed as a prototype assignment. The literal key in a JSON-RPC
    // payload arrives as an own property after JSON.parse.
    const wantAttrs = '{"__proto__":"p","city":"Z\\u00fcrich","labels":["team:eng"],"notes":"private","region":"emea","tags":["a","b"],"year":2024}';
    const ok =
      !isError &&
      ingest.attrHeaders.length === 1 &&
      h?.attributes === wantAttrs &&
      h?.schema === '{"notes":{"type":"encrypted_text"}}' &&
      ingest.labels === undefined;
    record("ingest attributes → X-Attributes/X-Attribute-Schema + labels as attribute", ok, ok ? undefined : `${JSON.stringify(h)} labels=${ingest.labels} ${JSON.stringify(content)}`);
  }
  // 13d2. __proto__ survives in attribute_schema too (null-prototype object there as well).
  {
    ingest.attrHeaders.length = 0;
    const { isError, content } = await callTool(stubClient, "grill_ingest", {
      token: "scope1",
      url: "https://example.com/doc.pdf",
      attributes: { ["__proto__"]: "p" },
      attribute_schema: { ["__proto__"]: { type: "encrypted_text" } },
    });
    const h = ingest.attrHeaders[0];
    const ok = !isError && h?.attributes === '{"__proto__":"p"}' && h?.schema === '{"__proto__":{"type":"encrypted_text"}}';
    record("__proto__ sent in attributes and attribute_schema", ok, ok ? undefined : `${JSON.stringify(h)} ${JSON.stringify(content)}`);
  }
  // 13d3. A declared empty array is legal (the only way to store []) and is
  //       sent with both headers; float declared over int elements is legal too.
  {
    ingest.attrHeaders.length = 0;
    const { isError, content } = await callTool(stubClient, "grill_ingest", {
      token: "scope1",
      url: "https://example.com/doc.pdf",
      attributes: { tags: [], w: [1, 2.5] },
      attribute_schema: { tags: { type: "[]string" }, w: { type: "[]float" } },
    });
    const h = ingest.attrHeaders[0];
    const ok =
      !isError &&
      h?.attributes === '{"tags":[],"w":[1,2.5]}' &&
      h?.schema === '{"tags":{"type":"[]string"},"w":{"type":"[]float"}}';
    record("declared empty array + []float over ints accepted", ok, ok ? undefined : `${JSON.stringify(h)} ${JSON.stringify(content)}`);
  }
  // 13d4. Non-finite number (1e400 → Infinity after JSON.parse) is refused.
  {
    ingest.attrHeaders.length = 0;
    const resp = await stubClient.requestRaw(
      "tools/call",
      '{"name":"grill_ingest","arguments":{"token":"scope1","url":"https://example.com/doc.pdf","attributes":{"x":1e400}}}',
    );
    const result = resp.result as { isError?: boolean; structuredContent?: EnvelopeContent } | undefined;
    const c = result?.structuredContent ?? {};
    const ok = result?.isError === true && c.code === "invalid_input" && (c.error ?? "").includes("is not a finite number") && ingest.attrHeaders.length === 0;
    record("ingest attributes invalid: non-finite 1e400", ok, ok ? undefined : JSON.stringify(c));
  }
  // 13d5. Accepted: float intent kept by declaring []float; RFC3339 variants and plain dates.
  {
    ingest.attrHeaders.length = 0;
    const dates = ["2024-05-01", "2024-05-01T12:00:00Z", "2024-05-01T12:00:00.123456Z", "2024-05-01T12:00:00+05:30", "2024-05-01T12:00:00.5-08:00"];
    const { isError, content } = await callTool(stubClient, "grill_ingest", {
      token: "scope1",
      url: "https://example.com/doc.pdf",
      attributes: { w: [1.0, 2.5], d: dates },
      attribute_schema: { w: { type: "[]float" }, d: { type: "[]datetime" } },
    });
    const h = ingest.attrHeaders[0];
    const ok =
      !isError &&
      h?.attributes === JSON.stringify({ d: dates, w: [1, 2.5] }) &&
      h?.schema === '{"d":{"type":"[]datetime"},"w":{"type":"[]float"}}';
    record("declared []float over 1.0 and RFC3339/plain-date []datetime accepted", ok, ok ? undefined : `${JSON.stringify(h)} ${JSON.stringify(content)}`);
  }
  // 13e. No attributes → neither header is sent.
  {
    ingest.attrHeaders.length = 0;
    await callTool(stubClient, "grill_ingest", { token: "scope1", url: "https://example.com/doc.pdf" });
    const h = ingest.attrHeaders[0];
    const ok = h !== undefined && h.attributes === undefined && h.schema === undefined;
    record("ingest without attributes sends no attribute headers", ok, ok ? undefined : JSON.stringify(h));
  }
  // 13f. Validation → invalid_input, and nothing reaches the API.
  {
    const cases: { name: string; tool: string; args: Record<string, unknown>; want: string }[] = [
      { name: "uppercase name", tool: "grill_ingest", args: { attributes: { DocYear: 2024 } }, want: 'attribute name "DocYear" must match' },
      { name: "top-level null", tool: "grill_ingest", args: { attributes: { gone: null } }, want: 'attribute "gone": value must be' },
      { name: "object value", tool: "grill_ingest_sync", args: { attributes: { x: { a: 1 } } }, want: 'attribute "x": value must be' },
      { name: "over cap", tool: "grill_ingest", args: { attributes: { x: "y".repeat(2048) } }, want: "capped at 2048" },
      // 400 × ü is 400 characters of input but 2400 once escaped: the cap counts the header.
      { name: "cap counts escaped length", tool: "grill_ingest", args: { attributes: { x: "\u00fc".repeat(400) } }, want: "capped at 2048" },
      { name: "too many names", tool: "grill_ingest", args: { attributes: Object.fromEntries(Array.from({ length: 65 }, (_, i) => [`a${i}`, 1])) }, want: "at most 64" },
      { name: "schema bad name", tool: "grill_ingest", args: { attribute_schema: { Notes: { type: "encrypted_text" } } }, want: 'attribute_schema name "Notes"' },
      {
        name: "orphan declaration",
        tool: "grill_ingest",
        args: { attributes: { region: "emea" }, attribute_schema: { notes: { type: "encrypted_text" } } },
        want: 'attribute_schema declares "notes", but attributes has no value for it',
      },
      {
        name: "schema without attributes",
        tool: "grill_ingest_sync",
        args: { attribute_schema: { notes: { type: "encrypted_text" } } },
        want: 'attribute_schema declares "notes", but attributes has no value for it',
      },
      {
        name: "__proto__ orphan declaration",
        tool: "grill_ingest",
        args: { attributes: { region: "emea" }, attribute_schema: { ["__proto__"]: { type: "encrypted_text" } } },
        want: 'attribute_schema declares "__proto__"',
      },
      { name: "mixed string and int", tool: "grill_ingest", args: { attributes: { x: ["a", 1] } }, want: 'attribute "x": mixed element types in array [int string]' },
      { name: "mixed int and float", tool: "grill_ingest", args: { attributes: { x: [1, 2.5] } }, want: 'attribute "x": mixed element types in array [float int]' },
      { name: "mixed bool and string", tool: "grill_ingest", args: { attributes: { x: [true, "a"] } }, want: "mixed element types" },
      { name: "undeclared empty array", tool: "grill_ingest", args: { attributes: { x: [] } }, want: 'attribute "x": an empty array has no type to infer; declare it in attribute_schema' },
      {
        name: "undeclared empty array (batch)",
        tool: "grill_ingest_batch",
        args: { file_paths: ["/nonexistent/never-read.txt"], attributes: { x: [] } },
        want: "an empty array has no type to infer",
      },
      {
        name: "empty array declared scalar",
        tool: "grill_ingest",
        args: { attributes: { x: [] }, attribute_schema: { x: { type: "string" } } },
        want: 'attribute "x" is declared "string", which needs a scalar',
      },
      {
        name: "declared []int with string",
        tool: "grill_ingest",
        args: { attributes: { x: [1, "a"] }, attribute_schema: { x: { type: "[]int" } } },
        want: 'attribute "x": element "a" is not a int',
      },
      { name: "int beyond 2^53", tool: "grill_ingest", args: { attributes: { x: 2 ** 54 } }, want: "outside the JSON-safe integer range" },
      { name: "array over 64 elements", tool: "grill_ingest", args: { attributes: { x: Array(65).fill(1) } }, want: "65 elements exceeds the cap of 64" },
      // 1.0 is 1 on the wire (and after the gateway's float64 round trip): an int.
      { name: "integral float is an int", tool: "grill_ingest", args: { attributes: { x: [1.0, 2.5] } }, want: 'attribute "x": mixed element types in array [float int]' },
      {
        name: "unknown declared type",
        tool: "grill_ingest",
        args: { attributes: { x: "a" }, attribute_schema: { x: { type: "text" } } },
        want: 'attribute_schema "x": unknown type "text"; known: []bool, []datetime, []encrypted_text, []float, []int, []string, bool, datetime, encrypted_text, float, int, string',
      },
      {
        name: "datetime garbage",
        tool: "grill_ingest",
        args: { attributes: { d: "not-a-date" }, attribute_schema: { d: { type: "datetime" } } },
        want: 'attribute "d": "not-a-date" is not a valid datetime; use RFC3339 (2024-05-01T12:00:00Z) or YYYY-MM-DD',
      },
      {
        name: "datetime impossible day",
        tool: "grill_ingest",
        args: { attributes: { d: "2024-02-30" }, attribute_schema: { d: { type: "datetime" } } },
        want: "is not a valid datetime; use RFC3339",
      },
      {
        name: "datetime missing timezone",
        tool: "grill_ingest",
        args: { attributes: { d: "2024-05-01T12:00:00" }, attribute_schema: { d: { type: "datetime" } } },
        want: "is not a valid datetime; use RFC3339",
      },
      {
        name: "datetime hour 24",
        tool: "grill_ingest",
        args: { attributes: { d: "2024-05-01T24:00:00Z" }, attribute_schema: { d: { type: "datetime" } } },
        want: "is not a valid datetime; use RFC3339",
      },
      {
        name: "[]datetime with one bad",
        tool: "grill_ingest",
        args: { attributes: { d: ["2024-01-02", "2024-1-2"] }, attribute_schema: { d: { type: "[]datetime" } } },
        want: '"2024-1-2" is not a valid datetime',
      },
      { name: "schema missing type", tool: "grill_ingest", args: { attribute_schema: { notes: {} } }, want: 'attribute_schema "notes" must be an object' },
    ];
    for (const c of cases) {
      ingest.attrHeaders.length = 0;
      const { isError, content } = await callTool(stubClient, c.tool, { token: "scope1", url: "https://example.com/doc.pdf", ...c.args });
      const ok = isError && content.code === "invalid_input" && (content.error ?? "").includes(c.want) && ingest.attrHeaders.length === 0;
      record(`ingest attributes invalid: ${c.name}`, ok, ok ? undefined : `requests=${ingest.attrHeaders.length} ${JSON.stringify(content)}`);
    }
    // Exactly 2048 characters is accepted (the cap is inclusive).
    ingest.attrHeaders.length = 0;
    const exact = { x: "y".repeat(2048 - '{"x":""}'.length) };
    const { isError } = await callTool(stubClient, "grill_ingest", { token: "scope1", url: "https://example.com/doc.pdf", attributes: exact });
    const ok = !isError && ingest.attrHeaders[0]?.attributes?.length === 2048;
    record("ingest attributes at exactly 2048 accepted", ok, ok ? undefined : String(ingest.attrHeaders[0]?.attributes?.length));
  }
  // 13g. Batch: attributes go on EVERY file; invalid attributes upload nothing.
  {
    const dir = mkdtempSync(join(tmpdir(), "grill-smoke-"));
    const files = ["a.txt", "b.txt", "c.txt"].map((n) => {
      const p = join(dir, n);
      writeFileSync(p, `hello ${n}`);
      return p;
    });
    try {
      ingest.attrHeaders.length = 0;
      const { isError, content } = await callTool(stubClient, "grill_ingest_batch", {
        token: "scope1",
        file_paths: files,
        attributes: { batch: "q3", notes: "private", year: 2024 },
        attribute_schema: { notes: { type: "encrypted_text" } },
      });
      let ok =
        !isError &&
        content.submitted_count === 3 &&
        ingest.attrHeaders.length === 3 &&
        ingest.attrHeaders.every(
          (h) => h.attributes === '{"batch":"q3","notes":"private","year":2024}' && h.schema === '{"notes":{"type":"encrypted_text"}}',
        );
      record("batch attaches attributes to every file", ok, ok ? undefined : `${JSON.stringify(ingest.attrHeaders)} ${JSON.stringify(content)}`);

      ingest.attrHeaders.length = 0;
      const bad = await callTool(stubClient, "grill_ingest_batch", { token: "scope1", file_paths: files, attributes: { "doc-year": 2024 } });
      ok =
        bad.isError &&
        bad.content.code === "invalid_input" &&
        ingest.attrHeaders.length === 0 &&
        Array.isArray(bad.content.results) &&
        bad.content.results.length === 0;
      record("batch invalid attributes → invalid_input, nothing uploaded", ok, ok ? undefined : `requests=${ingest.attrHeaders.length} ${JSON.stringify(bad.content)}`);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  }
  // 14. jobs_status surfaces the gateway grill object per result, normalized to
  //     the exact shape the Go implementation emits (see
  //     go/tools/grill_outcome_test.go): deduplicated always, doc_id when set,
  //     replaced_doc_ids omitted when empty.
  const wantGrill = JSON.stringify({ deduplicated: true, doc_id: "doc-orig" });
  {
    const { isError, content } = await callTool(stubClient, "grill_jobs_status", { token: "jsdedup", job_ids: ["job-1"] });
    const r = content.results?.[0];
    const ok = !isError && JSON.stringify(r?.grill) === wantGrill && content.grill === undefined;
    record("jobs_status surfaces grill dedup outcome", ok, ok ? undefined : JSON.stringify(content));
  }
  // 14b. Replacement outcome keeps a non-empty replaced_doc_ids (same shape as
  //      go/tools/grill_outcome_test.go's job-replaced case).
  {
    const { isError, content } = await callTool(stubClient, "grill_jobs_status", { token: "jsreplaced", job_ids: ["job-1"] });
    const r = content.results?.[0];
    const ok = !isError && JSON.stringify(r?.grill) === JSON.stringify({ deduplicated: false, doc_id: "job-1", replaced_doc_ids: ["doc-old"] });
    record("jobs_status surfaces grill replacement outcome", ok, ok ? undefined : JSON.stringify(content));
  }
  // 15. jobs_status omits grill entirely when the gateway did not send it.
  {
    const { content } = await callTool(stubClient, "grill_jobs_status", { token: "js200", job_ids: ["job-1"] });
    const r = content.results?.[0] as Record<string, unknown> | undefined;
    const ok = r !== undefined && !("grill" in r);
    record("jobs_status omits grill when absent", ok, ok ? undefined : JSON.stringify(content));
  }
  // 16. ingest_sync lifts the terminal event's grill object to the top level.
  {
    const { isError, content } = await callTool(stubClient, "grill_ingest_sync", { token: "jsdedup", url: "https://example.com/doc.pdf" });
    const ok = !isError && content.job_id === "job-url-1" && content.events?.length === 2 && JSON.stringify(content.grill) === wantGrill;
    record("ingest_sync surfaces grill dedup outcome", ok, ok ? undefined : JSON.stringify(content));
  }
  // 17. ingest_resume: same lift; and no grill key when the stream had none.
  {
    const { isError, content } = await callTool(stubClient, "grill_ingest_resume", { token: "jsdedup", job_id: "job-1" });
    const ok = !isError && content.job_id === "job-1" && JSON.stringify(content.grill) === wantGrill;
    record("ingest_resume surfaces grill dedup outcome", ok, ok ? undefined : JSON.stringify(content));
    const plain = await callTool(stubClient, "grill_ingest_resume", { token: "js200", job_id: "job-1" });
    const okPlain = !plain.isError && !("grill" in plain.content);
    record("ingest_resume omits grill when absent", okPlain, okPlain ? undefined : JSON.stringify(plain.content));
  }
  // 18. A `grill` of the wrong JSON type must not invalidate the status event it
  //     rides on. Go decodes the same way (go/tools/grill_outcome_test.go,
  //     TestJobStatusTolerantGrillDecode): a non-object grill is dropped and the
  //     job still reports done, rather than becoming parse_error or, on the SSE
  //     path, a terminal event the reader discards and then waits forever for.
  {
    const { isError, content } = await callTool(stubClient, "grill_jobs_status", { token: "jsbadgrill", job_ids: ["job-1"] });
    const r = content.results?.[0] as Record<string, unknown> | undefined;
    const ok = !isError && r?.status === "done" && r?.is_terminal === true && !("grill" in (r ?? {})) && !("code" in (r ?? {}));
    record("jobs_status: non-object grill dropped, job still done", ok, ok ? undefined : JSON.stringify(content));
  }
  {
    const { isError, content } = await callTool(stubClient, "grill_ingest_resume", { token: "jsbadgrill", job_id: "job-1" });
    const ok = !isError && content.job_id === "job-1" && content.events?.length === 2 && !("grill" in content);
    record("ingest_resume: non-object grill dropped, stream still terminates", ok, ok ? undefined : JSON.stringify(content));
  }
  // 19. Wrong field types inside grill are coerced field by field, not rejected
  //     wholesale, and non-string replaced_doc_ids entries are dropped rather
  //     than String()-ed — String(null) would hand an agent the doc id "null".
  {
    const { isError, content } = await callTool(stubClient, "grill_jobs_status", { token: "jsbadfields", job_ids: ["job-1"] });
    const r = content.results?.[0];
    const ok = !isError && JSON.stringify(r?.grill) === JSON.stringify({ deduplicated: false, replaced_doc_ids: ["a", "b"] });
    record("jobs_status coerces bad grill fields, drops non-string doc ids", ok, ok ? undefined : JSON.stringify(content));
  }
}

// HTTP (hosted) mode: file_path must be refused because the path would be
// resolved on the server, not the caller's machine. Spawns `-http` on a free
// port and drives the stateless Streamable HTTP transport with fetch.
async function httpModeTests(): Promise<void> {
  process.stdout.write("http-mode tests:\n");
  const port = await new Promise<number>((res) => {
    const s = createServer();
    s.listen(0, "127.0.0.1", () => {
      const { port } = s.address() as AddressInfo;
      s.close(() => res(port));
    });
  });
  const { GRILL_INGEST_ALLOWED_PREFIX: _pre, ...base } = childEnv({ POMA_API_KEY: "smoke-fake-key" });
  const proc = spawn(process.execPath, [BINARY, "-http", `127.0.0.1:${port}`], {
    env: base,
    stdio: ["ignore", "ignore", "pipe"],
  });
  await new Promise<void>((res, rej) => {
    const t = setTimeout(() => rej(new Error("http server did not start")), 10_000);
    proc.stderr.on("data", (d: Buffer) => {
      if (d.toString().includes("listening")) {
        clearTimeout(t);
        res();
      }
    });
    proc.once("exit", (code) => rej(new Error(`http server exited early (${code})`)));
  });
  try {
    let rpcId = 100;
    const call = async (name: string, args: Record<string, unknown>): Promise<EnvelopeContent & { isError?: boolean }> => {
      const r = await fetch(`http://127.0.0.1:${port}/`, {
        method: "POST",
        headers: { "content-type": "application/json", accept: "application/json, text/event-stream" },
        body: JSON.stringify({ jsonrpc: "2.0", id: rpcId++, method: "tools/call", params: { name, arguments: args } }),
      });
      const text = await r.text();
      const line = text.split("\n").find((l) => l.startsWith("data: "));
      const json = JSON.parse(line ? line.slice("data: ".length) : text) as {
        result?: { isError?: boolean; structuredContent?: EnvelopeContent };
      };
      return { ...(json.result?.structuredContent ?? {}), isError: json.result?.isError };
    };
    const single = await call("grill_ingest", { file_path: "/etc/hosts" });
    let ok = single.isError === true && single.code === "invalid_input" && (single.error ?? "").includes("hosted HTTP server");
    record("http mode: grill_ingest file_path → invalid_input", ok, ok ? undefined : JSON.stringify(single));
    const batch = await call("grill_ingest_batch", { file_paths: ["/etc/hosts"] });
    const first = batch.results?.[0];
    ok = first?.code === "invalid_input" && (first?.error ?? "").includes("hosted HTTP server");
    record("http mode: grill_ingest_batch file_paths → invalid_input per file", ok, ok ? undefined : JSON.stringify(batch));
  } finally {
    proc.kill();
    await new Promise<void>((res) => proc.once("exit", () => res()));
  }
}

async function main(): Promise<void> {
  if (!existsSync(BINARY)) {
    process.stderr.write(`error: ${BINARY} not found. Run \`npm run build\` first.\n`);
    process.exit(2);
  }

  const deadAPI = createServer((req, res) => {
    deadAPIHits.push(`${req.method} ${req.url}`);
    res.statusCode = 418;
    res.setHeader("content-type", "application/json");
    res.end('{"error":"smoke test reached the default API host; pass POMA_API_BASE_URL to a stub"}');
  });
  deadAPIURL = await new Promise<string>((res) => {
    deadAPI.listen(0, "127.0.0.1", () => res(`http://127.0.0.1:${(deadAPI.address() as AddressInfo).port}`));
  });

  // Validation handlers all check token presence first; supply a placeholder
  // so the validation messages we're asserting on actually surface.
  const client = new MCPClient({ POMA_API_KEY: "smoke-fake-key" });
  try {
    await offlineTests(client);
  } finally {
    await client.close();
  }

  const stub = await startStubAPI();
  const pagingClient = new MCPClient({ POMA_API_BASE_URL: stub.url });
  try {
    await docsListPagingTests(pagingClient, stub.docsRequests);
  } finally {
    await pagingClient.close();
    await stub.close();
  }

  // Error-code + scope tests need three clients: one pointed at a live stub, one
  // pointed at a dead port (transport error), and one with no credentials.
  const errStub = await startErrorStubAPI();
  // A closed port: bind, capture the address, close, then point a client at it.
  const deadURL = await new Promise<string>((res) => {
    const s = createServer();
    s.listen(0, "127.0.0.1", () => {
      const { port } = s.address() as AddressInfo;
      s.close(() => res(`http://127.0.0.1:${port}`));
    });
  });
  const stubClient = new MCPClient({ POMA_API_BASE_URL: errStub.url, POMA_API_KEY: "" });
  const deadClient = new MCPClient({ POMA_API_BASE_URL: deadURL, POMA_API_KEY: "" });
  const noTokenClient = new MCPClient({ POMA_API_BASE_URL: errStub.url, POMA_API_KEY: "" });
  try {
    await errorCodeTests(stubClient, deadClient, noTokenClient, errStub.ingest);
  } finally {
    await stubClient.close();
    await deadClient.close();
    await noTokenClient.close();
    await errStub.close();
  }

  await httpModeTests();

  await new Promise<void>((res) => deadAPI.close(() => res()));
  record(
    "no request reached the default API host",
    deadAPIHits.length === 0,
    deadAPIHits.length === 0 ? undefined : deadAPIHits.join(", "),
  );

  const passed = results.filter((r) => r.ok).length;
  const failed = results.length - passed;
  process.stdout.write(`\n${passed}/${results.length} passed${failed > 0 ? `, ${failed} failed` : ""}\n`);
  process.exit(failed === 0 ? 0 : 1);
}

main().catch((err: unknown) => {
  const msg = err instanceof Error ? err.stack ?? err.message : String(err);
  process.stderr.write(`smoke harness crashed: ${msg}\n`);
  process.exit(2);
});
