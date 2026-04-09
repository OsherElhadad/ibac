package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	v3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/huang195/ibac/internal/demo"
	"github.com/huang195/ibac/internal/finance"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// --- Session context tracking ---

type SessionEvent struct {
	Sequence  int       `json:"sequence"`
	Direction string    `json:"direction"` // "inbound" or "outbound"
	Phase     string    `json:"phase"`     // "request" or "response"
	Method    string    `json:"method"`
	Authority string    `json:"authority"`
	Path      string    `json:"path"`
	Body      string    `json:"body"`   // truncated to 500 chars
	Action    string    `json:"action"` // what the sidecar did: "captured intent", "logged (trusted)", "BLOCKED", etc.
	Timestamp time.Time `json:"timestamp"`
}

type SessionContext struct {
	OriginalIntent   string                `json:"original_intent"`
	Events           []SessionEvent        `json:"events"`
	Conversation     []finance.ChatMessage `json:"conversation,omitempty"`
	PendingToolCalls []finance.ToolCall    `json:"pending_tool_calls,omitempty"`
	mu               sync.Mutex
}

func (sc *SessionContext) AddEvent(direction, phase, method, authority, path, body string) int {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	// Truncate body to 500 chars
	if len(body) > 500 {
		body = body[:500]
	}
	idx := len(sc.Events)
	sc.Events = append(sc.Events, SessionEvent{
		Sequence:  idx + 1,
		Direction: direction,
		Phase:     phase,
		Method:    method,
		Authority: authority,
		Path:      path,
		Body:      body,
		Timestamp: time.Now(),
	})
	return idx
}

// SetEventBody updates the body of an existing event (e.g. when RequestBody arrives after RequestHeaders)
func (sc *SessionContext) SetEventBody(idx int, body string) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if idx < 0 || idx >= len(sc.Events) {
		return
	}
	if len(body) > 500 {
		body = body[:500]
	}
	sc.Events[idx].Body = body
}

// SetEventAction updates the action field of an existing event
func (sc *SessionContext) SetEventAction(idx int, action string) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if idx < 0 || idx >= len(sc.Events) {
		return
	}
	sc.Events[idx].Action = action
}

func cloneConversation(messages []finance.ChatMessage) []finance.ChatMessage {
	cloned := make([]finance.ChatMessage, len(messages))
	for i, msg := range messages {
		cloned[i] = msg
		if msg.ToolCalls != nil {
			cloned[i].ToolCalls = append([]finance.ToolCall(nil), msg.ToolCalls...)
		}
	}
	return cloned
}

func (sc *SessionContext) AppendConversation(msg finance.ChatMessage) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.Conversation = append(sc.Conversation, msg)
}

func (sc *SessionContext) EnsureConversationSeeded() {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if len(sc.Conversation) > 0 {
		return
	}
	sc.Conversation = append(sc.Conversation, finance.ChatMessage{
		Role:    "system",
		Content: finance.SystemPrompt,
	})
}

func (sc *SessionContext) ConversationSnapshot() []finance.ChatMessage {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return cloneConversation(sc.Conversation)
}

func normalizedConversationForSPARC(messages []finance.ChatMessage) []finance.ChatMessage {
	normalized := make([]finance.ChatMessage, 0, len(messages))
	for i := 0; i < len(messages); i++ {
		msg := messages[i]
		if msg.Role == "assistant" && len(msg.ToolCalls) > 0 && i+1 < len(messages) && messages[i+1].Role == "tool" {
			var payload struct {
				Status string `json:"status"`
			}
			if err := json.Unmarshal([]byte(messages[i+1].Content), &payload); err == nil {
				status := strings.ToLower(strings.TrimSpace(payload.Status))
				if status == "needs_clarification" || status == "validation_blocked" {
					i++
					continue
				}
			}
		}
		normalized = append(normalized, msg)
	}
	return normalized
}

func (sc *SessionContext) SetPendingToolCalls(calls []finance.ToolCall) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.PendingToolCalls = append([]finance.ToolCall(nil), calls...)
}

func (sc *SessionContext) ConsumeMatchingPendingToolCall(actual finance.ToolCall) (finance.ToolCall, bool) {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	for i, candidate := range sc.PendingToolCalls {
		normalizedCandidate := finance.NormalizeToolCall(candidate)
		if !toolCallsEquivalent(normalizedCandidate, actual) {
			continue
		}
		sc.PendingToolCalls = append(sc.PendingToolCalls[:i], sc.PendingToolCalls[i+1:]...)
		return normalizedCandidate, true
	}
	return finance.ToolCall{}, false
}

// sessionStore maps sessionID -> *SessionContext
var sessionStore sync.Map

// activeSessionID tracks the currently active session for correlating outbound traffic
// that lacks X-Session-Id headers (e.g., agent→ollama, agent→email-server)
var activeSessionID atomic.Value
var eventEmitter = demo.NewEventEmitterFromEnv()

// trustedDestinations are logged but not validated by the LLM
var trustedDestinations map[string]bool

func initTrustedDestinations() {
	trustedDestinations = make(map[string]bool)
	env := os.Getenv("TRUSTED_DESTINATIONS")
	if env == "" {
		return
	}
	for _, d := range strings.Split(env, ",") {
		d = strings.TrimSpace(d)
		if d != "" {
			trustedDestinations[d] = true
			log.Printf("[IBAC] Trusted destination: %s", d)
		}
	}
}

