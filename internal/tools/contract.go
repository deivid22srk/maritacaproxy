package tools

import (
        "encoding/json"
        "fmt"
        "regexp"
        "sort"
        "strings"
        "crypto/sha256"
        "encoding/hex"
)

// ToolChoiceMode represents the resolved tool_choice value.
type ToolChoiceMode string

const (
        ToolChoiceAuto    ToolChoiceMode = "auto"
        ToolChoiceNone    ToolChoiceMode = "none"
        ToolChoiceRequired ToolChoiceMode = "required"
        ToolChoiceForced  ToolChoiceMode = "forced"
)

var (
        contractCache = map[string]string{}
        manifestCache = map[string]string{}
)

// GetForcedToolName returns the forced tool name if tool_choice is "forced".
func GetForcedToolName(toolChoice interface{}) string {
        if tc, ok := toolChoice.(map[string]interface{}); ok {
                if fn, ok := tc["function"].(map[string]interface{}); ok {
                        if name, ok := fn["name"].(string); ok {
                                return name
                        }
                }
        }
        return ""
}

// GetToolChoiceMode parses the tool_choice field and returns the mode.
func GetToolChoiceMode(toolChoice interface{}) ToolChoiceMode {
        if toolChoice == nil {
                return ToolChoiceAuto
        }
        if s, ok := toolChoice.(string); ok {
                switch s {
                case "none":
                        return ToolChoiceNone
                case "required":
                        return ToolChoiceRequired
                default:
                        return ToolChoiceAuto
                }
        }
        if _, ok := toolChoice.(map[string]interface{}); ok {
                if GetForcedToolName(toolChoice) != "" {
                        return ToolChoiceForced
                }
        }
        return ToolChoiceAuto
}

// GetRecentToolNames returns the set of tool names that were invoked recently
// (in the last 12 messages).
func GetRecentToolNames(messages []map[string]interface{}) map[string]bool {
        recent := map[string]bool{}
        if len(messages) > 12 {
                messages = messages[len(messages)-12:]
        }
        for _, msg := range messages {
                role, _ := msg["role"].(string)
                if role == "assistant" {
                        if tcs, ok := msg["tool_calls"].([]interface{}); ok {
                                for _, tc := range tcs {
                                        if tcObj, ok := tc.(map[string]interface{}); ok {
                                                if fn, ok := tcObj["function"].(map[string]interface{}); ok {
                                                        if name, ok := fn["name"].(string); ok {
                                                                recent[name] = true
                                                        }
                                                }
                                        }
                                }
                        }
                }
                if role == "tool" || role == "function" {
                        if name, ok := msg["name"].(string); ok {
                                recent[name] = true
                        }
                }
        }
        return recent
}

// SelectCandidateTools scores and selects up to maxTools tools for the prompt.
func SelectCandidateTools(tools []FunctionToolDefinition, contextText, forcedToolName string, recentToolNames map[string]bool, maxTools int) []FunctionToolDefinition {
        if maxTools == 0 {
                maxTools = 12
        }
        if len(tools) <= maxTools {
                return tools
        }

        type scored struct {
                tool  FunctionToolDefinition
                score int
        }
        scoredList := make([]scored, 0, len(tools))
        for _, t := range tools {
                s := scoreToolForContext(t, contextText, forcedToolName, recentToolNames)
                if s > 0 || (forcedToolName != "" && t.Function.Name == forcedToolName) {
                        scoredList = append(scoredList, scored{t, s})
                }
        }

        sort.SliceStable(scoredList, func(i, j int) bool {
                if scoredList[i].score != scoredList[j].score {
                        return scoredList[i].score > scoredList[j].score
                }
                return scoredList[i].tool.Function.Name < scoredList[j].tool.Function.Name
        })

        if len(scoredList) == 0 {
                // Append file mutation tools even if no scoring
                selected := tools[:maxTools]
                return appendMissingFileMutationTools(selected, tools)
        }

        selected := make([]FunctionToolDefinition, 0, len(scoredList))
        for _, s := range scoredList[:min(maxTools, len(scoredList))] {
                selected = append(selected, s.tool)
        }
        return appendMissingFileMutationTools(selected, tools)
}

func min(a, b int) int {
        if a < b {
                return a
        }
        return b
}

