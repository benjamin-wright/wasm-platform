.PHONY: hello
hello:
	cargo build \
		--manifest-path examples/hello-world/Cargo.toml \
		--target wasm32-wasip2 \
		--release

.PHONY: run
run: hello
	cargo run --manifest-path components/execution-host/Cargo.toml

.PHONY: test
test:
	curl -X POST \
		-H "Content-Type: application/json" \
		-d '{"method": "GET", "path": "/hello", "body": "world"}' \
		http://localhost:3000/execute