func isTrustedDestination(authority string) bool {
	return trustedDestinations[authority]
}

// streamState holds per-stream metadata accumulated across headers and body phases
type streamState struct {
	direction       string
	sessionID       string
	method          string
	path            string
	authority       string
	statusCode      int
	requestEventIdx int // index of request event in SessionContext.Events, -1 if none
	toolCall        finance.ToolCall
	hasToolCall     bool
	syntheticResult string
	reflectionDone  bool
}

type processor struct {
	v3.UnimplementedExternalProcessorServer
	httpClient   *http.Client
	sparcBaseURL string
}

type sparcIssue struct {
	IssueType   string         `json:"issue_type"`
	MetricName  string         `json:"metric_name"`
	Explanation string         `json:"explanation"`
	Correction  map[string]any `json:"correction,omitempty"`
}

type sparcRequest struct {
	Messages  []finance.ChatMessage `json:"messages"`
	ToolSpecs []finance.Tool        `json:"tool_specs"`
	ToolCalls []finance.ToolCall    `json:"tool_calls"`
	SessionID string                `json:"session_id"`
	Stage     string                `json:"stage"`
}

type sparcResponse struct {
	Decision        string       `json:"decision"`
	ExecutionTimeMS float64      `json:"execution_time_ms"`
	OverallAvgScore *float64     `json:"overall_avg_score,omitempty"`
	Issues          []sparcIssue `json:"issues,omitempty"`
}

// --- Helper functions ---

func getHeaderValue(headers []*core.HeaderValue, key string) string {
	for _, header := range headers {
		if strings.EqualFold(header.Key, key) {
			return string(header.RawValue)
		}
	}
	return ""
}

// blockRequest returns a 403 Forbidden immediate response
func blockRequest(reason string) *v3.ProcessingResponse {
	body := fmt.Sprintf(`{"error":"blocked","reason":"%s"}`, reason)
	return &v3.ProcessingResponse{
		Response: &v3.ProcessingResponse_ImmediateResponse{
			ImmediateResponse: &v3.ImmediateResponse{
				Status: &typev3.HttpStatus{
					Code: typev3.StatusCode_Forbidden,
				},
				Body:    []byte(body),
				Details: "ibac_intent_violation",
			},
		},
	}
}

// allowBody returns a body response that passes through unchanged
func allowBody() *v3.ProcessingResponse {
	return &v3.ProcessingResponse{
		Response: &v3.ProcessingResponse_RequestBody{
			RequestBody: &v3.BodyResponse{},
		},
	}
}

// allowHeaders returns a headers response that passes through unchanged
func allowHeaders() *v3.ProcessingResponse {
	return &v3.ProcessingResponse{
		Response: &v3.ProcessingResponse_RequestHeaders{
			RequestHeaders: &v3.HeadersResponse{},
		},
	}
}

// allowResponseHeaders returns a response headers response that passes through unchanged
func allowResponseHeaders() *v3.ProcessingResponse {
	return &v3.ProcessingResponse{
		Response: &v3.ProcessingResponse_ResponseHeaders{
			ResponseHeaders: &v3.HeadersResponse{},
		},
	}
}

// allowResponseBody returns a response body response that passes through unchanged
func allowResponseBody() *v3.ProcessingResponse {
	return &v3.ProcessingResponse{
		Response: &v3.ProcessingResponse_ResponseBody{
			ResponseBody: &v3.BodyResponse{},
		},
	}
}

// getOrCreateSession returns the SessionContext for a session, creating it if needed
func getOrCreateSession(sessionID string) *SessionContext {
	if ctx, ok := sessionStore.Load(sessionID); ok {
		return ctx.(*SessionContext)
	}
	ctx := &SessionContext{}
	actual, _ := sessionStore.LoadOrStore(sessionID, ctx)
	return actual.(*SessionContext)
}

// resolveSessionID returns the session ID from the stream state, falling back to activeSessionID
func resolveSessionID(state *streamState) string {
	if state.sessionID != "" {
		return state.sessionID
	}
	if v := activeSessionID.Load(); v != nil {
		return v.(string)
	}
	return ""
}

// formatSessionContext renders session events as a numbered list for the LLM prompt
func formatSessionContext(sc *SessionContext) string {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	filtered := make([]SessionEvent, 0, len(sc.Events))
	for _, e := range sc.Events {
		if !shouldIncludeIntentPromptEvent(e) {
			continue
		}
		filtered = append(filtered, e)
	}

	if len(filtered) == 0 {
		return "(no prior activity)"
	}

	if len(filtered) > 12 {
		filtered = filtered[len(filtered)-12:]
	}

	var sb strings.Builder
	for _, e := range filtered {
		actionStr := ""
		if e.Action != "" {
			actionStr = fmt.Sprintf(" => %s", e.Action)
		}
		fmt.Fprintf(&sb, "%d. [%s %s] %s %s%s%s\n",
			e.Sequence, e.Direction, e.Phase, e.Method, e.Authority, e.Path, actionStr)
		if e.Body != "" {
			body := e.Body
			if len(body) > 220 {
				body = body[:220]
			}
			fmt.Fprintf(&sb, "   Body: %s\n", body)
		}
	}
	return sb.String()
}

