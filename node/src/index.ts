#!/usr/bin/env node
import { createReadStream, type ReadStream } from "node:fs";
import { createServer as createHTTPServer, type IncomingMessage, type ServerResponse } from "node:http";
import { Readable } from "node:stream";

import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import { StreamableHTTPServerTransport } from "@modelcontextprotocol/sdk/server/streamableHttp.js";

import { setHTTPMode } from "./client/ingestPayload.js";

import { createServer } from "./server.js";

interface CLIArgs {
  input?: string;
  http?: string;
}

function parseArgs(argv: string[]): CLIArgs {
  const out: CLIArgs = {};
  for (let i = 2; i < argv.length; i++) {
    const arg = argv[i];
    if (arg === "-h" || arg === "--help") {
      printUsage();
      process.exit(0);
    } else if (arg === "-input" || arg === "--input") {
      out.input = argv[++i];
    } else if (arg === "-http" || arg === "--http") {
      out.http = argv[++i];
    } else {
      console.error(`unknown flag: ${arg}`);
      printUsage();
      process.exit(2);
    }
  }
  return out;
}

function printUsage(): void {
  process.stderr.write(
    [
      "Usage: poma-grill-mcp [-input <path|->] | [-http <addr>]",
      "",
      "  -input <path|->    Stdio mode; '-' reads from stdin",
      "  -http <addr>       HTTP mode (e.g. :8080)",
      "",
      "Env: POMA_GRILL_API_KEY (project key) or POMA_API_KEY (account key) — required unless passed per-call as 'token'",
      "     POMA_API_BASE_URL (override https://api.poma-ai.com)",
      "     POMA_STATUS_API_BASE_URL (override https://api.poma-ai.com/status/v1)",
      "",
    ].join("\n"),
  );
}

async function runStdio(inputPath: string): Promise<void> {
  const server = createServer();
  let stdin: NodeJS.ReadableStream;
  if (inputPath === "-") {
    stdin = process.stdin;
  } else {
    stdin = createReadStream(inputPath) as ReadStream;
  }
  const transport = new StdioServerTransport(stdin as Readable, process.stdout);
  await server.connect(transport);
}

async function runHTTP(addr: string): Promise<void> {
  const { hostname, port } = parseAddr(addr);
  // file_path would read this process's filesystem; refuse it on the hosted
  // server unless GRILL_INGEST_ALLOWED_PREFIX opts a directory in.
  setHTTPMode(true);

  const httpServer = createHTTPServer(async (req: IncomingMessage, res: ServerResponse) => {
    const url = req.url ?? "";
    if (req.method === "GET" && url === "/health") {
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end('{"status":"ok"}');
      return;
    }
    // Stateless mode: each request is independent — no session id tracking.
    // The SDK refuses to reuse a stateless transport across requests ("Create
    // a new transport per request"), so a shared instance answered only the
    // first request and every later one got an empty 500. Build server +
    // transport per request and tear them down when the response closes.
    const server = createServer();
    const transport = new StreamableHTTPServerTransport({ sessionIdGenerator: undefined });
    res.on("close", () => {
      void transport.close();
      void server.close();
    });
    try {
      await server.connect(transport);
      await transport.handleRequest(req, res);
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err);
      process.stderr.write(`mcp request failed: ${msg}\n`);
      if (!res.headersSent) {
        res.writeHead(500, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ jsonrpc: "2.0", error: { code: -32603, message: "Internal error" }, id: null }));
      } else {
        res.end();
      }
    }
  });

  httpServer.listen(port, hostname, () => {
    process.stderr.write(`poma-grill-mcp listening on ${addr} (MCP at /, health at /health)\n`);
  });
}

function parseAddr(addr: string): { hostname: string; port: number } {
  // Accepts ":8080" or "host:8080".
  const idx = addr.lastIndexOf(":");
  if (idx < 0) {
    throw new Error(`invalid -http addr: ${addr} (expected ':PORT' or 'HOST:PORT')`);
  }
  const hostname = addr.slice(0, idx) || "0.0.0.0";
  const port = Number.parseInt(addr.slice(idx + 1), 10);
  if (!Number.isFinite(port) || port <= 0 || port > 65535) {
    throw new Error(`invalid -http port: ${addr.slice(idx + 1)}`);
  }
  return { hostname, port };
}

async function main(): Promise<void> {
  const args = parseArgs(process.argv);
  if (args.input && args.http) {
    console.error("error: -input and -http are mutually exclusive");
    process.exit(2);
  }
  if (args.input !== undefined) {
    await runStdio(args.input);
    return;
  }
  if (args.http !== undefined) {
    await runHTTP(args.http);
    return;
  }
  printUsage();
  process.exit(2);
}

main().catch((err: unknown) => {
  const msg = err instanceof Error ? err.message : String(err);
  console.error(`fatal: ${msg}`);
  process.exit(1);
});
