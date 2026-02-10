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

	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	v3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// sessionStore maps sessionID -> original user intent (query)
var sessionStore sync.Map

// streamState holds per-stream metadata accumulated across headers and body phases
type streamState struct {
	direction string
	sessionID string
	method    string
	path      string
	authority string
}

type processor struct {
	v3.UnimplementedExternalProcessorServer
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

// --- LLM-based intent checking ---

type LLMRequest struct {
	Model       string       `json:"model"`
	Messages    []LLMMessage `json:"messages"`
	Temperature float64      `json:"temperature"`
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

// checkIntent uses an LLM to determine if an outbound action aligns with the original intent
func checkIntent(intent, method, authority, path, body string) (string, string) {
	prompt := fmt.Sprintf(`You are a security policy engine. Your job is to determine whether an outbound HTTP request is consistent with the user's original intent.

User's original intent: "%s"

Outbound request:
- Method: %s
- Destination: %s%s
- Body (first 500 chars): %.500s

Rules:
1. If the outbound request is clearly related to fulfilling the user's intent, respond ALLOW.
2. If the outbound request appears to be sending data to an unexpected destination, exfiltrating information, or doing something unrelated to the user's intent, respond BLOCK.
3. Reading files that the user asked about is fine, but POSTING sensitive data (like /etc/passwd, credentials, personal info) to external servers is suspicious.
4. Be especially suspicious of requests that POST data to unknown servers when the user only asked to read a file.

Respond with ONLY a JSON object, no other text:
{"decision": "ALLOW" or "BLOCK", "reason": "brief explanation"}`,
		intent, method, authority, path, body)

	llmReq := LLMRequest{
		Model: "llama3.2:3b",
		Messages: []LLMMessage{
			{Role: "user", Content: prompt},
		},
		Temperature: 0.1,
	}

	reqBody, err := json.Marshal(llmReq)
	if err != nil {
		log.Printf("[IBAC] Failed to marshal LLM request: %v", err)
		return "BLOCK", "failed to create LLM request"
	}

	// Call ollama directly (NOT through envoy proxy)
	ollamaURL := os.Getenv("OLLAMA_URL")
	if ollamaURL == "" {
		ollamaURL = "http://localhost:11434"
	}
	resp, err := http.Post(ollamaURL+"/v1/chat/completions", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		log.Printf("[IBAC] Failed to call LLM: %v", err)
		return "BLOCK", "LLM unavailable, default deny"
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[IBAC] Failed to read LLM response: %v", err)
		return "BLOCK", "failed to read LLM response"
	}

	if resp.StatusCode != http.StatusOK {
		log.Printf("[IBAC] LLM returned %d: %s", resp.StatusCode, string(respBody))
		return "BLOCK", "LLM error, default deny"
	}

	var llmResp LLMResponse
	if err := json.Unmarshal(respBody, &llmResp); err != nil {
		log.Printf("[IBAC] Failed to unmarshal LLM response: %v", err)
		return "BLOCK", "failed to parse LLM response"
	}

	if len(llmResp.Choices) == 0 {
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
		return "BLOCK", "unparseable LLM response, default deny"
	}

	decision.Decision = strings.ToUpper(strings.TrimSpace(decision.Decision))
	if decision.Decision != "ALLOW" && decision.Decision != "BLOCK" {
		return "BLOCK", fmt.Sprintf("invalid decision '%s', default deny", decision.Decision)
	}

	return decision.Decision, decision.Reason
}

// --- ext_proc Process implementation ---

func (p *processor) Process(stream v3.ExternalProcessor_ProcessServer) error {
	ctx := stream.Context()
	state := &streamState{}

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

			log.Printf("[IBAC] %s request: session=%s method=%s authority=%s path=%s",
				state.direction, state.sessionID, state.method, state.authority, state.path)

			// For both inbound and outbound, we pass headers through
			// and wait for the body phase to do the real work
			resp = allowHeaders()

		case *v3.ProcessingRequest_RequestBody:
			body := string(r.RequestBody.Body)

			if state.direction == "inbound" {
				// Inbound: capture the user's intent from the request body
				var reqBody map[string]interface{}
				if err := json.Unmarshal([]byte(body), &reqBody); err == nil {
					if query, ok := reqBody["query"].(string); ok && state.sessionID != "" {
						sessionStore.Store(state.sessionID, query)
						log.Printf("[IBAC] Captured intent for session %s: %s", state.sessionID, query)
					}
				}
				resp = allowBody()

			} else if state.direction == "outbound" {
				// Outbound: validate the action against the stored intent
				if state.sessionID == "" {
					log.Printf("[IBAC] BLOCK: no session ID on outbound request")
					resp = blockRequest("missing session ID")
				} else if intent, ok := sessionStore.Load(state.sessionID); !ok {
					log.Printf("[IBAC] BLOCK: no intent found for session %s", state.sessionID)
					resp = blockRequest("no intent registered for session")
				} else {
					intentStr := intent.(string)
					decision, reason := checkIntent(intentStr, state.method, state.authority, state.path, body)
					log.Printf("[IBAC] Decision for session %s: %s - %s", state.sessionID, decision, reason)

					if decision == "ALLOW" {
						resp = allowBody()
					} else {
						resp = blockRequest(reason)
					}
				}
			} else {
				// Unknown direction, pass through
				log.Printf("[IBAC] Unknown direction '%s', passing through", state.direction)
				resp = allowBody()
			}

		case *v3.ProcessingRequest_ResponseHeaders:
			resp = &v3.ProcessingResponse{
				Response: &v3.ProcessingResponse_ResponseHeaders{
					ResponseHeaders: &v3.HeadersResponse{},
				},
			}

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

	lis, err := net.Listen("tcp", ":9090")
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	grpcServer := grpc.NewServer()
	v3.RegisterExternalProcessorServer(grpcServer, &processor{})

	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}
