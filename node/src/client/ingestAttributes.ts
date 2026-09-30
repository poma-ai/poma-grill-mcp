// Typed document attributes on ingest. Mirrors go/tools/ingest_attributes.go.
//
// The gateway takes them on POST /grill/ingest as two headers — grill ingest is
// octet-stream only, so a header is the ONLY carrier — and refuses a header
// over 2048 characters, a name outside ^[a-z0-9_]{1,64}$, or more than 64
// names. The same rules are checked here so the agent gets a precise
// invalid_input instead of a gateway 400; input is never truncated to fit.

export const attributesHeaderMaxLen = 2048;
export const attributesMaxNames = 64;
const attributeNameRe = /^[a-z0-9_]{1,64}$/;

/** Optional per-document metadata headers of a grill ingest; empty = not sent. */
export interface IngestHeaders {
  labels?: string; // X-Labels
  attributes?: string; // X-Attributes
  attributeSchema?: string; // X-Attribute-Schema
}

export function applyIngestHeaders(headers: Record<string, string>, meta: IngestHeaders): void {
  if (meta.labels) headers["X-Labels"] = meta.labels;
  if (meta.attributes) headers["X-Attributes"] = meta.attributes;
  if (meta.attributeSchema) headers["X-Attribute-Schema"] = meta.attributeSchema;
}

function isPlainObject(v: unknown): v is Record<string, unknown> {
  return v !== null && typeof v === "object" && !Array.isArray(v);
}

function isScalar(v: unknown): boolean {
  return typeof v === "string" || typeof v === "boolean" || (typeof v === "number" && Number.isFinite(v));
}

function validAttributeValue(v: unknown): boolean {
  if (v === null || isScalar(v)) return true;
  return Array.isArray(v) && v.every(isScalar);
}

// Compact JSON with top-level keys sorted and every non-ASCII UTF-16 code unit
// escaped as \uXXXX (lowercase hex, surrogate pairs above the BMP) — plain
// ASCII, safe as an HTTP header value, matching the Go encoder.
function compactASCIIJSON(obj: Record<string, unknown>): string {
  const sorted: Record<string, unknown> = {};
  for (const k of Object.keys(obj).sort()) sorted[k] = obj[k];
  return JSON.stringify(sorted).replace(
    /[\u0080-￿]/g,
    (c) => "\\u" + c.charCodeAt(0).toString(16).padStart(4, "0"),
  );
}

/**
 * Validates the `attributes` / `attribute_schema` tool arguments and renders
 * them as header values. Absent/empty input yields no headers. Throws an Error
 * (reported as invalid_input) on any violation.
 */
export function encodeIngestAttributes(
  attrsArg: unknown,
  schemaArg: unknown,
): { attributes: string; attributeSchema: string } {
  let attributes = "";
  let attributeSchema = "";

  if (attrsArg !== undefined && attrsArg !== null) {
    if (!isPlainObject(attrsArg)) {
      throw new Error("attributes must be an object mapping attribute name to value");
    }
    const names = Object.keys(attrsArg).sort();
    if (names.length > attributesMaxNames) {
      throw new Error(`attributes has ${names.length} names; at most ${attributesMaxNames} per document`);
    }
    for (const name of names) {
      if (!attributeNameRe.test(name)) {
        throw new Error(`attribute name ${JSON.stringify(name)} must match ^[a-z0-9_]{1,64}$ (lowercase letters, digits, underscore)`);
      }
      if (!validAttributeValue(attrsArg[name])) {
        throw new Error(
          `attribute ${JSON.stringify(name)}: value must be a string, number, boolean, an array of those, or null`,
        );
      }
    }
    if (names.length > 0) {
      const s = compactASCIIJSON(attrsArg);
      if (s.length > attributesHeaderMaxLen) {
        throw new Error(
          `attributes encode to ${s.length} characters; the X-Attributes header is capped at ${attributesHeaderMaxLen}. Nothing was ingested and nothing was truncated — send fewer or shorter attributes`,
        );
      }
      attributes = s;
    }
  }

  if (schemaArg !== undefined && schemaArg !== null) {
    if (!isPlainObject(schemaArg)) {
      throw new Error('attribute_schema must be an object like {"name": {"type": "encrypted_text"}}');
    }
    const names = Object.keys(schemaArg).sort();
    const decl: Record<string, unknown> = {};
    for (const name of names) {
      if (!attributeNameRe.test(name)) {
        throw new Error(
          `attribute_schema name ${JSON.stringify(name)} must match ^[a-z0-9_]{1,64}$ (lowercase letters, digits, underscore)`,
        );
      }
      const d = schemaArg[name];
      const t = isPlainObject(d) ? d.type : undefined;
      if (typeof t !== "string" || t.trim() === "") {
        throw new Error(`attribute_schema ${JSON.stringify(name)} must be an object like {"type": "encrypted_text"}`);
      }
      decl[name] = { type: t };
    }
    if (names.length > 0) {
      const s = compactASCIIJSON(decl);
      if (s.length > attributesHeaderMaxLen) {
        throw new Error(
          `attribute_schema encodes to ${s.length} characters; the X-Attribute-Schema header is capped at ${attributesHeaderMaxLen}. Nothing was ingested and nothing was truncated`,
        );
      }
      attributeSchema = s;
    }
  }

  return { attributes, attributeSchema };
}
