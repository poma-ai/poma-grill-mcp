package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
)

// Typed document attributes on ingest. The gateway (poma-services-go
// internal/services/jobs/api.go) takes them on POST /grill/ingest as two
// headers — grill ingest is octet-stream only, so a header is the ONLY
// carrier — and refuses a header over MaxAttributesHeaderLength (2048), a name
// outside ^[a-z0-9_]{1,64}$, or more than 64 names. The same rules are checked
// here so the agent gets a precise invalid_input instead of a gateway 400, and
// the input is never truncated to fit.
const (
	attributesHeaderMaxLen = 2048
	attributesMaxNames     = 64
)

var attributeNameRe = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// ingestAttributesDescription and ingestAttributeSchemaDescription are shared
// verbatim with the Node schema (schemas/tools.json). Keep byte-identical.
const ingestAttributesDescription = "Optional typed document attributes: a flat object of name → value, where a value is a string, number, boolean, or an array of those (never null), e.g. {\"region\":\"emea\",\"year\":2024}. Call grill_attributes first and reuse an existing name and type where one fits. Names must match ^[a-z0-9_]{1,64}$ and are permanent per project, counting against max_names; string, number and boolean values (and arrays of them) are typed by their value — declare encrypted_text or datetime in attribute_schema. Filter on them later with grill_search attribute_filters. Sent as the X-Attributes header: at most 64 names and 2048 characters of compact JSON; larger input is refused with invalid_input, never truncated."

const ingestAttributeSchemaDescription = "Optional type declarations, only for names present in attributes (a declaration for any other name is refused), shaped {\"name\": {\"type\": \"<POMA type>\"}}, e.g. {\"case_notes\":{\"type\":\"encrypted_text\"}}. Declare only where the value cannot say the type: encrypted_text must ALWAYS be declared (an undeclared new name is stored as a plain string), and datetime when introducing a new datetime name. A declaration that conflicts with the type already in force for that name is rejected. Sent as the X-Attribute-Schema header, same 2048-character cap."

// Legacy labels: kept working unchanged, but steered toward attributes.
const ingestLabelsDescription = "Legacy — being retired in favour of attributes; prefer attributes for new work. Optional key:value labels to attach to the ingested document, e.g. {\"team\":\"eng\"}. Sent as the X-Labels header. Avoid ':' and ',' in keys or values (used as delimiters)."

// attributesReuseGuidance is appended to every ingest tool description.
const attributesReuseGuidance = " Typed attributes: call grill_attributes first and reuse an existing name and type where one fits; pass values in `attributes`, and use `attribute_schema` only to declare a type the value cannot express, and only for names present in `attributes` (encrypted_text must always be declared; datetime for a new name). Names are permanent per project and count against max_names. `labels` is legacy and being retired in favour of attributes — prefer attributes for new work."

// Fresh schema instances per tool: jsonschema-go requires a tree, so a shared
// pointer must never appear twice within one tool's schema.
func ingestAttributesSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		AdditionalProperties: &jsonschema.Schema{
			Types: []string{"string", "number", "boolean", "array"},
			Items: &jsonschema.Schema{Types: []string{"string", "number", "boolean"}},
		},
		Description: ingestAttributesDescription,
	}
}

func ingestAttributeSchemaSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		AdditionalProperties: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"type": {Type: "string"},
			},
			Required: []string{"type"},
		},
		Description: ingestAttributeSchemaDescription,
	}
}

// ingestHeaders carries the optional per-document metadata headers of a grill
// ingest. Empty fields are not sent.
type ingestHeaders struct {
	Labels          string // X-Labels
	Attributes      string // X-Attributes
	AttributeSchema string // X-Attribute-Schema
}

func (h ingestHeaders) apply(headers map[string]string) {
	if h.Labels != "" {
		headers["X-Labels"] = h.Labels
	}
	if h.Attributes != "" {
		headers["X-Attributes"] = h.Attributes
	}
	if h.AttributeSchema != "" {
		headers["X-Attribute-Schema"] = h.AttributeSchema
	}
}

