import { createHash } from "node:crypto";

import { GrillClient } from "./client/grillClient.js";

// -- Project Scope ---------------------------------------------------
//
// Every grill operation runs against exactly one project namespace. Named
// projects use their project_id as the namespace; the account's default
// workspace uses "account_<account_id>". These tools surface which project the
// data belongs to via a `scope` object so the calling LLM can always tell the
// user, e.g. "these documents belong to your Default Workspace".
//
// Mirrors go/tools/grill.go (GrillScope, resolveScope, projects cache).

/** Describes which project an operation's data belongs to. */
export interface GrillScope {
  project_name?: string;
  project_id?: string;
  namespace?: string;
  is_default?: boolean;
  source?: string;
  hint?: string;
}

/** One entry of the /projects listing (superset of what grill_projects renders). */
export interface GrillProject {
  id: string;
  project_id: string;
  account_id: string;
  name: string;
  product: string;
  protected: boolean;
  orga_id: string;
  is_default: boolean;
}

/**
 * Decodes the /projects response, accepting either a bare array or a
 * { "projects": [...] } wrapper. Returns null on parse failure.
 */
export function parseProjects(body: Uint8Array): GrillProject[] | null {
  let parsed: unknown;
  try {
    parsed = JSON.parse(new TextDecoder("utf-8").decode(body));
  } catch {
    return null;
  }
  let arr: unknown;
  if (Array.isArray(parsed)) {
    arr = parsed;
  } else if (parsed !== null && typeof parsed === "object" && Array.isArray((parsed as Record<string, unknown>).projects)) {
    arr = (parsed as Record<string, unknown>).projects;
  } else {
    return null;
  }
  return (arr as Record<string, unknown>[]).map((p) => ({
    id: str(p.id),
    project_id: str(p.project_id),
    account_id: str(p.account_id),
    name: str(p.name),
    product: str(p.product),
    protected: p.protected === true,
    orga_id: str(p.orga_id),
    is_default: p.is_default === true,
  }));
}

function str(v: unknown): string {
  return typeof v === "string" ? v : "";
}

// projectsCache memoizes the /projects listing per token so scope resolution
// does not add a round-trip to every search/ingest. Entries expire after
// PROJECTS_CACHE_TTL_MS.
//
// MULTI-TENANT SAFETY: this MCP can serve many users concurrently, each with
// their own token. The cache is partitioned by a SHA-256 of the token, so an
// entry is only readable by a caller presenting the SAME token. The raw token
// is never stored, and expired entries are evicted so the map stays bounded.
const PROJECTS_CACHE_TTL_MS = 5 * 60 * 1000;
const PROJECTS_CACHE_MAX_ENTRIES = 10000; // pathological-churn backstop

interface ProjectsCacheEntry {
  projects: GrillProject[];
  expires: number;
}

const projectsCache = new Map<string, ProjectsCacheEntry>();

// Overridable in tests to exercise expiry deterministically.
export let nowFunc: () => number = () => Date.now();
export function setNowFunc(fn: () => number): void {
  nowFunc = fn;
}

function cacheKey(token: string): string {
  return createHash("sha256").update(token).digest("hex");
}

function cacheGet(token: string): GrillProject[] | undefined {
  if (token === "") return undefined;
  const e = projectsCache.get(cacheKey(token));
  if (!e || nowFunc() >= e.expires) return undefined;
  return e.projects;
}

function cachePut(token: string, projects: GrillProject[]): void {
  if (token === "") return;
  const now = nowFunc();
  for (const [k, e] of projectsCache) {
    if (now >= e.expires) projectsCache.delete(k);
  }
  if (projectsCache.size >= PROJECTS_CACHE_MAX_ENTRIES) projectsCache.clear();
  projectsCache.set(cacheKey(token), { projects, expires: now + PROJECTS_CACHE_TTL_MS });
}

/**
 * Returns the token's grill projects, using a short-lived cache. Returns null
 * on any error — scope resolution degrades gracefully and never blocks the
 * primary operation.
 */
