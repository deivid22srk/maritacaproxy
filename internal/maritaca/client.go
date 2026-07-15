// Package maritaca implements the chat API client used to talk to
// chat.maritaca.ai. It handles chat creation, message streaming (SSE),
// history, and converts between OpenAI-compatible requests and Maritaca's
// internal schema.
package maritaca

import (
        "bufio"
        "bytes"
        "encoding/json"
        "fmt"
        "io"
        "net/http"
        "strings"
        "time"

        "github.com/deivid22srk/maritacaproxy/internal/logger"
)

// Client is the HTTP client for Maritaca chat API.
type Client struct {
        BaseURL string
        HTTP    *http.Client
}

// NewClient constructs a new Maritaca chat client.
func NewClient(baseURL string, timeout time.Duration) *Client {
        return &Client{
                BaseURL: baseURL,
                HTTP: &http.Client{
                        Timeout: timeout,
                        CheckRedirect: func(req *http.Request, via []*http.Request) error {
                                return http.ErrUseLastResponse
                        },
                },
        }
}

// CreateChat creates a new chat session and returns the chat_id.
func (c *Client) CreateChat(accessToken string) (string, error) {
        req, _ := http.NewRequest("POST", c.BaseURL+"/api/chat/create", nil)
        req.Header.Set("Authorization", "Auth "+accessToken)
        req.Header.Set("Content-Type", "application/json")

        resp, err := c.HTTP.Do(req)
        if err != nil {
                return "", fmt.Errorf("create chat failed: %w", err)
        }
        defer resp.Body.Close()
        body, _ := io.ReadAll(resp.Body)
        if resp.StatusCode >= 400 {
                return "", fmt.Errorf("create chat failed (%d): %s", resp.StatusCode, string(body))
        }

        var data struct {
                ChatID string `json:"chat_id"`
                ID     string `json:"id"`
                ChatData struct {
                        ChatID string `json:"chat_id"`
                        ID     string `json:"id"`
                } `json:"chat_data"`
        }
        if err := json.Unmarshal(body, &data); err != nil {
                return "", fmt.Errorf("create chat parse error: %w (body=%s)", err, string(body))
        }
        if data.ChatData.ChatID != "" {
                return data.ChatData.ChatID, nil
        }
        if data.ChatID != "" {
                return data.ChatID, nil
        }
        if data.ChatData.ID != "" {
                return data.ChatData.ID, nil
        }
        return data.ID, nil
}

// MessageRequest is the body sent to /api/chat/message.
type MessageRequest struct {
        ChatID         string        `json:"chat_id"`
        Content        string        `json:"content"`
        Model          string        `json:"model"`
        Position       int           `json:"position"`
        IsUser         bool          `json:"is_user"`
        Files          []interface{} `json:"files"`
        WebSearch      bool          `json:"web_search"`
        CodeExecution  bool          `json:"code_execution"`
        DataOcean      bool          `json:"data_ocean"`
        Reasoning      bool          `json:"reasoning"`
        UseCompetitor  bool          `json:"use_competitor"`
        ComparisonMode bool          `json:"comparison_mode"`
        SourceInterface string       `json:"source_interface"`
        Delta          bool          `json:"delta,omitempty"`
}

// StreamEvents represents the different SSE event types emitted by Maritaca.
type StreamEvents struct {
        // OnStart is called once when the stream starts.
        OnStart func()
        // OnText is called for each text delta from the model.
        OnText func(text string)
        // OnReasoning is called for each reasoning chunk (thinking mode).
        OnReasoning func(text string)
        // OnReferences is called when references are emitted.
        OnReferences func(refs interface{})
        // OnStatus is called for status updates.
        OnStatus func(status string)
        // OnToolResult is called for tool execution results.
        OnToolResult func(result interface{})
        // OnError is called when an error event is emitted.
        OnError func(err error)
        // OnDone is called when the stream completes.
        OnDone func()
}

// SendMessage sends a chat message and streams the response via SSE.
// The provided events callbacks are invoked as events arrive.
// Returns when the stream terminates.
func (c *Client) SendMessage(accessToken string, msg MessageRequest, events StreamEvents) error {
        bodyBytes, _ := json.Marshal(msg)
        req, _ := http.NewRequest("POST", c.BaseURL+"/api/chat/message", bytes.NewReader(bodyBytes))
        req.Header.Set("Authorization", "Auth "+accessToken)
        req.Header.Set("Content-Type", "application/json")
        req.Header.Set("Accept", "text/event-stream")
        req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/137.0.0.0 Safari/537.36")
        req.Header.Set("Origin", "https://chat.maritaca.ai")
        req.Header.Set("Referer", "https://chat.maritaca.ai/")

        resp, err := c.HTTP.Do(req)
        if err != nil {
                return fmt.Errorf("send message failed: %w", err)
        }
        defer resp.Body.Close()

        if resp.StatusCode >= 400 {
                body, _ := io.ReadAll(resp.Body)
                return fmt.Errorf("send message failed (%d): %s", resp.StatusCode, string(body))
        }

        return parseSSEStream(resp.Body, events)
}

