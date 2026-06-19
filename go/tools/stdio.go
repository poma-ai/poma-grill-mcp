package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func OpenStdioInput(inputPath string) (*os.File, error) {
	if inputPath == "-" {
		return os.Stdin, nil
	}
	return os.Open(inputPath)
}

// stdioListTools checks if input has a tools/list request. Returns (reader, handled).
// If handled, we responded and caller should exit. If not, reader is the full input stream:
// for a pipe, the first line plus any bytes already buffered by the peeking reader (so multi-line MCP works).
// server must be the same instance that will serve subsequent requests — this avoids divergence.
func StdioListTools(in *os.File, server *mcp.Server) (reader io.Reader, handled bool) {
	fi, err := in.Stat()
	if err != nil || (fi.Mode()&os.ModeCharDevice) != 0 {
		return in, false
	}
	br := bufio.NewReader(in)
	line, err := br.ReadBytes('\n')
	if err != nil && err != io.EOF {
		return in, false
	}
	var req struct {
		JSONRPC string `json:"jsonrpc"`
		ID      any    `json:"id"`
		Method  string `json:"method"`
	}
	if err := json.Unmarshal(line, &req); err != nil || req.Method != "tools/list" {
		return io.MultiReader(bytes.NewReader(line), br), false
	}

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		_ = server.Run(ctx, serverTransport)
		close(done)
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "poma-grill-mcp-list", Version: "0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		slog.Error("list-tools connect failed", "err", err)
		return io.MultiReader(bytes.NewReader(line), br), false
	}
	defer session.Close()

	res, err := session.ListTools(ctx, nil)
	if err != nil {
		slog.Error("list-tools failed", "err", err)
		return io.MultiReader(bytes.NewReader(line), br), false
	}

	cancel()
	<-done

	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      req.ID,
		"result":  map[string]any{"tools": res.Tools},
	}
	b, _ := json.Marshal(resp)
	fmt.Println(string(b))
	return nil, true
}
