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
import { existsSync } from "node:fs";
import { createServer, type Server } from "node:http";
import type { AddressInfo } from "node:net";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { createInterface, type Interface } from "node:readline";

const HERE = dirname(fileURLToPath(import.meta.url));
const NODE_ROOT = resolve(HERE, "..");
const BINARY = resolve(NODE_ROOT, "dist", "index.js");

const EXPECTED_TOOLS = [
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
      env: { ...process.env, ...env },
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
// the tool must send GET /v3/grill/docs?cursor=<next_cursor> for follow-ups.

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
    if (u.pathname !== "/v3/grill/docs") {
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

async function main(): Promise<void> {
  if (!existsSync(BINARY)) {
    process.stderr.write(`error: ${BINARY} not found. Run \`npm run build\` first.\n`);
    process.exit(2);
  }

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
