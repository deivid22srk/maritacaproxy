// Package api implements the OpenAI-compatible HTTP API for MaritacaProxy.
// It exposes /v1/chat/completions, /v1/models, /health and other endpoints
// compatible with OpenAI SDK clients.
package api

import (
        "context"
        "encoding/json"
        "fmt"
        "net/http"
        "strings"
        "time"

        "github.com/deivid22srk/maritacaproxy/internal/account"
        "github.com/deivid22srk/maritacaproxy/internal/auth"
        "github.com/deivid22srk/maritacaproxy/internal/logger"
        "github.com/deivid22srk/maritacaproxy/internal/maritaca"
        "github.com/deivid22srk/maritacaproxy/internal/tools"
        "github.com/google/uuid"
)

// Server is the HTTP API server.
type Server struct {
        mgr         *account.Manager
        maritaca    *maritaca.Client
        auth        *auth.Authenticator
        apiKey      string
        maritacaURL string
        autoCreator AutoCreator
}

// AutoCreator is the interface implemented by the autocreate.Creator.
// We use an interface so the server can work without autocreate enabled.
type AutoCreator interface {
        CreateOne(ctx context.Context) (*account.Account, error)
}

// NewServer constructs a new API server.
func NewServer(mgr *account.Manager, maritacaURL, apiKey string, authCfg auth.Config) *Server {
        return &Server{
                mgr:         mgr,
                maritaca:    maritaca.NewClient(maritacaURL, 5*time.Minute),
                auth:        auth.New(authCfg),
                apiKey:      apiKey,
                maritacaURL: maritacaURL,
        }
}

// SetAutoCreator injects an autocreate.Creator for on-demand account creation.
func (s *Server) SetAutoCreator(c AutoCreator) {
        s.autoCreator = c
}

// RegisterRoutes mounts all API routes on the provided mux.
// Note: /v1/accounts/create is intentionally NOT registered here so that
// main.go can override it with the autocreate-enabled version when
// AUTO_ACCOUNT_ENABLED=true. If autocreate is disabled, the default
// (not-implemented) handler is registered via RegisterDefaultAccountsCreate.
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
        mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
        mux.HandleFunc("/v1/chat/completions/stop", s.handleChatStop)
        mux.HandleFunc("/v1/models", s.handleModels)
        mux.HandleFunc("/v1/models/", s.handleModel)
        mux.HandleFunc("/v1/accounts", s.handleAccounts)
        mux.HandleFunc("/v1/accounts/", s.handleAccountAction)
        mux.HandleFunc("/health", s.handleHealth)
        mux.HandleFunc("/", s.handleRoot)
}

// RegisterDefaultAccountsCreate registers the default (no-op) accounts/create
// handler. Called by main.go when AUTO_ACCOUNT_ENABLED=false.
func (s *Server) RegisterDefaultAccountsCreate(mux *http.ServeMux) {
        mux.HandleFunc("/v1/accounts/create", s.handleAccountsCreate)
}

// AuthMiddleware wraps the handler with API key authentication.
func (s *Server) AuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
        return func(w http.ResponseWriter, r *http.Request) {
                if s.apiKey != "" {
                        auth := r.Header.Get("Authorization")
                        if !strings.HasPrefix(auth, "Bearer ") {
                                writeJSON(w, http.StatusUnauthorized, map[string]interface{}{
                                        "error": map[string]string{"message": "Missing or invalid Authorization header"},
                                })
                                return
                        }
                        token := strings.TrimPrefix(auth, "Bearer ")
                        if token != s.apiKey {
                                writeJSON(w, http.StatusUnauthorized, map[string]interface{}{
                                        "error": map[string]string{"message": "Invalid API key"},
                                })
                                return
                        }
                }
                next(w, r)
        }
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
        if r.URL.Path == "/favicon.ico" || r.URL.Path == "/robots.txt" {
                w.WriteHeader(http.StatusNoContent)
                return
        }
        writeJSON(w, http.StatusOK, map[string]interface{}{
                "name":    "MaritacaProxy",
                "version": "0.1.0",
                "endpoints": []string{
                        "POST /v1/chat/completions",
                        "POST /v1/chat/completions/stop",
                        "GET  /v1/models",
                        "GET  /v1/models/:model",
                        "GET  /v1/accounts",
                        "POST /v1/accounts/create",
                        "DELETE /v1/accounts/:id",
                        "GET  /health",
                },
        })
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
        accounts := s.mgr.List()
        available := 0
        for _, a := range accounts {
                if !a.InUse && time.Now().After(a.CooldownUntil) && (a.AccessToken != "" || a.RefreshToken != "") {
                        available++
                }
        }
        writeJSON(w, http.StatusOK, map[string]interface{}{
                "status":    "ok",
                "timestamp": time.Now().Unix(),
                "accounts": map[string]int{
                        "total":     len(accounts),
                        "available": available,
                },
        })
}

