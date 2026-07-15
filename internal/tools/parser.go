// Package tools implements the streaming tool-call parser and contract builder
// used to translate OpenAI-style tool definitions into Maritaca's free-text chat.
//
// The implementation is a Go port of the TypeScript StreamingToolParser found in
// qwenproxy/src/tools/parser.ts, including the contract / manifest builder logic
// from qwenproxy/src/routes/tool-handler.ts.
package tools

import (
        "crypto/rand"
        "encoding/hex"
        "encoding/json"
        "fmt"
        "regexp"
        "sort"
        "strings"

        "github.com/deivid22srk/maritacaproxy/internal/logger"
)

// FunctionToolDefinition mirrors the OpenAI function tool schema.
type FunctionToolDefinition struct {
        Type     string          `json:"type"`
        Function FunctionSpec    `json:"function"`
}

// FunctionSpec is the inner function spec.
type FunctionSpec struct {
        Name        string          `json:"name"`
        Description string          `json:"description,omitempty"`
        Parameters  json.RawMessage `json:"parameters,omitempty"`
        Strict      bool            `json:"strict,omitempty"`
}

// ParsedToolCall represents a single tool invocation parsed from model output.
type ParsedToolCall struct {
        ID        string                 `json:"id"`
        Name      string                 `json:"name"`
        Arguments map[string]interface{} `json:"arguments"`
}

// ParserResult is the result of feeding a chunk into StreamingToolParser.
type ParserResult struct {
        Text       string          `json:"text"`
        ToolCalls  []ParsedToolCall `json:"tool_calls"`
}

// TagName holds the configured tool-call tag name. Default is "proxy_tool_call"
// (NOT "tool_call") because Maritaca's backend intercepts <tool_call> tags
// and tries to route them through its own tool registry, which rejects
// user-provided tool names. Using <proxy_tool_call> makes the tag pass through
// the backend untouched so we can parse it client-side.
var TagName = "proxy_tool_call"

// SetTagName changes the tool-call tag name. Call this once at startup from
// config. Must be set before any parser is constructed.
func SetTagName(name string) {
        if name == "" {
                return
        }
        TagName = name
        // Recompile regexes that depend on the tag name
        toolOpenRE = regexp.MustCompile(`<` + regexp.QuoteMeta(name) + `\b[^>]*>`)
        toolEnd = `</` + name + `>`
        toolShortEnd = `</` + name + `>`
        xmlNameAttrRE = regexp.MustCompile(`(?is)<` + regexp.QuoteMeta(name) + `\b[^>]*\bname\s*=\s*["']([^"']+)["']`)
        // Recompile prefix used in partial-tag detection
        toolStartLiteral = "<" + name + ">"
}

var (
        toolOpenRE        = regexp.MustCompile(`<proxy_tool_call\b[^>]*>`)
        toolEnd           = `</proxy_tool_call>`
        toolShortEnd      = `</proxy_tool_call>`
        xmlParamRE        = regexp.MustCompile(`(?is)<parameter\b[^>]*\bname\s*=\s*["']([^"']+)["'][^>]*>([\s\S]*?)</parameter>`)
        xmlUnclosedRE     = regexp.MustCompile(`(?is)<parameter\b[^>]*\bname\s*=\s*["']([^"']+)["'][^>]*>([\s\S]*)$`)
        xmlNameAttrRE     = regexp.MustCompile(`(?is)<proxy_tool_call\b[^>]*\bname\s*=\s*["']([^"']+)["']`)
        xmlNameTagRE      = regexp.MustCompile(`(?is)<name>([\s\S]*?)</name>`)
        envDetailsRE      = regexp.MustCompile(`^\s*<environment_details\b`)
        toolStartLiteral  = "<proxy_tool_call>"
)

// StreamingToolParser incrementally parses tool-call tags from streamed text.
type StreamingToolParser struct {
        buffer              string
        insideTool          bool
        currentOpenTag      string
        emittedToolCallCnt  int
        pendingLeadIn       string
        tools               []FunctionToolDefinition
}

