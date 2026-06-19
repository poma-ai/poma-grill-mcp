#!/usr/bin/env bash
# Build the poma-grill-mcp .mcpb bundle from the Node variant.
#
# Bundle layout (entry_point = server/index.js):
#   <root>/manifest.json
#   <root>/package.json        — runtime deps + version (index.js reads ../package.json)
#   <root>/node_modules/       — production deps only (@modelcontextprotocol/sdk)
#   <root>/server/*.js         — compiled output from `npm run build`
#
# Output: node/mcpb/poma-grill-mcp.mcpb
set -euo pipefail

MCPB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NODE_DIR="$(cd "$MCPB_DIR/.." && pwd)"
STAGE="$MCPB_DIR/build"
OUT="$MCPB_DIR/poma-grill-mcp.mcpb"

# 1. Keep manifest version in lockstep with package.json (MCPB does not sync them).
pkg_ver="$(node -p "require('$NODE_DIR/package.json').version")"
man_ver="$(node -p "require('$MCPB_DIR/manifest.json').version")"
if [ "$pkg_ver" != "$man_ver" ]; then
  echo "ERROR: version drift — package.json=$pkg_ver but manifest.json=$man_ver" >&2
  echo "       update node/mcpb/manifest.json 'version' to match." >&2
  exit 1
fi
echo "==> version $pkg_ver"

# 2. Compile the TypeScript -> dist/.
echo "==> npm run build"
( cd "$NODE_DIR" && npm run build )

# 3. Fresh staging dir.
rm -rf "$STAGE"
mkdir -p "$STAGE/server"

# 4. Assemble the bundle tree.
cp "$MCPB_DIR/manifest.json" "$STAGE/manifest.json"
cp "$NODE_DIR/package.json" "$STAGE/package.json"
cp "$NODE_DIR/package-lock.json" "$STAGE/package-lock.json"
cp -R "$NODE_DIR/dist/." "$STAGE/server/"

# 5. Production-only deps at the bundle root (resolved by node from server/index.js).
echo "==> npm ci --omit=dev"
( cd "$STAGE" && npm ci --omit=dev --ignore-scripts )
# package-lock is only needed for the install; don't ship it.
rm -f "$STAGE/package-lock.json"

# 6. Validate + pack.
echo "==> mcpb validate"
npx --yes @anthropic-ai/mcpb validate "$STAGE/manifest.json"
echo "==> mcpb pack"
npx --yes @anthropic-ai/mcpb pack "$STAGE" "$OUT"

echo "==> built $OUT"