// ─── Models ────────────────────────────────────────────────────────────────

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
        models := maritaca.DefaultModels()
        // Add thinking variants
        var allModels []map[string]interface{}
        for _, m := range models {
                allModels = append(allModels, map[string]interface{}{
                        "id":       m.ID,
                        "name":     m.Name,
                        "object":   "model",
                        "owned_by": m.OwnedBy,
                        "created":  m.Created,
                })
                allModels = append(allModels, map[string]interface{}{
                        "id":       m.ID + "-thinking",
                        "name":     m.Name + " (Thinking)",
                        "object":   "model",
                        "owned_by": m.OwnedBy,
                        "created":  m.Created,
                })
                allModels = append(allModels, map[string]interface{}{
                        "id":       m.ID + "-no-thinking",
                        "name":     m.Name + " (No Thinking)",
                        "object":   "model",
                        "owned_by": m.OwnedBy,
                        "created":  m.Created,
                })
        }
        writeJSON(w, http.StatusOK, map[string]interface{}{
                "object": "list",
                "data":   allModels,
        })
}

func (s *Server) handleModel(w http.ResponseWriter, r *http.Request) {
        modelID := strings.TrimPrefix(r.URL.Path, "/v1/models/")
        models := maritaca.DefaultModels()
        for _, m := range models {
                if m.ID == modelID {
                        writeJSON(w, http.StatusOK, map[string]interface{}{
                                "id":       m.ID,
                                "name":     m.Name,
                                "object":   "model",
                                "owned_by": m.OwnedBy,
                                "created":  m.Created,
                        })
                        return
                }
                if m.ID+"-thinking" == modelID || m.ID+"-no-thinking" == modelID {
                        writeJSON(w, http.StatusOK, map[string]interface{}{
                                "id":       modelID,
                                "name":     m.Name,
                                "object":   "model",
                                "owned_by": m.OwnedBy,
                                "created":  m.Created,
                        })
                        return
                }
        }
        writeJSON(w, http.StatusNotFound, map[string]interface{}{
                "error": map[string]string{"message": "Model not found"},
        })
}

// ─── Chat Completions ──────────────────────────────────────────────────────