// NewStreamingToolParser creates a parser instance with optional tool definitions
// used for name inference when the model omits the tool name.
func NewStreamingToolParser(tools []FunctionToolDefinition) *StreamingToolParser {
        return &StreamingToolParser{
                buffer:         "",
                insideTool:     false,
                currentOpenTag: toolStartLiteral,
                tools:          tools,
        }
}

// SetTools replaces the tool list used for name inference.
func (p *StreamingToolParser) SetTools(tools []FunctionToolDefinition) {
        p.tools = tools
}

// Feed appends a chunk to the parser buffer and returns any text or tool calls
// that have been fully parsed so far.
func (p *StreamingToolParser) Feed(chunk string) ParserResult {
        p.buffer += chunk
        result := ParserResult{}

        for len(p.buffer) > 0 {
                if !p.insideTool {
                        if !strings.Contains(p.buffer, "<") {
                                if p.emittedToolCallCnt == 0 {
                                        result.Text += p.buffer
                                }
                                p.buffer = ""
                                break
                        }
                        loc := toolOpenRE.FindStringIndex(p.buffer)
                        if loc != nil {
                                textBefore := p.buffer[:loc[0]]
                                result.Text += textBefore
                                p.insideTool = true
                                p.currentOpenTag = p.buffer[loc[0]:loc[1]]
                                p.buffer = p.buffer[loc[1]:]
                                continue
                        }
                        // No full open tag - check for partial at end
                        partialIdx := findPartialToolOpenIndex(p.buffer)
                        flushIdx := len(p.buffer)
                        if partialIdx != -1 {
                                flushIdx = partialIdx
                        }
                        if flushIdx > 0 {
                                textToEmit := p.buffer[:flushIdx]
                                if p.emittedToolCallCnt == 0 {
                                        result.Text += textToEmit
                                }
                                p.buffer = p.buffer[flushIdx:]
                        }
                        break
                } else {
                        endMatch := findToolEndMatch(p.buffer)
                        if endMatch == nil {
                                endMatch = findRecoverableTailEndMatch(p.buffer)
                        }
                        if endMatch != nil {
                                content := p.buffer[:endMatch.index]
                                p.buffer = p.buffer[endMatch.index+endMatch.length:]
                                p.processToolContent(content, &result)
                                p.insideTool = false
                                p.currentOpenTag = toolStartLiteral
                                if p.emittedToolCallCnt > 0 && envDetailsRE.MatchString(p.buffer) {
                                        p.buffer = ""
                                        break
                                }
                                if len(p.buffer) > 0 {
                                        nextMatch := toolOpenRE.FindStringIndex(p.buffer)
                                        if nextMatch != nil {
                                                result.Text += p.buffer[:nextMatch[0]]
                                                p.insideTool = true
                                                p.currentOpenTag = p.buffer[nextMatch[0]:nextMatch[1]]
                                                p.buffer = p.buffer[nextMatch[1]:]
                                        } else {
                                                partialIdx := findPartialToolOpenIndex(p.buffer)
                                                flushIdx := len(p.buffer)
                                                if partialIdx != -1 {
                                                        flushIdx = partialIdx
                                                }
                                                result.Text += p.buffer[:flushIdx]
                                                p.buffer = p.buffer[flushIdx:]
                                        }
                                }
                        } else {
                                break
                        }
                }
        }

        return result
}

// Flush processes any remaining buffer content and returns the final result.
func (p *StreamingToolParser) Flush() ParserResult {
        result := ParserResult{}
        if p.buffer == "" && p.pendingLeadIn == "" {
                return result
        }

        if p.insideTool {
                trimmed := strings.TrimSpace(p.buffer)
                if len(trimmed) > 0 {
                        recovered := p.tryRecoverToolCall(trimmed)
                        if recovered != nil {
                                result.ToolCalls = append(result.ToolCalls, *recovered)
                                p.emittedToolCallCnt++
                        } else {
                                logger.Warn("[parser] Dropping unrecoverable unclosed tool call at end of stream")
                                result.Text += p.pendingLeadIn
                                result.Text += p.currentOpenTag + p.buffer + toolEnd
                        }
                } else {
                        result.Text += p.pendingLeadIn
                }
        } else {
                result.Text += p.buffer
        }

        p.buffer = ""
        p.insideTool = false
        p.currentOpenTag = toolStartLiteral
        return result
}

