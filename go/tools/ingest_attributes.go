package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
)

// Typed document attributes on ingest. The gateway (poma-services-go
// internal/services/jobs/api.go) takes them on POST /ingest as two
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

// grillTypeNameList is grill's TYPES (poma-grill attribute_schema.py), sorted
// as grill lists them in its own "unknown type" error.
var grillTypeNameList = []string{
	"[]bool", "[]datetime", "[]encrypted_text", "[]float", "[]int", "[]string",
	"bool", "datetime", "encrypted_text", "float", "int", "string",
}

var grillTypeNames = func() map[string]bool {
	m := make(map[string]bool, len(grillTypeNameList))
	for _, t := range grillTypeNameList {
		m[t] = true
	}
	return m
}()

// ingestAttributesDescription and ingestAttributeSchemaDescription are shared
// verbatim with the Node schema (schemas/tools.json). Keep byte-identical.
const ingestAttributesDescription = "Optional typed document attributes: a flat object of name → value, where a value is a string, number, boolean, or an array of those (never null), e.g. {\"region\":\"emea\",\"year\":2024}. An array must be non-empty and hold one kind of element — all strings, all ints, all floats or all booleans (1 and 2.5 do not mix). A whole number such as 1.0 counts as an int, so to keep a float type for whole numbers declare float or []float in attribute_schema; an empty array is accepted only when attribute_schema declares its array type (e.g. \"[]string\"). Call grill_attributes first and reuse an existing name and type where one fits. Names must match ^[a-z0-9_]{1,64}$ and are permanent per project, counting against max_names; string, number and boolean values (and arrays of them) are typed by their value — declare encrypted_text or datetime in attribute_schema. Filter on them later with grill_search attribute_filters. Sent as the X-Attributes header: at most 64 names and 2048 characters of compact JSON; larger input is refused with invalid_input, never truncated."

const ingestAttributeSchemaDescription = "Optional type declarations, only for names present in attributes (a declaration for any other name is refused), shaped {\"name\": {\"type\": \"<POMA type>\"}}, where the type is one of string, int, float, bool, datetime, encrypted_text or their [] array forms, e.g. {\"case_notes\":{\"type\":\"encrypted_text\"}}. Declare only where the value cannot say the type: encrypted_text must ALWAYS be declared (an undeclared new name is stored as a plain string), datetime when introducing a new datetime name (values in RFC3339, e.g. \"2024-05-01T12:00:00Z\", or YYYY-MM-DD), and the array type (e.g. \"[]string\") of an empty array. A declaration that conflicts with the type already in force for that name is rejected. Sent as the X-Attribute-Schema header, same 2048-character cap."

// Legacy labels: kept working unchanged, but steered toward attributes.
const ingestLabelsDescription = "Legacy — being retired in favour of attributes; prefer attributes for new work. Optional key:value labels, e.g. {\"team\":\"eng\"}. Sent as the labels attribute: each pair becomes a \"key:value\" string in attributes.labels (sorted by key, after any labels you pass in attributes, duplicates dropped); no X-Labels header. Grill types labels as []encrypted_text (token-matched) unless the project already owns labels with another type, which then governs (an incompatible one fails at grill). Labels share the attribute limits: 64 elements, 64 names, 2048 characters of X-Attributes. Avoid ':' in keys."

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
//
// There is deliberately no X-Labels field: our clients never send the legacy
// header (D157). Legacy labels are translated into attributes.labels by
// encodeIngestMeta instead.
type ingestHeaders struct {
	Attributes      string // X-Attributes
	AttributeSchema string // X-Attribute-Schema
}

func (h ingestHeaders) apply(headers map[string]string) {
	if h.Attributes != "" {
		headers["X-Attributes"] = h.Attributes
	}
	if h.AttributeSchema != "" {
		headers["X-Attribute-Schema"] = h.AttributeSchema
	}
}

// labelsAttributeName is the typed attribute the legacy labels move into.
// Grill declares it []encrypted_text on first use (D157), so no
// X-Attribute-Schema entry is needed for it.
const labelsAttributeName = "labels"

// labelItemsFromMap renders the legacy labels tool argument as "key:value"
// items, keys sorted, empty/whitespace-only keys skipped — the same order and
// filtering the retired X-Labels serializer used.
func labelItemsFromMap(labels map[string]string) []string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		if strings.TrimSpace(k) == "" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	items := make([]string, 0, len(keys))
	for _, k := range keys {
		items = append(items, k+":"+labels[k])
	}
	return items
}

// labelItemsFromHeader splits a caller-supplied X-Labels header value
// ("key:value" items, comma-joined) into items, trimming whitespace and
// skipping empty items. Order is kept as sent.
func labelItemsFromHeader(h string) []string {
	var items []string
	for _, it := range strings.Split(h, ",") {
		if it = strings.TrimSpace(it); it != "" {
			items = append(items, it)
		}
	}
	return items
}