async function fetchProjectsCached(client: GrillClient, token: string): Promise<GrillProject[] | null> {
  const cached = cacheGet(token);
  if (cached) return cached;
  let res;
  try {
    res = await client.listProjects("grill");
  } catch {
    return null;
  }
  if (res.status !== 200) return null;
  const projects = parseProjects(res.body);
  if (!projects) return null;
  cachePut(token, projects);
  return projects;
}

/**
 * Maps a request's project context to a friendly scope. Provide the
 * authoritative namespace when known (grill_docs_list returns it); otherwise
 * pass "" and the resolved project_id. Never returns null — scope resolution
 * degrades to the raw identifiers if the projects listing is unavailable.
 */
export async function resolveScope(
  client: GrillClient,
  token: string,
  resolvedProjectID: string,
  namespace: string,
  source: string,
): Promise<GrillScope> {
  const projects = await fetchProjectsCached(client, token);
  return scopeFromProjects(projects ?? [], resolvedProjectID, namespace, source);
}

/**
 * Pure mapping from a project listing + request context to a friendly scope.
 * Split out from resolveScope so it is testable without a network round-trip.
 */
export function scopeFromProjects(
  projects: GrillProject[],
  resolvedProjectID: string,
  namespace: string,
  source: string,
): GrillScope {
  const scope: GrillScope = { project_id: resolvedProjectID, namespace, source };

  const find = (pred: (p: GrillProject) => boolean): GrillProject | undefined =>
    projects.find(pred);

  let p: GrillProject | undefined;
  if (namespace !== "") {
    // Grill docs namespaces are "account_<account_id>" for the default
    // workspace and "proj_<project_id>" for named projects. Tolerate a bare id
    // too, in case the wire format changes.
    if (namespace.startsWith("account_")) {
      const acct = namespace.slice("account_".length);
      p = find((x) => x.product === "grill" && x.is_default && x.account_id === acct);
    } else {
      const id = namespace.startsWith("proj_") ? namespace.slice("proj_".length) : namespace;
      p = find((x) => x.project_id === id || x.id === id);
    }
  } else if (resolvedProjectID !== "") {
    p = find((x) => x.project_id === resolvedProjectID || x.id === resolvedProjectID);
  } else {
    // Account default: the key owner's default grill workspace (own account, not
    // an org's) — identified by is_default with no orga.
    p = find((x) => x.product === "grill" && x.is_default && x.orga_id === "");
  }

  if (p) {
    scope.project_name = p.name;
    scope.project_id = p.project_id;
    scope.is_default = p.is_default;
    if (!scope.namespace || scope.namespace === "") {
      scope.namespace = p.is_default ? `account_${p.account_id}` : `proj_${p.project_id}`;
    }
  }

  if (scope.project_name && scope.is_default) {
    scope.hint =
      `This belongs to your default grill workspace "${scope.project_name}" — no specific project is selected. ` +
      `Pass project_id or set POMA_PROJECT_ID to target another project.`;
  } else if (scope.project_name) {
    scope.hint = `Scoped to project "${scope.project_name}".`;
  } else if (resolvedProjectID !== "") {
    scope.hint = `Scoped to project_id ${resolvedProjectID} (name unavailable).`;
  } else {
    scope.hint = "This belongs to your default grill workspace — no specific project is selected.";
  }

  return scope;
}

/**
 * Drops empty/false fields from a scope so the serialized object matches Go's
 * omitempty output (e.g. is_default omitted when false, project_id omitted when
 * empty). Returns undefined if nothing meaningful remains.
 */
export function scopeFields(scope: GrillScope): Record<string, unknown> | undefined {
  const o: Record<string, unknown> = {};
  if (scope.project_name) o.project_name = scope.project_name;
  if (scope.project_id) o.project_id = scope.project_id;
  if (scope.namespace) o.namespace = scope.namespace;
  if (scope.is_default) o.is_default = true;
  if (scope.source) o.source = scope.source;
  if (scope.hint) o.hint = scope.hint;
  return Object.keys(o).length > 0 ? o : undefined;
}