// OpenAIRequest is the OpenAI-compatible chat completion request.
type OpenAIRequest struct {
        Model              string                   `json:"model"`
        Messages           []map[string]interface{} `json:"messages"`
        Stream             bool                     `json:"stream"`
        Tools              []tools.FunctionToolDefinition `json:"tools"`
        ToolChoice         interface{}              `json:"tool_choice"`
        ParallelToolCalls  *bool                    `json:"parallel_tool_calls"`
        StreamOptions      *struct {
                IncludeUsage bool `json:"include_usage"`
        } `json:"stream_options"`
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodPost {
                writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{"error": map[string]string{"message": "method not allowed"}})
                return
        }

        var req OpenAIRequest
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": map[string]string{"message": "invalid request: " + err.Error()}})
                return
        }

        // Build the final prompt from messages
        prompt, hasTools, toolChoiceMode, _, candidateTools, isThinking := s.buildPrompt(&req)

        // Try up to 3 accounts. If an account hits quota (402), mark it on cooldown
        // and rotate to the next. If no other account is available and auto-create
        // is enabled, create a new account on-demand.
        maxAttempts := 3
        var acc *account.Account
        var chatID string
        triedIDs := map[string]bool{}

        for attempt := 1; attempt <= maxAttempts; attempt++ {
                // Get next available account (excluding ones we've already tried)
                acc = s.mgr.GetNextAvailable(triedIDs)
                if acc == nil {
                        // No available accounts - try auto-create if enabled.
                        // Use background context (NOT r.Context()) so the Playwright process
                        // isn't killed when the HTTP client times out or disconnects.
                        if s.autoCreator != nil {
                                logger.Info("[chat] No accounts available - auto-creating one (attempt %d/%d, background ctx)", attempt, maxAttempts)
                                ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
                                newAcc, err := s.autoCreator.CreateOne(ctx)
                                cancel()
                                if err != nil {
                                        logger.Error("[chat] Auto-create failed: %v", err)
                                        writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
                                                "error": map[string]string{"message": fmt.Sprintf("no accounts available and auto-create failed: %v", err)},
                                        })
                                        return
                                }
                                acc = newAcc
                                logger.Info("[chat] Auto-created new account: %s (id=%s)", acc.Email, acc.ID)
                        } else {
                                writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
                                        "error": map[string]string{"message": "No available accounts. Add accounts via POST /v1/accounts/create or set AUTO_ACCOUNT_ENABLED=true."},
                                })
                                return
                        }
                }

                triedIDs[acc.ID] = true
                logger.Info("[chat] Attempt %d/%d: routing to account %s (id=%s)", attempt, maxAttempts, acc.Email, acc.ID)
                s.mgr.MarkInUse(acc.ID)

                // Refresh token if needed
                if acc.IsTokenExpired() && acc.RefreshToken != "" {
                        logger.Info("[chat] Refreshing token for %s", acc.Email)
                        tokens, err := s.auth.RefreshToken(acc.RefreshToken)
                        if err != nil {
                                logger.Error("[chat] Token refresh failed: %v", err)
                                s.mgr.MarkCooldown(acc.ID, 5*time.Minute, "token-refresh-failed")
                                s.mgr.ReleaseInUse(acc.ID)
                                continue
                        }
                        if err := s.mgr.UpdateTokens(acc.ID, tokens.AccessToken, tokens.RefreshToken, tokens.ExpiresIn); err != nil {
                                logger.Warn("[chat] Failed to persist refreshed token: %v", err)
                        }
                        acc.AccessToken = tokens.AccessToken
                        acc.RefreshToken = tokens.RefreshToken
                        acc.TokenExpiry = time.Now().Add(time.Duration(tokens.ExpiresIn) * time.Second)
                }

                // Create chat session
                cid, err := s.maritaca.CreateChat(acc.AccessToken)
                if err != nil {
                        logger.Error("[chat] CreateChat failed for %s: %v", acc.Email, err)
                        if maritaca.IsQuotaExceeded(err) {
                                // Quota exceeded - long cooldown (6h, matches Maritaca's reset window)
                                s.mgr.MarkCooldown(acc.ID, 6*time.Hour, "quota-exceeded")
                                s.mgr.ReleaseInUse(acc.ID)
                                logger.Warn("[chat] Account %s hit quota limit, rotating...", acc.Email)
                                continue
                        }
                        // Other errors - short cooldown
                        s.mgr.MarkCooldown(acc.ID, 1*time.Minute, "create-chat-failed")
                        s.mgr.ReleaseInUse(acc.ID)
                        continue
                }
                chatID = cid
                logger.Info("[chat] Created chat session: %s", chatID)
                break
        }

        if acc == nil || chatID == "" {
                writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
                        "error": map[string]string{"message": "All accounts exhausted or quota-exceeded. Try again later or add more accounts."},
                })
                return
        }

        // Release in-use when the response is done (deferred)
        defer s.mgr.ReleaseInUse(acc.ID)

        // Build message request
        msgReq := maritaca.MessageRequest{
                ChatID:          chatID,
                Content:         prompt,
                Model:           isThinking.Model,
                Position:        0,
                IsUser:          true,
                Files:           []interface{}{},
                WebSearch:       false,
                CodeExecution:   false,
                DataOcean:       false,
                Reasoning:       isThinking.Reasoning,
                UseCompetitor:   false,
                ComparisonMode:  false,
                SourceInterface: "web",
                Delta:           true,
        }

        completionID := "chatcmpl-" + uuid.NewString()
        created := time.Now().Unix()

        if req.Stream {
                s.handleStreamChat(w, acc, msgReq, completionID, created, req.Model, hasTools, toolChoiceMode, candidateTools, req.Tools)
        } else {
                s.handleNonStreamChat(w, acc, msgReq, completionID, req.Model, hasTools, toolChoiceMode, candidateTools, req.Tools)
        }
}

// promptInfo holds parsed model info.
type promptInfo struct {
        Model     string
        Reasoning bool
}

