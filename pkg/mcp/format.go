package mcp

import (
	"encoding/json"
	"sort"
	"strings"
)

// ParamInfo is one parameter extracted from a tool's input schema, used to
// render invocation-ready signatures.
type ParamInfo struct {
	Name        string
	Type        string
	IsRequired  bool
	Description string
}

func parseInputSchema(schema json.RawMessage) (required, optional []ParamInfo) {
	var schemaMap map[string]interface{}
	if err := json.Unmarshal(schema, &schemaMap); err != nil {
		return nil, nil
	}

	properties, ok := schemaMap["properties"].(map[string]interface{})
	if !ok {
		return nil, nil
	}

	var requiredNames []string
	if req, ok := schemaMap["required"].([]interface{}); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				requiredNames = append(requiredNames, s)
			}
		}
	}

	isRequired := make(map[string]bool)
	for _, name := range requiredNames {
		isRequired[name] = true
	}

	for name, prop := range properties {
		propMap, ok := prop.(map[string]interface{})
		if !ok {
			continue
		}
		typeVal, _ := propMap["type"].(string)
		descVal, _ := propMap["description"].(string)

		param := ParamInfo{
			Name:        name,
			Type:        typeVal,
			IsRequired:  isRequired[name],
			Description: descVal,
		}

		if isRequired[name] {
			required = append(required, param)
		} else {
			optional = append(optional, param)
		}
	}

	// Deterministic order: identical schemas must always render identically —
	// unstable output defeats provider prompt caching across sessions and makes
	// results harder to diff. Required params follow the schema's "required"
	// array (author-intended order); optional params sort alphabetically.
	requiredRank := make(map[string]int, len(requiredNames))
	for i, name := range requiredNames {
		requiredRank[name] = i
	}
	sort.Slice(required, func(i, j int) bool { return requiredRank[required[i].Name] < requiredRank[required[j].Name] })
	sort.Slice(optional, func(i, j int) bool { return optional[i].Name < optional[j].Name })
	return required, optional
}

func formatToolSearchResult(serverName, toolName, description string, required, optional []ParamInfo, maxDescChars int) string {
	var sb strings.Builder
	sb.WriteString(serverName)
	sb.WriteString("_")
	sb.WriteString(toolName)
	sb.WriteString(": ")
	sb.WriteString(truncateDescription(description, maxDescChars))

	if len(required) > 0 {
		sb.WriteString(" [")
		for i, p := range required {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString(p.Name)
			sb.WriteString(": ")
			sb.WriteString(p.Type)
		}
		sb.WriteString("]")
	}

	if len(optional) > 0 {
		sb.WriteString(" {")
		for i, p := range optional {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString(p.Name)
			sb.WriteString(": ")
			sb.WriteString(p.Type)
		}
		sb.WriteString("}")
	}

	return sb.String()
}

func formatTool(tool Tool, serverName string, maxDescChars int) string {
	required, optional := parseInputSchema(tool.InputSchema)
	return formatToolSearchResult(serverName, tool.Name, tool.Description, required, optional, maxDescChars)
}

// truncateDescription cuts at a word boundary and appends a single ellipsis
// rune: a mid-word cut wastes the partial token, and "…" tokenizes shorter
// than "...". Falls back to a hard cut when no space is found in the last
// third of the budget.
func truncateDescription(description string, maxChars int) string {
	if maxChars <= 0 || len(description) <= maxChars {
		return description
	}
	const ellipsis = "…" // 3 bytes
	if maxChars <= len(ellipsis) {
		return description[:maxChars]
	}
	cut := maxChars - len(ellipsis)
	if i := strings.LastIndex(description[:cut], " "); i >= cut*2/3 {
		cut = i
	}
	return strings.TrimRight(description[:cut], " ,;:-") + ellipsis
}
