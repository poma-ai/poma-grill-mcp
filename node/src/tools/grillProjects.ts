import type { CallToolResult } from "@modelcontextprotocol/sdk/types.js";
import { errorResult, getToken, interpretAuthError, successResult, type ToolContext } from "../common.js";
import { GrillClient } from "../client/grillClient.js";

interface ProjectInfo {
  id: string;
  name?: string;
  product?: string;
  protected?: boolean;
  org_id?: string;
}

export async function grillProjects(
  args: Record<string, unknown>,
  _ctx: ToolContext,
): Promise<CallToolResult> {
  const token = getToken(args.token);
  if (token === "") {
    return errorResult("token is required (provide token or set POMA_API_KEY on the server)");
  }

  const product =
    typeof args.product === "string" && args.product !== "" ? args.product : undefined;

  // GrillClient with no projectID — listProjects doesn't send X-Project-ID.
  const client = new GrillClient(token);
  let res;
  try {
    res = await client.listProjects(product);
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err);
    return errorResult(`grill projects: ${msg}`);
  }

  const authErr = interpretAuthError(args.token, res.status, res.body, "grill projects");
  if (authErr) return errorResult(authErr);
  const text = new TextDecoder("utf-8").decode(res.body);
  if (res.status !== 200) {
    return errorResult(`grill projects: HTTP ${res.status}: ${text}`);
  }

  let projects: ProjectInfo[];
  try {
    const parsed = JSON.parse(text) as unknown;
    if (Array.isArray(parsed)) {
      projects = parsed as ProjectInfo[];
    } else if (
      parsed !== null &&
      typeof parsed === "object" &&
      "projects" in (parsed as object) &&
      Array.isArray((parsed as Record<string, unknown>).projects)
    ) {
      projects = (parsed as Record<string, unknown>).projects as ProjectInfo[];
    } else {
      return errorResult(`grill projects: unexpected response shape: ${text}`);
    }
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err);
    return errorResult(`grill projects: parse response: ${msg}`);
  }

  if (projects.length === 0) {
    return successResult({ projects: "No accessible projects found." });
  }

  const lines = ["Projects:"];
  for (const p of projects) {
    let line = `- ${p.name ?? p.id} (project_id: ${p.id}, product: ${p.product ?? "unknown"}, protected: ${p.protected ?? false}`;
    if (p.org_id) line += `, org: ${p.org_id}`;
    line += ")";
    lines.push(line);
  }

  return successResult({ projects: lines.join("\n") });
}
