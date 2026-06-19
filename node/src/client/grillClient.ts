import { apiBaseURL } from "../common.js";

export interface GrillResponse {
  body: Uint8Array;
  status: number;
}

function trimRightSlash(s: string): string {
  return s.endsWith("/") ? s.slice(0, -1) : s;
}

function joinURL(base: string, path: string): string {
  const b = trimRightSlash(base);
  const p = path.startsWith("/") ? path : "/" + path;
  return b + p;
}

export class GrillClient {
  constructor(
    private readonly token: string,
    private readonly projectID: string = "",
  ) {}

  get authToken(): string {
    return this.token;
  }

  // doGet issues a GET with no request body (fetch rejects a body on GET/HEAD).
  // Mirrors the Go client's c.Do(http.MethodGet, path, nil, nil).
  async doGet(path: string): Promise<GrillResponse> {
    const url = joinURL(apiBaseURL(), path);
    const headers: Record<string, string> = { Authorization: `Bearer ${this.token}` };
    if (this.projectID !== "") headers["X-Project-ID"] = this.projectID;
    const res = await fetch(url, { method: "GET", headers });
    const buf = new Uint8Array(await res.arrayBuffer());
    return { body: buf, status: res.status };
  }

  async doJSON(method: string, path: string, body: unknown): Promise<GrillResponse> {
    const url = joinURL(apiBaseURL(), path);
    const headers: Record<string, string> = {
      "Content-Type": "application/json",
      Authorization: `Bearer ${this.token}`,
    };
    if (this.projectID !== "") headers["X-Project-ID"] = this.projectID;
    const res = await fetch(url, {
      method,
      headers,
      body: JSON.stringify(body ?? {}),
    });
    const buf = new Uint8Array(await res.arrayBuffer());
    return { body: buf, status: res.status };
  }

  async ingestRaw(data: Uint8Array, filename: string): Promise<GrillResponse> {
    const url = joinURL(apiBaseURL(), "/grill/ingest");
    const safeName = sanitizeFilename(filename);
    // BodyInit accepts BufferSource; Uint8Array is allowed in Node 20+ fetch.
    const headers: Record<string, string> = {
      "Content-Type": "application/octet-stream",
      "Content-Disposition": `attachment; filename="${safeName}"`,
      "Content-Length": String(data.byteLength),
      Authorization: `Bearer ${this.token}`,
    };
    if (this.projectID !== "") headers["X-Project-ID"] = this.projectID;
    const res = await fetch(url, { method: "POST", headers, body: data });
    const buf = new Uint8Array(await res.arrayBuffer());
    return { body: buf, status: res.status };
  }

  // listProjects calls GET /projects (no X-Project-ID header).
  // If product is provided, appends ?product=<product> as a query param.
  async listProjects(product?: string): Promise<GrillResponse> {
    const path = product ? `/projects?product=${encodeURIComponent(product)}` : "/projects";
    const url = joinURL(apiBaseURL(), path);
    const res = await fetch(url, {
      method: "GET",
      headers: { Authorization: `Bearer ${this.token}` },
    });
    const buf = new Uint8Array(await res.arrayBuffer());
    return { body: buf, status: res.status };
  }
}

// Mirrors Go's grillSanitizeFilename: blank/dot/dotdot or filenames containing
// disposition-breaking characters fall back to "upload<ext>".
function sanitizeFilename(name: string): string {
  const ext = pathExt(name);
  if (name === "" || name === "." || name === "..") return `upload${ext}`;
  if (/["\\\r\n\0]/.test(name)) return `upload${ext}`;
  return name;
}

function pathExt(name: string): string {
  const idx = name.lastIndexOf(".");
  if (idx < 0 || idx === 0 || idx === name.length - 1) return "";
  // Only treat as extension if no slash after the last dot.
  const tail = name.slice(idx);
  if (tail.includes("/") || tail.includes("\\")) return "";
  return tail;
}

export interface ParsedJob {
  job_id: string;
}

export function parseJob(body: Uint8Array): ParsedJob | null {
  try {
    const text = new TextDecoder("utf-8").decode(body);
    const obj = JSON.parse(text) as Record<string, unknown>;
    const id = obj["job_id"];
    if (typeof id === "string" && id !== "") return { job_id: id };
    return null;
  } catch {
    return null;
  }
}
