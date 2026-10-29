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

// ingestCapture records the metadata headers of every /index4ai/v1/ingest request.
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
		case "/index4ai/v1/ingest":
			capt.mu.Lock()
			capt.requests = append(capt.requests, r.Header.Clone())
			capt.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"job_id":"job-attr-1"}`))
		case "/index4ai/v1/projects":
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
	// 30 names of 40 characters: the attributes fit in 2048, the schema
	// declaring each as encrypted_text does not.
	bigAttrs, bigSchema := map[string]json.RawMessage{}, map[string]json.RawMessage{}
	for i := 0; i < 30; i++ {
		n := fmt.Sprintf("n%039d", i)
		bigAttrs[n] = json.RawMessage(`"v"`)
		bigSchema[n] = json.RawMessage(`{"type":"encrypted_text"}`)
	}
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
		// 1.0 reaches grill as 1 after the gateway's float64 round trip.
		{"integral float literal is an int", rawMap(t, `{"x":[1.0,2.5]}`), nil, `mixed element types in array [float int]`},
		{"non-finite number", rawMap(t, `{"x":1e400}`), nil, `attribute "x": 1e400 is not a finite number`},
		{"non-finite in array", rawMap(t, `{"x":[1.5,-1e400]}`), nil, `-1e400 is not a finite number`},
		{"unknown declared type", rawMap(t, `{"x":"a"}`), rawMap(t, `{"x":{"type":"text"}}`), `attribute_schema "x": unknown type "text"; known: []bool, []datetime, []encrypted_text, []float, []int, []string, bool, datetime, encrypted_text, float, int, string`},
		{"unknown array type", rawMap(t, `{"x":[]}`), rawMap(t, `{"x":{"type":"[]text"}}`), `unknown type "[]text"`},
		{"datetime garbage", rawMap(t, `{"d":"not-a-date"}`), rawMap(t, `{"d":{"type":"datetime"}}`), `attribute "d": "not-a-date" is not a valid datetime; use RFC3339 (2024-05-01T12:00:00Z) or YYYY-MM-DD`},
		{"datetime impossible day", rawMap(t, `{"d":"2024-02-30"}`), rawMap(t, `{"d":{"type":"datetime"}}`), `is not a valid datetime; use RFC3339`},
		{"datetime missing timezone", rawMap(t, `{"d":"2024-05-01T12:00:00"}`), rawMap(t, `{"d":{"type":"datetime"}}`), `is not a valid datetime; use RFC3339`},
		{"datetime hour 24", rawMap(t, `{"d":"2024-05-01T24:00:00Z"}`), rawMap(t, `{"d":{"type":"datetime"}}`), `is not a valid datetime; use RFC3339`},
		{"[]datetime with one bad", rawMap(t, `{"d":["2024-01-02","2024-1-2"]}`), rawMap(t, `{"d":{"type":"[]datetime"}}`), `"2024-1-2" is not a valid datetime`},
		{"mixed bool and string", rawMap(t, `{"x":[true,"a"]}`), nil, `mixed element types`},
		{"undeclared empty array", rawMap(t, `{"x":[]}`), nil, `attribute "x": an empty array has no type to infer; declare it in attribute_schema`},
		{"empty array declared scalar", rawMap(t, `{"x":[]}`), rawMap(t, `{"x":{"type":"string"}}`), `attribute "x" is declared "string", which needs a scalar`},
		{"scalar declared array", rawMap(t, `{"x":"a"}`), rawMap(t, `{"x":{"type":"[]string"}}`), `attribute "x" is declared "[]string", which needs an array`},
		{"declared []int with string", rawMap(t, `{"x":[1,"a"]}`), rawMap(t, `{"x":{"type":"[]int"}}`), `attribute "x": element "a" is not a int`},
		{"declared int with float", rawMap(t, `{"x":2.5}`), rawMap(t, `{"x":{"type":"int"}}`), `element 2.5 is not a int`},
		{"declared encrypted_text with number", rawMap(t, `{"x":5}`), rawMap(t, `{"x":{"type":"encrypted_text"}}`), `element 5 is not a encrypted_text`},
		// 2^53+1 rounds to 2^53 in the gateway's float64 and passes grill; 2^53+2 does not.
		{"int beyond 2^53", rawMap(t, `{"x":9007199254740994}`), nil, `outside the JSON-safe integer range`},
		{"array over 64 elements", rawMap(t, `{"x":[`+strings.TrimSuffix(strings.Repeat("1,", 65), ",")+`]}`), nil, `65 elements exceeds the cap of 64`},
		{"top-level null", rawMap(t, `{"gone":null}`), nil, `attribute "gone": value must be`},
		{"too many names", many, nil, "at most 64"},
		{"attributes over cap", rawMap(t, `{"x":"`+long+`"}`), nil, "capped at 2048"},
		{"schema bad name", nil, rawMap(t, `{"Notes":{"type":"encrypted_text"}}`), `attribute_schema name "Notes"`},
		{"schema missing type", nil, rawMap(t, `{"notes":{}}`), `attribute_schema "notes" must be an object`},
		{"schema not object", nil, rawMap(t, `{"notes":"encrypted_text"}`), `attribute_schema "notes" must be an object`},
		{"schema over cap", bigAttrs, bigSchema, "X-Attribute-Schema header is capped at 2048"},
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
	// Legacy labels keep working, but as attributes.labels (D157).
	if got := h.Get("X-Attributes"); got != `{"labels":["team:eng"],"notes":"private","region":"emea","year":2024}` {
		t.Errorf("X-Attributes = %q", got)
	}
	if got := h.Get("X-Attribute-Schema"); got != `{"notes":{"type":"encrypted_text"}}` {
		t.Errorf("X-Attribute-Schema = %q", got)
	}
	if _, ok := h["X-Labels"]; ok {
		t.Errorf("X-Labels = %q, the legacy header must no longer be sent", h.Get("X-Labels"))
	}
}

// Legacy labels merge into an explicit attributes.labels: explicit entries
// first, then the translated ones in sorted-key order, duplicates dropped.
func TestGrillIngestMergesLegacyLabelsIntoAttributes(t *testing.T) {
	capt := startIngestStub(t)
	_, out, err := GrillIngest(context.Background(), nil, GrillIngestInput{
		Token:      "tok",
		URL:        "https://example.com/doc.pdf",
		Labels:     map[string]string{"team": "eng", "b": "2", "a": "1", " ": "skip"},
		Attributes: rawMap(t, `{"labels":["z:9","team:eng"],"year":2024}`),
	})
	if err != nil || out.Error != "" {
		t.Fatalf("unexpected error: %v / %s", err, out.Error)
	}
	h := capt.all()[0]
	if got, want := h.Get("X-Attributes"), `{"labels":["z:9","team:eng","a:1","b:2"],"year":2024}`; got != want {
		t.Errorf("X-Attributes = %s\nwant           %s", got, want)
	}
	if _, ok := h["X-Labels"]; ok {
		t.Error("X-Labels must not be sent")
	}
}

// An attributes.labels that is not an array of strings cannot take the legacy
// labels: refused, never silently dropped. An attributes.labels that is
// already invalid keeps its usual validation error.
func TestGrillIngestLegacyLabelsConflict(t *testing.T) {
	cases := []struct{ attrs, want string }{
		{`{"labels":"team:eng"}`, "must be an array of strings"},
		{`{"labels":[1,2]}`, "must be an array of strings"},
		{`{"labels":null}`, "null is not accepted"},
		// A null element must never become "" (json into []string would).
		{`{"labels":[null]}`, "null is not accepted"},
		{`{"labels":["x",null]}`, "null is not accepted"},
		{`{"labels":[]}`, ""}, // empty explicit array merges fine
	}
	for _, c := range cases {
		capt := startIngestStub(t)
		_, out, _ := GrillIngest(context.Background(), nil, GrillIngestInput{
			Token:      "tok",
			URL:        "https://example.com/doc.pdf",
			Labels:     map[string]string{"team": "eng"},
			Attributes: rawMap(t, c.attrs),
		})
		if c.want == "" {
			if out.Error != "" {
				t.Errorf("%s: unexpected error %s", c.attrs, out.Error)
			} else if got := capt.all()[0].Get("X-Attributes"); got != `{"labels":["team:eng"]}` {
				t.Errorf("%s: X-Attributes = %q", c.attrs, got)
			}
			continue
		}
		if out.Code != CodeInvalidInput || !strings.Contains(out.Error, c.want) {
			t.Errorf("%s: code=%s error=%q, want invalid_input containing %q", c.attrs, out.Code, out.Error, c.want)
		}
		if n := len(capt.all()); n != 0 {
			t.Errorf("%s: %d ingest requests sent, want 0", c.attrs, n)
		}
	}
}

// Attribute-rule errors raised with translated labels in play name the legacy
// source; without legacy labels the message is unchanged.
func TestEncodeIngestMetaNamesLegacyLabels(t *testing.T) {
	many := make(map[string]string, 65)
	for i := 0; i < 65; i++ {
		many[fmt.Sprintf("k%02d", i)] = "v"
	}
	cases := []struct {
		name   string
		labels map[string]string
		attrs  string
		schema string
		want   string
		n      int
	}{
		{"element cap", many, `{}`, ``, "exceeds the cap of 64", 65},
		{"size cap", map[string]string{"k": strings.Repeat("v", 2100)}, `{}`, ``, "capped at 2048", 1},
		{"schema type conflict", map[string]string{"team": "eng"}, `{}`, `{"labels":{"type":"string"}}`, `declared "string"`, 1},
		{"merge conflict", map[string]string{"team": "eng", "a": "1"}, `{"labels":"x"}`, ``, "must be an array of strings", 2},
		{"dedup counts only added", map[string]string{"team": "eng", "a": "1"}, `{"labels":["team:eng"]}`, `{"labels":{"type":"string"}}`, `declared "string"`, 1},
	}
	for _, c := range cases {
		var schema map[string]json.RawMessage
		if c.schema != "" {
			schema = rawMap(t, c.schema)
		}
		_, err := encodeIngestMeta(labelItemsFromMap(c.labels), labelsSourceArg, rawMap(t, c.attrs), schema)
		hint := fmt.Sprintf("(includes %d entries translated from the legacy `labels` argument)", c.n)
		if err == nil || !strings.Contains(err.Error(), c.want) || !strings.HasSuffix(err.Error(), hint) {
			t.Errorf("%s: err = %v, want %q ending %q", c.name, err, c.want, hint)
		}
	}
	// No legacy labels: no hint.
	_, err := encodeIngestMeta(nil, labelsSourceArg, rawMap(t, `{"labels":["x"]}`), rawMap(t, `{"labels":{"type":"string"}}`))
	if err == nil || strings.Contains(err.Error(), "legacy") {
		t.Errorf("err = %v, want an error without the legacy hint", err)
	}
}

// Without legacy labels an attributes.labels of any valid shape passes through
// untouched (no merge happens).
func TestEncodeIngestMetaNoLabelsLeavesAttributesAlone(t *testing.T) {
	h, err := encodeIngestMeta(nil, labelsSourceArg, rawMap(t, `{"labels":"plain"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if h.Attributes != `{"labels":"plain"}` {
		t.Errorf("Attributes = %q", h.Attributes)
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
		{"uniform floats", `{"w":[1.5,2.5]}`, ``, `{"w":[1.5,2.5]}`, ``},
		{"integral floats are ints", `{"w":[1.0,2]}`, ``, `{"w":[1.0,2]}`, ``},
		{"float intent kept by declaring []float", `{"w":[1.0,2.5]}`, `{"w":{"type":"[]float"}}`, `{"w":[1.0,2.5]}`, `{"w":{"type":"[]float"}}`},
		{"int at 2^53+1 rounds to 2^53", `{"n":9007199254740993}`, ``, `{"n":9007199254740993}`, ``},
		{"RFC3339 variants and plain dates", `{"d":["2024-05-01","2024-05-01T12:00:00Z","2024-05-01T12:00:00.123456Z","2024-05-01T12:00:00+05:30","2024-05-01T12:00:00.5-08:00"]}`, `{"d":{"type":"[]datetime"}}`, `{"d":["2024-05-01","2024-05-01T12:00:00Z","2024-05-01T12:00:00.123456Z","2024-05-01T12:00:00+05:30","2024-05-01T12:00:00.5-08:00"]}`, `{"d":{"type":"[]datetime"}}`},
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
