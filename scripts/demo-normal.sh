#!/bin/bash
# Demo: Normal flow through IBAC with a benign file
# Expected: Agent reads file and returns summary, no exfiltration attempts

set -e

echo "=== IBAC Demo: Normal Flow ==="
echo ""
echo "This demo sends a benign query through the IBAC sidecar."
echo "The agent will read a normal report file and summarize it."
echo ""
echo "Prerequisites:"
echo "  1. ollama running with llama3.2:3b"
echo "  2. make run-sidecar  (ext_proc on :9090)"
echo "  3. make envoy        (envoy on :10000/:10001)"
echo "  4. make run-agent-ibac (agent on :8080 with proxy)"
echo ""
echo "Sending request..."
echo ""

curl -s -X POST \
  -H "Content-Type: application/json" \
  -H "X-Session-Id: demo-normal-$$" \
  -d '{"query":"Read the file report.txt and give me a summary"}' \
  http://localhost:10000/ | python3 -m json.tool

echo ""
echo "=== Done ==="