func scoreToolForContext(tool FunctionToolDefinition, contextText, forcedToolName string, recentToolNames map[string]bool) int {
        name := tool.Function.Name
        description := tool.Function.Description
        params := getToolProperties(tool)
        paramKeys := make([]string, 0, len(params))
        for k := range params {
                paramKeys = append(paramKeys, k)
        }
        sort.Strings(paramKeys)

        tokens := tokenizeForToolScoring(contextText)
        score := 0

        if forcedToolName != "" && name == forcedToolName {
                score += 100
        }
        if recentToolNames[name] {
                score += 35
        }

        nameParts := strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
                return r == '_' || r == '.' || r == '/' || r == '-'
        })
        for _, part := range nameParts {
                if len(part) >= 3 && tokens[part] {
                        score += 20
                }
        }

        toolText := strings.ToLower(name + " " + description + " " + strings.Join(paramKeys, " "))
        for token := range tokens {
                if strings.Contains(toolText, token) {
                        score += 2
                }
        }

        for _, p := range paramKeys {
                if tokens[strings.ToLower(p)] {
                        score += 3
                }
        }

        return score
}

func tokenizeForToolScoring(text string) map[string]bool {
        tokens := map[string]bool{}
        lower := strings.ToLower(text)
        re := regexp.MustCompile(`[a-z0-9_./-]+`)
        for _, m := range re.FindAllString(lower, -1) {
                if len(m) >= 3 {
                        tokens[m] = true
                }
        }
        return tokens
}

// BuildCompactToolManifest builds a compact, cacheable manifest of tool names and signatures.
func BuildCompactToolManifest(tools []FunctionToolDefinition, forcedToolName string) string {
        if len(tools) == 0 {
                return ""
        }
        cacheKey := toolCacheKey(tools, forcedToolName, "")
        if v, ok := manifestCache[cacheKey]; ok {
                return v
        }

        var lines []string
        for _, tool := range tools {
                name := tool.Function.Name
                description := compactPromptText(tool.Function.Description, 140)
                params := getToolProperties(tool)
                required := getToolRequiredParams(tool)

                var sigParts []string
                paramKeys := make([]string, 0, len(params))
                for k := range params {
                        paramKeys = append(paramKeys, k)
                }
                sort.Strings(paramKeys)

                for _, paramName := range paramKeys {
                        schema := params[paramName].(map[string]interface{})
                        optional := ""
                        if !required[paramName] {
                                optional = "?"
                        }
                        typeStr, _ := schema["type"].(string)
                        if typeStr == "" {
                                typeStr = "any"
                        }
                        sigParts = append(sigParts, fmt.Sprintf("%s%s: %s", paramName, optional, typeStr))
                }
                signature := strings.Join(sigParts, ", ")

                marker := ""
                if forcedToolName != "" && name == forcedToolName {
                        marker = " [required]"
                }
                line := name + "(" + signature + ")"
                if description != "" {
                        line += " - " + description
                }
                line += marker
                lines = append(lines, line)
        }

        result := "[COMPACT TOOL MANIFEST]\n" + strings.Join(lines, "\n")
        manifestCache[cacheKey] = result
        return result
}

