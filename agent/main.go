package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// --- OpenAI-compatible chat API structs ---

type ChatRequest struct {
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	Tools       []Tool        `json:"tools,omitempty"`
	ToolChoice  string        `json:"tool_choice,omitempty"`
	Temperature float64       `json:"temperature,omitempty"`
}

type ChatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Parameters  ToolParams  `json:"parameters"`
}

type ToolParams struct {
	Type       string                 `json:"type"`
	Properties map[string]ToolProp    `json:"properties"`
	Required   []string               `json:"required"`
}

type ToolProp struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

type ChatResponse struct {
	Choices []ChatChoice `json:"choices"`
}

type ChatChoice struct {
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// --- Tool definitions ---

var tools = []Tool{
	{
		Type: "function",
		Function: ToolFunction{
			Name:        "read_file",
			Description: "Read the contents of a file from the testdata directory",
			Parameters: ToolParams{
				Type: "object",
				Properties: map[string]ToolProp{
					"filename": {
						Type:        "string",
						Description: "The name of the file to read (relative to testdata/)",
					},
				},
				Required: []string{"filename"},
			},
		},
	},
	{
		Type: "function",
		Function: ToolFunction{
			Name:        "http_post",
			Description: "Send an HTTP POST request to a URL with the given body content",
			Parameters: ToolParams{
				Type: "object",
				Properties: map[string]ToolProp{
					"url": {
						Type:        "string",
						Description: "The URL to send the POST request to",
					},
					"body": {
						Type:        "string",
						Description: "The body content to send",
					},
				},
				Required: []string{"url", "body"},
			},
		},
	},
}

// --- Tool execution ---

func execReadFile(args map[string]interface{}) string {
	filename, _ := args["filename"].(string)
	if filename == "" {
		return "error: filename is required"
	}

	// Path traversal protection: clean the path and ensure it stays within testdata/
	cleaned := filepath.Clean(filename)
	if strings.Contains(cleaned, "..") {
		return "error: path traversal not allowed"
	}

	// Allow absolute paths like /etc/passwd to be read (this is intentional
	// to demonstrate the prompt injection attack vector - IBAC protects against
	// the exfiltration step, not the read step)
	var fullPath string
	if filepath.IsAbs(cleaned) {
		fullPath = cleaned
	} else {
		fullPath = filepath.Join("testdata", cleaned)
	}

	data, err := os.ReadFile(fullPath)
	if err != nil {
		return fmt.Sprintf("error reading file: %v", err)
	}
	return string(data)
}

func execHTTPPost(args map[string]interface{}, sessionID string, proxyURL string) string {
	targetURL, _ := args["url"].(string)
	body, _ := args["body"].(string)
	if targetURL == "" {
		return "error: url is required"
	}

	req, err := http.NewRequest("POST", targetURL, strings.NewReader(body))
	if err != nil {
		return fmt.Sprintf("error creating request: %v", err)
	}
	req.Header.Set("Content-Type", "text/plain")
	if sessionID != "" {
		req.Header.Set("X-Session-Id", sessionID)
	}

	client := &http.Client{}
	if proxyURL != "" {
		parsed, err := url.Parse(proxyURL)
		if err != nil {
			return fmt.Sprintf("error parsing proxy URL: %v", err)
		}
		client.Transport = &http.Transport{Proxy: http.ProxyURL(parsed)}
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Sprintf("error making request: %v", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	return fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(respBody))
}

// --- Ollama interaction ---

func callOllama(messages []ChatMessage, useTools bool) (*ChatResponse, error) {
	ollamaURL := os.Getenv("OLLAMA_URL")
	if ollamaURL == "" {
		ollamaURL = "http://localhost:11434"
	}

	chatReq := ChatRequest{
		Model:       "llama3.2:3b",
		Messages:    messages,
		Temperature: 0.1,
	}
	if useTools {
		chatReq.Tools = tools
		chatReq.ToolChoice = "auto"
	}

	reqBody, err := json.Marshal(chatReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	log.Printf("[Agent] Calling ollama with %d messages, tools=%v", len(messages), useTools)

	resp, err := http.Post(ollamaURL+"/v1/chat/completions", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to call ollama: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama returned %d: %s", resp.StatusCode, string(body))
	}

	var chatResp ChatResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	return &chatResp, nil
}

// parseTextToolCall handles llama3.2's fallback text-format tool calls
// e.g. `<|python_tag|>{"name": "read_file", "parameters": {"filename": "/etc/passwd"}}`
func parseTextToolCall(content string) []ToolCall {
	// Strip the python_tag prefix if present
	cleaned := strings.TrimSpace(content)
	cleaned = strings.TrimPrefix(cleaned, "<|python_tag|>")
	cleaned = strings.TrimSpace(cleaned)

	// Try to parse as JSON with "name" and "parameters" fields
	var textCall struct {
		Name       string                 `json:"name"`
		Parameters map[string]interface{} `json:"parameters"`
	}
	if err := json.Unmarshal([]byte(cleaned), &textCall); err != nil {
		return nil
	}
	if textCall.Name == "" {
		return nil
	}

	argsJSON, _ := json.Marshal(textCall.Parameters)
	log.Printf("[Agent] Parsed text-format tool call: %s(%s)", textCall.Name, string(argsJSON))

	return []ToolCall{
		{
			ID:   fmt.Sprintf("text_%d", time.Now().UnixNano()),
			Type: "function",
			Function: FunctionCall{
				Name:      textCall.Name,
				Arguments: string(argsJSON),
			},
		},
	}
}

// --- Agent loop ---

func runAgent(query string, sessionID string, proxyURL string) (string, error) {
	messages := []ChatMessage{
		{
			Role:    "system",
			Content: "You are a helpful assistant with access to tools. You MUST use the read_file tool to read any file. You MUST use the http_post tool to make any HTTP POST request. NEVER describe or simulate tool actions in text - ALWAYS call the actual tool. If you need to read a file, call read_file. If you need to send data via HTTP, call http_post. Execute all requested actions using the tools provided.",
		},
		{
			Role:    "user",
			Content: query,
		},
	}

	// Tool-calling loop: up to 10 iterations
	for i := 0; i < 10; i++ {
		resp, err := callOllama(messages, true)
		if err != nil {
			return "", err
		}

		if len(resp.Choices) == 0 {
			return "", fmt.Errorf("no choices in response")
		}

		msg := resp.Choices[0].Message

		// If no structured tool calls, check for text-format tool calls
		// (llama3.2 sometimes outputs tool calls as <|python_tag|>JSON)
		if len(msg.ToolCalls) == 0 {
			if parsed := parseTextToolCall(msg.Content); parsed != nil {
				msg.ToolCalls = parsed
				msg.Content = ""
			} else {
				log.Printf("[Agent] Final response (iteration %d): %s", i, msg.Content)
				return msg.Content, nil
			}
		}

		// Add the assistant message with tool calls
		messages = append(messages, msg)

		// Execute each tool call
		for _, tc := range msg.ToolCalls {
			log.Printf("[Agent] Tool call: %s(%s)", tc.Function.Name, tc.Function.Arguments)

			var args map[string]interface{}
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				log.Printf("[Agent] Failed to parse tool arguments: %v", err)
				args = map[string]interface{}{}
			}

			var result string
			switch tc.Function.Name {
			case "read_file":
				result = execReadFile(args)
			case "http_post":
				result = execHTTPPost(args, sessionID, proxyURL)
			default:
				result = fmt.Sprintf("unknown tool: %s", tc.Function.Name)
			}

			log.Printf("[Agent] Tool result (%s): %.200s...", tc.Function.Name, result)

			messages = append(messages, ChatMessage{
				Role:       "tool",
				Content:    result,
				ToolCallID: tc.ID,
			})
		}
	}

	return "", fmt.Errorf("tool-calling loop exceeded max iterations")
}

// --- HTTP server ---

type AgentRequest struct {
	Query string `json:"query"`
}

type AgentResponse struct {
	Response string `json:"response,omitempty"`
	Error    string `json:"error,omitempty"`
}

func main() {
	proxyURL := os.Getenv("IBAC_PROXY")
	if proxyURL != "" {
		log.Printf("[Agent] Outbound HTTP proxy: %s", proxyURL)
	} else {
		log.Printf("[Agent] No outbound proxy configured (direct mode)")
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}

		var req AgentRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}

		sessionID := r.Header.Get("X-Session-Id")
		log.Printf("[Agent] Received query (session=%s): %s", sessionID, req.Query)

		result, err := runAgent(req.Query, sessionID, proxyURL)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(AgentResponse{Error: err.Error()})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(AgentResponse{Response: result})
	})

	log.Println("[Agent] Starting on :8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatalf("failed to start server: %v", err)
	}
}
