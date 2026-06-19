package tools

// Grill client placeholders.
//
// TODO: Delete this entire file and replace each call site in grill.go with the
// equivalent (*client.Client).Grill* method once github.com/poma-ai/poma-cli
// implements them. The function signatures below are intentionally designed to
// match the client methods that will replace them.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/poma-ai/poma-cli/pkg/client"
)

// grillIngestData sends POST /grill/ingest with raw file bytes.
// Placeholder for (*client.Client).GrillIngestData(data []byte, filename string) ([]byte, int, error).
func grillIngestData(c *client.Client, data []byte, filename string, projectID string) ([]byte, int, error) {
	name := grillSanitizeFilename(filename)
	headers := map[string]string{
		"Content-Disposition": `attachment; filename="` + name + `"`,
		"Content-Type":        "application/octet-stream",
		"Content-Length":      strconv.Itoa(len(data)),
	}
	if projectID != "" {
		headers["X-Project-ID"] = projectID
	}
	return c.Do(http.MethodPost, "/grill/ingest", bytes.NewReader(data), headers)
}

// grillSearch sends POST /grill/search.
// Placeholder for (*client.Client).GrillSearch(req GrillSearchRequest) ([]byte, int, error).
func grillSearch(c *client.Client, req grillSearchRequest, projectID string) ([]byte, int, error) {
	if projectID == "" {
		return c.DoJSON(http.MethodPost, "/grill/search", req)
	}
	return doJSONWithProjectID(c, http.MethodPost, "/grill/search", req, projectID)
}

// grillSearchInDoc sends POST /grill/searchInDoc.
// Placeholder for (*client.Client).GrillSearchInDoc(req GrillSearchInDocRequest) ([]byte, int, error).
func grillSearchInDoc(c *client.Client, req grillSearchInDocRequest, projectID string) ([]byte, int, error) {
	if projectID == "" {
		return c.DoJSON(http.MethodPost, "/grill/searchInDoc", req)
	}
	return doJSONWithProjectID(c, http.MethodPost, "/grill/searchInDoc", req, projectID)
}

// doJSONWithProjectID marshals body as JSON and calls c.Do with an X-Project-ID header.
func doJSONWithProjectID(c *client.Client, method, endpoint string, body any, projectID string) ([]byte, int, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, 0, err
	}
	headers := map[string]string{
		"Content-Type": "application/json",
		"X-Project-ID": projectID,
	}
	return c.Do(method, endpoint, bytes.NewReader(b), headers)
}

// grillListDocs sends GET /grill/docs.
// Placeholder for (*client.Client).GrillListDocs() ([]byte, int, error).
func grillListDocs(c *client.Client, projectID string) ([]byte, int, error) {
	var headers map[string]string
	if projectID != "" {
		headers = map[string]string{"X-Project-ID": projectID}
	}
	return c.Do(http.MethodGet, "/grill/docs", nil, headers)
}

// grillListProjects sends GET /projects (optionally filtered by product).
func grillListProjects(c *client.Client, product string) ([]byte, int, error) {
	p := "/projects"
	if product != "" {
		p += "?product=" + url.QueryEscape(product)
	}
	return c.Do(http.MethodGet, p, nil, nil)
}

// grillSanitizeFilename mirrors the sanitizeContentDispositionFilename logic in poma-cli.
// Placeholder: remove when grillIngestData is replaced by the client method (which handles this internally).
func grillSanitizeFilename(name string) string {
	ext := path.Ext(name)
	if name == "" || name == "." || name == ".." {
		return "upload" + ext
	}
	if strings.ContainsAny(name, "\"\\\r\n\x00") {
		return "upload" + ext
	}
	return name
}

// grillSearchRequest is the JSON body for POST /grill/search.
type grillSearchRequest struct {
	Query            string   `json:"query"`
	ExcludeDocIDs    []string `json:"exclude_doc_ids,omitempty"`
	ReturnAssets     bool     `json:"return_assets,omitempty"`
	ReturnPageImages bool     `json:"return_page_images,omitempty"`
}

// grillSearchInDocRequest is the JSON body for POST /grill/searchInDoc.
type grillSearchInDocRequest struct {
	Query            string   `json:"query"`
	DocFilter        string   `json:"doc_filter"`
	ExcludeDocIDs    []string `json:"exclude_doc_ids,omitempty"`
	ReturnAssets     bool     `json:"return_assets,omitempty"`
	ReturnPageImages bool     `json:"return_page_images,omitempty"`
}
