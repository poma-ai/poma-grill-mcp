#!/usr/bin/env bash
# Sign a .mcpb bundle with an EXTERNAL key (cloud HSM / PKCS#11 / any key
# openssl can use) instead of `mcpb sign`, which requires a software PEM key.
#
# Produces the mcpb signature block (as of mcpb 2.1.2, dist/node/sign.js):
#   MCPB_SIG_V1 <uint32-LE sig len> <PKCS#7 DER> MCPB_SIG_END
#   PKCS#7 = detached CMS SignedData, SHA-256, signed attrs (contentType,
#   messageDigest, signingTime), embedding leaf + intermediate certs.
#
# UNLIKE `mcpb sign`, the block is embedded as a proper ZIP end-of-central-
# directory COMMENT (length field patched) instead of dangling after the
# EOCD. Naive appending — which is what `mcpb sign` itself does — produces
# bundles that strict zip parsers reject; Claude Desktop fails to install
# them with "Invalid comment length. Expected: N. Found: 0" (mcpb #278).
# Marker-based signature extraction (mcpb CLI + Claude Desktop) scans
# backwards from EOF, so it finds the block either way.
#
# Because the EOCD comment-length field is inside the signed byte range,
# signing is two-pass: pass 1 learns the CMS size (constant for a fixed
# cert chain), the length field is patched, pass 2 signs the final bytes.
#
# Usage:
#   sign-external.sh <bundle.mcpb> <leaf-cert.pem> <intermediate.pem> <openssl-key-args...>
# Examples:
#   # software key (test):
#   sign-external.sh out.mcpb leaf.pem intermediate.pem -inkey key.pem
#   # PKCS#11 (Certum SimplySign via libp11 engine):
#   OPENSSL_ENGINES=/opt/homebrew/lib/engines-3 \
#   PKCS11_MODULE_PATH=/usr/local/lib/libSimplySignPKCS.dylib \
#   sign-external.sh out.mcpb leaf.pem intermediate.pem \
#     -engine pkcs11 -keyform engine -inkey 'pkcs11:token=...;type=private'
set -euo pipefail

# Engines need a real OpenSSL (macOS ships LibreSSL). Override with $OPENSSL.
OPENSSL="${OPENSSL:-$(command -v /opt/homebrew/opt/openssl@3/bin/openssl || command -v openssl)}"

BUNDLE="$1"; LEAF="$2"; INTER="$3"; shift 3

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

sign_der() { # $1 = output path, rest = key args; signs current $BUNDLE bytes
  local out="$1"; shift
  "$OPENSSL" cms -sign -binary -md sha256 \
    -signer "$LEAF" -certfile "$INTER" \
    -outform DER -out "$out" \
    -in "$BUNDLE" "$@"
}

# 1. Strip any existing signature block and reset the EOCD comment length
#    (idempotent re-sign; also repairs `mcpb unsign` leaving a stale field).
python3 - "$BUNDLE" <<'EOF'
import struct, sys
p = sys.argv[1]
data = bytearray(open(p, 'rb').read())
i = data.rfind(b'MCPB_SIG_V1')
if i != -1:
    data = data[:i]
eocd = data.rindex(b'PK\x05\x06')
assert len(data) >= eocd + 22, "truncated EOCD"
data = data[:eocd + 22]              # drop any residual comment bytes
struct.pack_into('<H', data, eocd + 20, 0)
open(p, 'wb').write(bytes(data))
EOF

# 2. Pass 1: learn the CMS signature size for this cert chain.
sign_der "$TMP/sig1.der" "$@"
BLOCK_LEN=$(( $(stat -f%z "$TMP/sig1.der" 2>/dev/null || stat -c%s "$TMP/sig1.der") + 27 ))

# 3. Declare the block as the ZIP comment (field is inside the signed bytes).
python3 - "$BUNDLE" "$BLOCK_LEN" <<'EOF'
import struct, sys
p, n = sys.argv[1], int(sys.argv[2])
assert n <= 0xFFFF, "signature block exceeds max ZIP comment length"
data = bytearray(open(p, 'rb').read())
eocd = data.rindex(b'PK\x05\x06')
struct.pack_into('<H', data, eocd + 20, n)
open(p, 'wb').write(bytes(data))
EOF

# 4. Pass 2: sign the final bytes; size must match the declared length.
sign_der "$TMP/sig2.der" "$@"
SIG2_LEN=$(stat -f%z "$TMP/sig2.der" 2>/dev/null || stat -c%s "$TMP/sig2.der")
if [ $(( SIG2_LEN + 27 )) -ne "$BLOCK_LEN" ]; then
  echo "FAIL: pass-2 CMS size changed ($SIG2_LEN + 27 != $BLOCK_LEN)" >&2; exit 1
fi

# 5. Append the signature block (now exactly filling the declared comment).
python3 - "$BUNDLE" "$TMP/sig2.der" <<'EOF'
import struct, sys
p, sp = sys.argv[1], sys.argv[2]
sig = open(sp, 'rb').read()
block = b'MCPB_SIG_V1' + struct.pack('<I', len(sig)) + sig + b'MCPB_SIG_END'
with open(p, 'ab') as f:
    f.write(block)
EOF

# 6. Verify. NOT with `mcpb verify` — that tool is broken as of mcpb 2.1.2
#    (node-forge's p7.verify() is an unimplemented stub that throws; the CLI
#    and Claude Desktop catch it and report "unsigned" for EVERY bundle,
#    including ones signed by `mcpb sign` itself — mcpb #260). Instead:
#    cryptographic CMS verification, OS trust-store chain check, and the
#    strict-zip invariant that Claude Desktop's unzipper enforces.
python3 - "$BUNDLE" "$TMP" <<'EOF'
import struct, sys, zipfile
p, tmp = sys.argv[1], sys.argv[2]
data = open(p, 'rb').read()
i = data.rindex(b'MCPB_SIG_V1')
n = struct.unpack('<I', data[i+11:i+15])[0]
open(tmp + '/content.bin', 'wb').write(data[:i])
open(tmp + '/extracted-sig.der', 'wb').write(data[i+15:i+15+n])
eocd = data.rindex(b'PK\x05\x06')
declared = struct.unpack('<H', data[eocd+20:eocd+22])[0]
actual = len(data) - (eocd + 22)
assert declared == actual, f"FAIL strict-zip: comment declares {declared}, trailing {actual}"
zipfile.ZipFile(p).testzip()   # archive readable with the comment in place
print(f"OK: strict zip — signature block ({actual} B) is the declared EOCD comment")
EOF
"$OPENSSL" cms -verify -binary -inform DER -in "$TMP/extracted-sig.der" \
  -content "$TMP/content.bin" -noverify -out /dev/null 2>/dev/null \
  && echo "OK: CMS signature valid over bundle content"
cat "$LEAF" "$INTER" > "$TMP/chain.pem"
security verify-cert -c "$TMP/chain.pem" -p codeSign >/dev/null 2>&1 \
  && echo "OK: cert chain trusted by macOS (codeSign purpose)"
