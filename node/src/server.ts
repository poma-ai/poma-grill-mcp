import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { Server } from "@modelcontextprotocol/sdk/server/index.js";
import {
  CallToolRequestSchema,
  ListToolsRequestSchema,
} from "@modelcontextprotocol/sdk/types.js";

import { loadToolDefinitions, type ToolDefinition } from "./schemas.js";
import { errorResult, type ToolContext, type ToolHandler } from "./common.js";

// Resolve the package version from package.json so MCP serverInfo.version
// tracks the npm release. Works in dev (../package.json from src/) and built
// (../package.json from dist/) and when installed via npm (the package's own
// package.json is always shipped at the package root).
function packageVersion(): string {
  try {
    const here = dirname(fileURLToPath(import.meta.url));
    const pkgPath = resolve(here, "..", "package.json");
    const pkg = JSON.parse(readFileSync(pkgPath, "utf8")) as { version?: string };
    return typeof pkg.version === "string" && pkg.version !== "" ? pkg.version : "0.0.0";
  } catch {
    return "0.0.0";
  }
}
import { grillDocsList } from "./tools/grillDocsList.js";
import { grillExplain } from "./tools/grillExplain.js";
import { grillIngest } from "./tools/grillIngest.js";
import { grillIngestSync } from "./tools/grillIngestSync.js";
import { grillIngestResume } from "./tools/grillIngestResume.js";
import { grillIngestBatch } from "./tools/grillIngestBatch.js";
import { grillJobsStatus } from "./tools/grillJobsStatus.js";
import { grillSearch } from "./tools/grillSearch.js";
import { grillProjects } from "./tools/grillProjects.js";

const handlers: Record<string, ToolHandler> = {
  grill_docs_list: grillDocsList,
  grill_explain: grillExplain,
  grill_ingest: grillIngest,
  grill_ingest_sync: grillIngestSync,
  grill_ingest_resume: grillIngestResume,
  grill_ingest_batch: grillIngestBatch,
  grill_jobs_status: grillJobsStatus,
  grill_search: grillSearch,
  grill_projects: grillProjects,
};

export function createServer(): Server {
  const tools = loadToolDefinitions();
  validateRegistration(tools);

  const server = new Server(
    { name: "poma-grill-mcp", version: packageVersion() },
    { capabilities: { tools: {} } },
  );

  server.setRequestHandler(ListToolsRequestSchema, async () => ({
    tools: tools.map((t) => ({
      name: t.name,
      description: t.description,
      inputSchema: t.inputSchema,
      ...(t.outputSchema ? { outputSchema: t.outputSchema } : {}),
    })),
  }));

  server.setRequestHandler(CallToolRequestSchema, async (request, extra) => {
    const { name, arguments: args, _meta } = request.params;
    const handler = handlers[name];
    if (!handler) {
      return errorResult(`unknown tool: ${name}`);
    }

    const progressToken = _meta?.progressToken;
    const ctx: ToolContext = {
      signal: extra.signal,
      ...(progressToken !== undefined
        ? {
            notifyProgress: async (event, seq) => {
              try {
                await extra.sendNotification({
                  method: "notifications/progress",
                  params: {
                    progressToken,
                    progress: seq,
                    total: 0,
                    message: JSON.stringify({
                      job_id: event.jobId,
                      status: event.status,
                      ...(event.error ? { error: event.error } : {}),
                    }),
                  },
                });
              } catch {
                // Notification delivery is best-effort.
              }
            },
          }
        : {}),
    };

    return handler(args ?? {}, ctx);
  });

  return server;
}

function validateRegistration(tools: ToolDefinition[]): void {
  const declared = new Set(tools.map((t) => t.name));
  const registered = new Set(Object.keys(handlers));
  for (const name of declared) {
    if (!registered.has(name)) {
      throw new Error(`tool ${name} declared in schemas but no handler registered`);
    }
  }
  for (const name of registered) {
    if (!declared.has(name)) {
      throw new Error(`handler ${name} registered but not declared in schemas`);
    }
  }
}