// mergeLabelsAttribute returns attrs with the legacy label items merged into
// attributes.labels: explicit attributes.labels entries first, then the
// translated items, duplicates dropped, plus how many translated items were
// added. attrs itself is not modified. With no items, attrs is returned
// unchanged. An attributes.labels that is not an array of non-null strings
// cannot take the items; that is an error, never a silent drop. (Decoding into
// []string would turn a null element into "" — hence []any.)
func mergeLabelsAttribute(attrs map[string]json.RawMessage, items []string) (map[string]json.RawMessage, int, error) {
	if len(items) == 0 {
		return attrs, 0, nil
	}
	merged := make([]string, 0, len(items))
	seen := make(map[string]bool, len(items))
	if raw, ok := attrs[labelsAttributeName]; ok {
		conflict := fmt.Errorf("attributes.labels must be an array of strings when the legacy labels argument (or X-Labels header) is also given, so the two can be merged; got %s", strings.TrimSpace(string(raw)))
		var explicit []any
		if err := json.Unmarshal(raw, &explicit); err != nil || explicit == nil {
			return nil, 0, conflict
		}
		for _, e := range explicit {
			str, ok := e.(string)
			if !ok {
				return nil, 0, conflict
			}
			if !seen[str] {
				seen[str] = true
				merged = append(merged, str)
			}
		}
	}
	added := 0
	for _, it := range items {
		if !seen[it] {
			seen[it] = true
			merged = append(merged, it)
			added++
		}
	}
	enc, err := json.Marshal(merged)
	if err != nil {
		return nil, 0, err
	}
	out := make(map[string]json.RawMessage, len(attrs)+1)
	for k, v := range attrs {
		out[k] = v
	}
	out[labelsAttributeName] = enc
	return out, added, nil
}

// legacyLabelsHint names the legacy source in an error, so a caller who only
// passed labels can tell why an attribute rule fired.
func legacyLabelsHint(err error, n int, source string) error {
	if n == 0 {
		return err
	}
	return fmt.Errorf("%w (includes %d entries translated from %s)", err, n, source)
}

// Label sources named in legacyLabelsHint.
const (
	labelsSourceArg    = "the legacy `labels` argument"
	labelsSourceHeader = "the legacy X-Labels header"
)

// encodeIngestMeta builds the ingest metadata headers from the legacy label
// items plus the attributes / attribute_schema input: the items are merged
// into attributes.labels, and the result goes through encodeIngestAttributes,
// so the merged labels obey every attribute rule (64 elements, 64 names,
// 2048-character header). When the merge is impossible, the attributes are
// first validated as given so an input that was already invalid reports its
// usual error. Errors raised with translated labels in play say so.
func encodeIngestMeta(labelItems []string, labelsSource string, attrs map[string]json.RawMessage, schema map[string]json.RawMessage) (ingestHeaders, error) {
	merged, added, merr := mergeLabelsAttribute(attrs, labelItems)
	if merr != nil {
		if _, _, err := encodeIngestAttributes(attrs, schema); err != nil {
			return ingestHeaders{}, err
		}
		return ingestHeaders{}, legacyLabelsHint(merr, len(labelItems), labelsSource)
	}
	a, s, err := encodeIngestAttributes(merged, schema)
	if err != nil {
		return ingestHeaders{}, legacyLabelsHint(err, added, labelsSource)
	}
	return ingestHeaders{Attributes: a, AttributeSchema: s}, nil
}