// EmittedToolCallCount returns the number of tool calls emitted so far.
func (p *StreamingToolParser) EmittedToolCallCount() int {
        return p.emittedToolCallCnt
}

// IsInsideTool returns whether the parser is currently inside a <tool_call> block.
func (p *StreamingToolParser) IsInsideTool() bool {
        return p.insideTool
}

func (p *StreamingToolParser) processToolContent(content string, result *ParserResult) {
        t := strings.TrimSpace(content)
        if t == "" {
                logger.Warn("[parser] Dropping empty tool call block")
                if p.emittedToolCallCnt == 0 && strings.TrimSpace(p.pendingLeadIn) != "" {
                        result.Text += p.pendingLeadIn
                }
                p.pendingLeadIn = ""
                return
        }

        t = unescapeDoubleEscaped(t)

        // 1) Try XML parameter format
        if xmlParsed := parseXMLParameterToolCall(t, p.currentOpenTag, p.tools); xmlParsed != nil {
                result.ToolCalls = append(result.ToolCalls, ParsedToolCall{
                        ID:        newCallID(),
                        Name:      xmlParsed.Name,
                        Arguments: xmlParsed.Arguments,
                })
                p.emittedToolCallCnt++
                p.pendingLeadIn = ""
                return
        }

        // 2) Try JSON array
        if strings.HasPrefix(t, "[") {
                var arr []interface{}
                if err := json.Unmarshal([]byte(t), &arr); err == nil {
                        for _, item := range arr {
                                if tc := p.parseToolCall(item); tc != nil {
                                        result.ToolCalls = append(result.ToolCalls, *tc)
                                        p.emittedToolCallCnt++
                                }
                        }
                        p.pendingLeadIn = ""
                        return
                }
        }

        // 3) Try JSON object
        if strings.HasPrefix(t, "{") || strings.Contains(t, "\"name\"") ||
                strings.Contains(t, "tool_calls") || strings.Contains(t, "function_call") {
                calls := p.parseToolContent(t)
                if len(calls) > 0 {
                        for _, tc := range calls {
                                if tc.Name == "" {
                                        if attrName := extractToolName(p.currentOpenTag, t); attrName != "" {
                                                tc.Name = attrName
                                        }
                                }
                                if tc.Name != "" {
                                        result.ToolCalls = append(result.ToolCalls, tc)
                                        p.emittedToolCallCnt++
                                }
                        }
                        p.pendingLeadIn = ""
                        return
                }
        }

        // 4) Malformed and unrecoverable
        logger.Warn("[parser] Dropping malformed tool call block: %s", truncate(t, 200))
        result.Text += p.pendingLeadIn
        result.Text += p.currentOpenTag + content + toolEnd
        p.pendingLeadIn = ""
}

func (p *StreamingToolParser) tryRecoverToolCall(block string) *ParsedToolCall {
        unescaped := unescapeDoubleEscaped(block)

        if xmlParsed := parseXMLParameterToolCall(unescaped, p.currentOpenTag, p.tools); xmlParsed != nil {
                return &ParsedToolCall{
                        ID:        newCallID(),
                        Name:      xmlParsed.Name,
                        Arguments: xmlParsed.Arguments,
                }
        }

        if recovered := parseRecoverableXMLToolCall(unescaped, p.currentOpenTag, p.tools); recovered != nil {
                return &ParsedToolCall{
                        ID:        newCallID(),
                        Name:      recovered.Name,
                        Arguments: recovered.Arguments,
                }
        }

        jsonParsed := p.parseToolContent(unescaped)
        if len(jsonParsed) > 0 {
                first := jsonParsed[0]
                if first.Name == "" {
                        if attrName := extractToolName(p.currentOpenTag, unescaped); attrName != "" {
                                first.Name = attrName
                        }
                }
                if first.Name != "" {
                        return &first
                }
        }
        return nil
}

