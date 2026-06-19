# syntax=docker/dockerfile:1.4
FROM cgr.dev/chainguard/go:latest-dev AS builder

LABEL org.opencontainers.image.authors="r@nice.pink"
LABEL org.opencontainers.image.source="https://github.com/poma-ai/poma-grill-mcp"
LABEL org.opencontainers.image.description="POMA Grill MCP"

WORKDIR /app

# Cache deps (layer before source for better caching)
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# Static binary: CGO_ENABLED=0 for portability.
# -ldflags="-s -w" strips debug symbols (~20-30% smaller).
# -trimpath for reproducible builds.
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w" \
    -trimpath \
    -o poma-grill-mcp .

FROM cgr.dev/chainguard/static:latest AS runner

LABEL org.opencontainers.image.authors="r@nice.pink"
LABEL org.opencontainers.image.source="https://github.com/poma-ai/poma-grill-mcp"
LABEL org.opencontainers.image.description="POMA Grill MCP"

# static includes CA certs for HTTPS (api.poma-ai.com)
USER nonroot:nonroot

COPY --from=builder --chown=nonroot:nonroot /app/poma-grill-mcp /app/poma-grill-mcp

# Stdio mode reads MCP lines from -input (stdin when set to -).
ENTRYPOINT ["/app/poma-grill-mcp", "-input", "-"]
