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
  // null is refused: grill rejects it on every ingest path; it only means
  // "remove" on the metadata-patch endpoint, which MCP does not use.
  if (isScalar(v)) return true;
  return Array.isArray(v) && v.every(isScalar);
}

// Compact JSON with top-level keys sorted and every non-ASCII UTF-16 code unit
// escaped as \uXXXX (lowercase hex, surrogate pairs above the BMP) — plain
// ASCII, safe as an HTTP header value, matching the Go encoder.
function compactASCIIJSON(obj: Record<string, unknown>): string {
  // Null-prototype object: a name like "__proto__" passes the name regex, and on
  // a plain object the assignment would set the prototype and drop the attribute.
  const sorted: Record<string, unknown> = Object.create(null) as Record<string, unknown>;
  for (const k of Object.keys(obj).sort()) sorted[k] = obj[k];
  return JSON.stringify(sorted).replace(
    /[\u0080-\uffff]/g,
    (c) => "\\u" + c.charCodeAt(0).toString(16).padStart(4, "0"),
  );
}

// Grill's per-value rules (poma-grill attribute_schema.py: infer_type,
// resolve_types, check_value_limits / _check_element), applied to the value
// exactly as it goes on the wire. Mirrors go/tools/ingest_attributes.go.
const attributesMaxArrayElems = 64; // grill ATTR_MAX_ARRAY_ELEMS default (config.toml attr_max_array_elems)
const attributesJSONSafeInt = 2 ** 53; // grill _JSON_SAFE_INT: |int| must be <= 2^53

// The kind grill infers for one scalar. A number is a float when its JSON
// literal — what JSON.stringify sends — has a '.', 'e' or 'E' (Python reads
// 2.5 and 1e+21 as float, 2 as int).
function attributeKind(v: unknown): string {
  if (typeof v === "string") return "string";
  if (typeof v === "boolean") return "bool";
  if (typeof v === "number") return /[.eE]/.test(JSON.stringify(v)) ? "float" : "int";
  return "";
}

function checkAttributeValue(name: string, v: unknown, declared: string | undefined): void {
  const q = JSON.stringify(name);
  const isArr = Array.isArray(v);
  const elems: unknown[] = isArr ? (v as unknown[]) : [v];
  let base: string;
  if (declared === undefined) {
    // Undeclared: grill infers the type from the value.
    if (isArr) {
      if (elems.length === 0) {
        throw new Error(
          `attribute ${q}: an empty array has no type to infer; declare it in attribute_schema with an array type (e.g. {${q}: {"type": "[]string"}}) or leave the attribute out`,
        );
      }
      const kinds = [...new Set(elems.map(attributeKind))].sort();
      if (kinds.length > 1) {
        throw new Error(
          `attribute ${q}: mixed element types in array [${kinds.join(" ")}]; every element must be the same kind (string, int, float or bool — int and float count as different kinds)`,
        );
      }
    }
    base = attributeKind(elems[0]);
  } else {
    // Declared: the value's shape must match the declared type.
    const wantArr = declared.startsWith("[]");
    if (isArr !== wantArr) {
      throw new Error(`attribute ${q} is declared ${JSON.stringify(declared)}, which needs ${wantArr ? "an array" : "a scalar"}`);
    }
    base = wantArr ? declared.slice(2) : declared;
    for (const e of elems) {
      const k = attributeKind(e);
      let ok = true;
      if (base === "bool" || base === "int" || base === "string") ok = k === base;
      else if (base === "float") ok = k === "int" || k === "float";
      else if (base === "datetime" || base === "encrypted_text") ok = k === "string";
      if (!ok) {
        throw new Error(`attribute ${q}: element ${JSON.stringify(e)} is not a ${base} (declared ${JSON.stringify(declared)})`);
      }
    }
  }
  if (isArr && elems.length > attributesMaxArrayElems) {
    throw new Error(`attribute ${q}: ${elems.length} elements exceeds the cap of ${attributesMaxArrayElems}`);
  }
  if (base === "int") {
    for (const e of elems) {
      if (typeof e === "number" && Math.abs(e) > attributesJSONSafeInt) {
        throw new Error(`attribute ${q}: ${JSON.stringify(e)} is outside the JSON-safe integer range (±2^53)`);
      }
    }
  }
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

  // Declarations first: the value checks depend on them (a declared type
  // decides array-vs-scalar and element kinds; only a declared array type
  // makes an empty array legal).
  const declTypes = new Map<string, string>();
  if (schemaArg !== undefined && schemaArg !== null) {
    if (!isPlainObject(schemaArg)) {
      throw new Error('attribute_schema must be an object like {"name": {"type": "encrypted_text"}}');
    }
    for (const name of Object.keys(schemaArg).sort()) {
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
      declTypes.set(name, t);
    }
  }

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
          `attribute ${JSON.stringify(name)}: value must be a string, number, boolean, or an array of those (null is not accepted at ingest)`,
        );
      }
      checkAttributeValue(name, attrsArg[name], declTypes.get(name));
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

  if (declTypes.size > 0) {
    const decl: Record<string, unknown> = Object.create(null) as Record<string, unknown>;
    for (const [name, t] of [...declTypes.entries()].sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))) {
      // Grill rejects a declaration for a name this document does not carry,
      // so an orphan must fail here, not after a 201.
      if (!isPlainObject(attrsArg) || !Object.prototype.hasOwnProperty.call(attrsArg, name)) {
        throw new Error(
          `attribute_schema declares ${JSON.stringify(name)}, but attributes has no value for it; attribute_schema only declares types for names present in attributes`,
        );
      }
      decl[name] = { type: t };
    }
    const s = compactASCIIJSON(decl);
    if (s.length > attributesHeaderMaxLen) {
      throw new Error(
        `attribute_schema encodes to ${s.length} characters; the X-Attribute-Schema header is capped at ${attributesHeaderMaxLen}. Nothing was ingested and nothing was truncated`,
      );
    }
    attributeSchema = s;
  }

  return { attributes, attributeSchema };
}
