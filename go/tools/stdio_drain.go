package tools

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// defaultStdioDrainStall bounds how long a stdio session waits, after end of
// input, for the server to answer requests it has already read. The clock
// restarts on every message the server writes, so a slow tool that keeps
// emitting progress notifications is never cut off — only a fully stalled
// handler is.
const defaultStdioDrainStall = 2 * time.Minute

// stdioDrainStall returns the stall bound, overridable with
// GRILL_STDIO_DRAIN_STALL as a Go duration ("30s", "5m"). "0" drains
// immediately at end of input, abandoning unanswered requests.
func stdioDrainStall() time.Duration {
	v := strings.TrimSpace(os.Getenv("GRILL_STDIO_DRAIN_STALL"))
	if v == "" {
		return defaultStdioDrainStall
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		slog.Warn("ignoring invalid GRILL_STDIO_DRAIN_STALL", "value", v, "using", defaultStdioDrainStall)
		return defaultStdioDrainStall
	}
	return d
}

// drainGate couples the stdio reader and writer so that end of input does not
// tear the session down before the server has answered what it already read.
//
// Closing stdin is how an MCP client signals shutdown, and the SDK's jsonrpc2
// connection reacts by rejecting every request still queued. That makes the
// batch shape of -input (a file, or a shell pipe that ends) lose its output
// entirely: the requests are read, then the EOF right behind them cancels them
// before any handler finishes. The gate records JSON-RPC ids on the way in,
// clears them as responses go out, and withholds io.EOF while any are
// outstanding.
//
// Accounting is per physical line, so it requires the NDJSON framing that
// -input documents: one message per line. The SDK's decoder is not
// line-oriented and would also accept a message pretty-printed across several
// lines; the gate records nothing for those, degrading to the unfixed
// behaviour (their responses can still be lost) rather than misbehaving.
type drainGate struct {
	// stall is how long waitDrained tolerates no output at all before giving up.
	stall time.Duration

	mu      sync.Mutex
	pending map[string]struct{}
	gaveUp  bool // the stall backstop fired; do not wait again

	// progress carries one pending wakeup for a waiter in waitDrained, signalling
	// that the server is still producing output.
	progress chan struct{}
}

func newDrainGate(stall time.Duration) *drainGate {
	return &drainGate{
		stall:    stall,
		pending:  make(map[string]struct{}),
		progress: make(chan struct{}, 1),
	}
}

// noteIncoming records the ids of any requests in data.
func (g *drainGate) noteIncoming(data []byte) {
	for _, m := range jsonrpcEnvelopes(data) {
		if m.Method == "" {
			continue // a response, or not a JSON-RPC message at all
		}
		key, ok := jsonrpcIDKey(m.ID)
		if !ok {
			continue // a notification: no response is owed
		}
		g.mu.Lock()
		g.pending[key] = struct{}{}
		g.mu.Unlock()
	}
}

// noteOutgoing clears the ids of any responses in data and reports progress.
func (g *drainGate) noteOutgoing(data []byte) {
	for _, m := range jsonrpcEnvelopes(data) {
		if m.Method != "" {
			continue // a server-initiated request or notification
		}
		if key, ok := jsonrpcIDKey(m.ID); ok {
			g.mu.Lock()
			delete(g.pending, key)
			g.mu.Unlock()
		}
	}
	select {
	case g.progress <- struct{}{}:
	default:
	}
}

// waitDrained blocks until every recorded request has been answered, or until
// the server produces nothing at all for g.stall.
func (g *drainGate) waitDrained() {
	timer := time.NewTimer(g.stall)
	defer timer.Stop()
	for {
		g.mu.Lock()
		n, gaveUp := len(g.pending), g.gaveUp
		g.mu.Unlock()
		if n == 0 || gaveUp {
			return
		}
		select {
		case <-g.progress:
			// Safe without draining timer.C: since Go 1.23, Stop/Reset do not
			// leave a stale tick behind (go.mod requires 1.25).
			timer.Stop()
			timer.Reset(g.stall)
		case <-timer.C:
			g.mu.Lock()
			g.gaveUp = true
			g.mu.Unlock()
			slog.Warn("stdio: end of input with unanswered requests", "pending", n, "stall", g.stall)
			return
		}
	}
}

// jsonrpcEnvelope is the subset of a JSON-RPC message needed to tell a request
// (has both method and id) from a notification (method, no id) and a response
// (id, no method).
type jsonrpcEnvelope struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
}

// jsonrpcEnvelopes decodes one NDJSON payload, which is either a single message
// or a batch array. Undecodable input yields no envelopes: framing is the SDK's
// job, and a parse error here must not hold the gate open.
func jsonrpcEnvelopes(data []byte) []jsonrpcEnvelope {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil
	}
	if data[0] == '[' {
		var batch []jsonrpcEnvelope
		if json.Unmarshal(data, &batch) != nil {
			return nil
		}
		return batch
	}
	var one jsonrpcEnvelope
	if json.Unmarshal(data, &one) != nil {
		return nil
	}
	return []jsonrpcEnvelope{one}
}

// jsonrpcIDKey canonicalizes a JSON-RPC id so that the value seen on the way in
// matches the one seen on the way out. It reports false for a missing or null
// id, which marks a notification.
//
// The number case must coerce exactly as jsonrpc2.MakeID does — int64
// truncation, not the decoded float — or the two disagree on any id that is
// not an integer in int64 range, the key never clears, and the gate waits out
// the whole stall. A client sending id 2.5 gets a response numbered 2.
func jsonrpcIDKey(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return "", false
	}
	switch id := v.(type) {
	case string:
		return "s" + id, true
	case float64:
		return "n" + strconv.FormatInt(int64(id), 10), true
	default:
		return "", false
	}
}

type drainReader struct {
	src  *bufio.Reader
	gate *drainGate
	buf  []byte // remainder of the line last read, not yet handed to the caller
	err  error
}

// Read hands out one line at a time so that every request is recorded before the
// server can see it, and holds back end of input until the gate has drained.
func (r *drainReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		if r.err != nil {
			// Any terminal read condition, not just EOF: stdout is a separate
			// descriptor, so a broken stdin does not mean the responses the
			// server already computed cannot be delivered.
			r.gate.waitDrained()
			return 0, r.err
		}
		line, err := r.src.ReadBytes('\n')
		r.err = err
		if len(line) > 0 {
			r.gate.noteIncoming(line)
			r.buf = line
		}
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

// Close does not close the underlying stream: in stdio mode it is os.Stdin,
// owned by the caller.
func (r *drainReader) Close() error { return nil }

type drainWriter struct {
	dst  io.Writer
	gate *drainGate
}

func (w *drainWriter) Write(p []byte) (int, error) {
	n, err := w.dst.Write(p)
	if n > 0 {
		w.gate.noteOutgoing(p[:n])
	}
	return n, err
}

// Close does not close the underlying stream: in stdio mode it is os.Stdout,
// owned by the caller.
func (w *drainWriter) Close() error { return nil }

// NewDrainingStdio pairs a stdio reader and writer through a drain gate, so the
// server answers every request it read before end of input ends the session.
// Neither underlying stream is closed by the returned values.
func NewDrainingStdio(in io.Reader, out io.Writer) (io.ReadCloser, io.WriteCloser) {
	gate := newDrainGate(stdioDrainStall())
	return &drainReader{src: bufio.NewReader(in), gate: gate},
		&drainWriter{dst: out, gate: gate}
}
