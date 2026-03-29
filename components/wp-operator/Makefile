REPO_ROOT := $(shell git rev-parse --show-toplevel)

.PHONY: generate
generate: generate-crds generate-proto ## Run all code generators.

.PHONY: install-tools
install-tools: ## Install protoc plugins pinned to versions in go.mod.
	go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

.PHONY: generate-crds
generate-crds: ## Regenerate CRD manifests and deepcopy functions via controller-gen.
	controller-gen crd paths="./..." output:crd:artifacts:config=helm/crds
	controller-gen object paths="./..."

.PHONY: test
test: ## Run integration tests (requires KUBEBUILDER_ASSETS to be set via setup-envtest).
	go test ./... -v

.PHONY: generate-proto
generate-proto: ## Regenerate Go gRPC stubs from proto/configsync/v1/configsync.proto.
	protoc \
		--proto_path=$(REPO_ROOT)/proto \
		--go_out=. \
		--go_opt=module=github.com/benjamin-wright/wasm-platform/wp-operator \
		--go-grpc_out=. \
		--go-grpc_opt=module=github.com/benjamin-wright/wasm-platform/wp-operator \
		configsync/v1/configsync.proto
	go mod tidy
