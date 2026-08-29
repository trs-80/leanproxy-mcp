package mcp

import "encoding/json"

type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Examples    []ToolExample   `json:"examples,omitempty"`
	Returns     ReturnSchema    `json:"returns,omitempty"`
	Categories  []string        `json:"categories,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type ToolExample struct {
	Input       map[string]interface{} `json:"input"`
	Description string                 `json:"description"`
}

type ReturnSchema struct {
	Type        string             `json:"type"`
	Description string             `json:"description"`
	Fields      []FieldDescription `json:"fields,omitempty"`
}

type FieldDescription struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Req  bool   `json:"required,omitempty"`
	Desc string `json:"description"`
}

var LeanproxyTools = []ToolDefinition{
	{
		Name:        "list_tools",
		Description: "List every tool on one MCP server with parameter signatures. Prefer search_tools to find a tool by keyword in a single call; use list_tools only to browse one server exhaustively.",
		Categories:  []string{"discovery", "meta"},
		Examples: []ToolExample{
			{
				Input: map[string]interface{}{
					"server_name": "github",
				},
				Description: "List all tools available on the github server",
			},
			{
				Input: map[string]interface{}{
					"server_name": "garmin",
				},
				Description: "List all tools available on the garmin server",
			},
			{
				Input: map[string]interface{}{
					"server_name":           "github",
					"max_description_chars": 150,
				},
				Description: "List github tools with shorter descriptions",
			},
		},
		Returns: ReturnSchema{
			Type:        "object",
			Description: "Returns a content block with formatted tool list for the specified server",
			Fields: []FieldDescription{
				{Name: "content", Type: "array", Desc: "Array of text content blocks"},
			},
		},
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"server_name": {
					"type": "string",
					"description": "MCP server name whose tools to list"
				},
				"max_description_chars": {
					"type": "number",
					"description": "Maximum characters for tool descriptions (default: 200, min: 50, max: 500)",
					"default": 200
				}
			},
			"required": ["server_name"]
		}`),
	},
	{
		Name:        "invoke_tool",
		Description: "Invoke a tool on a configured MCP server. Call this directly when you already know the server and tool (from search_tools, list_tools, or earlier in the conversation) - no discovery calls are required first. Errors include the tool schema and close-match suggestions, so a failed guess is cheap.",
		Categories:  []string{"execution", "meta"},
		Examples: []ToolExample{
			{
				Input: map[string]interface{}{
					"server": "github",
					"tool":   "list_issues",
					"arguments": map[string]interface{}{
						"owner":   "trs-80",
						"repo":    "leanproxy-mcp-bob",
						"state":   "open",
						"perPage": 10,
					},
				},
				Description: "List open issues on the leanproxy-mcp-bob repository",
			},
			{
				Input: map[string]interface{}{
					"server": "github",
					"tool":   "search_issues",
					"arguments": map[string]interface{}{
						"query":   "is:issue is:open label:bug",
						"owner":   "trs-80",
						"repo":    "leanproxy-mcp-bob",
						"perPage": 5,
					},
				},
				Description: "Search for open bug issues",
			},
		},
		Returns: ReturnSchema{
			Type:        "object",
			Description: "Returns the result from the remote MCP tool invocation",
			Fields: []FieldDescription{
				{Name: "content", Type: "array", Desc: "Array of content blocks from tool"},
			},
		},
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"server": {
					"type": "string",
					"description": "MCP server name, e.g. 'github'"
				},
				"tool": {
					"type": "string",
					"description": "Tool name without server prefix, e.g. 'list_issues'"
				},
				"arguments": {
					"type": "object",
					"description": "Tool arguments as key-value pairs"
				},
				"max_response_chars": {
					"type": "number",
					"description": "Truncate the tool result to this many characters (min 200). Use for large outputs you only need the start of."
				}
			},
			"required": ["server", "tool"],
			"additionalProperties": false
		}`),
	},
	{
		Name:        "search_tools",
		Description: "START HERE for discovery: keyword-search tools across ALL MCP servers in one call. Returns matching tools as 'server_tool: description [required: type, ...] {optional: type, ...}' - enough to call invoke_tool immediately, no list_servers/list_tools round trips needed.",
		Categories:  []string{"discovery", "meta"},
		Examples: []ToolExample{
			{
				Input:       map[string]interface{}{"query": "create issue"},
				Description: "Find tools for creating issues on any server",
			},
			{
				Input:       map[string]interface{}{"query": "activity", "server": "garmin", "limit": 5},
				Description: "Find up to 5 activity-related tools on the garmin server",
			},
		},
		Returns: ReturnSchema{
			Type:        "object",
			Description: "Content block listing matching tools with parameter signatures",
			Fields: []FieldDescription{
				{Name: "content", Type: "array", Desc: "Array of text content blocks"},
			},
		},
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"query": {
					"type": "string",
					"description": "Keywords matched against tool names and descriptions; any word may match, more matches rank higher. Empty lists everything."
				},
				"server": {
					"type": "string",
					"description": "Restrict results to one server"
				},
				"limit": {
					"type": "number",
					"description": "Maximum results (default 25)",
					"default": 25
				},
				"max_description_chars": {
					"type": "number",
					"description": "Truncate descriptions (default 120)",
					"default": 120
				}
			}
		}`),
	},
}

func GetToolDefinition(name string) *ToolDefinition {
	for i := range LeanproxyTools {
		if LeanproxyTools[i].Name == name {
			return &LeanproxyTools[i]
		}
	}
	return nil
}

func GetAllToolDefinitions() []ToolDefinition {
	return LeanproxyTools
}