// buildPrompt converts OpenAI messages into a single text prompt compatible
// with Maritaca's chat API. Tool definitions are injected as a system
// instruction with the contract format.
func (s *Server) buildPrompt(req *OpenAIRequest) (prompt string, hasTools bool, toolChoiceMode tools.ToolChoiceMode, forcedToolName string, candidateTools []tools.FunctionToolDefinition, info promptInfo) {
        var systemPrompt, body strings.Builder

        // Map tool_call IDs to names for "tool" role messages
        toolCallIDToName := map[string]string{}
        for _, msg := range req.Messages {
                role, _ := msg["role"].(string)
                if role == "assistant" {
                        if tcs, ok := msg["tool_calls"].([]interface{}); ok {
                                for _, tc := range tcs {
                                        if tcObj, ok := tc.(map[string]interface{}); ok {
                                                id, _ := tcObj["id"].(string)
                                                if fn, ok := tcObj["function"].(map[string]interface{}); ok {
                                                        name, _ := fn["name"].(string)
                                                        if id != "" && name != "" {
                                                                toolCallIDToName[id] = name
                                                        }
                                                }
                                        }
                                }
                        }
                }
        }

        for _, msg := range req.Messages {
                role, _ := msg["role"].(string)
                var contentStr string
                switch c := msg["content"].(type) {
                case string:
                        contentStr = c
                case []interface{}:
                        var parts []string
                        for _, p := range c {
                                if pObj, ok := p.(map[string]interface{}); ok {
                                        if t, _ := pObj["type"].(string); t == "text" {
                                                if text, _ := pObj["text"].(string); text != "" {
                                                        parts = append(parts, text)
                                                }
                                        }
                                }
                        }
                        contentStr = strings.Join(parts, "\n")
                case map[string]interface{}:
                        b, _ := json.Marshal(c)
                        contentStr = string(b)
                case nil:
                        contentStr = ""
                }

                switch role {
                case "system":
                        systemPrompt.WriteString(contentStr + "\n\n")
                case "user":
                        body.WriteString("User: " + contentStr + "\n\n")
                case "assistant":
                        assistantContent := contentStr
                        if tcs, ok := msg["tool_calls"].([]interface{}); ok {
                                for _, tc := range tcs {
                                        tcObj, ok := tc.(map[string]interface{})
                                        if !ok {
                                                continue
                                        }
                                        fn, _ := tcObj["function"].(map[string]interface{})
                                        if fn == nil {
                                                continue
                                        }
                                        name, _ := fn["name"].(string)
                                        args := fn["arguments"]
                                        var argsMap map[string]interface{}
                                        switch a := args.(type) {
                                        case string:
                                                _ = json.Unmarshal([]byte(a), &argsMap)
                                        case map[string]interface{}:
                                                argsMap = a
                                        }
                                        payload, _ := json.Marshal(map[string]interface{}{
                                                "name":      name,
                                                "arguments": argsMap,
                                        })
                                        openTag := "<" + tools.TagName + ">"
                                        closeTag := "</" + tools.TagName + ">"
                                        toolCallStr := "\n" + openTag + "\n" + string(payload) + "\n" + closeTag
                                        if assistantContent != "" {
                                                assistantContent += toolCallStr
                                        } else {
                                                assistantContent = strings.TrimPrefix(toolCallStr, "\n")
                                        }
                                }
                        }
                        body.WriteString("Assistant: " + strings.TrimSpace(assistantContent) + "\n\n")
                case "tool", "function":
                        toolName, _ := msg["name"].(string)
                        if toolName == "" {
                                if id, _ := msg["tool_call_id"].(string); id != "" {
                                        toolName = toolCallIDToName[id]
                                }
                        }
                        if toolName == "" {
                                toolName = "tool"
                        }
                        body.WriteString(fmt.Sprintf("Tool Response (%s): %s\n", toolName, contentStr))
                }
        }

        // Inject tool contract if tools are present
        hasTools = len(req.Tools) > 0
        toolChoiceMode = tools.GetToolChoiceMode(req.ToolChoice)
        forcedToolName = tools.GetForcedToolName(req.ToolChoice)

        if hasTools && toolChoiceMode != tools.ToolChoiceNone {
                // Format tools for system prompt
                formattedTools := make([]map[string]interface{}, 0, len(req.Tools))
                for _, t := range req.Tools {
                        if t.Type == "function" {
                                formattedTools = append(formattedTools, map[string]interface{}{
                                        "name":        t.Function.Name,
                                        "description": t.Function.Description,
                                        "parameters":  json.RawMessage(t.Function.Parameters),
                                })
                        }
                }
                toolsJSON, _ := json.Marshal(formattedTools)
                systemPrompt.WriteString(fmt.Sprintf("\n\n# TOOLS AVAILABLE\nYou have access to the following tools:\n%s\n\n", string(toolsJSON)))
                openTag := "<" + tools.TagName + ">"
                closeTag := "</" + tools.TagName + ">"
                systemPrompt.WriteString(fmt.Sprintf(`# TOOL CALLING FORMAT (MANDATORY)
CRITICAL: The platform you are running on has native <tool_call> tags that get INTERCEPTED by the backend. To call user-provided tools, you MUST use the custom tag %s instead. Do NOT use <tool_call> under any circumstances — it will be intercepted and rejected. Use %s / %s.

You do NOT have any built-in/native tools (web_search, code_execution, data_ocean are all DISABLED). The ONLY way to invoke a tool is by emitting a JSON object wrapped EXACTLY in %s tags as shown below:

%s
{"name": "tool_name", "arguments": {"param_name": "value"}}
%s

EXAMPLE OF MULTIPLE TOOL CALLS:
%s
{"name": "read_file", "arguments": {"path": "file1.txt"}}
%s
%s
{"name": "read_file", "arguments": {"path": "file2.txt"}}
%s

CRITICAL RULES:
1. ONLY use the %s/%s tags above. NEVER use <tool_call> or output raw JSON without tags.
2. You can call multiple tools by outputting multiple %s blocks consecutively.
3. Do NOT output any other text (explanations, chat, etc.) after your %s blocks. Wait for the user to provide the tool response.
4. The JSON inside the tags MUST be valid and include ALL required braces and the "arguments" field.
5. If you need to use a tool, do it IMMEDIATELY without preamble.
6. NEVER invent, guess, or hallucinate tool names. You MUST ONLY use the exact tool names provided in the 'TOOLS AVAILABLE' list above.
7. Do NOT mention that a tool failed or is unavailable. If a tool is listed, assume it works.
8. NEVER use <tool_call> — it will be intercepted by the platform's native tool system and return "Unknown tool name" errors. Use %s / %s.

`, openTag, openTag, closeTag, openTag, openTag, closeTag, openTag, closeTag, openTag, closeTag, openTag, closeTag, openTag, openTag, openTag, closeTag))

                if forcedToolName != "" {
                        systemPrompt.WriteString(fmt.Sprintf("CRITICAL: You MUST call the tool \"%s\" in this response.\n\n", forcedToolName))
                }

                // Select candidate tools and build contract
                recentToolNames := tools.GetRecentToolNames(req.Messages)
                contextText := systemPrompt.String() + body.String()
                candidateTools = tools.SelectCandidateTools(req.Tools, contextText, forcedToolName, recentToolNames, 12)
        }

        // Build final prompt
        if hasTools && toolChoiceMode != tools.ToolChoiceNone {
                parallelToolCalls := req.ParallelToolCalls == nil || *req.ParallelToolCalls
                if toolChoiceMode == tools.ToolChoiceForced {
                        parallelToolCalls = false
                }
                contract := tools.BuildToolCallContract(candidateTools, forcedToolName, parallelToolCalls)
                manifest := tools.BuildCompactToolManifest(candidateTools, forcedToolName)
                body.WriteString("\n\n" + contract)
                if manifest != "" {
                        body.WriteString("\n\n" + manifest)
                }
        }

        if hasTools && toolChoiceMode == tools.ToolChoiceNone {
                body.WriteString("\n\n[TOOL USE DISABLED]\nDo not call tools in this response. Answer directly using available context.")
        }

        // Determine model & reasoning
        info.Model = strings.ReplaceAll(strings.ReplaceAll(req.Model, "-no-thinking", ""), "-thinking", "")
        info.Reasoning = !strings.Contains(req.Model, "no-thinking")

        prompt = ""
        if systemPrompt.Len() > 0 {
                prompt = systemPrompt.String() + body.String()
        } else {
                prompt = body.String()
        }
        return prompt, hasTools, toolChoiceMode, forcedToolName, candidateTools, info
}

