package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/huang195/ibac/internal/demo"
)

type Session struct {
	ID        string        `json:"id"`
	Messages  []ChatMessage `json:"messages"`
	UpdatedAt time.Time     `json:"updated_at"`
	mu        sync.Mutex
}

var (
	sessions  sync.Map
	emitter   = demo.NewEventEmitterFromEnv()
	toolSet   = tools()
	modelName = firstNonEmpty(os.Getenv("LLM_MODEL"), "openai/gpt-oss-120b")
	llmURL    = firstNonEmpty(os.Getenv("LLM_URL"), "http://ce-proxy.ibac.svc.cluster.local:9100")
	maxIters  = 20
)

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func getSession(id string) *Session {
	if existing, ok := sessions.Load(id); ok {
		return existing.(*Session)
	}
	state := &Session{
		ID:       id,
		Messages: []ChatMessage{{Role: "system", Content: systemPrompt}},
	}
	actual, _ := sessions.LoadOrStore(id, state)
	return actual.(*Session)
}

func emitEvent(sessionID, stage, status, title, summary, rawLog string, data map[string]any) {
	if emitter == nil || sessionID == "" {
		return
	}
	emitter.Emit(demo.Event{
		SessionID: sessionID,
		Source:    "ce-demo-agent",
		Stage:     stage,
		Status:    status,
		Title:     title,
		Summary:   summary,
		Data:      data,
		RawLog:    rawLog,
	})
}

func emitAssistant(sessionID, reply string) {
	emitEvent(sessionID, "assistant_reply", "info", "Agent reply", reply, reply, map[string]any{"message": reply})
}

// callLLM posts an OpenAI-style chat request to the CE-Proxy.
func callLLM(sessionID string, messages []ChatMessage, useTools bool) (*ChatResponse, error) {
	payload := ChatRequest{
		Model:       modelName,
		Messages:    messages,
		Temperature: 0,
		MaxTokens:   8192, // gpt-oss-120b uses Harmony reasoning; needs room beyond the answer
	}
	if useTools {
		payload.Tools = toolSet
		payload.ToolChoice = "auto"
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal chat request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(llmURL, "/")+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Session-Id", sessionID)

	client := &http.Client{Timeout: 600 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call LLM proxy: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read LLM response: %w", err)
	}

	if resp.StatusCode == http.StatusRequestEntityTooLarge {
		var errResp struct {
			Error ChatError `json:"error"`
		}
		_ = json.Unmarshal(respBody, &errResp)
		return nil, fmt.Errorf("context_overflow: %s", errResp.Error.Message)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("LLM proxy returned %d: %s", resp.StatusCode, string(respBody))
	}

	var chat ChatResponse
	if err := json.Unmarshal(respBody, &chat); err != nil {
		return nil, fmt.Errorf("unmarshal LLM response: %w", err)
	}
	return &chat, nil
}

// parseTextToolCall handles the TOOL_CALL: {...} fallback syntax.
var toolCallLineRe = regexp.MustCompile(`(?m)TOOL_CALL:\s*(\{.*\})`)

func parseTextToolCall(content string) []ToolCall {
	match := toolCallLineRe.FindStringSubmatch(content)
	if len(match) < 2 {
		return nil
	}
	var parsed struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(match[1]), &parsed); err != nil {
		return nil
	}
	if parsed.Name == "" {
		return nil
	}
	args := "{}"
	if len(parsed.Arguments) > 0 {
		args = string(parsed.Arguments)
	}
	return []ToolCall{
		{
			ID:   fmt.Sprintf("call-%d", time.Now().UnixNano()),
			Type: "function",
			Function: FunctionCall{
				Name:      parsed.Name,
				Arguments: args,
			},
		},
	}
}

func normalizeToolCall(tc ToolCall) ToolCall {
	if tc.ID == "" {
		tc.ID = fmt.Sprintf("call-%d", time.Now().UnixNano())
	}
	if tc.Type == "" {
		tc.Type = "function"
	}
	if strings.TrimSpace(tc.Function.Arguments) == "" {
		tc.Function.Arguments = "{}"
	}
	return tc
}