func shouldIncludeIntentPromptEvent(e SessionEvent) bool {
	if e.Direction == "inbound" {
		return true
	}

	switch e.Authority {
	case "demo-observer.ibac.svc.cluster.local:7070", "ibac-ollama:11434", "host.docker.internal:11434", "sparc-reflector.ibac.svc.cluster.local:8090":
		return false
	}

	return true
}

func emitIBACEvent(sessionID, stage, status, title, summary, rawLog string, data map[string]any) {
	if eventEmitter == nil || sessionID == "" {
		return
	}
	eventEmitter.Emit(demo.Event{
		SessionID: sessionID,
		Source:    "ibac",
		Stage:     stage,
		Status:    status,
		Title:     title,
		Summary:   summary,
		Data:      data,
		RawLog:    rawLog,
	})
}

func emitSPARCEvent(sessionID, stage, status, title, summary, rawLog string, data map[string]any) {
	if eventEmitter == nil || sessionID == "" {
		return
	}
	eventEmitter.Emit(demo.Event{
		SessionID: sessionID,
		Source:    "sparc",
		Stage:     stage,
		Status:    status,
		Title:     title,
		Summary:   summary,
		Data:      data,
		RawLog:    rawLog,
	})
}

func sparcReflectorURL() string {
	baseURL := strings.TrimSpace(os.Getenv("SPARC_REFLECTOR_URL"))
	if baseURL == "" {
		baseURL = "http://sparc-reflector.ibac.svc.cluster.local:8090"
	}
	return strings.TrimRight(baseURL, "/")
}

func immediateJSONResponse(statusCode typev3.StatusCode, body string, details string) *v3.ProcessingResponse {
	return &v3.ProcessingResponse{
		Response: &v3.ProcessingResponse_ImmediateResponse{
			ImmediateResponse: &v3.ImmediateResponse{
				Status: &typev3.HttpStatus{Code: statusCode},
				Headers: &v3.HeaderMutation{
					SetHeaders: []*core.HeaderValueOption{
						{Header: &core.HeaderValue{Key: "content-type", RawValue: []byte("application/json")}},
					},
				},
				Body:    []byte(body),
				Details: details,
			},
		},
	}
}

func isFinanceBackendDestination(authority string) bool {
	return authority == "finance-backend.ibac.svc.cluster.local:8181"
}

func isOllamaDestination(authority string) bool {
	return authority == "ibac-ollama:11434" || authority == "host.docker.internal:11434"
}

func isReflectableFinanceRequest(authority, method, path string) bool {
	if !isFinanceBackendDestination(authority) {
		return false
	}
	switch {
	case method == http.MethodGet && strings.HasPrefix(path, "/transactions/"):
		return true
	case method == http.MethodGet && strings.HasPrefix(path, "/customers/"):
		return true
	case method == http.MethodPost && path == "/refunds":
		return true
	case method == http.MethodGet && strings.HasPrefix(path, "/invoices/"):
		return true
	default:
		return false
	}
}

func buildObservedFinanceToolCall(method, path, body string) (finance.ToolCall, bool) {
	switch {
	case method == http.MethodGet && strings.HasPrefix(path, "/transactions/"):
		args, _ := json.Marshal(map[string]any{"transaction_id": strings.TrimPrefix(path, "/transactions/")})
		return finance.ToolCall{
			ID:   fmt.Sprintf("observed_%d", time.Now().UnixNano()),
			Type: "function",
			Function: finance.FunctionCall{
				Name:      "get_transaction",
				Arguments: string(args),
			},
		}, true
	case method == http.MethodGet && strings.HasPrefix(path, "/customers/"):
		args, _ := json.Marshal(map[string]any{"customer_id": strings.TrimPrefix(path, "/customers/")})
		return finance.ToolCall{
			ID:   fmt.Sprintf("observed_%d", time.Now().UnixNano()),
			Type: "function",
			Function: finance.FunctionCall{
				Name:      "lookup_customer",
				Arguments: string(args),
			},
		}, true
	case method == http.MethodGet && strings.HasPrefix(path, "/invoices/"):
		args, _ := json.Marshal(map[string]any{"invoice_id": strings.TrimPrefix(path, "/invoices/")})
		return finance.ToolCall{
			ID:   fmt.Sprintf("observed_%d", time.Now().UnixNano()),
			Type: "function",
			Function: finance.FunctionCall{
				Name:      "get_invoice",
				Arguments: string(args),
			},
		}, true
	case method == http.MethodPost && path == "/refunds":
		args := finance.ParseArgs(body)
		argsJSON, _ := json.Marshal(args)
		return finance.ToolCall{
			ID:   fmt.Sprintf("observed_%d", time.Now().UnixNano()),
			Type: "function",
			Function: finance.FunctionCall{
				Name:      "issue_refund",
				Arguments: string(argsJSON),
			},
		}, true
	default:
		return finance.ToolCall{}, false
	}
}

func toolCallsEquivalent(a, b finance.ToolCall) bool {
	if a.Function.Name != b.Function.Name {
		return false
	}
	argsA := finance.ParseArgs(a.Function.Arguments)
	argsB := finance.ParseArgs(b.Function.Arguments)
	switch a.Function.Name {
	case "get_transaction", "issue_refund":
		return fmt.Sprint(argsA["transaction_id"]) == fmt.Sprint(argsB["transaction_id"])
	case "lookup_customer":
		return fmt.Sprint(argsA["customer_id"]) == fmt.Sprint(argsB["customer_id"])
	case "get_invoice":
		return fmt.Sprint(argsA["invoice_id"]) == fmt.Sprint(argsB["invoice_id"])
	default:
		return a.Function.Arguments == b.Function.Arguments
	}
}