func (s *Server) handleStreamChat(
        w http.ResponseWriter,
        acc *account.Account,
        msgReq maritaca.MessageRequest,
        completionID string,
        created int64,
        model string,
        hasTools bool,
        toolChoiceMode tools.ToolChoiceMode,
        candidateTools []tools.FunctionToolDefinition,
        allTools []tools.FunctionToolDefinition,
) {
        flusher, ok := w.(http.Flusher)
        if !ok {
                writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": map[string]string{"message": "streaming not supported"}})
                return
        }

        w.Header().Set("Content-Type", "text/event-stream")
        w.Header().Set("Cache-Control", "no-cache, no-transform")
        w.Header().Set("Connection", "keep-alive")
        w.Header().Set("X-Accel-Buffering", "no")
        w.WriteHeader(http.StatusOK)
        flusher.Flush()

        // Initial heartbeat
        fmt.Fprint(w, ": heartbeat\n\n")
        flusher.Flush()

        writeEvent := func(data interface{}) {
                b, _ := json.Marshal(data)
                fmt.Fprintf(w, "data: %s\n\n", string(b))
                flusher.Flush()
        }

        // Initial role event
        writeEvent(map[string]interface{}{
                "id":      completionID,
                "object":  "chat.completion.chunk",
                "created": created,
                "model":   model,
                "choices": []map[string]interface{}{
                        {
                                "index":         0,
                                "delta":         map[string]interface{}{"role": "assistant", "content": ""},
                                "logprobs":      nil,
                                "finish_reason": nil,
                        },
                },
        })

        // Tool parser (only if tools enabled)
        var toolParser *tools.StreamingToolParser
        if hasTools && toolChoiceMode != tools.ToolChoiceNone {
                toolParser = tools.NewStreamingToolParser(candidateTools)
        }

        emittedToolCallIDs := map[string]bool{}
        emitToolCall := func(tc tools.ParsedToolCall, index int) {
                if emittedToolCallIDs[tc.ID] {
                        return
                }
                emittedToolCallIDs[tc.ID] = true
                argsJSON, _ := json.Marshal(tc.Arguments)
                writeEvent(map[string]interface{}{
                        "id":      completionID,
                        "object":  "chat.completion.chunk",
                        "created": created,
                        "model":   model,
                        "choices": []map[string]interface{}{
                                {
                                        "index":         0,
                                        "delta":         map[string]interface{}{"tool_calls": []map[string]interface{}{
                                                {
                                                        "index":    index,
                                                        "id":       tc.ID,
                                                        "type":     "function",
                                                        "function": map[string]interface{}{"name": tc.Name, "arguments": string(argsJSON)},
                                                },
                                        }},
                                        "logprobs":      nil,
                                        "finish_reason": nil,
                                },
                        },
                })
        }

        // Send the message and stream
        var lastFinishReason string = "stop"
        promptTokens := len(msgReq.Content) / 4
        completionTokens := 0

        events := maritaca.StreamEvents{
                OnStart: func() {
                        logger.Debug("[chat] Stream started")
                },
                OnText: func(text string) {
                        if toolParser != nil {
                                result := toolParser.Feed(text)
                                if result.Text != "" {
                                        if tools.LooksLikeUnwrappedToolCall(result.Text) {
                                                baseIdx := toolParser.EmittedToolCallCount()
                                                for i, tc := range tools.ParseUnwrappedToolCalls(result.Text) {
                                                        emitToolCall(tc, baseIdx+i)
                                                }
                                        } else {
                                                writeEvent(map[string]interface{}{
                                                        "id":      completionID,
                                                        "object":  "chat.completion.chunk",
                                                        "created": created,
                                                        "model":   model,
                                                        "choices": []map[string]interface{}{
                                                                {
                                                                        "index":         0,
                                                                        "delta":         map[string]interface{}{"content": result.Text},
                                                                        "logprobs":      nil,
                                                                        "finish_reason": nil,
                                                                },
                                                        },
                                                })
                                        }
                                }
                                for i, tc := range result.ToolCalls {
                                        emitToolCall(tc, toolParser.EmittedToolCallCount()-len(result.ToolCalls)+i)
                                }
                        } else {
                                completionTokens += len(text) / 4
                                writeEvent(map[string]interface{}{
                                        "id":      completionID,
                                        "object":  "chat.completion.chunk",
                                        "created": created,
                                        "model":   model,
                                        "choices": []map[string]interface{}{
                                                {
                                                        "index":         0,
                                                        "delta":         map[string]interface{}{"content": text},
                                                        "logprobs":      nil,
                                                        "finish_reason": nil,
                                                },
                                        },
                                })
                        }
                },
                OnReasoning: func(text string) {
                        writeEvent(map[string]interface{}{
                                "id":      completionID,
                                "object":  "chat.completion.chunk",
                                "created": created,
                                "model":   model,
                                "choices": []map[string]interface{}{
                                        {
                                                "index":         0,
                                                "delta":         map[string]interface{}{"reasoning_content": text},
                                                "logprobs":      nil,
                                                "finish_reason": nil,
                                        },
                                },
                        })
                },
                OnError: func(err error) {
                        logger.Error("[chat] Stream error: %v", err)
                },
        }

        if err := s.maritaca.SendMessage(acc.AccessToken, msgReq, events); err != nil {
                logger.Error("[chat] SendMessage failed: %v", err)
                // If this is a quota-exceeded error, mark account on cooldown so
                // the next request will pick a different account (or auto-create one).
                if maritaca.IsQuotaExceeded(err) {
                        s.mgr.MarkCooldown(acc.ID, 6*time.Hour, "quota-exceeded-stream")
                        logger.Warn("[chat] Account %s hit quota limit during stream - marked for 6h cooldown", acc.Email)
                }
                writeEvent(map[string]interface{}{
                        "id":      completionID,
                        "object":  "chat.completion.chunk",
                        "created": created,
                        "model":   model,
                        "choices": []map[string]interface{}{
                                {
                                        "index":         0,
                                        "delta":         map[string]interface{}{"content": "[stream error: " + err.Error() + "]"},
                                        "logprobs":      nil,
                                        "finish_reason": "stop",
                                },
                        },
                })
        }

        // Flush remaining buffer from tool parser
        if toolParser != nil {
                flushResult := toolParser.Flush()
                if flushResult.Text != "" {
                        if tools.LooksLikeUnwrappedToolCall(flushResult.Text) {
                                baseIdx := toolParser.EmittedToolCallCount()
                                for i, tc := range tools.ParseUnwrappedToolCalls(flushResult.Text) {
                                        emitToolCall(tc, baseIdx+i)
                                }
                        } else {
                                writeEvent(map[string]interface{}{
                                        "id":      completionID,
                                        "object":  "chat.completion.chunk",
                                        "created": created,
                                        "model":   model,
                                        "choices": []map[string]interface{}{
                                                {
                                                        "index":         0,
                                                        "delta":         map[string]interface{}{"content": flushResult.Text},
                                                        "logprobs":      nil,
                                                        "finish_reason": nil,
                                                },
                                        },
                                })
                        }
                }
                for i, tc := range flushResult.ToolCalls {
                        emitToolCall(tc, toolParser.EmittedToolCallCount()-len(flushResult.ToolCalls)+i)
                }
                if toolParser.EmittedToolCallCount() > 0 {
                        lastFinishReason = "tool_calls"
                }
        }

        // Final usage chunk
        usage := map[string]interface{}{
                "prompt_tokens":         promptTokens,
                "completion_tokens":     completionTokens,
                "total_tokens":          promptTokens + completionTokens,
                "prompt_tokens_details": map[string]int{"cached_tokens": 0},
        }

        writeEvent(map[string]interface{}{
                "id":      completionID,
                "object":  "chat.completion.chunk",
                "created": created,
                "model":   model,
                "choices": []map[string]interface{}{
                        {
                                "index":         0,
                                "delta":         map[string]interface{}{},
                                "logprobs":      nil,
                                "finish_reason": lastFinishReason,
                        },
                },
                "usage": usage,
        })

        fmt.Fprint(w, "data: [DONE]\n\n")
        flusher.Flush()
}

