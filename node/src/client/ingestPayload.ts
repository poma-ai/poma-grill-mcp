import { readFileSync, statSync, realpathSync } from "node:fs";
import { basename, isAbsolute, resolve, sep } from "node:path";

const DEFAULT_INGEST_MAX_BYTES = 512 * 1024 * 1024; // 512 MiB

export interface IngestInput {
  file_base64?: string;
  file_path?: string;
  filename?: string;
}

export interface ResolvedPayload {
  data: Uint8Array;
  filename: string;
}

// 0 means unlimited; unset uses the default.
export function ingestMaxBytes(): number {
  const raw = (process.env.GRILL_INGEST_MAX_BYTES ?? "").trim();
  if (raw === "") return DEFAULT_INGEST_MAX_BYTES;
  const n = Number.parseInt(raw, 10);
  if (!Number.isFinite(n) || n < 0) return DEFAULT_INGEST_MAX_BYTES;
  return n;
}

function isUnderAllowedPrefix(targetAbs: string, prefixAbs: string): boolean {
  const rp = realpathSync(targetAbs);
  const rd = realpathSync(prefixAbs);
  if (rp === rd) return true;
  return rp.startsWith(rd + sep);
}

function readFileForIngest(path: string): Uint8Array {
  const trimmed = path.trim();
  if (trimmed === "" || trimmed === ".") {
    throw new Error("file_path is empty");
  }
  let abs = trimmed;
  if (!isAbsolute(abs)) {
    abs = resolve(process.cwd(), abs);
  }

  const prefix = (process.env.GRILL_INGEST_ALLOWED_PREFIX ?? "").trim();
  if (prefix !== "") {
    const prefixAbs = isAbsolute(prefix) ? prefix : resolve(process.cwd(), prefix);
    let allowed = false;
    try {
      allowed = isUnderAllowedPrefix(abs, prefixAbs);
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err);
      throw new Error(`file_path allowlist check: ${msg}`);
    }
    if (!allowed) {
      throw new Error("file_path must be under GRILL_INGEST_ALLOWED_PREFIX");
    }
  }

  const stat = statSync(abs);
  if (!stat.isFile()) {
    throw new Error("file_path must be a regular file");
  }

  const max = ingestMaxBytes();
  if (max > 0 && stat.size > max) {
    throw new Error(`file size ${stat.size} exceeds GRILL_INGEST_MAX_BYTES (${max})`);
  }

  return new Uint8Array(readFileSync(abs));
}

export function resolveIngestPayload(input: IngestInput): ResolvedPayload {
  const b64 = (input.file_base64 ?? "").trim();
  const fpath = (input.file_path ?? "").trim();

  if (b64 !== "" && fpath !== "") {
    throw new Error("provide only one of file_base64 or file_path");
  }
  if (b64 === "" && fpath === "") {
    throw new Error("one of file_base64 or file_path is required");
  }

  const max = ingestMaxBytes();
  let data: Uint8Array;

  if (b64 !== "") {
    if (max > 0) {
      // Upper-bound estimate before decoding.
      const est = Math.ceil((b64.length * 3) / 4);
      if (est > max) {
        throw new Error(`file_base64 decodes to more than GRILL_INGEST_MAX_BYTES (${max})`);
      }
    }
    let decoded: Buffer;
    try {
      decoded = Buffer.from(b64, "base64");
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err);
      throw new Error(`invalid file_base64: ${msg}`);
    }
    if (decoded.length === 0) {
      throw new Error("decoded file_base64 is empty");
    }
    if (max > 0 && decoded.length > max) {
      throw new Error(`decoded file exceeds GRILL_INGEST_MAX_BYTES (${max})`);
    }
    data = new Uint8Array(decoded);
  } else {
    try {
      data = readFileForIngest(fpath);
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err);
      throw new Error(`read file_path: ${msg}`);
    }
    if (data.length === 0) {
      throw new Error("file at file_path is empty");
    }
  }

  let filename = (input.filename ?? "").trim();
  if (filename === "" && fpath !== "") {
    filename = basename(fpath.trim());
  }
  if (filename === "" || filename === "." || filename === "..") {
    const ext = guessExtensionFromContent(data);
    if (ext === "") {
      throw new Error("could not infer file extension: provide filename");
    }
    filename = `upload${ext}`;
  } else {
    filename = basename(filename);
  }

  return { data, filename };
}

// Lightweight magic-byte sniffer for common file types — mirrors Go's
// http.DetectContentType + mime.ExtensionsByType for the subset that the
// POMA Grill currently accepts. Returns extension with leading dot, or "".
function guessExtensionFromContent(data: Uint8Array): string {
  if (data.length === 0) return "";
  // PDF: %PDF-
  if (data.length >= 5 && data[0] === 0x25 && data[1] === 0x50 && data[2] === 0x44 && data[3] === 0x46 && data[4] === 0x2d) return ".pdf";
  // PNG: 89 50 4E 47 0D 0A 1A 0A
  if (data.length >= 8 && data[0] === 0x89 && data[1] === 0x50 && data[2] === 0x4e && data[3] === 0x47) return ".png";
  // JPEG: FF D8 FF
  if (data.length >= 3 && data[0] === 0xff && data[1] === 0xd8 && data[2] === 0xff) return ".jpg";
  // GIF: GIF8
  if (data.length >= 4 && data[0] === 0x47 && data[1] === 0x49 && data[2] === 0x46 && data[3] === 0x38) return ".gif";
  // ZIP / DOCX / XLSX / PPTX (all start with PK)
  if (data.length >= 2 && data[0] === 0x50 && data[1] === 0x4b) return ".zip";
  return "";
}