// encodeIngestAttributes validates the attributes and attribute_schema
// arguments and renders them as the X-Attributes / X-Attribute-Schema header
// values. Empty input yields empty headers (not sent). Any violation is an
// error the caller reports as invalid_input.
func encodeIngestAttributes(attrs map[string]json.RawMessage, schema map[string]json.RawMessage) (string, string, error) {
	var attrHdr, schemaHdr string
	if len(attrs) > 0 {
		if len(attrs) > attributesMaxNames {
			return "", "", fmt.Errorf("attributes has %d names; at most %d per document", len(attrs), attributesMaxNames)
		}
		vals := make(map[string]any, len(attrs))
		for _, name := range sortedKeys(attrs) {
			if !attributeNameRe.MatchString(name) {
				return "", "", fmt.Errorf("attribute name %q must match ^[a-z0-9_]{1,64}$ (lowercase letters, digits, underscore)", name)
			}
			v, err := decodeUseNumber(attrs[name])
			if err != nil {
				return "", "", fmt.Errorf("attribute %q: %v", name, err)
			}
			if !validAttributeValue(v, true) {
				return "", "", fmt.Errorf("attribute %q: value must be a string, number, boolean, or an array of those (null is not accepted at ingest)", name)
			}
			vals[name] = v
		}
		s, err := compactASCIIJSON(vals)
		if err != nil {
			return "", "", fmt.Errorf("attributes: %v", err)
		}
		if len(s) > attributesHeaderMaxLen {
			return "", "", fmt.Errorf("attributes encode to %d characters; the X-Attributes header is capped at %d. Nothing was ingested and nothing was truncated — send fewer or shorter attributes", len(s), attributesHeaderMaxLen)
		}
		attrHdr = s
	}
	if len(schema) > 0 {
		decl := make(map[string]any, len(schema))
		for _, name := range sortedKeys(schema) {
			if !attributeNameRe.MatchString(name) {
				return "", "", fmt.Errorf("attribute_schema name %q must match ^[a-z0-9_]{1,64}$ (lowercase letters, digits, underscore)", name)
			}
			var d struct {
				Type *string `json:"type"`
			}
			if err := json.Unmarshal(schema[name], &d); err != nil || d.Type == nil || strings.TrimSpace(*d.Type) == "" {
				return "", "", fmt.Errorf("attribute_schema %q must be an object like {\"type\": \"encrypted_text\"}", name)
			}
			// Grill rejects a declaration for a name this document does not
			// carry, so an orphan must fail here, not after a 201.
			if _, ok := attrs[name]; !ok {
				return "", "", fmt.Errorf("attribute_schema declares %q, but attributes has no value for it; attribute_schema only declares types for names present in attributes", name)
			}
			decl[name] = map[string]string{"type": *d.Type}
		}
		s, err := compactASCIIJSON(decl)
		if err != nil {
			return "", "", fmt.Errorf("attribute_schema: %v", err)
		}
		if len(s) > attributesHeaderMaxLen {
			return "", "", fmt.Errorf("attribute_schema encodes to %d characters; the X-Attribute-Schema header is capped at %d. Nothing was ingested and nothing was truncated", len(s), attributesHeaderMaxLen)
		}
		schemaHdr = s
	}
	return attrHdr, schemaHdr, nil
}

func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// decodeUseNumber decodes one JSON value keeping numbers as json.Number, so a
// large integer is re-emitted exactly rather than through float64.
func decodeUseNumber(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

func validAttributeValue(v any, allowArray bool) bool {
	switch x := v.(type) {
	case nil:
		// Grill rejects null on every ingest path (resolve_types / check_value_limits);
		// null only means "remove" on the metadata-patch endpoint, which MCP does not use.
		return false
	case string, bool, json.Number:
		return true
	case []any:
		if !allowArray {
			return false
		}
		for _, e := range x {
			if e == nil || !validAttributeValue(e, false) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// compactASCIIJSON marshals v as compact JSON with object keys sorted, HTML
// characters left alone, and every non-ASCII character escaped as \uXXXX
// (surrogate pairs above the BMP). The result is plain ASCII, safe as an HTTP
// header value, and matches the Node implementation's encoder.
func compactASCIIJSON(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	out := bytes.TrimRight(buf.Bytes(), "\n")
	var b strings.Builder
	for len(out) > 0 {
		r, size := utf8.DecodeRune(out)
		out = out[size:]
		switch {
		case r < 0x80:
			b.WriteRune(r)
		case r > 0xFFFF:
			r1, r2 := utf16.EncodeRune(r)
			fmt.Fprintf(&b, `\u%04x\u%04x`, r1, r2)
		default:
			fmt.Fprintf(&b, `\u%04x`, r)
		}
	}
	return b.String(), nil
}
