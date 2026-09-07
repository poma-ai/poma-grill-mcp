package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// bigFloatID is 12345678901234567890 as a float64, in a var so the int64
// conversion happens at run time the way the SDK's does rather than being
// rejected as a constant overflow.
var bigFloatID = 12345678901234567890.0

func TestJSONRPCIDKey(t *testing.T) {
	tests := []struct {
		raw     string
		want    string
		wantsID bool
	}{
		{`2`, "n2", true},
		{`2.0`, "n2", true}, // same request, however the peer chose to encode it
		// The SDK coerces a JSON number id with int64 truncation
		// (jsonrpc2.MakeID), and answers 2.5 with id 2. Keying on the decoded
		// float instead would never match that response, and the gate would
		// wait out the whole stall having already emitted it.
		{`2.5`, "n2", true},
		{`-2.5`, "n-2", true},
		// Past 2^63 the SDK's int64(float64) saturates; match it rather than
		// inventing a key of our own.
		{`12345678901234567890`, "n" + strconv.FormatInt(int64(bigFloatID), 10), true},
		{`"str-id"`, "sstr-id", true},
		{`null`, "", false},
		{``, "", false},
		{`{}`, "", false},
	}
	for _, tt := range tests {
		got, ok := jsonrpcIDKey(json.RawMessage(tt.raw))
		if ok != tt.wantsID || got != tt.want {
			t.Errorf("jsonrpcIDKey(%q) = (%q, %v), want (%q, %v)", tt.raw, got, ok, tt.want, tt.wantsID)
		}
	}
}

func TestDrainGatePendingAccounting(t *testing.T) {
	g := newDrainGate(time.Second)

	g.noteIncoming([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	if n := len(g.pending); n != 0 {
		t.Fatalf("notification recorded as pending: %d", n)
	}

	g.noteIncoming([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	g.noteIncoming([]byte(`[{"jsonrpc":"2.0","id":"a","method":"tools/list"},{"jsonrpc":"2.0","method":"notifications/x"}]`))
	if n := len(g.pending); n != 2 {
		t.Fatalf("pending after 2 requests = %d, want 2", n)
	}

	// A progress notification is not a response and must not clear anything.
	g.noteOutgoing([]byte(`{"jsonrpc":"2.0","method":"notifications/progress","params":{}}`))
	if n := len(g.pending); n != 2 {
		t.Fatalf("progress notification cleared pending: %d, want 2", n)
	}

	g.noteOutgoing([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	g.noteOutgoing([]byte(`{"jsonrpc":"2.0","id":"a","error":{"code":-1,"message":"x"}}`))
	if n := len(g.pending); n != 0 {
		t.Fatalf("pending after both answered = %d, want 0", n)
	}
}

// TestDrainReaderWithholdsEOF is the core regression: the reader must not report
// end of input while a request it handed over is still unanswered.
func TestDrainReaderWithholdsEOF(t *testing.T) {
	gate := newDrainGate(5 * time.Second)
	r := &drainReader{
		src:  bufio.NewReader(strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/list\"}\n")),
		gate: gate,
	}

	// Consume exactly the request line. Reading past it is what triggers the EOF
	// path, and that must not happen before the response exists.
	if _, err := r.Read(make([]byte, 1<<16)); err != nil {
		t.Fatalf("reading the request line: %v", err)
	}
	if n := len(g0Pending(gate)); n != 1 {
		t.Fatalf("pending after reading one request = %d, want 1", n)
	}

	eof := make(chan error, 1)
	go func() {
		_, err := r.Read(make([]byte, 8))
		eof <- err
	}()

	select {
	case err := <-eof:
		t.Fatalf("EOF reported while request 1 was unanswered (err=%v)", err)
	case <-time.After(150 * time.Millisecond):
	}

	gate.noteOutgoing([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))

	select {
	case err := <-eof:
		if err != io.EOF {
			t.Fatalf("Read after drain = %v, want io.EOF", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("EOF never reported after the response was written")
	}
}

// TestDrainReaderEOFAfterStall proves the backstop: a request the server never
// answers releases end of input rather than hanging the process.
func TestDrainReaderEOFAfterStall(t *testing.T) {
	gate := newDrainGate(50 * time.Millisecond)
	r := &drainReader{
		src:  bufio.NewReader(strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/list\"}\n")),
		gate: gate,
	}
	if _, err := r.Read(make([]byte, 1<<16)); err != nil {
		t.Fatalf("reading the request line: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := r.Read(make([]byte, 8))
		done <- err
	}()
	select {
	case err := <-done:
		if err != io.EOF {
			t.Fatalf("Read after stall = %v, want io.EOF", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stall backstop never fired")
	}

	// Having given up once, a further read must return immediately rather than
	// wait out another full stall.
	start := time.Now()
	if _, err := r.Read(make([]byte, 8)); err != io.EOF {
		t.Fatalf("second Read after stall = %v, want io.EOF", err)
	}
	if waited := time.Since(start); waited > 25*time.Millisecond {
		t.Errorf("second Read waited %v; the stall backstop was re-armed", waited)
	}
}

// g0Pending reports the gate's outstanding ids, for tests only.
func g0Pending(g *drainGate) map[string]struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]struct{}, len(g.pending))
	for k := range g.pending {
		out[k] = struct{}{}
	}
	return out
}

// syncBuffer is an io.Writer safe for the server's write goroutine to share with
// the test goroutine reading what it produced.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestDrainingStdioServesBatchedInput drives a real mcp.Server over the
// draining transport with input that ends immediately, the shape that
// previously produced no output at all.
func TestDrainingStdioServesBatchedInput(t *testing.T) {
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":"two","method":"tools/list"}`,
		"",
	}, "\n")

	var out syncBuffer
	in, w := NewDrainingStdio(strings.NewReader(input), &out)

	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "noop"},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return nil, nil, nil
		})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := server.Run(ctx, &mcp.IOTransport{Reader: in, Writer: w}); err != nil {
		t.Fatalf("server.Run: %v", err)
	}

	got := map[string]bool{}
	for line := range strings.SplitSeq(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var resp struct {
			ID     json.RawMessage `json:"id"`
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			t.Fatalf("unmarshal response %q: %v", line, err)
		}
		if len(resp.Result) == 0 {
			t.Errorf("response without result: %s", line)
		}
		if key, ok := jsonrpcIDKey(resp.ID); ok {
			got[key] = true
		}
	}
	for _, want := range []string{"n1", "stwo"} {
		if !got[want] {
			t.Errorf("no response for request %s; output was:\n%s", want, out.String())
		}
	}
}

// TestDrainingStdioReleasesOnCoercedID is the F14 regression: an id the SDK
// coerces (a fraction, or past int64 range) must still clear the gate, or the
// session hangs for the whole stall after having written every response.
func TestDrainingStdioReleasesOnCoercedID(t *testing.T) {
	for _, id := range []string{"2", "2.5", "-2.5", "12345678901234567890"} {
		t.Run(id, func(t *testing.T) {
			t.Setenv("GRILL_STDIO_DRAIN_STALL", "30s") // long enough that a wedge fails the test
			input := `{"jsonrpc":"2.0","id":` + id + `,"method":"tools/list"}` + "\n"

			var out syncBuffer
			in, w := NewDrainingStdio(strings.NewReader(input), &out)
			server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
			mcp.AddTool(server, &mcp.Tool{Name: "noop"},
				func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
					return nil, nil, nil
				})

			done := make(chan error, 1)
			go func() {
				done <- server.Run(context.Background(), &mcp.IOTransport{Reader: in, Writer: w})
			}()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("server.Run: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("session did not end: the gate never matched the response to id %s\noutput was:\n%s", id, out.String())
			}
			// The point is that the response's id canonicalizes to the same key
			// the gate recorded on the way in, whatever the SDK coerced it to.
			// A response at all (result or error) proves the pairing held.
			want, ok := jsonrpcIDKey(json.RawMessage(id))
			if !ok {
				t.Fatalf("test id %s is not a correlatable id", id)
			}
			var resp struct {
				ID json.RawMessage `json:"id"`
			}
			line := strings.TrimSpace(out.String())
			if err := json.Unmarshal([]byte(line), &resp); err != nil {
				t.Fatalf("unmarshal response %q: %v", line, err)
			}
			got, ok := jsonrpcIDKey(resp.ID)
			if !ok || got != want {
				t.Errorf("response id key = %q, gate recorded %q; response was:\n%s", got, want, line)
			}
		})
	}
}

