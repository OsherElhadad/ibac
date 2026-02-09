.PHONY: build agent agent-ibac sidecar evil-server envoy clean

build: agent sidecar evil-server

agent:
	go build -o bin/agent ./agent/

sidecar:
	go build -o bin/sidecar ./sidecar/

evil-server:
	go build -o bin/evil-server ./evil-server/

# Run agent in direct mode (no IBAC proxy)
run-agent:
	./bin/agent

# Run agent with IBAC proxy (outbound goes through envoy :10001)
run-agent-ibac:
	IBAC_PROXY=http://localhost:10001 ./bin/agent

run-sidecar:
	./bin/sidecar

run-evil-server:
	./bin/evil-server

# Run envoy via func-e (install with: brew install func-e)
envoy:
	func-e run -c $(PWD)/envoy/envoy.yaml

# Alternative: run envoy via Docker
envoy-docker:
	docker run --rm --network=host \
		-v $(PWD)/envoy:/etc/envoy \
		envoyproxy/envoy:v1.28-latest \
		-c /etc/envoy/envoy.yaml

clean:
	rm -rf bin/