func (s *Server) handleNonStreamChat(
        w http.ResponseWriter,
        acc *account.Account,
        msgReq maritaca.MessageRequest,
        completionID string,
        model string,
        hasTools bool,
        toolChoiceMode tools.ToolChoiceMode,
        candidateTools []tools.FunctionToolDefinition,
        allTools []tools.FunctionToolDefinition,
) {
        var fullContent strings.Builder
        var reasoningContent strings.Builder
        var toolCallsOut []map[string]interface{}
        seenToolCallIDs := map[string]bool{}

        var toolParser *tools.StreamingToolParser
        if hasTools && toolChoiceMode != tools.ToolChoiceNone {
                toolParser = tools.NewStreamingToolParser(candidateTools)
        }

        pushToolCall := func(tc tools.ParsedToolCall, index int) {
                if seenToolCallIDs[tc.ID] {
                        return
                }
                seenToolCallIDs[tc.ID] = true
                argsJSON, _ := json.Marshal(tc.Arguments)
                toolCallsOut = append(toolCallsOut, map[string]interface{}{
                        "index": index,
                        "id":    tc.ID,
                        "type":  "function",
                        "function": map[string]interface{}{
                                "name":      tc.Name,
                                "arguments": string(argsJSON),
                        },
                })
        }

        events := maritaca.StreamEvents{
                OnText: func(text string) {
                        if toolParser != nil {
                                result := toolParser.Feed(text)
                                if result.Text != "" {
                                        if tools.LooksLikeUnwrappedToolCall(result.Text) {
                                                baseIdx := toolParser.EmittedToolCallCount()
                                                for i, tc := range tools.ParseUnwrappedToolCalls(result.Text) {
                                                        pushToolCall(tc, baseIdx+i)
                                                }
                                        } else {
                                                fullContent.WriteString(result.Text)
                                        }
                                }
                                for i, tc := range result.ToolCalls {
                                        pushToolCall(tc, toolParser.EmittedToolCallCount()-len(result.ToolCalls)+i)
                                }
                        } else {
                                fullContent.WriteString(text)
                        }
                },
                OnReasoning: func(text string) {
                        reasoningContent.WriteString(text)
                },
        }

        if err := s.maritaca.SendMessage(acc.AccessToken, msgReq, events); err != nil {
                logger.Error("[chat] SendMessage failed: %v", err)
                // Mark quota-exceeded accounts for 6h cooldown so next request rotates
                if maritaca.IsQuotaExceeded(err) {
                        s.mgr.MarkCooldown(acc.ID, 6*time.Hour, "quota-exceeded-nonstream")
                        logger.Warn("[chat] Account %s hit quota limit - marked for 6h cooldown", acc.Email)
                }
                writeJSON(w, http.StatusBadGateway, map[string]interface{}{"error": map[string]string{"message": err.Error()}})
                return
        }

        // Flush tool parser
        if toolParser != nil {
                flushResult := toolParser.Flush()
                if flushResult.Text != "" {
                        if tools.LooksLikeUnwrappedToolCall(flushResult.Text) {
                                baseIdx := toolParser.EmittedToolCallCount()
                                for i, tc := range tools.ParseUnwrappedToolCalls(flushResult.Text) {
                                        pushToolCall(tc, baseIdx+i)
                                }
                        } else {
                                fullContent.WriteString(flushResult.Text)
                        }
                }
                for i, tc := range flushResult.ToolCalls {
                        pushToolCall(tc, toolParser.EmittedToolCallCount()-len(flushResult.ToolCalls)+i)
                }
        }

        promptTokens := len(msgReq.Content) / 4
        completionTokens := fullContent.Len() / 4
        finishReason := "stop"
        if len(toolCallsOut) > 0 {
                finishReason = "tool_calls"
        }

        message := map[string]interface{}{
                "role":    "assistant",
                "content": nil,
        }
        if len(toolCallsOut) == 0 {
                message["content"] = fullContent.String()
        }
        if reasoningContent.Len() > 0 {
                message["reasoning_content"] = reasoningContent.String()
        }
        if len(toolCallsOut) > 0 {
                message["tool_calls"] = toolCallsOut
        }

        writeJSON(w, http.StatusOK, map[string]interface{}{
                "id":      completionID,
                "object":  "chat.completion",
                "created": time.Now().Unix(),
                "model":   model,
                "choices": []map[string]interface{}{
                        {
                                "index":         0,
                                "message":       message,
                                "logprobs":      nil,
                                "finish_reason": finishReason,
                        },
                },
                "usage": map[string]interface{}{
                        "prompt_tokens":         promptTokens,
                        "completion_tokens":     completionTokens,
                        "total_tokens":          promptTokens + completionTokens,
                        "prompt_tokens_details": map[string]int{"cached_tokens": 0},
                },
        })
}

