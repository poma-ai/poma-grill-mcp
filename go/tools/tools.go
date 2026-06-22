package tools

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Register adds all POMA tools to the MCP server.
func Register(server *mcp.Server) {
	mcp.AddTool(server, grillExplainTool, GrillExplain)
	mcp.AddTool(server, grillIngestTool, GrillIngest)
	mcp.AddTool(server, grillIngestSyncTool, GrillIngestSync)
	mcp.AddTool(server, grillIngestResumeTool, GrillIngestResume)
	mcp.AddTool(server, grillIngestBatchTool, GrillIngestBatch)
	mcp.AddTool(server, grillJobsStatusTool, GrillJobsStatus)
	mcp.AddTool(server, grillSearchTool, GrillSearch)
	mcp.AddTool(server, grillDocsListTool, GrillDocsList)
	mcp.AddTool(server, grillProjectsTool, GrillProjects)
	// TODO: enable once poma-cli implements the underlying client methods
	// mcp.AddTool(server, grillDocsDeleteTool, GrillDocsDelete)
}

// boolPtr returns a pointer to b, for the optional *bool tool-annotation hints
// (DestructiveHint, OpenWorldHint) where nil means "unset / use spec default".
func boolPtr(b bool) *bool { return &b }
