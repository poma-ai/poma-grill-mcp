import { existsSync, readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

export interface ToolDefinition {
  name: string;
  description: string;
  inputSchema: Record<string, unknown>;
  outputSchema?: Record<string, unknown>;
}

interface SchemasFile {
  tools: ToolDefinition[];
}

function findSchemasPath(): string {
  const here = dirname(fileURLToPath(import.meta.url));
  // Bundled with the npm package: dist/schemas/tools.json sits next to the
  // compiled module (scripts/copy-schemas.mjs places it there during build).
  const bundled = resolve(here, "schemas", "tools.json");
  if (existsSync(bundled)) return bundled;
  // In-repo dev fallback: tsx src/ or unbundled dist/ — schemas/ lives two
  // dirs up at the repo root.
  const repoRoot = resolve(here, "..", "..", "schemas", "tools.json");
  if (existsSync(repoRoot)) return repoRoot;
  throw new Error(
    `schemas/tools.json not found. Looked at:\n  ${bundled}\n  ${repoRoot}`,
  );
}

export function loadToolDefinitions(): ToolDefinition[] {
  const path = findSchemasPath();
  const raw = readFileSync(path, "utf8");
  const parsed = JSON.parse(raw) as SchemasFile;
  return parsed.tools;
}