// handleChatStop aborts an in-progress chat generation.
func (s *Server) handleChatStop(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodPost {
                writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{"error": map[string]string{"message": "method not allowed"}})
                return
        }
        var body struct {
                ChatID string `json:"chat_id"`
        }
        if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
                writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": map[string]string{"message": err.Error()}})
                return
        }
        // Best-effort stop - we don't track active streams per chat for now
        writeJSON(w, http.StatusOK, map[string]interface{}{"success": true})
}

// ─── Account management ────────────────────────────────────────────────────

func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodGet {
                writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{"error": map[string]string{"message": "method not allowed"}})
                return
        }
        accounts := s.mgr.List()
        items := make([]map[string]interface{}, 0, len(accounts))
        for _, a := range accounts {
                items = append(items, map[string]interface{}{
                        "id":             a.ID,
                        "email":          a.Email,
                        "in_use":         a.InUse,
                        "cooldown_until": a.CooldownUntil,
                        "token_expiry":   a.TokenExpiry,
                        "has_token":      a.AccessToken != "",
                        "has_refresh":    a.RefreshToken != "",
                        "created_at":     a.CreatedAt,
                        "last_used":      a.LastUsed,
                })
        }
        writeJSON(w, http.StatusOK, map[string]interface{}{
                "object": "list",
                "data":   items,
        })
}

