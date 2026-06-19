package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/poma-ai/poma-grill-mcp/oauth"
	"github.com/poma-ai/poma-grill-mcp/tools"
)

// version is overridden at build time via -ldflags "-X main.version=<tag>".
var version = "dev"

var httpAddr = flag.String("http", "", "if set, serve MCP via HTTP at this address (e.g. :8080) instead of stdio")

// inputPath is required in stdio mode.
var inputPath = flag.String("input", "", "stdio: MCP NDJSON file or stdin \"-\"")

// apiKeyMiddleware extracts an API token from the incoming HTTP request and injects
// it into the request context so tool handlers can use it without requiring the
// POMA_API_KEY environment variable. Precedence: x-api-key header, then
// Authorization: Bearer <token>.
func apiKeyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if key := r.Header.Get("x-api-key"); key != "" {
			r = r.WithContext(tools.WithAPIToken(r.Context(), key))
		} else if auth := r.Header.Get("Authorization"); len(auth) > 7 && auth[:7] == "Bearer " {
			r = r.WithContext(tools.WithAPIToken(r.Context(), auth[7:]))
		}
		next.ServeHTTP(w, r)
	})
}

// loggingMiddleware wraps an http.Handler to log requests.
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		if r.URL.Path == "/health" {
			return
		}
		slog.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"remote", r.RemoteAddr,
			"status", rw.status,
		)
	})
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

func main() {
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "poma-grill-mcp",
		Version: version,
	}, nil)
	tools.Register(server)

	if *httpAddr != "" {
		runHttpMcpServer(server)
		return
	}

	runStdioMcpServer(server)
}

func runStdioMcpServer(server *mcp.Server) {
	if *inputPath == "" {
		slog.Error("stdio mode requires -input <path> (use - for stdin)")
		os.Exit(2)
	}

	inFile, err := tools.OpenStdioInput(*inputPath)
	if err != nil {
		slog.Error("open -input", "path", *inputPath, "err", err)
		os.Exit(1)
	}
	if inFile != os.Stdin {
		defer inFile.Close()
	}

	// Stdio mode: when input is a pipe/file and the first line is tools/list, respond and exit.
	reader, handled := tools.StdioListTools(inFile, server)
	if handled {
		if inFile != os.Stdin {
			_ = inFile.Close()
		}
		os.Exit(0)
	}

	slog.Info("starting poma-mcp server")

	transport := &mcp.IOTransport{
		Reader: io.NopCloser(reader),
		Writer: &nopCloserWriter{os.Stdout},
	}
	if err := server.Run(context.Background(), transport); err != nil {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
}

func runHttpMcpServer(server *mcp.Server) {
	if *inputPath != "" {
		slog.Warn("ignoring -input in HTTP mode")
	}
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	// Always register the RFC 9728 protected-resource well-known endpoint.
	// The authorization server has moved to poma-services-go; the MCP is a thin
	// protected resource pointing IDEs at the api to authorize.
	//
	// POMA_GRILL_ENABLE_OAUTH is no longer read — it is a no-op if set.
	oauth.Register(mux, *httpAddr)

	// When POMA_API_JWT_SECRET is set the deployment uses full OAuth JWT verification.
	// Otherwise fall through to the legacy api-key-only path.
	//
	// apiKeyMiddleware is applied at the server level (loggingMiddleware → apiKeyMiddleware →
	// mux) so that ALL routes — including /ingest-upload and any future additions — have the
	// per-request token injected into context before their handlers run.
	//
	// RequireBearer is layered on top of "/" only (the MCP path) when JWT mode is active.
	// /ingest-upload inherits token injection from apiKeyMiddleware at the server level, and
	// its own handler already enforces that a non-empty token is present.
	if os.Getenv("POMA_API_JWT_SECRET") != "" {
		mux.Handle("/", oauth.RequireBearer(handler))
	} else {
		mux.Handle("/", handler)
	}
	mux.HandleFunc("/ingest-upload", tools.HandleIngestUpload)

	httpServer := &http.Server{Addr: *httpAddr, Handler: loggingMiddleware(apiKeyMiddleware(mux))}

	// Graceful shutdown: wait for SIGINT/SIGTERM, then drain in-flight requests.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		slog.Info("shutting down HTTP server")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			slog.Error("HTTP server shutdown error", "err", err)
		}
	}()

	slog.Info("MCP HTTP server listening", "addr", *httpAddr, "mcp", "/", "ingest_upload", "/ingest-upload")
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("HTTP server error", "err", err)
		os.Exit(1)
	}
}

type nopCloserWriter struct{ io.Writer }

func (nopCloserWriter) Close() error { return nil }
