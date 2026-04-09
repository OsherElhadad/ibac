package main

import (
	"strings"
	"testing"

	"github.com/huang195/ibac/internal/finance"
)

func TestBuildObservedFinanceToolCallMapsFinanceBackendRequests(t *testing.T) {
	tests := []struct {
		method   string
		path     string
		body     string
		toolName string
		argKey   string
		argValue string
	}{
		{method: "GET", path: "/transactions/TX4827", toolName: "get_transaction", argKey: "transaction_id", argValue: "TX4827"},
		{method: "GET", path: "/customers/C921", toolName: "lookup_customer", argKey: "customer_id", argValue: "C921"},
		{method: "GET", path: "/invoices/INV-8834", toolName: "get_invoice", argKey: "invoice_id", argValue: "INV-8834"},
		{method: "POST", path: "/refunds", body: `{"transaction_id":"TX4827","amount":450,"refund_reason":"duplicate charge"}`, toolName: "issue_refund", argKey: "transaction_id", argValue: "TX4827"},
	}

	for _, tc := range tests {
		got, ok := buildObservedFinanceToolCall(tc.method, tc.path, tc.body)
		if !ok {
			t.Fatalf("expected %s %s to map to a finance tool", tc.method, tc.path)
		}
		if got.Function.Name != tc.toolName {
			t.Fatalf("expected tool %s, got %s", tc.toolName, got.Function.Name)
		}
		args := finance.ParseArgs(got.Function.Arguments)
		if args[tc.argKey] != tc.argValue {
			t.Fatalf("expected %s=%s, got %#v", tc.argKey, tc.argValue, args[tc.argKey])
		}
	}
}

func TestParseObservedToolCallsCapturesToolCallsFromOllamaResponse(t *testing.T) {
	body := `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_transaction","arguments":"{\"transaction_id\":\"TX4821\"}"}}]}}]}`

	calls := parseObservedToolCalls(body)
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	if calls[0].Function.Name != "get_transaction" {
		t.Fatalf("expected get_transaction, got %s", calls[0].Function.Name)
	}
}

func TestConsumeMatchingPendingToolCallUsesNormalizedArguments(t *testing.T) {
	sc := &SessionContext{}
	sc.SetPendingToolCalls([]finance.ToolCall{
		{
			ID:   "call_1",
			Type: "function",
			Function: finance.FunctionCall{
				Name:      "issue_refund",
				Arguments: `{"transaction_id":"TX4827","amount":450,"reason":"duplicate charge"}`,
			},
		},
	})

	actual := finance.ToolCall{
		ID:   "observed",
		Type: "function",
		Function: finance.FunctionCall{
			Name:      "issue_refund",
			Arguments: `{"transaction_id":"TX4827","amount":450,"refund_reason":"duplicate charge"}`,
		},
	}

	matched, ok := sc.ConsumeMatchingPendingToolCall(actual)
	if !ok {
		t.Fatalf("expected pending tool call to match")
	}
	if !strings.Contains(matched.Function.Arguments, `"refund_reason":"duplicate charge"`) {
		t.Fatalf("expected normalized refund_reason in matched call, got %s", matched.Function.Arguments)
	}
}

func TestSyntheticClarificationResultDoesNotMentionSPARC(t *testing.T) {
	result := syntheticClarificationResult(finance.ToolCall{
		Function: finance.FunctionCall{
			Name:      "get_transaction",
			Arguments: `{"transaction_id":"TX4821"}`,
		},
	}, []sparcIssue{{
		MetricName:  "general_hallucination_check",
		Explanation: "The transaction_id is not grounded in the conversation.",
	}})

	if strings.Contains(strings.ToLower(result), "sparc") {
		t.Fatalf("synthetic tool result should not mention SPARC: %s", result)
	}
	if !strings.Contains(result, "transaction ID") {
		t.Fatalf("expected synthetic clarification to ask for transaction ID: %s", result)
	}
}