// handleAccountsCreate is set up by main.go because it needs the autocreate.Creator.
func (s *Server) handleAccountsCreate(w http.ResponseWriter, r *http.Request) {
        // Placeholder - replaced by main.go
        writeJSON(w, http.StatusNotImplemented, map[string]interface{}{
                "error": map[string]string{"message": "auto-account creation not enabled - set AUTO_ACCOUNT_ENABLED=true"},
        })
}

func (s *Server) handleAccountAction(w http.ResponseWriter, r *http.Request) {
        parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/accounts/"), "/")
        if len(parts) == 0 || parts[0] == "" {
                writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": map[string]string{"message": "account id required"}})
                return
        }
        accID := parts[0]
        acc := s.mgr.Get(accID)
        if acc == nil {
                writeJSON(w, http.StatusNotFound, map[string]interface{}{"error": map[string]string{"message": "account not found"}})
                return
        }
        switch r.Method {
        case http.MethodDelete:
                if err := s.mgr.Remove(accID); err != nil {
                        writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": map[string]string{"message": err.Error()}})
                        return
                }
                writeJSON(w, http.StatusOK, map[string]interface{}{"success": true})
        case http.MethodGet:
                writeJSON(w, http.StatusOK, map[string]interface{}{
                        "id":             acc.ID,
                        "email":          acc.Email,
                        "in_use":         acc.InUse,
                        "cooldown_until": acc.CooldownUntil,
                        "token_expiry":   acc.TokenExpiry,
                        "has_token":      acc.AccessToken != "",
                        "has_refresh":    acc.RefreshToken != "",
                        "created_at":     acc.CreatedAt,
                        "last_used":      acc.LastUsed,
                })
        default:
                writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{"error": map[string]string{"message": "method not allowed"}})
        }
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
        w.Header().Set("Content-Type", "application/json")
        w.WriteHeader(status)
        _ = json.NewEncoder(w).Encode(v)
}