// encodeIngestAttributes validates the attributes and attribute_schema
// arguments and renders them as the X-Attributes / X-Attribute-Schema header
// values. Empty input yields empty headers (not sent). Any violation is an
// error the caller reports as invalid_input.
func encodeIngestAttributes(attrs map[string]json.RawMessage, schema map[string]json.RawMessage) (string, string, error) {
	var attrHdr, schemaHdr string

	// Declarations first: the value checks below depend on them (a declared
	// type decides array-vs-scalar and element kinds; only a declared array
	// type makes an empty array legal).
	declTypes := make(map[string]string, len(schema))
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
		if !grillTypeNames[*d.Type] {
			return "", "", fmt.Errorf("attribute_schema %q: unknown type %q; known: %s", name, *d.Type, strings.Join(grillTypeNameList, ", "))
		}
		declTypes[name] = *d.Type
	}

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
			if err := checkAttributeValue(name, v, declTypes[name]); err != nil {
				return "", "", err
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

	if len(declTypes) > 0 {
		decl := make(map[string]any, len(declTypes))
		for _, name := range sortedKeys(schema) {
			// Grill rejects a declaration for a name this document does not
			// carry, so an orphan must fail here, not after a 201.
			if _, ok := attrs[name]; !ok {
				return "", "", fmt.Errorf("attribute_schema declares %q, but attributes has no value for it; attribute_schema only declares types for names present in attributes", name)
			}
			decl[name] = map[string]string{"type": declTypes[name]}
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

// Grill's per-value rules (poma-grill attribute_schema.py: infer_type,
// resolve_types, check_value_limits / _check_element), applied to the value
// exactly as it goes on the wire. Mirrored here because the gateway passes
// values through unchecked: without this a violating value gets a 201 and the
// job then fails inside grill.
const (
	attributesMaxArrayElems = 64      // grill ATTR_MAX_ARRAY_ELEMS default (config.toml attr_max_array_elems)
	attributesJSONSafeInt   = 1 << 53 // grill _JSON_SAFE_INT: |int| must be <= 2^53
)

// attributeKind is the kind grill infers for one scalar: "string", "bool",
// "int" or "float" — judged on the value grill actually receives. The gateway
// decodes attribute numbers as float64 and the queue re-encodes them, so 1.0
// reaches grill as 1 (an int) and only a non-integral value stays a float;
// Go's encoder switches to exponent form at 1e21, which Python reads as float.
func attributeKind(v any) string {
	switch x := v.(type) {
	case string:
		return "string"
	case bool:
		return "bool"
	case json.Number:
		f, err := strconv.ParseFloat(string(x), 64)
		if err != nil || math.IsInf(f, 0) {
			return "" // non-finite; refused before kinds are compared
		}
		if f == math.Trunc(f) && math.Abs(f) < 1e21 {
			return "int"
		}
		return "float"
	}
	return ""
}

func checkAttributeValue(name string, v any, declared string) error {
	arr, isArr := v.([]any)
	elems := []any{v}
	if isArr {
		elems = arr
	}
	// grill: "attribute values must be finite numbers". JSON cannot spell NaN,
	// but a literal like 1e400 overflows float64 to +Inf.
	for _, e := range elems {
		if n, ok := e.(json.Number); ok {
			if f, err := strconv.ParseFloat(string(n), 64); err != nil || math.IsInf(f, 0) {
				return fmt.Errorf("attribute %q: %s is not a finite number", name, n)
			}
		}
	}

	var base string
	if declared == "" {
		// Undeclared: grill infers the type from the value.
		if isArr {
			if len(arr) == 0 {
				return fmt.Errorf("attribute %q: an empty array has no type to infer; declare it in attribute_schema with an array type (e.g. {\"%s\": {\"type\": \"[]string\"}}) or leave the attribute out", name, name)
			}
			kinds := map[string]bool{}
			for _, e := range arr {
				kinds[attributeKind(e)] = true
			}
			if len(kinds) > 1 {
				ks := make([]string, 0, len(kinds))
				for k := range kinds {
					ks = append(ks, k)
				}
				sort.Strings(ks)
				return fmt.Errorf("attribute %q: mixed element types in array %v; every element must be the same kind (string, int, float or bool — int and float count as different kinds)", name, ks)
			}
		}
		base = attributeKind(elems[0]) // elems is non-empty here
	} else {
		// Declared: the value's shape must match the declared type.
		wantArr := strings.HasPrefix(declared, "[]")
		if isArr != wantArr {
			shape := "a scalar"
			if wantArr {
				shape = "an array"
			}
			return fmt.Errorf("attribute %q is declared %q, which needs %s", name, declared, shape)
		}
		base = strings.TrimPrefix(declared, "[]")
		for _, e := range elems {
			k := attributeKind(e)
			ok := true
			switch base {
			case "bool", "int", "string":
				ok = k == base
			case "float":
				ok = k == "int" || k == "float"
			case "datetime", "encrypted_text":
				ok = k == "string"
			}
			if !ok {
				return fmt.Errorf("attribute %q: element %s is not a %s (declared %q)", name, string(mustJSON(e)), base, declared)
			}
			if base == "datetime" && !datetimeValid(e.(string)) {
				return fmt.Errorf("attribute %q: %s is not a valid datetime; use RFC3339 (2024-05-01T12:00:00Z) or YYYY-MM-DD", name, string(mustJSON(e)))
			}
		}
	}

	if isArr && len(arr) > attributesMaxArrayElems {
		return fmt.Errorf("attribute %q: %d elements exceeds the cap of %d", name, len(arr), attributesMaxArrayElems)
	}
	if base == "int" {
		// Compared after the gateway's float64 round trip, which is the value
		// grill range-checks.
		for _, e := range elems {
			if n, ok := e.(json.Number); ok {
				if f, _ := strconv.ParseFloat(string(n), 64); math.Abs(f) > attributesJSONSafeInt {
					return fmt.Errorf("attribute %q: %s is outside the JSON-safe integer range (±2^53)", name, n)
				}
			}
		}
	}
	return nil
}

// datetimeValid accepts RFC3339 (with a Z or ±hh:mm offset, optional
// fractional seconds) or a plain YYYY-MM-DD date. Deliberately narrower than
// grill's datetime.fromisoformat: exotic ISO forms are refused here.
func datetimeValid(s string) bool {
	if _, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return true
	}
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
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