func syntheticClarificationResult(toolCall finance.ToolCall, issues []sparcIssue) string {
	message := "I need one more detail before I can continue."
	missingField := ""

	switch toolCall.Function.Name {
	case "get_transaction":
		message = "Could you share the exact full transaction ID before I continue with the refund?"
		missingField = "transaction_id"
	case "issue_refund":
		message = "Could you confirm the refund reason before I continue?"
		missingField = "refund_reason"
	}

	for _, issue := range issues {
		lower := strings.ToLower(issue.Explanation + " " + issue.MetricName)
		if strings.Contains(lower, "transaction") && strings.Contains(lower, "id") {
			message = "Could you share the exact full transaction ID before I continue with the refund?"
			missingField = "transaction_id"
			break
		}
		if strings.Contains(lower, "refund_reason") || strings.Contains(lower, "refund reason") {
			message = "Could you confirm the refund reason before I continue?"
			missingField = "refund_reason"
			break
		}
	}

	payload, _ := json.Marshal(map[string]any{
		"status":        "needs_clarification",
		"message":       message,
		"missing_field": missingField,
		"tool":          toolCall.Function.Name,
	})
	return string(payload)
}

func parseObservedToolCalls(body string) []finance.ToolCall {
	var response finance.ChatResponse
	if err := json.Unmarshal([]byte(body), &response); err != nil || len(response.Choices) == 0 {
		return nil
	}

	msg := response.Choices[0].Message
	if len(msg.ToolCalls) > 0 {
		calls := make([]finance.ToolCall, 0, len(msg.ToolCalls))
		for _, tc := range msg.ToolCalls {
			calls = append(calls, finance.NormalizeToolCall(tc))
		}
		return calls
	}
	if parsed := finance.ParseTextToolCall(msg.Content); parsed != nil {
		calls := make([]finance.ToolCall, 0, len(parsed))
		for _, tc := range parsed {
			calls = append(calls, finance.NormalizeToolCall(tc))
		}
		return calls
	}
	return nil
}

