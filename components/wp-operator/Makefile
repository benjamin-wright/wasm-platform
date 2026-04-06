REPO_ROOT := $(shell git rev-parse --show-toplevel)
LOCALBIN ?= $(shell pwd)/bin
ENVTEST_K8S_VERSION ?= 1.32.0

$(LOCALBIN):
	mkdir -p $(LOCALBIN)

.PHONY: generate
generate: generate-proto generate-crds ## Run all code generators.

.PHONY: install-tools
install-tools: ## Install protoc plugins pinned to versions in go.mod.
	go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

.PHONY: generate-crds
generate-crds: ## Regenerate CRD manifests and deepcopy functions via controller-gen.
	controller-gen crd paths="./api/..." output:crd:artifacts:config=helm/crds
	controller-gen object paths="./api/..."

.PHONY: setup-envtest
setup-envtest: $(LOCALBIN) ## Download setup-envtest into ./bin.
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest

.PHONY: test
test: ## Run integration tests.
	KUBEBUILDER_ASSETS="$(shell $(LOCALBIN)/setup-envtest use $(ENVTEST_K8S_VERSION) -p path)" go test ./... -v

.PHONY: generate-proto
generate-proto: ## Regenerate Go gRPC stubs from proto files.
	protoc \
		--proto_path=$(REPO_ROOT)/proto \
		--go_out=. \
		--go_opt=module=github.com/benjamin-wright/wasm-platform/wp-operator \
		--go-grpc_out=. \
		--go-grpc_opt=module=github.com/benjamin-wright/wasm-platform/wp-operator \
		configsync/v1/configsync.proto \
		gateway/v1/gateway.proto
	go mod tidy
