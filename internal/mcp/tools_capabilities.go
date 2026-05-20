package mcp

// Tool name manifest. One named constant per advertised tool, gathered
// in one file so that operators and reviewers can see at a glance which
// capabilities the bosun MCP server exposes without grepping every
// tool_*.go init() block.
//
// New tools should add a constant here in the same change that lands
// their tool_*.go file. The constants are intentionally not consumed by
// the registration code itself — that path stays string-literal driven
// so a typo in a Name field is caught by an end-to-end ListTools test
// rather than silently aliasing to the wrong tool — but they ARE the
// single source of truth callers (web UI, docs gen, operator dashboards)
// should reference if they need to talk about tools by name in Go code.
const (
	ToolAnnounce      = "bosun_announce"
	ToolAttach        = "bosun_attach"
	ToolCheck         = "bosun_check"
	ToolCheckTree     = "bosun_check_tree"
	ToolClaim         = "bosun_claim"
	ToolDone          = "bosun_done"
	ToolHeartbeat     = "bosun_heartbeat"
	ToolPredict       = "bosun_predict"
	ToolRelease       = "bosun_release"
	ToolSpawn         = "bosun_spawn"
	ToolStuck         = "bosun_stuck"
	ToolSubtask       = "bosun_subtask"
	ToolSubtaskCancel = "bosun_subtask_cancel"
	ToolUsage         = "bosun_usage"
)