func (p *processor) reflectFinanceToolCall(sessionID string, conversation []finance.ChatMessage, toolCall finance.ToolCall) (*sparcResponse, error) {
	payload := sparcRequest{
		Messages:  conversation,
		ToolSpecs: finance.Tools(),
		ToolCalls: []finance.ToolCall{toolCall},
		SessionID: sessionID,
		Stage:     "pre_tool",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	reflectURL := p.sparcBaseURL + "/reflect"
	req, err := http.NewRequest(http.MethodPost, reflectURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if sessionID != "" {
		req.Header.Set("X-Session-Id", sessionID)
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sparc-reflector returned %d: %s", resp.StatusCode, string(respBody))
	}

	var parsed sparcResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, err
	}
	return &parsed, nil
}

func (p *processor) evaluateFinanceRequest(sessionID string, sc *SessionContext, actual finance.ToolCall) (*sparcResponse, finance.ToolCall, string, error) {
	sc.EnsureConversationSeeded()
	pending, ok := sc.ConsumeMatchingPendingToolCall(actual)
	if !ok {
		return nil, finance.ToolCall{}, "", fmt.Errorf("no matching pending tool call for %s", actual.Function.Name)
	}

	emitSPARCEvent(sessionID, "sparc_observed_proposal", "started", "Observed model tool proposal", fmt.Sprintf("Sidecar observed %s from the model output.", pending.Function.Name), pending.Function.Arguments, map[string]any{
		"tool_name": pending.Function.Name,
		"arguments": finance.ParseArgs(pending.Function.Arguments),
	})

	conversation := normalizedConversationForSPARC(sc.ConversationSnapshot())
	emitSPARCEvent(sessionID, "sparc_reflection", "started", "Started SPARC reflection", fmt.Sprintf("Sidecar is evaluating %s before the backend request is allowed.", pending.Function.Name), fmt.Sprintf("POST %s/reflect", p.sparcBaseURL), map[string]any{
		"tool_name": pending.Function.Name,
	})

	resp, err := p.reflectFinanceToolCall(sessionID, conversation, pending)
	if err != nil {
		return nil, finance.ToolCall{}, "", err
	}

	status := "success"
	title := "SPARC approved tool call"
	summary := fmt.Sprintf("SPARC approved %s for execution.", pending.Function.Name)
	if strings.EqualFold(resp.Decision, "reject") {
		status = "blocked"
		title = "SPARC blocked tool call"
		summary = fmt.Sprintf("SPARC blocked %s because the call was not well grounded.", pending.Function.Name)
	} else if strings.EqualFold(resp.Decision, "error") {
		status = "error"
		title = "SPARC returned an error"
		summary = fmt.Sprintf("SPARC reported an error while evaluating %s.", pending.Function.Name)
	}

	data := map[string]any{
		"tool_name":         pending.Function.Name,
		"decision":          resp.Decision,
		"execution_time_ms": resp.ExecutionTimeMS,
		"issues":            resp.Issues,
	}
	if resp.OverallAvgScore != nil {
		data["overall_avg_score"] = *resp.OverallAvgScore
	}
	emitSPARCEvent(sessionID, "sparc_result", status, title, summary, fmt.Sprintf("SPARC decision=%s", resp.Decision), data)

	if strings.EqualFold(resp.Decision, "reject") || strings.EqualFold(resp.Decision, "error") {
		synthetic := syntheticClarificationResult(pending, resp.Issues)
		sc.AppendConversation(finance.ChatMessage{Role: "assistant", ToolCalls: []finance.ToolCall{pending}})
		sc.AppendConversation(finance.ChatMessage{Role: "tool", ToolCallID: pending.ID, Content: synthetic})
		emitSPARCEvent(sessionID, "sparc_synthetic_tool_result", "blocked", "Returned clarification tool result", "Sidecar returned a synthetic tool result so the agent can ask for the missing detail.", synthetic, map[string]any{
			"tool_name": pending.Function.Name,
		})
		return resp, pending, synthetic, nil
	}

	sc.AppendConversation(finance.ChatMessage{Role: "assistant", ToolCalls: []finance.ToolCall{pending}})
	return resp, pending, "", nil
}

func captureInboundIntent(sessionID, body string, requestEventIdx int) {
	var reqBody map[string]interface{}
	if err := json.Unmarshal([]byte(body), &reqBody); err != nil {
		return
	}
	query, ok := reqBody["query"].(string)
	if !ok || sessionID == "" {
		return
	}

	sc := getOrCreateSession(sessionID)
	sc.mu.Lock()
	sc.OriginalIntent = query
	sc.mu.Unlock()
	sc.EnsureConversationSeeded()
	sc.AppendConversation(finance.ChatMessage{Role: "user", Content: query})
	activeSessionID.Store(sessionID)
	log.Printf("[IBAC] Captured intent for session %s: %s", sessionID, query)
	sc.SetEventAction(requestEventIdx, "captured intent")
	emitIBACEvent(sessionID, "intent_capture", "info", "Captured user intent", query, fmt.Sprintf("intent=%s", query), map[string]any{
		"query": query,
	})
}

func captureObservedOllamaResponse(sessionID, body string) {
	if sessionID == "" {
		return
	}
	sc := getOrCreateSession(sessionID)
	toolCalls := parseObservedToolCalls(body)
	sc.SetPendingToolCalls(toolCalls)
}

func captureInboundAssistantReply(sessionID, body string) {
	if sessionID == "" {
		return
	}

	var respBody map[string]any
	if err := json.Unmarshal([]byte(body), &respBody); err != nil {
		return
	}
	reply, _ := respBody["response"].(string)
	reply = strings.TrimSpace(reply)
	if reply == "" {
		return
	}
	getOrCreateSession(sessionID).AppendConversation(finance.ChatMessage{
		Role:    "assistant",
		Content: reply,
	})
}

// --- LLM-based intent checking ---

type LLMRequest struct {
	Model       string       `json:"model"`
	Messages    []LLMMessage `json:"messages"`
	Temperature float64      `json:"temperature"`
	MaxTokens   int          `json:"max_tokens,omitempty"`
}

type LLMMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type LLMResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

type IntentDecision struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

// loadPromptTemplate reads the intent prompt template from a file.
// It looks for intent_prompt.txt next to the binary first, then falls back to the current directory.
func loadPromptTemplate() string {
	paths := []string{"intent_prompt.txt", "sidecar/intent_prompt.txt"}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err == nil {
			log.Printf("[IBAC] Loaded intent prompt from %s", p)
			return string(data)
		}
	}
	log.Fatal("[IBAC] Could not load intent_prompt.txt")
	return ""
}

var intentPromptTemplate = loadPromptTemplate()

// formatHTTPAction formats an HTTP request as an action description for the intent prompt
func formatHTTPAction(method, authority, path, body string) string {
	if len(body) > 220 {
		body = body[:220]
	}
	return fmt.Sprintf(`Type: Outbound HTTP request
- Method: %s
- Destination: %s%s
- Body (first 220 chars): %s`, method, authority, path, body)
}