// BuildToolCallContract builds the contract that instructs the model how to emit tool calls.
func BuildToolCallContract(tools []FunctionToolDefinition, forcedToolName string, parallelToolCalls bool) string {
        cacheKey := toolCacheKey(tools, forcedToolName, "")
        if parallelToolCalls {
                cacheKey += "##p"
        } else {
                cacheKey += "##s"
        }
        if v, ok := contractCache[cacheKey]; ok {
                return v
        }

        names := make([]string, 0, len(tools))
        for _, t := range tools {
                if t.Function.Name != "" {
                        names = append(names, t.Function.Name)
                }
        }
        toolList := "none"
        if len(names) > 0 {
                toolList = strings.Join(names, ", ")
        }

        forcedLine := "Only call a tool when the user request requires an external action."
        if forcedToolName != "" {
                forcedLine = fmt.Sprintf("You MUST call exactly the tool \"%s\" unless the user request is impossible or unsafe. Do not call any other tool first.", forcedToolName)
        }

        parallelLine := "Emit at most one tool call block."
        if parallelToolCalls {
                parallelLine = "You may emit multiple tool call blocks only when the user explicitly asks for multiple independent actions."
        }

        fileMutationNames := []string{}
        for _, t := range tools {
                if isFileMutationTool(t) {
                        fileMutationNames = append(fileMutationNames, t.Function.Name)
                }
        }
        fileMutationLine := ""
        if len(fileMutationNames) > 0 {
                fileMutationLine = fmt.Sprintf("Workspace file mutation capabilities detected in these exact tools: %s. When the user asks to create, edit, patch, replace, rename, move, delete, or save files, choose the matching tool by its description and parameter schema, not by a preferred generic name.", strings.Join(fileMutationNames, ", "))
        }

        openTag := "<" + TagName + ">"
        closeTag := "</" + TagName + ">"
        result := fmt.Sprintf(`[TOOL CALL CONTRACT - MUST FOLLOW]
Available tool names: %s
%s
Format (USE THIS EXACT TAG - do NOT use <tool_call> as it conflicts with platform native tools):

%s
{"name": "tool_name", "arguments": {"param_name": "value"}}
%s

Rules:
1. Use the exact tool name as provided by the client. Tool names vary by editor/integration; do not require names like read_file, edit_file, write_file, or apply_patch to exist.
2. Do not invent, guess, rename, or approximate tool names. If a tool capability exists under a different name, call that exact provided name.
3. Do not output raw JSON as a tool call.
4. %s
5. %s
6. If no tool is needed, do not emit any tool call block.
7. Put only valid JSON inside each %s block. No markdown fences, comments, or explanatory text inside the block.
8. If you emit a tool call, stop after the closing %s tag and wait for the tool response.
9. CRITICAL: Do NOT use the <tool_call> tag (it conflicts with native platform tools). Use %s / %s instead.`, toolList, fileMutationLine, openTag, closeTag, forcedLine, parallelLine, openTag, closeTag, openTag, closeTag)

        contractCache[cacheKey] = result
        return result
}

// LooksLikeUnwrappedToolCall returns true if text appears to be a raw JSON tool call
// without surrounding <tool_call> tags.
func LooksLikeUnwrappedToolCall(text string) bool {
        trimmed := strings.TrimSpace(text)
        if !strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[") {
                return false
        }
        return strings.Contains(trimmed, "\"name\"") && strings.Contains(trimmed, "\"arguments\"")
}

// ParseUnwrappedToolCalls extracts tool calls from raw JSON without tags.
func ParseUnwrappedToolCalls(text string) []ParsedToolCall {
        if !LooksLikeUnwrappedToolCall(text) {
                return nil
        }
        parsed, err := robustParseJSON(text)
        if err != nil || parsed == nil {
                return nil
        }
        items := []interface{}{}
        if arr, ok := parsed.([]interface{}); ok {
                items = arr
        } else {
                items = []interface{}{parsed}
        }
        var calls []ParsedToolCall
        for _, item := range items {
                obj, ok := item.(map[string]interface{})
                if !ok {
                        continue
                }
                name, _ := obj["name"].(string)
                if name == "" {
                        if fn, ok := obj["function"].(map[string]interface{}); ok {
                                name, _ = fn["name"].(string)
                        }
                }
                if name == "" {
                        name, _ = obj["tool_name"].(string)
                        if name == "" {
                                name, _ = obj["tool"].(string)
                        }
                }
                if name == "" {
                        continue
                }

                var args interface{}
                if a, ok := obj["arguments"]; ok {
                        args = a
                } else if fn, ok := obj["function"].(map[string]interface{}); ok {
                        if a, ok := fn["arguments"]; ok {
                                args = a
                        }
                } else if a, ok := obj["args"]; ok {
                        args = a
                } else if a, ok := obj["parameters"]; ok {
                        args = a
                } else if a, ok := obj["input"]; ok {
                        args = a
                }

                id, _ := obj["id"].(string)
                if id == "" {
                        id, _ = obj["tool_call_id"].(string)
                }
                if id == "" {
                        id = newCallID()
                }

                calls = append(calls, ParsedToolCall{
                        ID:        id,
                        Name:      name,
                        Arguments: parseToolArguments(args),
                })
        }
        return calls
}

func toolCacheKey(tools []FunctionToolDefinition, forcedToolName, extra string) string {
        names := make([]string, 0, len(tools))
        for _, t := range tools {
                names = append(names, t.Function.Name)
        }
        h := sha256.Sum256([]byte(strings.Join(names, "|") + "##" + forcedToolName + "##" + extra))
        return hex.EncodeToString(h[:])
}

func compactPromptText(text string, maxChars int) string {
        if maxChars == 0 {
                maxChars = 180
        }
        space := regexp.MustCompile(`\s+`)
        compact := strings.TrimSpace(space.ReplaceAllString(text, " "))
        if len(compact) <= maxChars {
                return compact
        }
        return compact[:maxChars] + "..."
}

