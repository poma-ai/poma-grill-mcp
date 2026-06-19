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