func (p *StreamingToolParser) parseToolContent(str string) []ParsedToolCall {
        var calls []ParsedToolCall

        // Single parse
        if parsed, err := robustParseJSON(str); err == nil && parsed != nil {
                if obj, ok := parsed.(map[string]interface{}); ok {
                        if tc := p.parseToolCall(obj); tc != nil {
                                calls = append(calls, *tc)
                        }
                }
        }

        for _, part := range splitTopLevelJSONValues(str) {
                parsed, err := robustParseJSON(part)
                if err != nil || parsed == nil {
                        continue
                }
                var items []interface{}
                if arr, ok := parsed.([]interface{}); ok {
                        items = arr
                } else {
                        items = []interface{}{parsed}
                }
                for _, item := range items {
                        tc := p.parseToolCall(item)
                        if tc == nil {
                                continue
                        }
                        dup := false
                        for _, c := range calls {
                                if c.ID == tc.ID || (c.Name == tc.Name && eqArgs(c.Arguments, tc.Arguments)) {
                                        dup = true
                                        break
                                }
                        }
                        if !dup {
                                calls = append(calls, *tc)
                        }
                }
        }

        // Line-by-line
        if strings.Contains(str, "\n") {
                for _, line := range strings.Split(str, "\n") {
                        line = strings.TrimSpace(line)
                        if !strings.HasPrefix(line, "{") || !strings.HasSuffix(line, "}") {
                                continue
                        }
                        var obj map[string]interface{}
                        if err := json.Unmarshal([]byte(line), &obj); err != nil {
                                continue
                        }
                        tc := p.parseToolCall(obj)
                        if tc == nil {
                                continue
                        }
                        dup := false
                        for _, c := range calls {
                                if c.Name == tc.Name && eqArgs(c.Arguments, tc.Arguments) {
                                        dup = true
                                        break
                                }
                        }
                        if !dup {
                                calls = append(calls, *tc)
                        }
                }
        }

        return calls
}

