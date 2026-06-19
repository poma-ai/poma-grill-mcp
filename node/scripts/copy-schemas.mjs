// Copy the shared tool schema into dist/ so the published npm package is
// self-contained. Runs after `tsc` as part of `npm run build`.

import { copyFileSync, existsSync, mkdirSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const HERE = dirname(fileURLToPath(import.meta.url));
const NODE_ROOT = resolve(HERE, "..");
const SRC = resolve(NODE_ROOT, "..", "schemas", "tools.json");
const DEST = resolve(NODE_ROOT, "dist", "schemas", "tools.json");

if (!existsSync(SRC)) {
  console.error(`copy-schemas: source not found: ${SRC}`);
  process.exit(1);
}

mkdirSync(dirname(DEST), { recursive: true });
copyFileSync(SRC, DEST);
console.log(`copy-schemas: ${SRC} -> ${DEST}`);