func TestStdioDrainStallOverride(t *testing.T) {
	tests := []struct {
		env  string
		want time.Duration
	}{
		{"", defaultStdioDrainStall},
		{"30s", 30 * time.Second},
		{"0", 0},
		{"5m", 5 * time.Minute},
		{"nonsense", defaultStdioDrainStall},
		{"-1s", defaultStdioDrainStall},
	}
	for _, tt := range tests {
		t.Setenv("GRILL_STDIO_DRAIN_STALL", tt.env)
		if got := stdioDrainStall(); got != tt.want {
			t.Errorf("stdioDrainStall() with %q = %v, want %v", tt.env, got, tt.want)
		}
	}
}

// TestDrainReaderDrainsOnHardError covers F16: stdout is a separate descriptor,
// so a broken stdin must not abandon responses the server already computed.
func TestDrainReaderDrainsOnHardError(t *testing.T) {
	gate := newDrainGate(3 * time.Second)
	r := &drainReader{
		src:  bufio.NewReader(&errAfterLine{line: []byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/list\"}\n")}),
		gate: gate,
	}
	if _, err := r.Read(make([]byte, 1<<16)); err != nil {
		t.Fatalf("reading the request line: %v", err)
	}

	got := make(chan error, 1)
	go func() {
		_, err := r.Read(make([]byte, 8))
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("hard read error reported while request 1 was unanswered (err=%v)", err)
	case <-time.After(150 * time.Millisecond):
	}

	gate.noteOutgoing([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	select {
	case err := <-got:
		if err == nil || err == io.EOF {
			t.Fatalf("Read = %v, want the underlying hard error", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("hard read error never surfaced after the response was written")
	}
}

// errAfterLine yields one line, then fails with something other than io.EOF.
type errAfterLine struct {
	line []byte
	done bool
}

func (e *errAfterLine) Read(p []byte) (int, error) {
	if e.done {
		return 0, errors.New("input/output error")
	}
	e.done = true
	n := copy(p, e.line)
	return n, nil
}
