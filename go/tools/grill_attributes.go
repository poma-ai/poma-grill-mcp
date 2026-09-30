package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// -- Grill Attributes ------------------------------------------------

// grillAttributesDefaultNote is used when the gateway omits its own note.
const grillAttributesDefaultNote = "Reuse an existing attribute name and type where one fits; a name, once declared, is permanent and counts against max_names."

var grillAttributesInputSchema = &jsonschema.Schema{
	Type: "object",
	Properties: map[string]*jsonschema.Schema{
		"token": {
			Type:        "string",
			Description: "POMA API JWT. Usually not needed — the server inherits the token from the Authorization header in the MCP client config or the POMA_API_KEY env var. Only pass explicitly to override.",
		},
		"project_id": projectIDSchema,
	},
}

var grillAttributesOutputSchema = &jsonschema.Schema{
	Type: "object",
	Properties: map[string]*jsonschema.Schema{
		"attributes": {
			Type:        "array",
			Description: "Attribute names the project has declared, each with its type (e.g. string, encrypted_text).",
			Items: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"name": {Type: "string"},
					"type": {Type: "string"},
				},
			},
		},
		"max_names":           {Type: "integer", Description: "Maximum number of distinct attribute names the project may ever declare."},
		"note":                {Type: "string", Description: "Guidance from the server on reusing attribute names."},
		"scope":               scopeSchema,
		"error":               {Type: "string"},
		"code":                errorCodeSchema,
		"retryable":           retryableSchema,
		"retry_after_seconds": retryAfterSchema,
	},
}

var grillAttributesTool = &mcp.Tool{
	Name: "grill_attributes",
	Annotations: &mcp.ToolAnnotations{
		Title:         "List Attributes",
		ReadOnlyHint:  true,
		OpenWorldHint: boolPtr(true),
	},
	Description:  "List the typed document attributes already declared in the project: each attribute's name and type, plus max_names (the per-project cap on distinct names). Call this BEFORE passing `attributes` to grill_ingest / grill_ingest_sync / grill_ingest_batch: reuse an existing name and type where one fits (do not invent `doc_year` when `year` exists) and declare a new name only when none fits — a name, once declared, is permanent and counts against max_names. Also call it before building grill_search attribute_filters, to learn which names exist and which type (and so which operators) each has; a filter on a name not listed here matches nothing. The response includes a `scope` object identifying the project — tell the user which project the list belongs to." + errorHandlingGuidance,
	InputSchema:  grillAttributesInputSchema,
	OutputSchema: grillAttributesOutputSchema,
}

type GrillAttributesInput struct {
	Token     string `json:"token,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
}

type GrillAttribute struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type GrillAttributesOutput struct {
	Attributes []GrillAttribute `json:"attributes"`
	MaxNames   int              `json:"max_names,omitempty"`
	Note       string           `json:"note,omitempty"`
	Scope      *GrillScope      `json:"scope,omitempty"`
	// GrillError promotes error, code, retryable, retry_after_seconds to the top level.
	GrillError
}

// attributesError builds the error output. `attributes` is declared as an
// array in the output schema; a nil slice would serialise as null and fail
// the SDK's output validation.
func attributesError(ge GrillError) GrillAttributesOutput {
	return GrillAttributesOutput{Attributes: []GrillAttribute{}, GrillError: ge}
}

func GrillAttributes(ctx context.Context, _ *mcp.CallToolRequest, input GrillAttributesInput) (*mcp.CallToolResult, GrillAttributesOutput, error) {
	token := getToken(ctx, input.Token)
	if token == "" {
		return errResult(), attributesError(errOut(CodeMissingToken, "token is required (provide token or set POMA_API_KEY on the server)")), nil
	}

	projectID := getProjectID(input.ProjectID)
	c := grillClient(token)
	body, st, err := grillListAttributes(c, projectID)
	if err != nil {
		ge := GrillError{Error: "grill attributes: " + err.Error(), Code: CodeTransportError, Retryable: isRetryableCode(CodeTransportError, 0)}
		return errResult(), attributesError(ge), nil
	}
	if authErr, authCode := interpretAuthError(ctx, input.Token, st, body, "grill attributes"); authErr != "" {
		return errResult(), attributesError(errOut(authCode, "%s", authErr)), nil
	}
	if conflict, ok := interpretProjectConflict(st, body, "grill attributes"); ok {
		return errResult(), attributesError(errOut(CodeInvalidInput, "%s", conflict)), nil
	}
	if st != http.StatusOK {
		msg := fmt.Sprintf("grill attributes: HTTP %d: %s", st, string(body))
		if st == http.StatusServiceUnavailable {
			msg += " (the project's attribute schema could not be read right now; retry later, and do not declare new attribute names until it can be read)"
		}
		ge := GrillError{Error: msg, Code: CodeUpstreamError, Retryable: isRetryableCode(CodeUpstreamError, st)}
		return errResult(), attributesError(ge), nil
	}

	var wire struct {
		Attributes []GrillAttribute `json:"attributes"`
		MaxNames   int              `json:"max_names"`
		Note       string           `json:"note"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return errResult(), attributesError(errOut(CodeParseError, "grill attributes: parse response: %v", err)), nil
	}

	out := GrillAttributesOutput{
		Attributes: wire.Attributes,
		MaxNames:   wire.MaxNames,
		Note:       wire.Note,
	}
	if out.Attributes == nil {
		out.Attributes = []GrillAttribute{}
	}
	if out.Note == "" {
		out.Note = grillAttributesDefaultNote
	}
	_, source := projectIDSource(token, input.ProjectID)
	out.Scope = resolveScope(c, token, projectID, "", source)
	slog.Info("grill attributes", "count", len(out.Attributes), "max_names", out.MaxNames, "project", out.Scope.ProjectName)
	return nil, out, nil
}