func (p *StreamingToolParser) parseToolCall(parsed interface{}) *ParsedToolCall {
        if parsed == nil {
                return nil
        }
        obj, ok := parsed.(map[string]interface{})
        if !ok {
                return nil
        }
        obj = normalizeToolCallObject(obj)

        // Handle tool_calls array
        if tc, ok := obj["tool_calls"].([]interface{}); ok && len(tc) > 0 {
                if first, ok := tc[0].(map[string]interface{}); ok {
                        return p.parseToolCall(first)
                }
        }

        // Handle function_call
        if fc, ok := obj["function_call"].(map[string]interface{}); ok {
                name, _ := fc["name"].(string)
                args := fc["arguments"]
                if args == nil {
                        args = map[string]interface{}{}
                }
                return &ParsedToolCall{
                        ID:        getStringOr(obj, "id", ""),
                        Name:      name,
                        Arguments: parseToolArguments(args),
                }
        }

        name := getStringOr(obj, "name", "")
        if fn, ok := obj["function"].(map[string]interface{}); ok && name == "" {
                name, _ = fn["name"].(string)
        }
        if name == "" {
                name = getStringOr(obj, "tool_name", getStringOr(obj, "tool", ""))
        }
        if name == "" {
                return nil
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

        id := getStringOr(obj, "id", getStringOr(obj, "tool_call_id", ""))
        if id == "" {
                id = newCallID()
        }

        return &ParsedToolCall{
                ID:        id,
                Name:      name,
                Arguments: parseToolArguments(args),
        }
}

// ─── Helpers ──────────────────────────────────────────────────────────────

type endMatch struct {
        index  int
        length int
}

func findToolEndMatch(buffer string) *endMatch {
        inString := false
        escaped := false

        for i := 0; i < len(buffer); i++ {
                ch := buffer[i]
                if escaped {
                        escaped = false
                        continue
                }
                if ch == '\\' {
                        escaped = true
                        continue
                }
                if ch == '"' {
                        inString = !inString
                        continue
                }
                if inString || ch != '<' {
                        continue
                }
                if matchesAt(buffer, i, toolEnd) {
                        return &endMatch{i, len(toolEnd)}
                }
                if matchesAt(buffer, i, toolShortEnd) {
                        return &endMatch{i, len(toolShortEnd)}
                }
                // Truncated close variants - use the tag's closing prefix
                // e.g. for "proxy_tool_call" we'd check "</proxy_tool_call" truncated
                closeTagStart := "</" + TagName
                if matchesAt(buffer, i, closeTagStart) {
                        // Check if followed by "<" (meaning a new tag starts after our truncated close)
                        if i+len(closeTagStart) < len(buffer) && buffer[i+len(closeTagStart)] == '<' {
                                after := buffer[i+len(closeTagStart):]
                                if envDetailsRE.MatchString(after) {
                                        return &endMatch{i, len(closeTagStart)}
                                }
                        }
                        // Or if it's a truncated close like "</proxy_tool_cal" (incomplete)
                        next := byte(0)
                        if i+len(closeTagStart) < len(buffer) {
                                next = buffer[i+len(closeTagStart)]
                        }
                        if next != 0 && next != '_' && next != '>' {
                                return &endMatch{i, len(closeTagStart)}
                        }
                }
        }
        return nil
}

func findRecoverableTailEndMatch(buffer string) *endMatch {
        lower := strings.ToLower(buffer)
        for _, tag := range []string{toolEnd, toolShortEnd} {
                idx := strings.LastIndex(lower, tag)
                if idx != -1 && idx+len(tag) == len(buffer) {
                        return &endMatch{idx, len(tag)}
                }
        }
        return nil
}

func matchesAt(buf string, idx int, val string) bool {
        if idx+len(val) > len(buf) {
                return false
        }
        for j := 0; j < len(val); j++ {
                c := buf[idx+j]
                t := val[j]
                if c != t && (c|0x20) != (t|0x20) {
                        return false
                }
        }
        return true
}

func findPartialToolOpenIndex(buffer string) int {
        prefix := "<" + TagName
        prefixLen := len(prefix)
        bufLen := len(buffer)

        lastPartialIdx := -1
        for i := bufLen - 1; i >= max(0, bufLen-prefixLen-1); i-- {
                if buffer[i] != '<' {
                        continue
                }
                match := true
                for j := 1; j < prefixLen && i+j < bufLen; j++ {
                        c := buffer[i+j]
                        t := prefix[j]
                        if c != t && (c|0x20) != (t|0x20) {
                                match = false
                                break
                        }
                }
                if match {
                        lastPartialIdx = i
                        break
                }
        }
        if lastPartialIdx != -1 && !strings.Contains(buffer[lastPartialIdx:], ">") {
                return lastPartialIdx
        }

        for i := 1; i <= len(prefix); i++ {
                sub := prefix[:i]
                if bufLen < len(sub) {
                        continue
                }
                match := true
                for j := 0; j < len(sub); j++ {
                        c := buffer[bufLen-len(sub)+j]
                        t := sub[j]
                        if c != t && (c|0x20) != (t|0x20) {
                                match = false
                                break
                        }
                }
                if match {
                        return bufLen - i
                }
        }
        return -1
}

func max(a, b int) int {
        if a > b {
                return a
        }
        return b
}

func decodeXMLEntities(s string) string {
        s = strings.ReplaceAll(s, "&quot;", "\"")
        s = strings.ReplaceAll(s, "&apos;", "'")
        s = strings.ReplaceAll(s, "&lt;", "<")
        s = strings.ReplaceAll(s, "&gt;", ">")
        s = strings.ReplaceAll(s, "&amp;", "&")
        return s
}

func unescapeDoubleEscaped(content string) string {
        trimmed := strings.TrimSpace(content)
        if trimmed == "" {
                return content
        }
        isJSONLike := strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[")
        isXMLLike := strings.HasPrefix(trimmed, "<")
        if !isJSONLike && !isXMLLike {
                return content
        }
        firstQuoteIdx := strings.Index(trimmed, "\"")
        if firstQuoteIdx == -1 {
                return content
        }
        firstEscapedIdx := strings.Index(trimmed, "\\\"")
        if firstEscapedIdx != -1 && (firstQuoteIdx == -1 || firstEscapedIdx < firstQuoteIdx) {
                s := strings.ReplaceAll(content, "\\\"", "\"")
                s = strings.ReplaceAll(s, "\\\\", "\\")
                return s
        }
        return content
}

func normalizeToolCallObject(parsed map[string]interface{}) map[string]interface{} {
        if t, _ := parsed["type"].(string); t == "function" {
                if fn, ok := parsed["function"].(map[string]interface{}); ok {
                        name, _ := fn["name"].(string)
                        args := fn["arguments"]
                        if args == nil {
                                args = parsed["arguments"]
                        }
                        if args == nil {
                                args = map[string]interface{}{}
                        }
                        return map[string]interface{}{
                                "id":            parsed["id"],
                                "name":          name,
                                "arguments":     args,
                                "tool_call_id":  parsed["tool_call_id"],
                        }
                }
        }
        return parsed
}

func splitTopLevelJSONValues(input string) []string {
        var values []string
        start := -1
        braceDepth := 0
        bracketDepth := 0
        inString := false
        escaped := false

        for i := 0; i < len(input); i++ {
                ch := input[i]
                if escaped {
                        escaped = false
                        continue
                }
                if ch == '\\' {
                        escaped = true
                        continue
                }
                if ch == '"' {
                        inString = !inString
                        continue
                }
                if inString {
                        continue
                }
                if (ch == '{' || ch == '[') && braceDepth == 0 && bracketDepth == 0 {
                        start = i
                }
                if ch == '{' {
                        braceDepth++
                }
                if ch == '}' {
                        braceDepth--
                }
                if ch == '[' {
                        bracketDepth++
                }
                if ch == ']' {
                        bracketDepth--
                }
                if start != -1 && braceDepth == 0 && bracketDepth == 0 && (ch == '}' || ch == ']') {
                        values = append(values, input[start:i+1])
                        start = -1
                }
        }
        return values
}

func coerceParameterValue(rawValue string) interface{} {
        value := strings.TrimSpace(decodeXMLEntities(rawValue))
        switch value {
        case "true":
                return true
        case "false":
                return false
        case "null":
                return nil
        }
        if isNumeric(value) {
                var f float64
                if _, err := fmt.Sscanf(value, "%g", &f); err == nil {
                        return f
                }
        }
        if (strings.HasPrefix(value, "{") && strings.HasSuffix(value, "}")) ||
                (strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]")) {
                var v interface{}
                if err := json.Unmarshal([]byte(value), &v); err == nil {
                        return v
                }
        }
        return value
}

func isNumeric(s string) bool {
        if s == "" {
                return false
        }
        hasDigit := false
        hasDot := false
        for i, ch := range s {
                if ch == '-' && i == 0 {
                        continue
                }
                if ch == '.' {
                        if hasDot {
                                return false
                        }
                        hasDot = true
                        continue
                }
                if ch < '0' || ch > '9' {
                        return false
                }
                hasDigit = true
        }
        return hasDigit
}

func extractToolName(openTag, block string) string {
        combined := openTag + "\n" + block
        if m := xmlNameAttrRE.FindStringSubmatch(combined); len(m) > 1 {
                return m[1]
        }
        if m := xmlNameTagRE.FindStringSubmatch(block); len(m) > 1 {
                return strings.TrimSpace(decodeXMLEntities(m[1]))
        }
        return ""
}

func inferToolNameFromParameters(args map[string]interface{}, tools []FunctionToolDefinition) string {
        if len(args) == 0 || len(tools) == 0 {
                return ""
        }
        argKeys := make([]string, 0, len(args))
        for k := range args {
                argKeys = append(argKeys, k)
        }
        sort.Strings(argKeys)

        matches := []FunctionToolDefinition{}
        for _, tool := range tools {
                props := getToolProperties(tool)
                allMatch := true
                for _, k := range argKeys {
                        if _, ok := props[k]; !ok {
                                allMatch = false
                                break
                        }
                }
                if allMatch {
                        matches = append(matches, tool)
                }
        }
        if len(matches) == 1 {
                return matches[0].Function.Name
        }
        return ""
}

func parseXMLParameterToolCall(block, openTag string, tools []FunctionToolDefinition) *struct {
        Name      string
        Arguments map[string]interface{}
} {
        args := map[string]interface{}{}
        matches := xmlParamRE.FindAllStringSubmatch(block, -1)
        for _, m := range matches {
                args[m[1]] = coerceParameterValue(m[2])
        }
        if len(args) == 0 {
                return nil
        }
        name := extractToolName(openTag, block)
        if name == "" {
                name = inferToolNameFromParameters(args, tools)
        }
        if name == "" {
                return nil
        }
        return &struct {
                Name      string
                Arguments map[string]interface{}
        }{Name: name, Arguments: args}
}

func parseRecoverableXMLToolCall(block, openTag string, tools []FunctionToolDefinition) *struct {
        Name      string
        Arguments map[string]interface{}
} {
        args := map[string]interface{}{}
        closedRE := regexp.MustCompile(`(?is)<parameter\b[^>]*\bname\s*=\s*["']([^"']+)["'][^>]*>([\s\S]*?)</parameter>`)
        matches := closedRE.FindAllStringSubmatch(block, -1)
        lastClosedEnd := 0
        if len(matches) > 0 {
                for _, m := range matches {
                        args[m[1]] = coerceParameterValue(m[2])
                        if idx := strings.Index(block[lastClosedEnd:], m[0]); idx >= 0 {
                                lastClosedEnd += idx + len(m[0])
                        }
                }
        }
        tail := block[lastClosedEnd:]
        if m := xmlUnclosedRE.FindStringSubmatch(tail); len(m) > 1 {
                args[m[1]] = coerceParameterValue(m[2])
        }
        if len(args) == 0 {
                return nil
        }
        name := extractToolName(openTag, block)
        if name == "" {
                name = inferToolNameFromParameters(args, tools)
        }
        if name == "" {
                return nil
        }
        return &struct {
                Name      string
                Arguments map[string]interface{}
        }{Name: name, Arguments: args}
}

func parseToolArguments(value interface{}) map[string]interface{} {
        switch v := value.(type) {
        case string:
                var parsed interface{}
                if err := json.Unmarshal([]byte(v), &parsed); err == nil {
                        if obj, ok := parsed.(map[string]interface{}); ok {
                                return obj
                        }
                }
                return map[string]interface{}{}
        case map[string]interface{}:
                return v
        default:
                return map[string]interface{}{}
        }
}

func getStringOr(obj map[string]interface{}, key, def string) string {
        if v, ok := obj[key]; ok {
                if s, ok := v.(string); ok && s != "" {
                        return s
                }
        }
        return def
}

func eqArgs(a, b map[string]interface{}) bool {
        aj, _ := json.Marshal(a)
        bj, _ := json.Marshal(b)
        return string(aj) == string(bj)
}

func truncate(s string, n int) string {
        if len(s) <= n {
                return s
        }
        return s[:n] + "..."
}

func newCallID() string {
        b := make([]byte, 16)
        _, _ = rand.Read(b)
        return "call_" + hex.EncodeToString(b)
}

// getToolProperties returns the JSON properties of a tool's parameters schema.
func getToolProperties(tool FunctionToolDefinition) map[string]interface{} {
        if len(tool.Function.Parameters) == 0 {
                return map[string]interface{}{}
        }
        var schema struct {
                Properties map[string]interface{} `json:"properties"`
        }
        if err := json.Unmarshal(tool.Function.Parameters, &schema); err != nil {
                return map[string]interface{}{}
        }
        if schema.Properties == nil {
                return map[string]interface{}{}
        }
        return schema.Properties
}

// getToolRequiredParams returns the required parameters set for a tool.
func getToolRequiredParams(tool FunctionToolDefinition) map[string]bool {
        required := map[string]bool{}
        if len(tool.Function.Parameters) == 0 {
                return required
        }
        var schema struct {
                Required []string `json:"required"`
        }
        if err := json.Unmarshal(tool.Function.Parameters, &schema); err != nil {
                return required
        }
        for _, r := range schema.Required {
                required[r] = true
        }
        return required
}

// robustParseJSON is a permissive JSON parser tolerant of common LLM output
// mistakes (trailing commas, missing closing braces, unquoted keys).
func robustParseJSON(str string) (interface{}, error) {
        trimmed := strings.TrimSpace(str)
        trimmed = strings.TrimPrefix(trimmed, "```json")
        trimmed = strings.TrimSuffix(trimmed, "```")
        trimmed = strings.TrimSpace(trimmed)

        firstBrace := strings.Index(trimmed, "{")
        if firstBrace == -1 {
                return nil, fmt.Errorf("no JSON object found")
        }
        jsonPart := trimmed[firstBrace:]

        // Try direct
        var v interface{}
        if err := json.Unmarshal([]byte(jsonPart), &v); err == nil {
                return v, nil
        }

        // Try cleaning up
        cleaned := quoteUnquotedKeys(jsonPart)
        cleaned = fixMissingOpeningQuotes(cleaned)
        cleaned = quoteUnquotedStringValues(cleaned)

        if err := json.Unmarshal([]byte(cleaned), &v); err == nil {
                return v, nil
        }

        // Balance braces
        balanced := balanceJSON(cleaned)
        if err := json.Unmarshal([]byte(balanced), &v); err == nil {
                return v, nil
        }

        // Aggressive cleanup
        aggressive := removeTrailingCommas(balanced)
        aggressive = balanceJSON(aggressive)
        if err := json.Unmarshal([]byte(aggressive), &v); err == nil {
                return v, nil
        }

        return nil, fmt.Errorf("could not parse JSON")
}

func quoteUnquotedKeys(input string) string {
        var out strings.Builder
        inString := false
        escaped := false

        for i := 0; i < len(input); i++ {
                ch := input[i]
                if escaped {
                        out.WriteByte(ch)
                        escaped = false
                        continue
                }
                if ch == '\\' {
                        out.WriteByte(ch)
                        escaped = true
                        continue
                }
                if ch == '"' {
                        inString = !inString
                        out.WriteByte(ch)
                        continue
                }
                if inString {
                        out.WriteByte(ch)
                        continue
                }
                if isAlpha(ch) || ch == '_' {
                        j := i
                        for j < len(input) && (isAlnum(input[j]) || input[j] == '_') {
                                j++
                        }
                        ident := input[i:j]
                        k := j
                        for k < len(input) && isSpace(input[k]) {
                                k++
                        }
                        if k < len(input) && input[k] == ':' {
                                out.WriteString("\"" + ident + "\"")
                        } else {
                                out.WriteString(ident)
                        }
                        i = j - 1
                        continue
                }
                out.WriteByte(ch)
        }
        return out.String()
}

func fixMissingOpeningQuotes(input string) string {
        // Simple stub - in practice, the model output we see is well-formed JSON
        return input
}

func quoteUnquotedStringValues(input string) string {
        // Simple stub - in practice, the model output we see is well-formed JSON
        return input
}

func balanceJSON(input string) string {
        out := strings.TrimSpace(input)
        openBraces := 0
        openBrackets := 0
        inString := false
        escaped := false
        for i := 0; i < len(out); i++ {
                ch := out[i]
                if escaped {
                        escaped = false
                        continue
                }
                if ch == '\\' {
                        escaped = true
                        continue
                }
                if ch == '"' {
                        inString = !inString
                        continue
                }
                if inString {
                        continue
                }
                if ch == '{' {
                        openBraces++
                }
                if ch == '}' {
                        openBraces--
                }
                if ch == '[' {
                        openBrackets++
                }
                if ch == ']' {
                        openBrackets--
                }
        }
        if inString {
                out += "\""
        }
        for i := 0; i < openBrackets; i++ {
                out += "]"
        }
        for i := 0; i < openBraces; i++ {
                out += "}"
        }
        return out
}

func removeTrailingCommas(input string) string {
        input = strings.ReplaceAll(input, ",}", "}")
        input = strings.ReplaceAll(input, ",]", "]")
        return input
}

func isAlpha(ch byte) bool {
        return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z')
}

func isAlnum(ch byte) bool {
        return isAlpha(ch) || (ch >= '0' && ch <= '9')
}

func isSpace(ch byte) bool {
        return ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r'
}