func lowerContains(haystack, needle string) bool {
	if needle == "" {
		return false
	}
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

func intentMentionsDestination(intent, authority, path string) bool {
	host := strings.Split(authority, ":")[0]
	return lowerContains(intent, authority) || lowerContains(intent, host) || (path != "" && path != "/" && lowerContains(intent, path))
}

func traceIntroducedDestination(sc *SessionContext, authority, path string) bool {
	fullDestination := authority + path
	host := strings.Split(authority, ":")[0]

	sc.mu.Lock()
	defer sc.mu.Unlock()

	for _, event := range sc.Events {
		if lowerContains(event.Body, fullDestination) || lowerContains(event.Body, authority) || lowerContains(event.Body, host) {
			return true
		}
	}
	return false
}

func preflightIntentDecision(sc *SessionContext, method, authority, path, body string) (string, string, bool) {
	upperMethod := strings.ToUpper(method)
	switch upperMethod {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return "", "", false
	}

	if intentMentionsDestination(sc.OriginalIntent, authority, path) {
		return "", "", false
	}

	if traceIntroducedDestination(sc, authority, path) {
		return "BLOCK", "Outbound write target was introduced by retrieved content, not by the user. Treating this as prompt injection and blocking the request.", true
	}

	if lowerContains(sc.OriginalIntent, "process invoice") && (lowerContains(body, "invoice_id") || lowerContains(body, "amount")) {
		return "BLOCK", "Processing an invoice does not authorize sending invoice data to a new outbound destination that the user never named.", true
	}

	return "", "", false
}

// checkIntent uses an LLM to determine if an outbound action aligns with the original intent
func checkIntent(sc *SessionContext, method, authority, path, body string) (string, string) {
	action := formatHTTPAction(method, authority, path, body)
	sessionTrace := formatSessionContext(sc)
	prompt := fmt.Sprintf(intentPromptTemplate, sc.OriginalIntent, sessionTrace, action)

	llmReq := LLMRequest{
		Model: "llama3.2:3b",
		Messages: []LLMMessage{
			{Role: "user", Content: prompt},
		},
		Temperature: 0.1,
		MaxTokens:   80,
	}

	reqBody, err := json.Marshal(llmReq)
	if err != nil {
		log.Printf("[IBAC] Failed to marshal LLM request: %v", err)
		if decision, reason, ok := preflightIntentDecision(sc, method, authority, path, body); ok {
			return decision, reason
		}
		return "BLOCK", "failed to create LLM request"
	}

	// Call ollama directly (NOT through envoy proxy)
	ollamaURL := os.Getenv("OLLAMA_URL")
	if ollamaURL == "" {
		ollamaURL = "http://localhost:11434"
	}
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Post(ollamaURL+"/v1/chat/completions", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		log.Printf("[IBAC] Failed to call LLM: %v", err)
		if decision, reason, ok := preflightIntentDecision(sc, method, authority, path, body); ok {
			return decision, reason
		}
		return "BLOCK", "LLM unavailable, default deny"
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[IBAC] Failed to read LLM response: %v", err)
		if decision, reason, ok := preflightIntentDecision(sc, method, authority, path, body); ok {
			return decision, reason
		}
		return "BLOCK", "failed to read LLM response"
	}

	if resp.StatusCode != http.StatusOK {
		log.Printf("[IBAC] LLM returned %d: %s", resp.StatusCode, string(respBody))
		if decision, reason, ok := preflightIntentDecision(sc, method, authority, path, body); ok {
			return decision, reason
		}
		return "BLOCK", "LLM error, default deny"
	}

	var llmResp LLMResponse
	if err := json.Unmarshal(respBody, &llmResp); err != nil {
		log.Printf("[IBAC] Failed to unmarshal LLM response: %v", err)
		if decision, reason, ok := preflightIntentDecision(sc, method, authority, path, body); ok {
			return decision, reason
		}
		return "BLOCK", "failed to parse LLM response"
	}

	if len(llmResp.Choices) == 0 {
		if decision, reason, ok := preflightIntentDecision(sc, method, authority, path, body); ok {
			return decision, reason
		}
		return "BLOCK", "empty LLM response"
	}

	content := llmResp.Choices[0].Message.Content
	log.Printf("[IBAC] LLM raw response: %s", content)

	// Try to parse the JSON decision
	// The LLM may wrap the JSON in markdown code blocks, so strip those
	content = strings.TrimSpace(content)
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	content = strings.TrimSpace(content)

	var decision IntentDecision
	if err := json.Unmarshal([]byte(content), &decision); err != nil {
		log.Printf("[IBAC] Failed to parse decision JSON: %v (content: %s)", err, content)
		if fallbackDecision, fallbackReason, ok := preflightIntentDecision(sc, method, authority, path, body); ok {
			return fallbackDecision, fallbackReason
		}
		return "BLOCK", "unparseable LLM response, default deny"
	}

	decision.Decision = strings.ToUpper(strings.TrimSpace(decision.Decision))
	if decision.Decision != "ALLOW" && decision.Decision != "BLOCK" {
		if fallbackDecision, fallbackReason, ok := preflightIntentDecision(sc, method, authority, path, body); ok {
			return fallbackDecision, fallbackReason
		}
		return "BLOCK", fmt.Sprintf("invalid decision '%s', default deny", decision.Decision)
	}

	return decision.Decision, decision.Reason
}

// --- ext_proc Process implementation ---

func (p *processor) Process(stream v3.ExternalProcessor_ProcessServer) error {
	ctx := stream.Context()
	state := &streamState{requestEventIdx: -1}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		req, err := stream.Recv()
		if err != nil {
			return status.Errorf(codes.Unknown, "cannot receive stream request: %v", err)
		}

		var resp *v3.ProcessingResponse

		switch r := req.Request.(type) {
		case *v3.ProcessingRequest_RequestHeaders:
			headers := r.RequestHeaders.Headers

			state.direction = getHeaderValue(headers.Headers, "x-ibac-direction")
			state.sessionID = getHeaderValue(headers.Headers, "x-session-id")
			state.method = getHeaderValue(headers.Headers, ":method")
			state.path = getHeaderValue(headers.Headers, ":path")
			state.authority = getHeaderValue(headers.Headers, ":authority")

			// For outbound: if no session ID in headers, use activeSessionID fallback
			if state.direction == "outbound" && state.sessionID == "" {
				state.sessionID = resolveSessionID(state)
			}

			log.Printf("[IBAC] %s request: session=%s method=%s authority=%s path=%s",
				state.direction, state.sessionID, state.method, state.authority, state.path)

			// Log request event (body and action will be updated in RequestBody if it arrives)
			sessionID := resolveSessionID(state)
			if sessionID != "" {
				sc := getOrCreateSession(sessionID)
				state.requestEventIdx = sc.AddEvent(state.direction, "request", state.method, state.authority, state.path, "")
				// Set a default action; RequestBody will overwrite with the actual decision
				if state.direction == "outbound" && isTrustedDestination(state.authority) && !isReflectableFinanceRequest(state.authority, state.method, state.path) {
					sc.SetEventAction(state.requestEventIdx, "ALLOW (trusted)")
				}
			}

			resp = allowHeaders()

			if state.direction == "outbound" && isReflectableFinanceRequest(state.authority, state.method, state.path) && state.method == http.MethodGet {
				if sessionID == "" {
					resp = blockRequest("missing session ID")
					break
				}

				actual, ok := buildObservedFinanceToolCall(state.method, state.path, "")
				if !ok {
					resp = blockRequest("failed to map finance backend request")
					break
				}

				sc := getOrCreateSession(sessionID)
				sparcResp, pending, synthetic, err := p.evaluateFinanceRequest(sessionID, sc, actual)
				state.reflectionDone = true
				if err != nil {
					synthetic = syntheticClarificationResult(actual, nil)
					sc.SetEventAction(state.requestEventIdx, "BLOCK (SPARC fail-closed)")
					emitSPARCEvent(sessionID, "sparc_result", "error", "SPARC validation failed closed", fmt.Sprintf("Sidecar could not validate %s, so it returned a clarification-needed tool result instead of allowing the backend call.", actual.Function.Name), err.Error(), map[string]any{
						"tool_name": actual.Function.Name,
					})
					state.syntheticResult = synthetic
					resp = immediateJSONResponse(typev3.StatusCode_OK, synthetic, "sparc_validation_block")
					break
				}

				state.toolCall = pending
				state.hasToolCall = true
				if synthetic != "" || strings.EqualFold(sparcResp.Decision, "reject") || strings.EqualFold(sparcResp.Decision, "error") {
					sc.SetEventAction(state.requestEventIdx, "BLOCK (SPARC)")
					state.syntheticResult = synthetic
					resp = immediateJSONResponse(typev3.StatusCode_OK, synthetic, "sparc_validation_block")
					break
				}

				sc.SetEventAction(state.requestEventIdx, "ALLOW (SPARC approved)")
			}

		case *v3.ProcessingRequest_RequestBody:
			body := string(r.RequestBody.Body)
			sessionID := resolveSessionID(state)

			// Update the request event's body (event was created in RequestHeaders)
			if sessionID != "" && state.requestEventIdx >= 0 {
				sc := getOrCreateSession(sessionID)
				sc.SetEventBody(state.requestEventIdx, body)
			}

			if state.direction == "inbound" {
				captureInboundIntent(sessionID, body, state.requestEventIdx)
				resp = allowBody()

			} else if state.direction == "outbound" {
				if isReflectableFinanceRequest(state.authority, state.method, state.path) {
					if state.reflectionDone {
						resp = allowBody()
						break
					}
					if sessionID == "" {
						resp = blockRequest("missing session ID")
						break
					}

					actual, ok := buildObservedFinanceToolCall(state.method, state.path, body)
					if !ok {
						resp = blockRequest("failed to map finance backend request")
						break
					}

					sc := getOrCreateSession(sessionID)
					sparcResp, pending, synthetic, err := p.evaluateFinanceRequest(sessionID, sc, actual)
					state.reflectionDone = true
					if err != nil {
						synthetic = syntheticClarificationResult(actual, nil)
						sc.SetEventAction(state.requestEventIdx, "BLOCK (SPARC fail-closed)")
						emitSPARCEvent(sessionID, "sparc_result", "error", "SPARC validation failed closed", fmt.Sprintf("Sidecar could not validate %s, so it returned a clarification-needed tool result instead of allowing the backend call.", actual.Function.Name), err.Error(), map[string]any{
							"tool_name": actual.Function.Name,
						})
						state.syntheticResult = synthetic
						resp = immediateJSONResponse(typev3.StatusCode_OK, synthetic, "sparc_validation_block")
						break
					}

					state.toolCall = pending
					state.hasToolCall = true
					if synthetic != "" || strings.EqualFold(sparcResp.Decision, "reject") || strings.EqualFold(sparcResp.Decision, "error") {
						sc.SetEventAction(state.requestEventIdx, "BLOCK (SPARC)")
						state.syntheticResult = synthetic
						resp = immediateJSONResponse(typev3.StatusCode_OK, synthetic, "sparc_validation_block")
						break
					}

					sc.SetEventAction(state.requestEventIdx, "ALLOW (SPARC approved)")
					resp = allowBody()
					break
				}

				// Check if destination is trusted
				if isTrustedDestination(state.authority) {
					log.Printf("[IBAC] Trusted destination %s, logging only", state.authority)
					if sessionID != "" {
						getOrCreateSession(sessionID).SetEventAction(state.requestEventIdx, "ALLOW (trusted)")
						emitIBACEvent(sessionID, "outbound_intercept", "info", "Trusted outbound request", fmt.Sprintf("Allowed trusted destination %s.", state.authority), fmt.Sprintf("%s %s%s", state.method, state.authority, state.path), map[string]any{
							"authority": state.authority,
							"method":    state.method,
							"path":      state.path,
						})
					}
					resp = allowBody()
				} else {
					// Untrusted destination: validate against session context
					if sessionID != "" {
						emitIBACEvent(sessionID, "outbound_intercept", "started", "Intercepted outbound request", fmt.Sprintf("Intercepted outbound request to %s%s.", state.authority, state.path), fmt.Sprintf("%s %s%s", state.method, state.authority, state.path), map[string]any{
							"authority": state.authority,
							"method":    state.method,
							"path":      state.path,
						})
					}
					if sessionID == "" {
						log.Printf("[IBAC] BLOCK: no session ID on outbound request to untrusted %s", state.authority)
						emitIBACEvent(sessionID, "decision", "blocked", "Blocked outbound request", "Blocked an outbound request with no session ID.", fmt.Sprintf("%s %s%s", state.method, state.authority, state.path), map[string]any{
							"authority": state.authority,
						})
						resp = blockRequest("missing session ID")
					} else {
						sc := getOrCreateSession(sessionID)
						if sc.OriginalIntent == "" {
							log.Printf("[IBAC] BLOCK: no intent found for session %s", sessionID)
							sc.SetEventAction(state.requestEventIdx, "BLOCK (no intent)")
							emitIBACEvent(sessionID, "decision", "blocked", "Blocked outbound request", "Blocked an outbound request because no intent was registered.", fmt.Sprintf("%s %s%s", state.method, state.authority, state.path), map[string]any{
								"authority": state.authority,
							})
							resp = blockRequest("no intent registered for session")
						} else {
							log.Printf("[IBAC] Session context for %s:\n%s", sessionID, formatSessionContext(sc))
							decision, reason := checkIntent(sc, state.method, state.authority, state.path, body)
							log.Printf("[IBAC] Decision for session %s: %s - %s", sessionID, decision, reason)
							status := "success"
							title := "Allowed outbound request"
							if decision != "ALLOW" {
								status = "blocked"
								title = "Blocked outbound request"
							}
							emitIBACEvent(sessionID, "decision", status, title, reason, fmt.Sprintf("%s %s%s", state.method, state.authority, state.path), map[string]any{
								"authority": state.authority,
								"method":    state.method,
								"path":      state.path,
								"decision":  decision,
								"reason":    reason,
							})

							if decision == "ALLOW" {
								sc.SetEventAction(state.requestEventIdx, "ALLOW (validated)")
								resp = allowBody()
							} else {
								sc.SetEventAction(state.requestEventIdx, fmt.Sprintf("BLOCK: %s", reason))
								resp = blockRequest(reason)
							}
						}
					}
				}
			} else {
				// Unknown direction, pass through
				log.Printf("[IBAC] Unknown direction '%s', passing through", state.direction)
				resp = allowBody()
			}

		case *v3.ProcessingRequest_ResponseHeaders:
			statusText := getHeaderValue(r.ResponseHeaders.Headers.Headers, ":status")
			if statusText != "" {
				fmt.Sscanf(statusText, "%d", &state.statusCode)
			}
			resp = allowResponseHeaders()

		case *v3.ProcessingRequest_ResponseBody:
			body := string(r.ResponseBody.Body)
			sessionID := resolveSessionID(state)

			// Log response event
			if sessionID != "" {
				sc := getOrCreateSession(sessionID)
				action := "logged"
				if state.direction == "outbound" && isTrustedDestination(state.authority) {
					action = "logged (trusted)"
				}
				idx := sc.AddEvent(state.direction, "response", state.method, state.authority, state.path, body)
				sc.SetEventAction(idx, action)
				log.Printf("[IBAC] Logged %s response for session %s: %s%s (%d bytes)",
					state.direction, sessionID, state.authority, state.path, len(body))
			}

			if state.direction == "outbound" && isOllamaDestination(state.authority) && strings.HasSuffix(state.path, "/v1/chat/completions") {
				captureObservedOllamaResponse(sessionID, body)
			}

			if state.direction == "outbound" && state.hasToolCall && isReflectableFinanceRequest(state.authority, state.method, state.path) && state.syntheticResult == "" && sessionID != "" {
				getOrCreateSession(sessionID).AppendConversation(finance.ChatMessage{
					Role:       "tool",
					Content:    body,
					ToolCallID: state.toolCall.ID,
				})
			}

			if state.direction == "inbound" {
				captureInboundAssistantReply(sessionID, body)
			}

			// Clear activeSessionID when inbound response completes
			if state.direction == "inbound" {
				activeSessionID.Store("")
				log.Printf("[IBAC] Cleared active session (inbound response complete)")
			}

			resp = allowResponseBody()

		default:
			log.Printf("[IBAC] Unknown request type: %T", r)
			resp = &v3.ProcessingResponse{}
		}

		if err := stream.Send(resp); err != nil {
			return status.Errorf(codes.Unknown, "cannot send stream response: %v", err)
		}
	}
}

func main() {
	log.Println("[IBAC] Starting ext_proc sidecar on :9090")

	initTrustedDestinations()

	httpClient := &http.Client{Timeout: 300 * time.Second}

	lis, err := net.Listen("tcp", ":9090")
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	grpcServer := grpc.NewServer()
	v3.RegisterExternalProcessorServer(grpcServer, &processor{
		httpClient:   httpClient,
		sparcBaseURL: sparcReflectorURL(),
	})

	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}
