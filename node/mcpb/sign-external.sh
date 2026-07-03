#!/usr/bin/env bash
# Sign a .mcpb bundle with an EXTERNAL key (cloud HSM / PKCS#11 / any key
# openssl can use) instead of `mcpb sign`, which requires a software PEM key.
#
# Replicates mcpb 2.1.2 signature format (dist/node/sign.js):
#   signed file = <original bytes> MCPB_SIG_V1 <uint32-LE sig len> <PKCS#7 DER> MCPB_SIG_END
#   PKCS#7 = detached CMS SignedData, SHA-256, signed attrs (contentType,
#   messageDigest, signingTime), embedding leaf + intermediate certs.
#
# Usage:
#   sign-external.sh <bundle.mcpb> <leaf-cert.pem> <intermediate.pem> <openssl-key-args...>
# Examples:
#   # software key (test):
#   sign-external.sh out.mcpb leaf.pem intermediate.pem -inkey key.pem
#   # PKCS#11 (Certum SimplySign via libp11 provider):
#   sign-external.sh out.mcpb leaf.pem intermediate.pem \
#     -engine pkcs11 -keyform engine -inkey 'pkcs11:token=SimplySign;type=private'
set -euo pipefail

# Engines need a real OpenSSL (macOS ships LibreSSL). Override with $OPENSSL.
OPENSSL="${OPENSSL:-$(command -v /opt/homebrew/opt/openssl@3/bin/openssl || command -v openssl)}"

BUNDLE="$1"; LEAF="$2"; INTER="$3"; shift 3

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# 1. Strip any existing signature block (idempotent re-sign).
npx --yes @anthropic-ai/mcpb unsign "$BUNDLE" >/dev/null 2>&1 || true

# 2. Detached CMS signature over the exact bundle bytes.
"$OPENSSL" cms -sign -binary -md sha256 \
  -signer "$LEAF" -certfile "$INTER" \
  -outform DER -out "$TMP/sig.der" \
  -in "$BUNDLE" "$@"

# 3. Append the MCPB signature block.
node - "$BUNDLE" "$TMP/sig.der" <<'EOF'
const fs = require('fs');
const [bundle, sigPath] = process.argv.slice(2);
const sig = fs.readFileSync(sigPath);
const len = Buffer.alloc(4);
len.writeUInt32LE(sig.length, 0);
fs.appendFileSync(bundle, Buffer.concat([
  Buffer.from('MCPB_SIG_V1', 'utf-8'), len, sig, Buffer.from('MCPB_SIG_END', 'utf-8'),
]));
EOF

# 4. Verify. NOT with `mcpb verify` — that tool is broken as of mcpb 2.1.2
#    (node-forge's p7.verify() is an unimplemented stub that throws; the CLI
#    and Claude Desktop catch it and report "unsigned" for EVERY bundle,
#    including ones signed by `mcpb sign` itself). Instead: cryptographic CMS
#    verification + OS trust-store chain check (same check Desktop runs).
tail -c 200000 "$BUNDLE" | grep -q "MCPB_SIG_V1" || { echo "FAIL: no signature block"; exit 1; }
python3 - "$BUNDLE" "$TMP" <<'EOF'
import sys
data = open(sys.argv[1], 'rb').read()
i = data.rindex(b'MCPB_SIG_V1')
n = int.from_bytes(data[i+11:i+15], 'little')
open(sys.argv[2] + '/content.bin', 'wb').write(data[:i])
open(sys.argv[2] + '/extracted-sig.der', 'wb').write(data[i+15:i+15+n])
EOF
"$OPENSSL" cms -verify -binary -inform DER -in "$TMP/extracted-sig.der" \
  -content "$TMP/content.bin" -noverify -out /dev/null 2>/dev/null \
  && echo "OK: CMS signature valid over bundle content"
cat "$LEAF" "$INTER" > "$TMP/chain.pem"
security verify-cert -c "$TMP/chain.pem" -p codeSign >/dev/null 2>&1 \
  && echo "OK: cert chain trusted by macOS (codeSign purpose)"
