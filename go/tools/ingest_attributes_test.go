package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func rawMap(t *testing.T, js string) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(js), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// ingestCapture records the metadata headers of every /v3/grill/ingest request.
type ingestCapture struct {
	mu       sync.Mutex
	requests []http.Header
}

func (c *ingestCapture) all() []http.Header {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]http.Header(nil), c.requests...)
}

func startIngestStub(t *testing.T) *ingestCapture {
	t.Helper()
	capt := &ingestCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v3/grill/ingest":
			capt.mu.Lock()
			capt.requests = append(capt.requests, r.Header.Clone())
			capt.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"job_id":"job-attr-1"}`))
		case "/v3/projects":
			_, _ = w.Write([]byte(`[]`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("POMA_API_BASE_URL", srv.URL)
	return capt
}

func TestEncodeIngestAttributesHeaderValues(t *testing.T) {
	attrs := rawMap(t, `{"notes":"private","year":2024,"region":"emea","tags":["a","b"],"big":9007199254740992,"ok":true,"city":"Zürich 🍫"}`)
	schema := rawMap(t, `{"notes":{"type":"encrypted_text"}}`)
	a, s, err := encodeIngestAttributes(attrs, schema)
	if err != nil {
		t.Fatal(err)
	}
	// Keys sorted, compact, non-ASCII escaped, big int preserved exactly.
	want := `{"big":9007199254740992,"city":"Z` + "\\u00fcrich \\ud83c\\udf6b" + `","notes":"private","ok":true,"region":"emea","tags":["a","b"],"year":2024}`
	if a != want {
		t.Errorf("X-Attributes = %s\nwant           %s", a, want)
	}
	if s != `{"notes":{"type":"encrypted_text"}}` {
		t.Errorf("X-Attribute-Schema = %s", s)
	}
	// Decodes back to the same values.
	var back map[string]any
	if err := json.Unmarshal([]byte(a), &back); err != nil || back["city"] != "Zürich 🍫" {
		t.Errorf("round trip failed: %v %v", err, back["city"])
	}

	a, s, err = encodeIngestAttributes(nil, nil)
	if err != nil || a != "" || s != "" {
		t.Errorf("empty input must yield no headers, got %q %q %v", a, s, err)
	}
}

func TestEncodeIngestAttributesValidation(t *testing.T) {
	long := strings.Repeat("x", attributesHeaderMaxLen)
	many := map[string]json.RawMessage{}
	for i := 0; i < attributesMaxNames+1; i++ {
		many[fmt.Sprintf("a%03d", i)] = json.RawMessage(`1`)
	}
	tests := []struct {
		name   string
		attrs  map[string]json.RawMessage
		schema map[string]json.RawMessage
		want   string
	}{
		{"uppercase name", rawMap(t, `{"Year":1}`), nil, `attribute name "Year" must match`},
		{"hyphen name", rawMap(t, `{"doc-year":1}`), nil, `attribute name "doc-year" must match`},
		{"name too long", rawMap(t, `{"`+strings.Repeat("a", 65)+`":1}`), nil, "must match"},
		{"object value", rawMap(t, `{"x":{"a":1}}`), nil, `attribute "x": value must be`},
		{"nested array", rawMap(t, `{"x":[[1]]}`), nil, `attribute "x": value must be`},
		{"null in array", rawMap(t, `{"x":[null]}`), nil, `attribute "x": value must be`},
		{"orphan declaration", rawMap(t, `{"region":"emea"}`), rawMap(t, `{"notes":{"type":"encrypted_text"}}`), `attribute_schema declares "notes", but attributes has no value for it`},
		{"schema without attributes", nil, rawMap(t, `{"notes":{"type":"encrypted_text"}}`), `attribute_schema declares "notes", but attributes has no value for it`},
		{"mixed string and int", rawMap(t, `{"x":["a",1]}`), nil, `attribute "x": mixed element types in array [int string]`},
		{"mixed int and float", rawMap(t, `{"x":[1,2.5]}`), nil, `attribute "x": mixed element types in array [float int]`},
		{"mixed int and float literal 2.0", rawMap(t, `{"x":[1,2.0]}`), nil, `mixed element types in array [float int]`},
		{"mixed bool and string", rawMap(t, `{"x":[true,"a"]}`), nil, `mixed element types`},
		{"undeclared empty array", rawMap(t, `{"x":[]}`), nil, `attribute "x": an empty array has no type to infer; declare it in attribute_schema`},
		{"empty array declared scalar", rawMap(t, `{"x":[]}`), rawMap(t, `{"x":{"type":"string"}}`), `attribute "x" is declared "string", which needs a scalar`},
		{"scalar declared array", rawMap(t, `{"x":"a"}`), rawMap(t, `{"x":{"type":"[]string"}}`), `attribute "x" is declared "[]string", which needs an array`},
		{"declared []int with string", rawMap(t, `{"x":[1,"a"]}`), rawMap(t, `{"x":{"type":"[]int"}}`), `attribute "x": element "a" is not a int`},
		{"declared int with float", rawMap(t, `{"x":2.5}`), rawMap(t, `{"x":{"type":"int"}}`), `element 2.5 is not a int`},
		{"declared encrypted_text with number", rawMap(t, `{"x":5}`), rawMap(t, `{"x":{"type":"encrypted_text"}}`), `element 5 is not a encrypted_text`},
		{"int beyond 2^53", rawMap(t, `{"x":9007199254740993}`), nil, `outside the JSON-safe integer range`},
		{"array over 64 elements", rawMap(t, `{"x":[`+strings.TrimSuffix(strings.Repeat("1,", 65), ",")+`]}`), nil, `65 elements exceeds the cap of 64`},
		{"top-level null", rawMap(t, `{"gone":null}`), nil, `attribute "gone": value must be`},
		{"too many names", many, nil, "at most 64"},
		{"attributes over cap", rawMap(t, `{"x":"`+long+`"}`), nil, "capped at 2048"},
		{"schema bad name", nil, rawMap(t, `{"Notes":{"type":"encrypted_text"}}`), `attribute_schema name "Notes"`},
		{"schema missing type", nil, rawMap(t, `{"notes":{}}`), `attribute_schema "notes" must be an object`},
		{"schema not object", nil, rawMap(t, `{"notes":"encrypted_text"}`), `attribute_schema "notes" must be an object`},
		{"schema over cap", rawMap(t, `{"notes":"x"}`), rawMap(t, `{"notes":{"type":"`+long+`"}}`), "X-Attribute-Schema header is capped at 2048"},
	}
	if len(many) != attributesMaxNames+1 {
		t.Fatalf("test setup: %d names", len(many))
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := encodeIngestAttributes(tt.attrs, tt.schema)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want substring %q", err, tt.want)
			}
		})
	}
}

// The cap is inclusive: exactly 2048 characters is accepted, 2049 refused.
func TestEncodeIngestAttributesCapBoundary(t *testing.T) {
	overhead := len(`{"x":""}`)
	at := rawMap(t, `{"x":"`+strings.Repeat("y", attributesHeaderMaxLen-overhead)+`"}`)
	a, _, err := encodeIngestAttributes(at, nil)
	if err != nil || len(a) != attributesHeaderMaxLen {
		t.Fatalf("len=%d err=%v, want exactly %d accepted", len(a), err, attributesHeaderMaxLen)
	}
	over := rawMap(t, `{"x":"`+strings.Repeat("y", attributesHeaderMaxLen-overhead+1)+`"}`)
	if _, _, err := encodeIngestAttributes(over, nil); err == nil {
		t.Fatal("2049 characters must be refused")
	}
	// The cap applies AFTER encoding: 400 × "ü" is 400 characters of input
	// but 2400 of escaped header.
	esc := rawMap(t, `{"x":"`+strings.Repeat("ü", 400)+`"}`)
	if _, _, err := encodeIngestAttributes(esc, nil); err == nil || !strings.Contains(err.Error(), "capped at 2048") {
		t.Fatalf("escaped length must count, err = %v", err)
	}
}

func TestGrillIngestSendsAttributeHeaders(t *testing.T) {
	capt := startIngestStub(t)
	_, out, err := GrillIngest(context.Background(), nil, GrillIngestInput{
		Token:           "tok",
		URL:             "https://example.com/doc.pdf",
		Labels:          map[string]string{"team": "eng"},
		Attributes:      rawMap(t, `{"notes":"private","region":"emea","year":2024}`),
		AttributeSchema: rawMap(t, `{"notes":{"type":"encrypted_text"}}`),
	})
	if err != nil || out.Error != "" {
		t.Fatalf("unexpected error: %v / %s", err, out.Error)
	}
	reqs := capt.all()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(reqs))
	}
	h := reqs[0]
	if got := h.Get("X-Attributes"); got != `{"notes":"private","region":"emea","year":2024}` {
		t.Errorf("X-Attributes = %q", got)
	}
	if got := h.Get("X-Attribute-Schema"); got != `{"notes":{"type":"encrypted_text"}}` {
		t.Errorf("X-Attribute-Schema = %q", got)
	}
	if got := h.Get("X-Labels"); got != "team:eng" {
		t.Errorf("X-Labels = %q, labels must keep working", got)
	}
}

func TestGrillIngestWithoutAttributesSendsNoHeaders(t *testing.T) {
	capt := startIngestStub(t)
	_, out, _ := GrillIngest(context.Background(), nil, GrillIngestInput{Token: "tok", URL: "https://example.com/doc.pdf"})
	if out.Error != "" {
		t.Fatal(out.Error)
	}
	h := capt.all()[0]
	if _, ok := h["X-Attributes"]; ok {
		t.Error("X-Attributes sent without attributes")
	}
	if _, ok := h["X-Attribute-Schema"]; ok {
		t.Error("X-Attribute-Schema sent without attribute_schema")
	}
}

// An invalid attribute set is refused before anything is uploaded.
func TestGrillIngestInvalidAttributesNoRequest(t *testing.T) {
	capt := startIngestStub(t)
	res, out, _ := GrillIngestSync(context.Background(), nil, GrillIngestInput{
		Token:      "tok",
		URL:        "https://example.com/doc.pdf",
		Attributes: rawMap(t, `{"DocYear":2024}`),
	})
	if res == nil || !res.IsError || out.Code != CodeInvalidInput || !strings.Contains(out.Error, "DocYear") {
		t.Fatalf("want invalid_input naming DocYear, got %+v", out.GrillError)
	}
	if n := len(capt.all()); n != 0 {
		t.Fatalf("requests = %d, want 0", n)
	}
}

func TestGrillIngestBatchAttachesAttributesToEveryFile(t *testing.T) {
	capt := startIngestStub(t)
	dir := t.TempDir()
	var paths []string
	for _, n := range []string{"a.txt", "b.txt", "c.txt"} {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, []byte("hello "+n), 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	_, out, err := GrillIngestBatch(context.Background(), nil, GrillIngestBatchInput{
		Token:           "tok",
		FilePaths:       paths,
		Attributes:      rawMap(t, `{"batch":"q3","notes":"private","year":2024}`),
		AttributeSchema: rawMap(t, `{"notes":{"type":"encrypted_text"}}`),
	})
	if err != nil || out.Error != "" {
		t.Fatalf("unexpected error: %v / %s", err, out.Error)
	}
	reqs := capt.all()
	if len(reqs) != 3 || out.SubmittedCount != 3 {
		t.Fatalf("requests = %d submitted = %d, want 3", len(reqs), out.SubmittedCount)
	}
	for i, h := range reqs {
		if got := h.Get("X-Attributes"); got != `{"batch":"q3","notes":"private","year":2024}` {
			t.Errorf("request %d X-Attributes = %q", i, got)
		}
		if got := h.Get("X-Attribute-Schema"); got != `{"notes":{"type":"encrypted_text"}}` {
			t.Errorf("request %d X-Attribute-Schema = %q", i, got)
		}
	}
}

func TestGrillIngestBatchInvalidAttributesUploadsNothing(t *testing.T) {
	capt := startIngestStub(t)
	p := filepath.Join(t.TempDir(), "a.txt")
	_ = os.WriteFile(p, []byte("hi"), 0o600)
	res, out, _ := GrillIngestBatch(context.Background(), nil, GrillIngestBatchInput{
		Token:      "tok",
		FilePaths:  []string{p},
		Attributes: rawMap(t, `{"x":"`+strings.Repeat("y", attributesHeaderMaxLen)+`"}`),
	})
	if res == nil || !res.IsError || out.Code != CodeInvalidInput || !strings.Contains(out.Error, "capped at 2048") {
		t.Fatalf("want invalid_input over cap, got %+v", out.GrillError)
	}
	if n := len(capt.all()); n != 0 {
		t.Fatalf("requests = %d, want 0 — nothing may upload on invalid attributes", n)
	}
	if out.Results == nil {
		t.Error("results must be [] not null")
	}
}

// Values grill accepts must still pass: a declared empty array (the only way
// to store []), float declared over int elements, a uniform float array.
func TestEncodeIngestAttributesAcceptsGrillLegalShapes(t *testing.T) {
	sixtyFour := `[` + strings.TrimSuffix(strings.Repeat("1,", 64), ",") + `]`
	cases := []struct {
		name, attrs, schema, wantAttrs, wantSchema string
	}{
		{"declared empty array", `{"tags":[]}`, `{"tags":{"type":"[]string"}}`, `{"tags":[]}`, `{"tags":{"type":"[]string"}}`},
		{"float declared over ints", `{"w":[1,2.5]}`, `{"w":{"type":"[]float"}}`, `{"w":[1,2.5]}`, `{"w":{"type":"[]float"}}`},
		{"uniform floats", `{"w":[1.0,2.5]}`, ``, `{"w":[1.0,2.5]}`, ``},
		{"int at -2^53", `{"n":-9007199254740992}`, ``, `{"n":-9007199254740992}`, ``},
		{"64 elements", `{"x":` + sixtyFour + `}`, ``, `{"x":` + sixtyFour + `}`, ``},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var schema map[string]json.RawMessage
			if c.schema != "" {
				schema = rawMap(t, c.schema)
			}
			a, s, err := encodeIngestAttributes(rawMap(t, c.attrs), schema)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if a != c.wantAttrs || s != c.wantSchema {
				t.Fatalf("headers = %s / %s, want %s / %s", a, s, c.wantAttrs, c.wantSchema)
			}
		})
	}
}

// A declared empty array reaches the gateway with both headers; an undeclared
// one never leaves the MCP server.
func TestGrillIngestEmptyArrayNeedsDeclaration(t *testing.T) {
	capt := startIngestStub(t)
	res, out, _ := GrillIngest(context.Background(), nil, GrillIngestInput{
		Token: "tok", URL: "https://example.com/doc.pdf",
		Attributes: rawMap(t, `{"tags":[]}`),
	})
	if res == nil || !res.IsError || out.Code != CodeInvalidInput || !strings.Contains(out.Error, "empty array") {
		t.Fatalf("undeclared []: want invalid_input, got %+v", out.GrillError)
	}
	if n := len(capt.all()); n != 0 {
		t.Fatalf("requests = %d after a refused ingest, want 0", n)
	}

	_, out, _ = GrillIngest(context.Background(), nil, GrillIngestInput{
		Token: "tok", URL: "https://example.com/doc.pdf",
		Attributes:      rawMap(t, `{"tags":[]}`),
		AttributeSchema: rawMap(t, `{"tags":{"type":"[]string"}}`),
	})
	if out.Error != "" {
		t.Fatalf("declared []: unexpected error %s", out.Error)
	}
	reqs := capt.all()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(reqs))
	}
	if got := reqs[0].Get("X-Attributes"); got != `{"tags":[]}` {
		t.Errorf("X-Attributes = %q", got)
	}
	if got := reqs[0].Get("X-Attribute-Schema"); got != `{"tags":{"type":"[]string"}}` {
		t.Errorf("X-Attribute-Schema = %q", got)
	}
}