// parseSSEStream reads an SSE stream line by line and dispatches events.
// Maritaca emits events in the format:
//   event: <name>
//   data: <json>
//
// Event names include: start, message (text), reasoning, reasoning_delta,
// references, status, tool_result, error, done.
func parseSSEStream(r io.Reader, events StreamEvents) error {
        scanner := bufio.NewScanner(r)
        scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

        var eventName string
        for scanner.Scan() {
                line := scanner.Text()

                if strings.HasPrefix(line, ":") {
                        // SSE comment (heartbeat)
                        continue
                }

                if strings.HasPrefix(line, "id:") {
                        // ID line - ignore
                        continue
                }

                if strings.HasPrefix(line, "event:") {
                        eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
                        // "end" event signals stream completion (no data follows)
                        if eventName == "end" {
                                if events.OnDone != nil {
                                        events.OnDone()
                                }
                                return nil
                        }
                        continue
                }

                if strings.HasPrefix(line, "data:") {
                        data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
                        if data == "[DONE]" {
                                if events.OnDone != nil {
                                        events.OnDone()
                                }
                                return nil
                        }
                        if data == "" {
                                // Empty data line - skip (common for start/end events)
                                continue
                        }
                        dispatchEvent(eventName, data, events)
                        continue
                }

                // Some streams send raw JSON without event: prefix
                if strings.HasPrefix(strings.TrimSpace(line), "{") {
                        dispatchEvent(eventName, strings.TrimSpace(line), events)
                }
        }

        if err := scanner.Err(); err != nil {
                if events.OnError != nil {
                        events.OnError(err)
                }
                return err
        }
        if events.OnDone != nil {
                events.OnDone()
        }
        return nil
}

func dispatchEvent(name, data string, events StreamEvents) {
        // Parse JSON data
        var obj map[string]interface{}
        if err := json.Unmarshal([]byte(data), &obj); err != nil {
                // Treat as plain text
                obj = map[string]interface{}{"text": data}
        }

        switch name {
        case "start":
                if events.OnStart != nil {
                        events.OnStart()
                }
        case "message", "text", "":
                if text, ok := obj["text"].(string); ok && text != "" {
                        if events.OnText != nil {
                                events.OnText(text)
                        }
                }
        case "reasoning", "reasoning_delta":
                // reasoning_delta uses "text" field; reasoning uses "reasoning" field
                var text string
                if t, ok := obj["text"].(string); ok {
                        text = t
                } else if t, ok := obj["reasoning"].(string); ok {
                        text = t
                }
                if text != "" && events.OnReasoning != nil {
                        events.OnReasoning(text)
                }
        case "references":
                if events.OnReferences != nil {
                        events.OnReferences(obj["references"])
                }
        case "status":
                if s, ok := obj["status"].(string); ok && events.OnStatus != nil {
                        events.OnStatus(s)
                }
        case "tool_result", "tool_results":
                if events.OnToolResult != nil {
                        events.OnToolResult(obj)
                }
        case "error":
                if events.OnError != nil {
                        msg, _ := obj["message"].(string)
                        if msg == "" {
                                if e, ok := obj["error"].(string); ok {
                                        msg = e
                                } else {
                                        msg = fmt.Sprintf("stream error: %s", data)
                                }
                        }
                        events.OnError(fmt.Errorf("%s", msg))
                }
        case "done":
                if events.OnDone != nil {
                        events.OnDone()
                }
        default:
                logger.Debug("[maritaca] Unhandled event %s: %s", name, data)
        }
}

// FetchModels retrieves the list of available models from Maritaca.
// Note: Maritaca doesn't have a public /api/models endpoint - we return the
// known list of models.
func (c *Client) FetchModels(accessToken string) ([]Model, error) {
        // Maritaca doesn't expose a public models endpoint. Return hardcoded list.
        return DefaultModels(), nil
}

// Model represents a chat model.
type Model struct {
        ID      string `json:"id"`
        Name    string `json:"name"`
        Object  string `json:"object"`
        OwnedBy string `json:"owned_by"`
        Created int64  `json:"created"`
}

// DefaultModels returns the known list of Maritaca models.
func DefaultModels() []Model {
        now := time.Now().Unix()
        models := []struct {
                ID, Name string
        }{
                {"sabia-3", "Sabia 3"},
                {"sabia-4", "Sabia 4"},
                {"sabia-4-pro", "Sabia 4 Pro"},
                {"sabia-4-thinking", "Sabia 4 Thinking"},
                {"sabiazinho-3", "Sabiazinho 3"},
                {"sabiazinho-4", "Sabiazinho 4"},
                {"sabiazinho-4-pro", "Sabiazinho 4 Pro"},
                {"sabia2-medium", "Sabia 2 Medium"},
                {"sabia2-small", "Sabia 2 Small"},
        }
        var out []Model
        for _, m := range models {
                out = append(out, Model{
                        ID:      m.ID,
                        Name:    m.Name,
                        Object:  "model",
                        OwnedBy: "maritaca",
                        Created: now,
                })
        }
        return out
}

// StopGeneration aborts an in-progress generation.
func (c *Client) StopGeneration(accessToken, chatID string) error {
        body := map[string]interface{}{"chat_id": chatID}
        bodyBytes, _ := json.Marshal(body)
        req, _ := http.NewRequest("POST", c.BaseURL+"/api/chat/clear", bytes.NewReader(bodyBytes))
        req.Header.Set("Authorization", "Auth "+accessToken)
        req.Header.Set("Content-Type", "application/json")

        resp, err := c.HTTP.Do(req)
        if err != nil {
                return err
        }
        defer resp.Body.Close()
        if resp.StatusCode >= 400 {
                b, _ := io.ReadAll(resp.Body)
                return fmt.Errorf("stop failed (%d): %s", resp.StatusCode, string(b))
        }
        return nil
}