func isFileMutationTool(tool FunctionToolDefinition) bool {
        schemaText := collectSchemaText(tool.Function.Parameters)
        nameLower := strings.ToLower(tool.Function.Name)
        descLower := strings.ToLower(tool.Function.Description)
        combined := splitToolText(nameLower + " " + descLower + " " + schemaText)
        tokens := map[string]bool{}
        for _, t := range combined {
                tokens[t] = true
        }

        fileTargets := []string{"file", "files", "path", "filepath", "uri", "document", "workspace", "buffer"}
        hasFileTarget := false
        for _, t := range fileTargets {
                if tokens[t] {
                        hasFileTarget = true
                        break
                }
        }

        mutationVerbs := []string{"append", "apply", "change", "changes", "create", "delete", "diff", "edit", "edits", "insert", "modify", "move", "overwrite", "patch", "remove", "rename", "replace", "save", "str", "truncate", "update", "write"}
        hasMutationVerb := false
        for _, t := range mutationVerbs {
                if tokens[t] {
                        hasMutationVerb = true
                        break
                }
        }

        mutationPayloads := []string{"content", "diff", "edit", "edits", "new", "newtext", "newstring", "old", "oldtext", "oldstring", "patch", "replacement", "text", "value"}
        hasMutationPayload := false
        for _, t := range mutationPayloads {
                if tokens[t] {
                        hasMutationPayload = true
                        break
                }
        }

        readOnlyVerbs := []string{"read", "list", "search", "find", "grep", "show", "get", "open", "view", "inspect", "diagnostics", "hover", "definition", "references"}
        hasReadOnlyVerb := false
        for _, t := range readOnlyVerbs {
                if tokens[t] {
                        hasReadOnlyVerb = true
                        break
                }
        }

        return hasFileTarget && hasMutationVerb && (hasMutationPayload || !hasReadOnlyVerb)
}

func collectSchemaText(schemaRaw json.RawMessage) string {
        if len(schemaRaw) == 0 {
                return ""
        }
        var schema map[string]interface{}
        if err := json.Unmarshal(schemaRaw, &schema); err != nil {
                return ""
        }
        var values []string
        collectSchemaTextFromMap(schema, &values)
        return strings.Join(values, " ")
}

func collectSchemaTextFromMap(schema map[string]interface{}, values *[]string) {
        if desc, ok := schema["description"].(string); ok {
                *values = append(*values, desc)
        }
        if title, ok := schema["title"].(string); ok {
                *values = append(*values, title)
        }
        if c, ok := schema["const"].(string); ok {
                *values = append(*values, c)
        }
        if enums, ok := schema["enum"].([]interface{}); ok {
                for _, e := range enums {
                        if s, ok := e.(string); ok {
                                *values = append(*values, s)
                        }
                }
        }
        if props, ok := schema["properties"].(map[string]interface{}); ok {
                for k, v := range props {
                        *values = append(*values, k)
                        if m, ok := v.(map[string]interface{}); ok {
                                collectSchemaTextFromMap(m, values)
                        }
                }
        }
        if items, ok := schema["items"].(map[string]interface{}); ok {
                collectSchemaTextFromMap(items, values)
        }
        for _, key := range []string{"anyOf", "oneOf", "allOf"} {
                if arr, ok := schema[key].([]interface{}); ok {
                        for _, item := range arr {
                                if m, ok := item.(map[string]interface{}); ok {
                                        collectSchemaTextFromMap(m, values)
                                }
                        }
                }
        }
}

func splitToolText(value string) []string {
        camelSplit := regexp.MustCompile(`([a-z])([A-Z])`)
        out := camelSplit.ReplaceAllString(value, "$1 $2")
        parts := regexp.MustCompile(`[^a-z0-9]+`).Split(strings.ToLower(out), -1)
        var filtered []string
        for _, p := range parts {
                if p != "" {
                        filtered = append(filtered, p)
                }
        }
        return filtered
}

func appendMissingFileMutationTools(selected, tools []FunctionToolDefinition) []FunctionToolDefinition {
        selectedNames := map[string]bool{}
        for _, t := range selected {
                selectedNames[t.Function.Name] = true
        }
        for _, t := range tools {
                if selectedNames[t.Function.Name] {
                        continue
                }
                if isFileMutationTool(t) {
                        selected = append(selected, t)
                        selectedNames[t.Function.Name] = true
                }
        }
        return selected
}