func runTurn(state *Session, query, sessionID string) (string, error) {
	state.Messages = append(state.Messages, ChatMessage{Role: "user", Content: query})
	emitEvent(sessionID, "user_turn", "info", "Received user request", query, query, nil)

	// Publish the list of answer pieces the UI should track for this turn.
	// The ce-observer uses this to render a tracker panel (pending → collected
	// → still-present-after-masking).
	pieces := answerPiecesForQuery(query)
	piecesData := make([]map[string]any, 0, len(pieces))
	for _, p := range pieces {
		piecesData = append(piecesData, map[string]any{
			"name":        p.Name,
			"label":       p.Label,
			"source_tool": p.SourceTool,
			"example":     p.Example,
		})
	}
	emitEvent(sessionID, "scenario_intro", "info",
		fmt.Sprintf("Answer requires %d piece(s)", len(pieces)),
		"The observer will track each piece as it's collected.",
		"",
		map[string]any{"answer_pieces": piecesData, "query": query})

	for i := 0; i < maxIters; i++ {
		resp, err := callLLM(sessionID, state.Messages, true)
		if err != nil {
			if strings.HasPrefix(err.Error(), "context_overflow") {
				reply := fmt.Sprintf("I cannot continue: the context window has been exceeded (%v). " +
					"The investigation so far is preserved in the session.", err)
				state.Messages = append(state.Messages, ChatMessage{Role: "assistant", Content: reply})
				emitEvent(sessionID, "agent_blocked", "error",
					"Context overflow — cannot call LLM",
					err.Error(), err.Error(),
					map[string]any{"error": err.Error()})
				return reply, nil
			}
			return "", err
		}
		if len(resp.Choices) == 0 {
			return "", errors.New("no choices in LLM response")
		}

		msg := resp.Choices[0].Message

		// gpt-oss-120b uses Harmony channels: final answer in `content`,
		// internal reasoning in `reasoning_content`. If `content` is empty
		// but `reasoning_content` is present, the agent used up its budget
		// in the reasoning channel — surface that as the reply so the
		// demo still shows an answer.
		if strings.TrimSpace(msg.Content) == "" && strings.TrimSpace(msg.ReasoningContent) != "" {
			msg.Content = strings.TrimSpace(msg.ReasoningContent)
		}

		// If the model emitted a text-based TOOL_CALL: line, promote it.
		if len(msg.ToolCalls) == 0 {
			if parsed := parseTextToolCall(msg.Content); parsed != nil {
				msg.ToolCalls = parsed
				msg.Content = ""
			}
		}

		// Final answer (no tool call).
		if len(msg.ToolCalls) == 0 {
			reply := strings.TrimSpace(msg.Content)
			state.Messages = append(state.Messages, ChatMessage{Role: "assistant", Content: reply})
			emitAssistant(sessionID, reply)
			return reply, nil
		}

		for _, tc := range msg.ToolCalls {
			tc = normalizeToolCall(tc)

			if !isKnownTool(tc.Function.Name) {
				nudge := "Unknown tool. Use only tools from the inventory: get_user_profile, get_session_events, lookup_ip_reputation, get_file_access_log, get_data_exfil_attempts, search_logins_by_ip."
				emitEvent(sessionID, "unknown_tool", "warn", "Unknown tool call",
					fmt.Sprintf("Model proposed '%s' which is not a known tool.", tc.Function.Name),
					"", map[string]any{"tool_name": tc.Function.Name})
				state.Messages = append(state.Messages, ChatMessage{Role: "user", Content: nudge})
				break
			}

			args := parseArgs(tc.Function.Arguments)
			emitEvent(sessionID, "model_proposal", "started", "Tool call proposed",
				fmt.Sprintf("%s(%s)", tc.Function.Name, tc.Function.Arguments),
				fmt.Sprintf("%s(%s)", tc.Function.Name, tc.Function.Arguments),
				map[string]any{"tool_name": tc.Function.Name, "arguments": args})

			state.Messages = append(state.Messages, ChatMessage{Role: "assistant", ToolCalls: []ToolCall{tc}})

			result := executeTool(tc.Function.Name, args)
			hl := highlightForTool(tc.Function.Name)
			emitEvent(sessionID, "tool_result", "success", "Tool output",
				fmt.Sprintf("%s returned %d chars — signal: %s", tc.Function.Name, len(result), hl.Value),
				"",
				map[string]any{
					"tool_name":   tc.Function.Name,
					"result_size": len(result),
					"tool_output": result,
					"highlight":   hl,
				})

			// Announce each piece this tool call made available. IMPORTANT:
			// a tool can be called with decoy arguments (e.g. get_session_events
			// for an aux session) that return a fixture NOT containing the
			// piece's signal value. We only flip the piece to "collected"
			// when its example substring actually appears in THIS result —
			// otherwise the UI would show pieces as collected/dropped before
			// the relevant tool call has ever returned the value.
			for _, p := range pieces {
				if p.SourceTool != tc.Function.Name {
					continue
				}
				if p.Example != "" && !strings.Contains(result, p.Example) {
					continue
				}
				emitEvent(sessionID, "answer_piece_collected", "success",
					fmt.Sprintf("Piece collected: %s", p.Label),
					fmt.Sprintf("%s = %s", p.Name, p.Example),
					"",
					map[string]any{
						"name":        p.Name,
						"label":       p.Label,
						"value":       p.Example,
						"source_tool": tc.Function.Name,
					})
			}

			state.Messages = append(state.Messages, ChatMessage{
				Role:       "tool",
				Content:    result,
				ToolCallID: tc.ID,
			})
		}
	}

	return "", fmt.Errorf("tool loop exceeded max iterations (%d)", maxIters)
}

func main() {
	log.Printf("[ce-demo-agent] starting — LLM_URL=%s model=%s", llmURL, modelName)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	mux.HandleFunc("/debug/session/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/debug/session/")
		if id == "" {
			http.NotFound(w, r)
			return
		}
		s, ok := sessions.Load(id)
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s)
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req AgentRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}

		sessionID := r.Header.Get("X-Session-Id")
		if sessionID == "" {
			http.Error(w, "X-Session-Id header is required", http.StatusBadRequest)
			return
		}

		state := getSession(sessionID)
		state.mu.Lock()
		defer state.mu.Unlock()

		reply, err := runTurn(state, req.Query, sessionID)
		state.UpdatedAt = time.Now().UTC()
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(AgentResponse{Error: err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(AgentResponse{Response: reply})
	})

	if err := http.ListenAndServe(":8080", mux); err != nil {
		log.Fatalf("failed to start ce-demo-agent: %v", err)
	}
}
